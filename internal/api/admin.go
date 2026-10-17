package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/recon"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

func (s *Server) adminRoutes(mux *http.ServeMux) {
	a := s.adminAuth
	mux.Handle("POST /admin/tenants", a(RoleAdmin, s.onboardTenant))
	mux.Handle("GET /admin/tenants", a(RoleViewer, s.listTenants))
	mux.Handle("GET /admin/tenants/{id}", a(RoleViewer, s.getTenant))
	mux.Handle("PATCH /admin/tenants/{id}", a(RoleAdmin, s.updateTenant))
	mux.Handle("POST /admin/tenants/{id}/rotate-keys", a(RoleAdmin, s.rotateKeys))
	mux.Handle("GET /admin/psps", a(RoleViewer, s.listPSPs))
	mux.Handle("PUT /admin/psps/{id}", a(RoleAdmin, s.upsertPSP))
	mux.Handle("GET /admin/txns", a(RoleViewer, s.listTxns))
	mux.Handle("GET /admin/txns/{id}", a(RoleViewer, s.getTxnAdmin))
	mux.Handle("POST /admin/txns/{id}/check", a(RoleOps, s.checkTxn))
	mux.Handle("POST /admin/recon/run", a(RoleOps, s.runRecon))
	mux.Handle("GET /admin/recon", a(RoleViewer, s.getRecon))
	mux.Handle("POST /admin/settlements/run", a(RoleOps, s.runSettlement))
	mux.Handle("GET /admin/settlements", a(RoleViewer, s.listSettlements))
	mux.Handle("POST /admin/settlements/{id}/paid", a(RoleOps, s.markPaid))
	mux.Handle("GET /admin/refunds/offline", a(RoleOps, s.offlineRefundFile))
	mux.Handle("POST /admin/refunds/offline-results", a(RoleOps, s.offlineRefundResults))
	mux.Handle("GET /admin/webhooks", a(RoleViewer, s.listWebhooks))
	mux.Handle("POST /admin/webhooks/{id}/replay", a(RoleOps, s.replayWebhook))
	mux.Handle("GET /admin/disputes", a(RoleViewer, s.listDisputes))
	mux.Handle("GET /admin/audit", a(RoleViewer, s.listAudit))
	mux.Handle("GET /admin/audit/verify", a(RoleViewer, s.verifyAudit))
	mux.HandleFunc("GET /admin/", s.console)
}

var prefixRe = regexp.MustCompile(`^[a-z][a-z0-9]{1,19}$`)

type tenantRequest struct {
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	WebhookURL  string          `json:"webhook_url"`
	VPAPrefix   string          `json:"vpa_prefix"`
	IPAllowlist []string        `json:"ip_allowlist"`
	CertCN      string          `json:"cert_cn"`
	FeeBps      *int            `json:"fee_bps"`
	Limits      *domain.Limits  `json:"limits"`
	Routing     *domain.Routing `json:"routing"`
}

func validWebhook(u string) bool {
	if u == "" {
		return true
	}
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != ""
}

