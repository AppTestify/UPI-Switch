// Package pspsim simulates a sponsor PSP bank together with the NPCI network and the payer side,
// so the switch can be run and tested end to end without real bank connectivity.
//
// Outcomes are driven by keywords in the payer VPA:
//
//	"nofunds"  → FAILURE Z9 (insufficient funds)     "badpin"  → FAILURE ZM
//	"decline"  → payer declines collect / mandate     "invalid" → ReqValAdd fails (ZH)
//	"deemed"   → RespPay DEEMED (RB); NPCI file later shows SUCCESS
//	"timeout"  → no callback is sent; ReqChkTxn returns SUCCESS (exercises polling)
//	"lost"     → no callback; ReqChkTxn stays PENDING; NPCI file shows FAILED (exercises DEEMED + TCC)
//
// Anything else succeeds.
package pspsim

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/intent"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/security"
)

// Config configures one simulated bank.
type Config struct {
	ID              string        // psp id as registered in the switch, e.g. "bank-a"
	OrgID           string        // 3-letter org id used in message heads
	Handle          string        // VPA handle, e.g. "banka"
	CallbackURL     string        // switch callback URL for this bank
	CallbackDelay   time.Duration // delay before async responses
	TechDeclineRate float64       // probability a ReqPay is negatively acked with a technical code
	DropCallbacks   float64       // probability a callback is silently dropped
	Location        *time.Location
}

type record struct {
	ID       string
	Kind     string // PAYMENT | REFUND
	Amount   int64
	RRN      string
	Result   string // what ReqChkTxn reports
	NPCI     string // status in the NPCI raw file
	Bank     string // status in the bank MIS
	At       time.Time
	Payer    string
	Payee    string
	callback *npci.Message
}

// Sim is one simulated sponsor bank.
type Sim struct {
	cfg       Config
	signer    security.Signer
	switchKey *rsa.PublicKey
	log       *slog.Logger
	http      *http.Client
	down      atomic.Bool

	mu      sync.Mutex
	records map[string]*record
	rate    struct{ tech, drop float64 }
	wg      sync.WaitGroup
}

// New creates a simulator. switchKey verifies the switch's signatures.
func New(cfg Config, signer security.Signer, switchKey *rsa.PublicKey, log *slog.Logger) *Sim {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Sim{cfg: cfg, signer: signer, switchKey: switchKey, log: log.With("psp", cfg.ID),
		http: &http.Client{Timeout: 10 * time.Second}, records: map[string]*record{}}
	s.rate.tech, s.rate.drop = cfg.TechDeclineRate, cfg.DropCallbacks
	return s
}

// SetDown makes the bank return HTTP 503 to every switch request (downtime drill).
func (s *Sim) SetDown(v bool) { s.down.Store(v) }

// SetSwitchKey replaces the switch verification key.
func (s *Sim) SetSwitchKey(k *rsa.PublicKey) { s.mu.Lock(); s.switchKey = k; s.mu.Unlock() }

// SetCallbackURL changes where callbacks go.
func (s *Sim) SetCallbackURL(u string) { s.mu.Lock(); s.cfg.CallbackURL = u; s.mu.Unlock() }

// Wait blocks until all in-flight async callbacks have been sent.
func (s *Sim) Wait() { s.wg.Wait() }

