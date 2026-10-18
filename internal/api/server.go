package api

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/metrics"
	"github.com/AppTestify/UPI-Switch/internal/ratelimit"
	"github.com/AppTestify/UPI-Switch/internal/recon"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// AdminToken grants an admin-console role.
type AdminToken struct {
	Name  string
	Token string
	Role  string
}

// Config is the HTTP layer's configuration.
type Config struct {
	AdminTokens      []AdminToken
	MaxClockSkew     time.Duration
	DefaultRateLimit int
	TrustProxy       bool
	BankIPAllowlist  []string
	ConsoleUsers     []ConsoleUser
	SessionKey       []byte        // signs console session cookies; sign-in is disabled when empty
	SessionTTL       time.Duration // console session lifetime (default 12h)
	Location         *time.Location
}

// Server wires handlers to the orchestrator.
type Server struct {
	cfg     Config
	svc     *core.Service
	store   *store.Store
	recon   *recon.Engine
	limiter ratelimit.Limiter
	log     *slog.Logger
	metrics *metrics.Registry
}

// New creates the HTTP server.
func New(cfg Config, svc *core.Service, st *store.Store, rc *recon.Engine, lim ratelimit.Limiter, log *slog.Logger, m *metrics.Registry) *Server {
	if cfg.MaxClockSkew <= 0 {
		cfg.MaxClockSkew = 5 * time.Minute
	}
	if cfg.DefaultRateLimit <= 0 {
		cfg.DefaultRateLimit = 500
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if lim == nil {
		lim = ratelimit.NewLocal()
	}
	return &Server{cfg: cfg, svc: svc, store: st, recon: rc, limiter: lim, log: log, metrics: m}
}

// Handler returns the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	tenant := func(h http.HandlerFunc) http.Handler { return s.tenantAuth(h) }
	tenantPost := func(h http.HandlerFunc) http.Handler { return s.tenantAuth(s.idempotent(h)) }

	// Interface 1: payment gateway → switch.
	mux.Handle("POST /v1/upi/intent", tenantPost(s.createIntent(false)))
	mux.Handle("POST /v1/upi/qr", tenantPost(s.createIntent(true)))
	mux.Handle("POST /v1/upi/collect", tenantPost(s.createCollect))
	mux.Handle("GET /v1/upi/status/{id}", tenant(s.getStatus))
	mux.Handle("GET /v1/upi/status", tenant(s.getStatus))
	mux.Handle("POST /v1/upi/refund", tenantPost(s.createRefund))
	mux.Handle("POST /v1/upi/refund/bulk", tenantPost(s.bulkRefund))
	mux.Handle("GET /v1/upi/refund/{id}", tenant(s.getRefund))
	mux.Handle("POST /v1/upi/vpa/validate", tenant(s.validateVPA))
	mux.Handle("POST /v1/upi/mandate", tenantPost(s.createMandate))
	mux.Handle("GET /v1/upi/mandate/{id}", tenant(s.getMandate))
	mux.Handle("POST /v1/upi/mandate/{id}/execute", tenantPost(s.executeMandate))
	mux.Handle("POST /v1/upi/mandate/{id}/revoke", tenantPost(s.revokeMandate))
	mux.Handle("POST /v1/upi/disputes", tenantPost(s.raiseDispute))
	mux.Handle("GET /v1/upi/disputes/{id}", tenant(s.getDispute))
	mux.Handle("GET /v1/upi/reports/recon", tenant(s.tenantRecon))
	mux.Handle("GET /v1/upi/reports/settlements", tenant(s.tenantSettlements))

	// Interface 4: PSP bank → switch callback.
	mux.HandleFunc("POST /v1/npci/callback/{psp}", s.callback)

	// Admin console API (RBAC).
	s.adminRoutes(mux)

	// Operations.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.metrics.Write(w)
	})
	mux.HandleFunc("GET /v1/keys/switch.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(security.EncodePublicKeyPEM(s.svc.Signer().Public()))
	})
	return s.observe(mux)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		fail(w, http.StatusServiceUnavailable, "db_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "psps": s.svc.Router().Health()})
}

// ---- interface 1 handlers ----

func (s *Server) createIntent(qr bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req core.IntentRequest
		if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
			s.writeErr(w, r, err)
			return
		}
		req.QR = req.QR || qr
		res, err := s.svc.CreateIntent(r.Context(), tenantFrom(r.Context()), req)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
	}
}