// onboardTenant registers a new payment gateway without code changes and returns its secrets once.
func (s *Server) onboardTenant(w http.ResponseWriter, r *http.Request) {
	var req tenantRequest
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if req.Name == "" || !prefixRe.MatchString(req.VPAPrefix) || !validWebhook(req.WebhookURL) {
		fail(w, http.StatusBadRequest, "validation_error", "name, vpa_prefix (lowercase, 2–20 chars) and a valid webhook_url are required")
		return
	}
	t := &domain.Tenant{
		ID: "pg_" + security.RandomToken(6), Name: req.Name, Status: "ACTIVE", APIKeyID: "ak_" + security.RandomToken(10),
		HMACSecret: security.RandomToken(32), WebhookURL: req.WebhookURL, WebhookSecret: "whsec_" + security.RandomToken(24),
		VPAPrefix: req.VPAPrefix, IPAllowlist: req.IPAllowlist, CertCN: req.CertCN,
		Limits: domain.Limits{MaxTxnPaise: 100000_00, VelocityPerVPA: 10, VelocityWindowSecs: 600, RateLimitPerSec: 500},
	}
	if req.FeeBps != nil {
		t.FeeBps = *req.FeeBps
	}
	if req.Limits != nil {
		t.Limits = *req.Limits
	}
	if req.Routing != nil {
		t.Routing = *req.Routing
	}
	err := s.store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.CreateTenant(r.Context(), t); err != nil {
			return err
		}
		return tx.Audit(r.Context(), actorFrom(r.Context()), "tenant.onboard", "tenant", t.ID, map[string]any{"name": t.Name, "vpa_prefix": t.VPAPrefix})
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tenant": t, "api_key_id": t.APIKeyID, "hmac_secret": t.HMACSecret, "webhook_secret": t.WebhookSecret})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListTenants(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": ts})
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	var req tenantRequest
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	var t *domain.Tenant
	err := s.store.InTx(r.Context(), func(tx *store.Store) error {
		var err error
		t, err = tx.GetTenant(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		if req.Name != "" {
			t.Name = req.Name
		}
		if req.Status == "ACTIVE" || req.Status == "SUSPENDED" {
			t.Status = req.Status
		} else if req.Status != "" {
			return &core.Error{Status: http.StatusBadRequest, Code: "validation_error", Message: "status must be ACTIVE or SUSPENDED"}
		}
		if req.WebhookURL != "" {
			if !validWebhook(req.WebhookURL) {
				return &core.Error{Status: http.StatusBadRequest, Code: "validation_error", Message: "invalid webhook_url"}
			}
			t.WebhookURL = req.WebhookURL
		}
		if req.IPAllowlist != nil {
			t.IPAllowlist = req.IPAllowlist
		}
		if req.CertCN != "" {
			t.CertCN = req.CertCN
		}
		if req.FeeBps != nil {
			t.FeeBps = *req.FeeBps
		}
		if req.Limits != nil {
			t.Limits = *req.Limits
		}
		if req.Routing != nil {
			t.Routing = *req.Routing
		}
		if err := tx.UpdateTenant(r.Context(), t); err != nil {
			return err
		}
		return tx.Audit(r.Context(), actorFrom(r.Context()), "tenant.update", "tenant", t.ID, req)
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) rotateKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hs, ws := security.RandomToken(32), "whsec_"+security.RandomToken(24)
	err := s.store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.RotateTenantSecrets(r.Context(), id, hs, ws); err != nil {
			return err
		}
		return tx.Audit(r.Context(), actorFrom(r.Context()), "tenant.rotate_keys", "tenant", id, map[string]any{})
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"hmac_secret": hs, "webhook_secret": ws})
}

func (s *Server) listPSPs(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListPSPs(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	for i := range ps {
		ps[i].PublicKeyPEM = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"psps": ps, "health": s.svc.Router().Health()})
}