// Handler returns the bank's HTTP routes.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upi/{api}", s.upi)
	mux.HandleFunc("POST /sim/pay", s.payIntent)
	mux.HandleFunc("POST /sim/down", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Down bool }
		json.NewDecoder(r.Body).Decode(&req)
		s.SetDown(req.Down)
		writeJSON(w, map[string]bool{"down": req.Down})
	})
	mux.HandleFunc("POST /sim/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TechDeclineRate *float64 `json:"tech_decline_rate"`
			DropCallbacks   *float64 `json:"drop_callbacks"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		if req.TechDeclineRate != nil {
			s.rate.tech = *req.TechDeclineRate
		}
		if req.DropCallbacks != nil {
			s.rate.drop = *req.DropCallbacks
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /files/{kind}", s.file)
	mux.HandleFunc("GET /keys/psp.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Write(security.EncodePublicKeyPEM(s.signer.Public()))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"ok": !s.down.Load()}) })
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Sim) reply(w http.ResponseWriter, m *npci.Message) {
	b, err := npci.Marshal(m, s.signer)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Write(b)
}

func (s *Sim) upi(w http.ResponseWriter, r *http.Request) {
	if s.down.Load() {
		http.Error(w, "bank unavailable", http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	key := s.switchKey
	s.mu.Unlock()
	m, err := npci.Parse(body, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if m.API() != r.PathValue("api") || m.Txn == nil {
		http.Error(w, "api mismatch", http.StatusBadRequest)
		return
	}
	switch m.API() {
	case npci.APIReqHbt:
		s.reply(w, npci.Response(m, npci.APIRespHbt, s.cfg.OrgID, npci.ResultSuccess, ""))
	case npci.APIReqValAdd:
		s.valAdd(w, m)
	case npci.APIReqChkTxn:
		s.chkTxn(w, m)
	case npci.APIReqPay:
		s.reqPay(w, m)
	case npci.APIReqMandate:
		s.reqMandate(w, m)
	case npci.APIReqComplaint:
		s.reply(w, npci.Ack(m, s.cfg.OrgID, ""))
		resp := npci.Response(m, npci.APIRespComplaint, s.cfg.OrgID, npci.ResultSuccess, "")
		flag := map[string]string{"TCC": "CREDIT_CONFIRMED", "RET": "RETURN_ACCEPTED", "CHARGEBACK": "CHARGEBACK_ACCEPTED"}[m.Complaint.Type]
		if flag == "" {
			flag = "RESOLVED"
		}
		resp.Complaint = &npci.Complaint{Type: m.Complaint.Type, AdjFlag: flag, CRN: npci.NewRRN()}
		s.async(resp)
	default:
		http.Error(w, "unsupported api", http.StatusBadRequest)
	}
}

func (s *Sim) valAdd(w http.ResponseWriter, m *npci.Message) {
	pe := m.Payee()
	if pe == nil || strings.Contains(pe.Addr, "invalid") {
		s.reply(w, npci.Response(m, npci.APIRespValAdd, s.cfg.OrgID, npci.ResultFailure, npci.CodeInvalidVPA))
		return
	}
	resp := npci.Response(m, npci.APIRespValAdd, s.cfg.OrgID, npci.ResultSuccess, "")
	user, _, _ := strings.Cut(pe.Addr, "@")
	resp.Resp.MaskName = strings.ToUpper(security.MaskMobile(user))
	resp.Resp.Type = "PERSON"
	s.reply(w, resp)
}

func (s *Sim) chkTxn(w http.ResponseWriter, m *npci.Message) {
	s.mu.Lock()
	rec := s.records[m.Txn.ID]
	s.mu.Unlock()
	if rec == nil {
		s.reply(w, npci.Response(m, npci.APIRespChkTxn, s.cfg.OrgID, npci.ResultNotFound, ""))
		return
	}
	code := ""
	if rec.callback != nil && rec.callback.Resp != nil {
		code = rec.callback.Resp.ErrCode
	}
	if rec.Result == npci.ResultSuccess {
		code = ""
	}
	resp := npci.Response(m, npci.APIRespChkTxn, s.cfg.OrgID, rec.Result, code)
	resp.Txn.CustRef = rec.RRN
	if rec.Payer != "" {
		resp.Payer = &npci.Party{Addr: rec.Payer, Amount: &npci.Amount{Value: domain.FormatAmount(rec.Amount), Curr: "INR"}}
	}
	s.reply(w, resp)
}

// outcome decides a payment result from the payer VPA.
func outcome(payer string) (result, code, chk, npciStatus string, send bool) {
	switch {
	case strings.Contains(payer, "nofunds"):
		return npci.ResultFailure, npci.CodeInsufficientFunds, npci.ResultFailure, "FAILED", true
	case strings.Contains(payer, "badpin"):
		return npci.ResultFailure, npci.CodeInvalidPIN, npci.ResultFailure, "FAILED", true
	case strings.Contains(payer, "decline"):
		return npci.ResultFailure, npci.CodeCollectDeclined, npci.ResultFailure, "FAILED", true
	case strings.Contains(payer, "deemed"):
		return npci.ResultDeemed, npci.CodeDeemed, npci.ResultDeemed, "SUCCESS", true
	case strings.Contains(payer, "timeout"):
		return npci.ResultSuccess, "", npci.ResultSuccess, "SUCCESS", false
	case strings.Contains(payer, "lost"):
		return "", "", npci.ResultPending, "FAILED", false
	}
	return npci.ResultSuccess, "", npci.ResultSuccess, "SUCCESS", true
}

func (s *Sim) reqPay(w http.ResponseWriter, m *npci.Message) {
	s.mu.Lock()
	tech := s.rate.tech
	s.mu.Unlock()
	if tech > 0 && rand.Float64() < tech {
		s.reply(w, npci.Ack(m, s.cfg.OrgID, npci.CodeBankDown))
		return
	}
	pe := m.Payee()
	if m.Payer == nil || pe == nil || m.Payer.Amount == nil {
		s.reply(w, npci.Ack(m, s.cfg.OrgID, "XB"))
		return
	}
	amt, err := domain.ParseAmount(m.Payer.Amount.Value)
	if err != nil {
		s.reply(w, npci.Ack(m, s.cfg.OrgID, "XB"))
		return
	}
	s.reply(w, npci.Ack(m, s.cfg.OrgID, ""))
	if m.Txn.Type == npci.TxnRefund {
		s.settle(m, "REFUND", amt, m.Payer.Addr, pe.Addr, npci.ResultSuccess, "", npci.ResultSuccess, "SUCCESS", true)
		return
	}
	// COLLECT and MANDATE_EXECUTE: the payer is the customer.
	result, code, chk, npciStatus, send := outcome(m.Payer.Addr)
	s.settle(m, "PAYMENT", amt, m.Payer.Addr, pe.Addr, result, code, chk, npciStatus, send)
}

// settle records the transaction and schedules its RespPay callback.
func (s *Sim) settle(m *npci.Message, kind string, amt int64, payer, payee, result, code, chk, npciStatus string, send bool) {
	rrn := npci.NewRRN()
	resp := npci.Response(m, npci.APIRespPay, s.cfg.OrgID, result, code)
	resp.Txn.CustRef = rrn
	amount := &npci.Amount{Value: domain.FormatAmount(amt), Curr: "INR"}
	resp.Payer = &npci.Party{Addr: payer, Amount: amount, SeqNum: "1"}
	resp.Payees = &npci.Payees{Payee: []npci.Party{{Addr: payee, Amount: amount, SeqNum: "1"}}}
	resp.Resp.Ref = []npci.Ref{{Type: "PAYER", Addr: payer, RespCode: code}, {Type: "PAYEE", Addr: payee, RespCode: code}}
	bank := npciStatus
	rec := &record{ID: m.Txn.ID, Kind: kind, Amount: amt, RRN: rrn, Result: chk, NPCI: npciStatus, Bank: bank,
		At: time.Now(), Payer: payer, Payee: payee, callback: resp}
	s.mu.Lock()
	s.records[m.Txn.ID] = rec
	drop := s.rate.drop
	s.mu.Unlock()
	if !send || (drop > 0 && rand.Float64() < drop) {
		s.log.Info("callback suppressed", "txn", m.Txn.ID)
		return
	}
	s.async(resp)
}

func (s *Sim) reqMandate(w http.ResponseWriter, m *npci.Message) {
	if m.Mandate == nil || m.Payer == nil {
		s.reply(w, npci.Ack(m, s.cfg.OrgID, "XB"))
		return
	}
	s.reply(w, npci.Ack(m, s.cfg.OrgID, ""))
	if m.Mandate.Type != "CREATE" {
		return
	}
	result, code := npci.ResultSuccess, ""
	if strings.Contains(m.Payer.Addr, "decline") {
		result, code = npci.ResultFailure, npci.CodeCollectDeclined
	}
	resp := npci.Response(m, npci.APIRespMandate, s.cfg.OrgID, result, code)
	md := *m.Mandate
	if result == npci.ResultSuccess {
		md.UMN = strings.ToLower(security.RandomToken(16)) + "@" + s.cfg.Handle
	}
	resp.Mandate = &md
	s.async(resp)
}

// payIntent simulates a customer scanning a QR / tapping an intent and approving with their UPI PIN:
// payer PSP → NPCI → debit → credit to the payee at this sponsor bank → RespPay to the switch.
func (s *Sim) payIntent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntentURI string `json:"intent_uri"`
		PayerVPA  string `json:"payer_vpa"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PayerVPA == "" {
		http.Error(w, "intent_uri and payer_vpa required", 400)
		return
	}
	s.mu.Lock()
	key := s.switchKey
	s.mu.Unlock()
	v, err := intent.Parse(req.IntentURI, key)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !strings.HasSuffix(v.Get("pa"), "@"+s.cfg.Handle) {
		http.Error(w, "payee "+v.Get("pa")+" is not held at this bank", 400)
		return
	}
	amt, err := domain.ParseAmount(v.Get("am"))
	if err != nil {
		http.Error(w, "bad amount", 400)
		return
	}
	req0 := npci.New(npci.APIReqPay, s.cfg.OrgID) // the credit leg as NPCI would deliver it
	req0.Txn = &npci.Txn{ID: v.Get("tid"), RefID: v.Get("tr"), Note: v.Get("tn"), Ts: npci.Timestamp(time.Now()), Type: npci.TxnPay, InitiationMode: v.Get("mode")}
	result, code, chk, npciStatus, send := outcome(req.PayerVPA)
	s.settle(req0, "PAYMENT", amt, req.PayerVPA, v.Get("pa"), result, code, chk, npciStatus, send)
	s.mu.Lock()
	rec := s.records[req0.Txn.ID]
	s.mu.Unlock()
	writeJSON(w, map[string]any{"txn_id": req0.Txn.ID, "result": result, "code": code, "rrn": rec.RRN, "callback_sent": send})
}