func (s *Server) createCollect(w http.ResponseWriter, r *http.Request) {
	var req core.CollectRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	txn, err := s.svc.CreateCollect(r.Context(), tenantFrom(r.Context()), req)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	t := tenantFrom(r.Context())
	id, ref := r.PathValue("id"), r.URL.Query().Get("merchant_ref")
	if id == "" && ref == "" {
		fail(w, http.StatusBadRequest, "validation_error", "txn id or merchant_ref is required")
		return
	}
	txn, err := s.svc.GetTxn(r.Context(), t.ID, id, ref)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if r.URL.Query().Get("refresh") == "true" && !txn.Status.IsFinal() && txn.Status != domain.StatusDeemed {
		if err := s.svc.CheckTxnNow(r.Context(), txn); err != nil {
			s.log.Warn("on-demand status check failed", "txn", txn.ID, "err", err)
		}
		if txn, err = s.svc.GetTxn(r.Context(), t.ID, txn.ID, ""); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	refunds, err := s.store.ListRefundsForTxn(r.Context(), txn.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transaction": txn, "refunds": refunds})
}

func (s *Server) createRefund(w http.ResponseWriter, r *http.Request) {
	var req core.RefundRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	ref, err := s.svc.CreateRefund(r.Context(), tenantFrom(r.Context()), req, false)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

type bulkResult struct {
	MerchantRefundRef string         `json:"merchant_refund_ref"`
	Refund            *domain.Refund `json:"refund,omitempty"`
	Error             *core.Error    `json:"error,omitempty"`
}

func (s *Server) bulkRefund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Refunds []core.RefundRequest `json:"refunds"`
	}
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if len(req.Refunds) == 0 || len(req.Refunds) > 1000 {
		fail(w, http.StatusBadRequest, "validation_error", "refunds must contain 1 to 1000 items")
		return
	}
	out := make([]bulkResult, 0, len(req.Refunds))
	for _, item := range req.Refunds {
		ref, err := s.svc.CreateRefund(r.Context(), tenantFrom(r.Context()), item, true)
		res := bulkResult{MerchantRefundRef: item.MerchantRefundRef, Refund: ref}
		if err != nil {
			ce, ok := err.(*core.Error)
			if !ok {
				s.log.Error("bulk refund item", "err", err)
				ce = &core.Error{Code: "internal_error", Message: "internal error"}
			}
			res.Error, res.Refund = ce, nil
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusMultiStatus, map[string]any{"results": out})
}

func (s *Server) getRefund(w http.ResponseWriter, r *http.Request) {
	ref, err := s.svc.GetRefund(r.Context(), tenantFrom(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) validateVPA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VPA string `json:"vpa"`
	}
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	res, err := s.svc.ValidateVPA(r.Context(), tenantFrom(r.Context()), req.VPA)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) createMandate(w http.ResponseWriter, r *http.Request) {
	var req core.MandateRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	m, err := s.svc.CreateMandate(r.Context(), tenantFrom(r.Context()), req)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) getMandate(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.GetMandate(r.Context(), tenantFrom(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) executeMandate(w http.ResponseWriter, r *http.Request) {
	var req core.ExecuteRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	txn, err := s.svc.ExecuteMandate(r.Context(), tenantFrom(r.Context()), r.PathValue("id"), req)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

func (s *Server) revokeMandate(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.RevokeMandate(r.Context(), tenantFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) raiseDispute(w http.ResponseWriter, r *http.Request) {
	var req core.DisputeRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	d, err := s.svc.RaiseDispute(r.Context(), tenantFrom(r.Context()), req)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) getDispute(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.GetDispute(r.Context(), tenantFrom(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func parseDate(r *http.Request) (time.Time, error) {
	d := r.URL.Query().Get("date")
	if d == "" {
		return time.Now().AddDate(0, 0, -1), nil
	}
	return time.Parse("2006-01-02", d)
}

func (s *Server) tenantRecon(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "validation_error", "date must be YYYY-MM-DD")
		return
	}
	run, err := s.store.LatestReconRun(r.Context(), date)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	items, err := s.store.ReconItems(r.Context(), run.ID, tenantFrom(r.Context()).ID, r.URL.Query().Get("all") != "true")
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "date": date.Format("2006-01-02"), "items": items})
}

func (s *Server) tenantSettlements(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "validation_error", "date must be YYYY-MM-DD (settlement date)")
		return
	}
	lines, err := s.store.ListSettlements(r.Context(), tenantFrom(r.Context()).ID, date)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settle_date": date.Format("2006-01-02"), "settlements": lines})
}

// ---- interface 4 ----

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if !ipAllowed(s.clientIP(r), s.cfg.BankIPAllowlist) {
		fail(w, http.StatusForbidden, "ip_not_allowed", "source IP not in bank allow-list")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	ack, err := s.svc.HandleCallback(r.Context(), r.PathValue("psp"), body)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Write(ack)
}
