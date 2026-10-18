package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// complianceRoutes are the merchant, refund, mandate, downtime and turnaround endpoints.
func (s *Server) complianceRoutes(mux *http.ServeMux) {
	tenant := func(h http.HandlerFunc) http.Handler { return s.tenantAuth(h) }
	tenantPost := func(h http.HandlerFunc) http.Handler { return s.tenantAuth(s.idempotent(h)) }
	mux.Handle("POST /v1/upi/merchants", tenantPost(s.registerMerchant))
	mux.Handle("GET /v1/upi/merchants/{id}", tenant(s.getMerchant))
	mux.Handle("POST /v1/upi/mandate/{id}/notify", tenantPost(s.notifyMandate))
	mux.Handle("GET /v1/upi/downtimes", tenant(s.tenantDowntimes))

	a := s.adminAuth
	mux.Handle("GET /admin/merchants", a(RoleViewer, s.adminMerchants))
	mux.Handle("POST /admin/merchants", a(RoleOps, s.adminRegisterMerchant))
	mux.Handle("GET /admin/merchants/{tenant}/{id}", a(RoleViewer, s.adminMerchant))
	mux.Handle("POST /admin/merchants/{tenant}/{id}/decision", a(RoleOps, s.adminDecideMerchant))
	mux.Handle("GET /admin/refunds", a(RoleViewer, s.adminRefunds))
	mux.Handle("POST /admin/refunds", a(RoleOps, s.adminCreateRefund))
	mux.Handle("GET /admin/mandates", a(RoleViewer, s.adminMandates))
	mux.Handle("GET /admin/mandates/{id}", a(RoleViewer, s.adminMandate))
	mux.Handle("POST /admin/mandates/{id}/revoke", a(RoleOps, s.adminRevokeMandate))
	mux.Handle("GET /admin/incidents", a(RoleViewer, s.adminIncidents))
	mux.Handle("GET /admin/compliance/tat", a(RoleViewer, s.adminTAT))
	mux.Handle("GET /admin/compliance/rules", a(RoleViewer, s.adminRules))
}

// withFields renders v as JSON and adds extra fields, for rows that embed a type with its own MarshalJSON.
func withFields(v any, extra map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	for k, x := range extra {
		out[k] = x
	}
	return out
}

func pageArgs(r *http.Request) (limit, offset int) {
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	return max(limit, 0), max(offset, 0)
}

// ---- merchants ----