// async sends a response to the switch callback after the configured delay, retrying on failure.
func (s *Sim) async(resp *npci.Message) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		time.Sleep(s.cfg.CallbackDelay)
		for attempt := 0; attempt < 3; attempt++ {
			if err := s.sendCallback(resp); err != nil {
				s.log.Warn("callback failed", "txn", resp.Txn.ID, "attempt", attempt+1, "err", err)
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
				continue
			}
			return
		}
	}()
}

func (s *Sim) sendCallback(resp *npci.Message) error {
	// Fresh head per attempt; the switch dedupes on msgId, so a retry of the same body would be dropped.
	resp.Head.MsgID = npci.NewMsgID(s.cfg.OrgID)
	resp.Head.Ts = npci.Timestamp(time.Now())
	b, err := npci.Marshal(resp, s.signer)
	if err != nil {
		return err
	}
	s.mu.Lock()
	url, key := s.cfg.CallbackURL, s.switchKey
	s.mu.Unlock()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/xml")
	r, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	rb, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("switch returned HTTP %d: %s", r.StatusCode, rb)
	}
	ack, err := npci.Parse(rb, key)
	if err != nil {
		return err
	}
	if ack.Resp != nil && ack.Resp.Result != npci.ResultSuccess {
		s.log.Warn("switch rejected callback", "txn", resp.Txn.ID, "code", ack.Resp.ErrCode)
	}
	return nil
}

// file serves the bank MIS ("mis") or NPCI raw ("npci-raw") settlement file for a date.
func (s *Sim) file(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind != "mis" && kind != "npci-raw" {
		http.NotFound(w, r)
		return
	}
	date := r.URL.Query().Get("date")
	b := s.File(kind, date)
	sum := sha256.Sum256(b)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("X-Checksum-SHA256", hex.EncodeToString(sum[:]))
	w.Write(b)
}

// File renders a settlement file for a YYYY-MM-DD date.
func (s *Sim) File(kind, date string) []byte {
	s.mu.Lock()
	var recs []*record
	for _, rec := range s.records {
		if rec.At.In(s.cfg.Location).Format("2006-01-02") == date {
			recs = append(recs, rec)
		}
	}
	s.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].At.Before(recs[j].At) })
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	cw.Write([]string{"txn_id", "rrn", "type", "amount", "status", "date"})
	for _, rec := range recs {
		st := rec.NPCI
		if kind == "mis" {
			st = rec.Bank
		}
		cw.Write([]string{rec.ID, rec.RRN, rec.Kind, domain.FormatAmount(rec.Amount), st, date})
	}
	cw.Flush()
	return buf.Bytes()
}