func (s *Server) upsertPSP(w http.ResponseWriter, r *http.Request) {
	var p store.PSPRecord
	if err := decode(r, nil, &p); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p.ID = r.PathValue("id")
	if p.Name == "" || p.Endpoint == "" || p.Handle == "" {
		fail(w, http.StatusBadRequest, "validation_error", "name, endpoint and handle are required")
		return
	}
	if p.PublicKeyPEM != "" {
		if _, err := security.ParsePublicKeyPEM([]byte(p.PublicKeyPEM)); err != nil {
			fail(w, http.StatusBadRequest, "validation_error", "public_key_pem: "+err.Error())
			return
		}
	}
	err := s.store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.UpsertPSP(r.Context(), p); err != nil {
			return err
		}
		return tx.Audit(r.Context(), actorFrom(r.Context()), "psp.upsert", "psp", p.ID, map[string]any{"endpoint": p.Endpoint, "enabled": p.Enabled, "cost_bps": p.CostBps})
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.svc.ReloadPSPs(r.Context()); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p.PublicKeyPEM = ""
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listTxns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TxnFilter{TenantID: q.Get("tenant"), Status: q.Get("status")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if d := q.Get("date"); d != "" {
		day, err := time.Parse("2006-01-02", d)
		if err != nil {
			fail(w, http.StatusBadRequest, "validation_error", "date must be YYYY-MM-DD")
			return
		}
		f.From, f.To = s.recon.DayBounds(day)
	}
	txns, err := s.store.ListTxns(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transactions": txns})
}

func (s *Server) getTxnAdmin(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTxnAny(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	events, err := s.store.Events(r.Context(), t.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	refunds, err := s.store.ListRefundsForTxn(r.Context(), t.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transaction": t, "events": events, "refunds": refunds})
}

func (s *Server) checkTxn(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTxnAny(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.svc.CheckTxnNow(r.Context(), t); err != nil {
		s.writeErr(w, r, err)
		return
	}
	t, err = s.store.GetTxnAny(r.Context(), t.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type reconRequest struct {
	Date    string `json:"date"`
	BankCSV string `json:"bank_csv"` // optional: inline files instead of pulling from the banks
	NPCICSV string `json:"npci_csv"`
}

func (s *Server) runRecon(w http.ResponseWriter, r *http.Request) {
	var req reconRequest
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	date, err := time.Parse("2006-01-02", req.Date)
	if err != nil {
		fail(w, http.StatusBadRequest, "validation_error", "date must be YYYY-MM-DD")
		return
	}
	var files *recon.Files
	if req.BankCSV != "" || req.NPCICSV != "" {
		files = &recon.Files{}
		if files.Bank, err = recon.ParseCSV([]byte(req.BankCSV), ""); err == nil {
			files.NPCI, err = recon.ParseCSV([]byte(req.NPCICSV), "")
		}
	} else {
		files, err = s.recon.FetchFiles(r.Context(), date)
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "recon_files", err.Error())
		return
	}
	run, items, err := s.recon.Run(r.Context(), date, files)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.store.Audit(r.Context(), actorFrom(r.Context()), "recon.run", "recon_run", run.ID, map[string]any{"date": req.Date})
	exceptions := []store.ReconItem{}
	for _, it := range items {
		if it.Category != recon.CatMatched && it.Category != recon.CatDeemedResolved && it.Category != recon.CatPendingResolved {
			exceptions = append(exceptions, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "exceptions": exceptions, "items": len(items)})
}

func (s *Server) getRecon(w http.ResponseWriter, r *http.Request) {
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
	items, err := s.store.ReconItems(r.Context(), run.ID, r.URL.Query().Get("tenant"), r.URL.Query().Get("all") != "true")
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "items": items})
}

func (s *Server) runSettlement(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date string `json:"date"`
	}
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	date, err := time.Parse("2006-01-02", req.Date)
	if err != nil {
		fail(w, http.StatusBadRequest, "validation_error", "date (business date of the payments) must be YYYY-MM-DD")
		return
	}
	lines, err := s.recon.Settle(r.Context(), date)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.store.Audit(r.Context(), actorFrom(r.Context()), "settlement.run", "settlement", req.Date, map[string]any{"lines": len(lines)})
	writeJSON(w, http.StatusOK, map[string]any{"settlements": lines})
}

func (s *Server) listSettlements(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "validation_error", "date must be YYYY-MM-DD (settlement date)")
		return
	}
	lines, err := s.store.ListSettlements(r.Context(), r.URL.Query().Get("tenant"), date)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=payout-%s.csv", date.Format("2006-01-02")))
		w.Write(recon.PayoutFile(lines))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settlements": lines})
}

func (s *Server) markPaid(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayoutRef string `json:"payout_ref"`
	}
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if req.PayoutRef == "" {
		fail(w, http.StatusBadRequest, "validation_error", "payout_ref is required")
		return
	}
	err := s.store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.MarkSettlementPaid(r.Context(), r.PathValue("id"), req.PayoutRef); err != nil {
			return err
		}
		return tx.Audit(r.Context(), actorFrom(r.Context()), "settlement.paid", "settlement", r.PathValue("id"), req)
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "PAID"})
}

func (s *Server) offlineRefundFile(w http.ResponseWriter, r *http.Request) {
	refunds, err := s.store.ListRefunds(r.Context(), r.URL.Query().Get("tenant"), string(domain.StatusOfflineQueued), 1000)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	cw := csv.NewWriter(w)
	cw.Write([]string{"refund_id", "txn_id", "tenant_id", "psp_id", "amount", "created_at"})
	for _, rf := range refunds {
		cw.Write([]string{rf.ID, rf.TxnID, rf.TenantID, rf.PSPID, domain.FormatAmount(rf.AmountPaise), rf.CreatedAt.Format(time.RFC3339)})
	}
	cw.Flush()
}

func (s *Server) offlineRefundResults(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Results []core.OfflineRefundResult `json:"results"`
	}
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	n, err := s.svc.ApplyOfflineRefundResults(r.Context(), actorFrom(r.Context()), req.Results)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"applied": n})
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	evts, err := s.store.ListWebhooks(r.Context(), q.Get("tenant"), q.Get("status"), limit)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": evts})
}

func (s *Server) replayWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "validation_error", "id must be numeric")
		return
	}
	err = s.store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.ReplayWebhook(r.Context(), id); err != nil {
			return err
		}
		return tx.Audit(r.Context(), actorFrom(r.Context()), "webhook.replay", "webhook", strconv.FormatInt(id, 10), map[string]any{})
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "PENDING"})
}

func (s *Server) listDisputes(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.ListDisputes(r.Context(), r.URL.Query().Get("tenant"), 200)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disputes": ds})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	es, err := s.store.ListAudit(r.Context(), limit)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": es})
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) {
	broken, n, err := s.store.VerifyAudit(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"intact": broken == 0, "entries": n, "first_broken_id": broken})
}