func (s *Server) registerMerchant(w http.ResponseWriter, r *http.Request) {
	var req core.MerchantRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	t := tenantFrom(r.Context())
	m, err := s.svc.RegisterMerchant(r.Context(), t, req, "tenant:"+t.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) getMerchant(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.GetMerchant(r.Context(), tenantFrom(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, withFields(m, limitsView(m)))
}

// limitsView is the NPCI limit that applies to the merchant today.
func limitsView(m *domain.Merchant) map[string]any {
	c := domain.LimitsFor(m.MCC, m.Verified())
	full := domain.CategoryFor(m.MCC)
	return map[string]any{"limits": map[string]any{
		"category": full.Name, "txn_limit": domain.FormatAmount(c.TxnPaise), "daily_limit": domain.FormatAmount(c.DailyPaise),
		"category_txn_limit": domain.FormatAmount(full.TxnPaise), "needs_kyc_for_higher_limit": !m.Verified() && full.TxnPaise > c.TxnPaise,
		"source": c.Source,
	}}
}

func merchantRow(row store.MerchantRow, days int) map[string]any {
	sr := 0.0
	if row.Count > 0 {
		sr = float64(row.Success) / float64(row.Count)
	}
	// Profile monitoring: compare actual monthly volume with what the merchant declared at onboarding.
	monthly := row.Paise * 30 / int64(max(days, 1))
	drift := row.DeclaredPaise > 0 && monthly > row.DeclaredPaise*3/2
	return withFields(row.Merchant, map[string]any{
		"count": row.Count, "success_rate": sr, "volume": domain.FormatAmount(row.Paise), "volume_paise": row.Paise,
		"profile_drift": drift, "monthly_run_rate": domain.FormatAmount(monthly), "limits": limitsView(row.Merchant)["limits"],
	})
}

func (s *Server) adminMerchants(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	const days = 30
	f := store.MerchantFilter{TenantID: q.Get("tenant"), KYCStatus: q.Get("kyc"), Status: q.Get("status"), Query: q.Get("q"),
		Since: time.Now().AddDate(0, 0, -days)}
	f.Limit, f.Offset = pageArgs(r)
	rows, total, err := s.store.ListMerchants(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	counts, err := s.store.MerchantKYCCounts(r.Context(), f.TenantID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, merchantRow(row, days))
	}
	writeJSON(w, http.StatusOK, map[string]any{"merchants": out, "total": total, "counts": counts, "days": days})
}

func (s *Server) adminMerchant(w http.ResponseWriter, r *http.Request) {
	tenantID, id := r.PathValue("tenant"), r.PathValue("id")
	m, err := s.svc.GetMerchant(r.Context(), tenantID, id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	const days = 30
	rows, _, err := s.store.ListMerchants(r.Context(), store.MerchantFilter{TenantID: tenantID, Query: id, Since: time.Now().AddDate(0, 0, -days), Limit: 50})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := withFields(m, limitsView(m))
	for _, row := range rows {
		if row.ID == id {
			for k, v := range merchantRow(row, days) {
				out[k] = v
			}
			for k, v := range limitsView(m) {
				out[k] = v
			}
		}
	}
	txns, err := s.store.ListTxns(r.Context(), store.TxnFilter{TenantID: tenantID, Merchant: id, Limit: 10})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if txns == nil {
		txns = []*domain.Txn{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"merchant": out, "transactions": txns})
}

func (s *Server) adminRegisterMerchant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string `json:"tenant_id"`
		core.MerchantRequest
	}
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	t, err := s.store.GetTenant(r.Context(), req.TenantID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	m, err := s.svc.RegisterMerchant(r.Context(), t, req.MerchantRequest, actorFrom(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) adminDecideMerchant(w http.ResponseWriter, r *http.Request) {
	var d core.MerchantDecision
	if err := decode(r, nil, &d); err != nil {
		s.writeErr(w, r, err)
		return
	}
	m, err := s.svc.DecideMerchant(r.Context(), r.PathValue("tenant"), r.PathValue("id"), d, actorFrom(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, withFields(m, limitsView(m)))
}

// ---- refunds ----

func (s *Server) adminRefunds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RefundFilter{TenantID: q.Get("tenant"), Status: q.Get("status"), Mode: q.Get("mode"), Query: q.Get("q")}
	f.Limit, f.Offset = pageArgs(r)
	rows, total, err := s.store.ListRefundsAdmin(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	sum, err := s.store.RefundSummary(r.Context(), f.TenantID, time.Now().AddDate(0, 0, -30))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		end := now
		if row.Status.IsFinal() {
			end = row.UpdatedAt
		}
		out = append(out, withFields(row.Refund, map[string]any{
			"merchant_id": row.MerchantID, "payee_name": row.PayeeName, "payer_vpa": row.PayerVPA, "txn_amount": domain.FormatAmount(row.TxnPaise),
			"txn_rrn": row.TxnRRN, "txn_created_at": row.TxnCreatedAt, "age_hours": end.Sub(row.CreatedAt).Hours(),
		}))
	}
	summary := map[string]any{}
	for k, v := range sum {
		summary[k] = map[string]any{"count": v[0], "amount": domain.FormatAmount(v[1])}
	}
	writeJSON(w, http.StatusOK, map[string]any{"refunds": out, "total": total, "summary": summary})
}

func (s *Server) adminCreateRefund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TxnID  string `json:"txn_id"`
		Amount string `json:"amount"`
		Reason string `json:"reason"`
	}
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if req.Reason == "" {
		fail(w, http.StatusBadRequest, "validation_error", "reason is required for refunds raised from the console")
		return
	}
	txn, err := s.store.GetTxnAny(r.Context(), req.TxnID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	t, err := s.store.GetTenant(r.Context(), txn.TenantID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	ref, err := s.svc.CreateRefund(r.Context(), t, core.RefundRequest{TxnID: txn.ID, Amount: req.Amount,
		MerchantRefundRef: "OPS-" + security.RandomToken(6), Actor: actorFrom(r.Context()) + " (" + req.Reason + ")"}, false)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

// ---- mandates ----

func mandateRow(row store.MandateRow) map[string]any {
	extra := map[string]any{"debits": row.Debits, "debited": domain.FormatAmount(row.DebitedPaise), "last_debit_at": row.LastDebitAt,
		"afa_cap": domain.FormatAmount(core.AFACap(row.MCC))}
	if row.NextDebit != nil {
		extra["next_debit_date"] = row.NextDebit.Format("2006-01-02")
	}
	return withFields(row.Mandate, extra)
}

func (s *Server) adminMandates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.MandateFilter{TenantID: q.Get("tenant"), Status: q.Get("status"), Query: q.Get("q")}
	f.Limit, f.Offset = pageArgs(r)
	rows, total, err := s.store.ListMandates(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	counts, err := s.store.MandateCounts(r.Context(), f.TenantID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, mandateRow(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"mandates": out, "total": total, "counts": counts})
}

func (s *Server) adminMandate(w http.ResponseWriter, r *http.Request) {
	rows, _, err := s.store.ListMandates(r.Context(), store.MandateFilter{Query: r.PathValue("id"), Limit: 1})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if len(rows) == 0 {
		fail(w, http.StatusNotFound, "not_found", "mandate not found")
		return
	}
	id := rows[0].ID
	debits, err := s.store.MandateDebits(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	notes, err := s.store.ListNotifications(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	events, err := s.store.Events(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if debits == nil {
		debits = []*domain.Txn{}
	}
	if notes == nil {
		notes = []*domain.MandateNotification{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mandate": mandateRow(rows[0]), "debits": debits, "notifications": notes, "events": events})
}

func (s *Server) adminRevokeMandate(w http.ResponseWriter, r *http.Request) {
	md, err := s.store.GetMandateAny(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	t, err := s.store.GetTenant(r.Context(), md.TenantID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out, err := s.svc.RevokeMandateBy(r.Context(), t, md.ID, actorFrom(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) notifyMandate(w http.ResponseWriter, r *http.Request) {
	var req core.NotifyRequest
	if err := decode(r, bodyFrom(r.Context()), &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	n, err := s.svc.NotifyMandate(r.Context(), tenantFrom(r.Context()), r.PathValue("id"), req)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

// ---- bank downtime ----

func (s *Server) tenantDowntimes(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.Downtimes(r.Context(), time.Now().AddDate(0, 0, -7))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"downtimes": list})
}

func (s *Server) adminIncidents(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 90 {
		days = 30
	}
	list, err := s.svc.Downtimes(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": list, "policy": map[string]any{
		"window": fmt.Sprintf("%.0f minutes", core.DefaultIncidentPolicy.Window.Minutes()), "min_samples": core.DefaultIncidentPolicy.MinSamples,
		"open_at": core.DefaultIncidentPolicy.OpenAt, "high_at": core.DefaultIncidentPolicy.HighAt, "close_below": core.DefaultIncidentPolicy.CloseBelow,
	}})
}

// ---- RBI turnaround and the rules the switch enforces ----

func (s *Server) adminTAT(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 90 {
		days = 30
	}
	cases, err := s.store.TATCases(r.Context(), r.URL.Query().Get("tenant"), time.Now().AddDate(0, 0, -days))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	now := time.Now()
	out := make([]core.TATView, 0, len(cases))
	counts := map[string]int{}
	var owed int64
	for _, c := range cases {
		v := core.EvaluateTAT(c, now, s.cfg.Location)
		out = append(out, v)
		counts[v.State]++
		owed += v.CompensationPaise
	}
	resolved := counts[core.TATResolvedInTime] + counts[core.TATResolvedLate]
	inTAT := 1.0
	if resolved > 0 {
		inTAT = float64(counts[core.TATResolvedInTime]) / float64(resolved)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cases": out, "counts": counts, "compensation": domain.FormatAmount(owed), "resolved_in_tat_rate": inTAT, "days": days,
		"rule": map[string]any{"tat_days": core.TATDays, "compensation_per_day": domain.FormatAmount(core.CompensationPerDay),
			"source": "RBI DPSS.CO.PD No.629/02.01.014/2019-20 (20 Sep 2019); NPCI OC-93"},
	})
}

func (s *Server) adminRules(w http.ResponseWriter, r *http.Request) {
	cfg := s.svc.Config()
	cats := append([]domain.Category{domain.GeneralCategory}, domain.Categories...)
	writeJSON(w, http.StatusOK, map[string]any{
		"status_checks": map[string]any{
			"first_after_seconds": cfg.FirstCheckAfter.Seconds(), "max_checks": cfg.MaxCheckAttempts, "window_minutes": cfg.CheckWindow.Minutes(),
			"source": "NPCI UPI-OC-215 FY25-26",
		},
		"timeouts": map[string]any{
			"ReqPay": npci.Timeout(npci.APIReqPay).Seconds(), "ReqChkTxn": npci.Timeout(npci.APIReqChkTxn).Seconds(),
			"ReqValAdd": npci.Timeout(npci.APIReqValAdd).Seconds(), "source": "NPCI UPI-OC-214 FY25-26",
		},
		"categories": cats,
		"autopay": map[string]any{
			"pre_debit_notice_hours": cfg.PreDebitNotice.Hours(), "afa_cap": domain.FormatAmount(core.AFACap("5411")),
			"afa_cap_insurance_mf": domain.FormatAmount(core.AFACap("6300")), "max_tries": 4,
			"peak_hours": []string{"10:00–13:00", "17:00–21:30"}, "peak_hours_enforced": !cfg.AllowPeakMandates,
			"source": "RBI/DPSS/2026-27/396; NPCI UPI-OC-215-A FY25-26",
		},
		"tat": map[string]any{"days": core.TATDays, "compensation_per_day": domain.FormatAmount(core.CompensationPerDay),
			"source": "RBI DPSS.CO.PD No.629/02.01.014/2019-20"},
		"collect":    map[string]any{"p2p_collect": false, "source": "NPCI circular, 29 Jul 2025"},
		"payee_name": map[string]any{"rule": "registered name of verified merchants", "source": "NPCI UPI-OC-101-A FY25-26"},
	})
}
