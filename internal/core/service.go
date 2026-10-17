// Package core is the transaction orchestrator: it runs the payment state machine for intent, QR,
// collect and AutoPay payments, refunds and disputes, and talks to sponsor PSP banks.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/metrics"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/risk"
	"github.com/AppTestify/UPI-Switch/internal/router"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// Config tunes the orchestrator.
type Config struct {
	OrgID            string        // 3-letter org prefix for txn ids and message heads
	IntentExpiry     time.Duration // validity of an intent / dynamic QR
	CollectExpiry    time.Duration // how long the payer has to approve a collect
	FirstCheckAfter  time.Duration // no callback within this → ReqChkTxn
	MaxBackoff       time.Duration // cap on the poll backoff
	MaxCheckAttempts int           // after this many inconclusive checks a PENDING txn becomes DEEMED
	CallbackMaxSkew  time.Duration // replay window for bank callbacks
}

func (c *Config) defaults() {
	if c.OrgID == "" {
		c.OrgID = "ATF"
	}
	if c.IntentExpiry <= 0 {
		c.IntentExpiry = 15 * time.Minute
	}
	if c.CollectExpiry <= 0 {
		c.CollectExpiry = 15 * time.Minute
	}
	if c.FirstCheckAfter <= 0 {
		c.FirstCheckAfter = 60 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 10 * time.Minute
	}
	if c.MaxCheckAttempts <= 0 {
		c.MaxCheckAttempts = 6
	}
	if c.CallbackMaxSkew <= 0 {
		c.CallbackMaxSkew = 5 * time.Minute
	}
}

// Service is the orchestrator.
type Service struct {
	cfg    Config
	store  *store.Store
	client *npci.Client
	router *router.Router
	risk   *risk.Engine
	vault  *security.Vault
	signer security.Signer
	log    *slog.Logger
	m      *metrics.Registry

	mu    sync.RWMutex
	peers map[string]npci.Peer
}

// Deps are the collaborators of the orchestrator.
type Deps struct {
	Store   *store.Store
	Signer  security.Signer
	Vault   *security.Vault
	Router  *router.Router
	Risk    *risk.Engine
	HTTP    *http.Client
	Logger  *slog.Logger
	Metrics *metrics.Registry
}

// New builds the orchestrator.
func New(cfg Config, d Deps) *Service {
	cfg.defaults()
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = metrics.New()
	}
	return &Service{
		cfg: cfg, store: d.Store, router: d.Router, risk: d.Risk, vault: d.Vault, signer: d.Signer, log: d.Logger, m: d.Metrics,
		client: &npci.Client{OrgID: cfg.OrgID, Signer: d.Signer, HTTP: d.HTTP},
		peers:  map[string]npci.Peer{},
	}
}

// Config returns the effective configuration.
func (s *Service) Config() Config { return s.cfg }

// Router exposes the PSP router (admin view).
func (s *Service) Router() *router.Router { return s.router }

// Signer exposes the HSM signer (public key endpoint).
func (s *Service) Signer() security.Signer { return s.signer }

// ReloadPSPs refreshes sponsor-bank config and keys from the config DB.
func (s *Service) ReloadPSPs(ctx context.Context) error {
	recs, err := s.store.ListPSPs(ctx)
	if err != nil {
		return err
	}
	peers := map[string]npci.Peer{}
	var banks []domain.PSPBank
	for _, r := range recs {
		var p npci.Peer
		p.ID, p.Endpoint = r.ID, r.Endpoint
		if r.PublicKeyPEM != "" {
			k, err := security.ParsePublicKeyPEM([]byte(r.PublicKeyPEM))
			if err != nil {
				return fmt.Errorf("psp %s public key: %w", r.ID, err)
			}
			p.Key = k
		} else {
			s.log.Warn("psp has no public key; disabling", "psp", r.ID)
			r.Enabled = false
		}
		peers[r.ID] = p
		banks = append(banks, r.PSPBank)
	}
	s.mu.Lock()
	s.peers = peers
	s.mu.Unlock()
	s.router.SetPSPs(banks)
	return nil
}

func (s *Service) peer(id string) (npci.Peer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.peers[id]
	if !ok || p.Key == nil {
		return npci.Peer{}, fmt.Errorf("unknown psp %q", id)
	}
	return p, nil
}

// exchange sends one message to a PSP and feeds latency/outcome to the router.
func (s *Service) exchange(ctx context.Context, pspID string, m *npci.Message) (*npci.Message, error) {
	p, err := s.peer(pspID)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := s.client.Exchange(ctx, p, m)
	lat := time.Since(start)
	tech := npci.IsTechnical(err)
	if err == nil && resp != nil && resp.Resp != nil && npci.IsTechnicalDecline(resp.Resp.ErrCode) {
		tech = true
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.router.Record(pspID, lat, tech, msg)
	s.m.ObserveBank(pspID, m.API(), lat, err == nil)
	return resp, err
}

// ---- errors ----

// Error is an API-facing error with an HTTP status and machine code.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TxnID   string `json:"txn_id,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errValidation(format string, a ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "validation_error", Message: fmt.Sprintf(format, a...)}
}
func errNotFound(what string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: what + " not found"}
}
func errUnprocessable(code, format string, a ...any) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: code, Message: fmt.Sprintf(format, a...)}
}
func errUpstream(format string, a ...any) *Error {
	return &Error{Status: http.StatusBadGateway, Code: "bank_unavailable", Message: fmt.Sprintf(format, a...)}
}

func storeErr(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound(what)
	}
	return err
}

// ---- helpers ----

var (
	vpaRe  = regexp.MustCompile(`^[a-zA-Z0-9.\-_]{2,256}@[a-zA-Z][a-zA-Z0-9]{1,64}$`)
	mccRe  = regexp.MustCompile(`^\d{4}$`)
	refRe  = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,64}$`)
	slugRe = regexp.MustCompile(`[^a-z0-9]`)
)

// ValidVPA reports whether s looks like a UPI virtual payment address.
func ValidVPA(s string) bool { return vpaRe.MatchString(s) }

func payeeVPA(t *domain.Tenant, merchantID, handle string) string {
	m := slugRe.ReplaceAllString(strings.ToLower(merchantID), "")
	if len(m) > 40 {
		m = m[:40]
	}
	if m == "" {
		return t.VPAPrefix + "@" + handle
	}
	return t.VPAPrefix + "." + m + "@" + handle
}

func (s *Service) backoff(attempts int) time.Duration {
	d := s.cfg.FirstCheckAfter
	for i := 0; i < attempts && d < s.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > s.cfg.MaxBackoff {
		d = s.cfg.MaxBackoff
	}
	return d
}

func ptr[T any](v T) *T { return &v }

func eventID(entity, status string) string { return entity + ":" + status }

// Merchant identifies the PG's sub-merchant on a payment.
type Merchant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MCC  string `json:"mcc"`
}

func (m Merchant) validate() error {
	if m.ID == "" || !refRe.MatchString(m.ID) {
		return errValidation("merchant.id is required (letters, digits, . _ -; max 64)")
	}
	if m.Name == "" || len(m.Name) > 99 {
		return errValidation("merchant.name is required (max 99 chars)")
	}
	if !mccRe.MatchString(m.MCC) {
		return errValidation("merchant.mcc must be a 4-digit merchant category code")
	}
	return nil
}

func validateRef(name, v string) error {
	if !refRe.MatchString(v) {
		return errValidation("%s is required (letters, digits, . _ -; max 64)", name)
	}
	return nil
}
