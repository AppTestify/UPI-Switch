// Command seed fills a switch database with realistic demo history: five fictional payment
// gateways and their merchants, weeks of UPI payments with a daily traffic curve, success rates per
// sponsor bank, business and technical declines (including one bank incident), deemed payments
// resolved by recon, refunds, T+1 settlements and payouts, reconciliation runs, disputes and a few
// dead-lettered webhooks. It only adds rows for its own tenants and refuses to run twice.
//
//	DATABASE_URL=... DATA_KEY=... go run ./cmd/seed
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AppTestify/UPI-Switch/internal/config"
	"github.com/AppTestify/UPI-Switch/internal/db"
	"github.com/AppTestify/UPI-Switch/internal/demodata"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

type payer struct{ masked, token, enc string }

type pspProfile struct {
	id, handle string
	weight     float64
	sr, td     float64 // success rate and technical-decline rate
}

type dayStats struct {
	total    int
	deemed   []store.ReconItem
	samples  []sampleTxn
	tenantOf map[string]map[string]int
}

type sampleTxn struct {
	id, tenant, rrn string
	paise           int64
}

var (
	ctx   = context.Background()
	rng   = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))
	actor = "console:admin@apptestify.in"
)

func main() {
	days := flag.Int("days", atoi(config.Get("SEED_DAYS", "30")), "days of history")
	perDay := flag.Int("per-day", atoi(config.Get("SEED_PER_DAY", "12000")), "average payments per day")
	hook := flag.String("webhook-url", config.Get("SEED_WEBHOOK_URL", "http://127.0.0.1:9900/webhooks"), "webhook URL for the seeded gateways")
	flag.Parse()

	dataKey := config.Get("DATA_KEY", "")
	if dataKey == "" {
		fail(errors.New("DATA_KEY must match the switch's DATA_KEY"))
	}
	loc, err := time.LoadLocation(config.Get("RECON_TZ", "Asia/Kolkata"))
	if err != nil {
		fail(err)
	}
	pool, err := db.Open(ctx, config.Get("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/upiswitch?sslmode=disable"))
	if err != nil {
		fail(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		fail(err)
	}
	vault := security.NewVault(dataKey)
	st := store.New(pool, vault)

	var seeded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transactions WHERE tenant_id=$1)`, demodata.Tenants[0].ID).Scan(&seeded); err != nil {
		fail(err)
	}
	if seeded {
		fmt.Println("demo data is already present; nothing to do")
		return
	}
	psps, err := profiles(st)
	if err != nil {
		fail(err)
	}

	fmt.Println("onboarding demo payment gateways")
	for _, t := range demodata.Tenants {
		tn := &domain.Tenant{ID: t.ID, Name: t.Name, Status: "ACTIVE", APIKeyID: "ak_" + security.RandomToken(10),
			HMACSecret: security.RandomToken(32), WebhookURL: *hook, WebhookSecret: "whsec_" + security.RandomToken(24),
			VPAPrefix: t.Prefix, FeeBps: t.FeeBps,
			Limits: domain.Limits{MaxTxnPaise: 100000_00, VelocityPerVPA: 10, VelocityWindowSecs: 600, RateLimitPerSec: 500}}
		if _, err := st.GetTenant(ctx, t.ID); err == nil {
			continue // left by an interrupted run
		}
		if err := st.CreateTenant(ctx, tn); err != nil {
			fail(fmt.Errorf("tenant %s: %w", t.ID, err))
		}
		must(st.Audit(ctx, actor, "tenant.onboard", "tenant", t.ID, map[string]any{"name": t.Name, "vpa_prefix": t.Prefix, "source": "demo seed"}))
	}

	fmt.Println("generating payer identities")
	payers := make([]payer, 25000)
	for i := range payers {
		v := demodata.PayerVPA(rng)
		payers[i] = payer{security.MaskVPA(v), vault.Token(v), vault.Encrypt(v)}
	}

	now := time.Now().In(loc)
	start := now.AddDate(0, 0, -*days).Truncate(time.Hour)
	incidentFrom := time.Date(now.Year(), now.Month(), now.Day(), 19, 10, 0, 0, loc).AddDate(0, 0, -3)
	incidentTo := incidentFrom.Add(85 * time.Minute)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	w := newWriter(pool)
	stats := map[string]*dayStats{}
	var successIDs []sampleTxn
	n := 0
	fmt.Printf("writing %d days of payments (~%d a day)\n", *days, *perDay)
	for h := start; h.Before(now); h = h.Add(time.Hour) {
		progress := float64(h.Sub(start)) / float64(now.Sub(start))
		mean := float64(*perDay) / 24 * demodata.HourWeight(h.Hour()) * demodata.DayWeight(h) * (0.82 + 0.3*progress) * (1 + 0.08*rng.NormFloat64())
		count := int(math.Max(0, math.Round(mean)))
		for i := 0; i < count; i++ {
			at := h.Add(time.Duration(rng.Int64N(int64(time.Hour))))
			if at.After(now) {
				continue
			}
			t := demodata.PickTenant(rng)
			m := t.Merchants[rng.IntN(len(t.Merchants))]
			p := pickPSP(psps)
			td := p.td
			if p.id == psps[1%len(psps)].id && at.After(incidentFrom) && at.Before(incidentTo) {
				td = 0.21 // sponsor bank incident: CBS timeouts for 85 minutes
			}
			py := payers[rng.IntN(len(payers))]
			if rng.IntN(5) == 0 { // repeat customers
				py = payers[rng.IntN(len(payers)/20)]
			}
			id := npci.NewTxnID("ATF")
			typ := pickType(m)
			amt := demodata.Amount(rng, m)
			status, code, reason := outcome(p, td, typ)
			final := at.Add(latency(status, code))
			if final.After(now) {
				final = now
			}
			rrn := ""
			if code != "ZH" {
				rrn = fmt.Sprintf("%d%011d", 4+rng.IntN(5), rng.Int64N(1e11))
			}
			if status == domain.StatusDeemed && now.Sub(at) > 26*time.Hour {
				// recon resolved it the next morning
				w.event(id, "", "INITIATED", "api", "created", at)
				w.event(id, "INITIATED", "DEEMED", "callback", "deemed approved; awaiting reconciliation", final)
				resolved := time.Date(at.Year(), at.Month(), at.Day()+1, 2, 15, 0, 0, loc)
				w.event(id, "DEEMED", "SUCCESS", "recon", "resolved by T+1 reconciliation", resolved)
				d := dayOf(stats, at)
				d.deemed = append(d.deemed, store.ReconItem{TenantID: t.ID, TxnID: id, RRN: rrn, Kind: "PAYMENT", Category: "DEEMED_RESOLVED",
					LedgerStatus: "DEEMED", BankStatus: "SUCCESS", NPCIStatus: "SUCCESS", LedgerPaise: amt, BankPaise: amt, NPCIPaise: amt, Action: "marked SUCCESS from NPCI file"})
				status, code, reason, final = domain.StatusSuccess, "00", "resolved by T+1 reconciliation", resolved
			} else {
				w.event(id, "", "INITIATED", "api", "created", at)
				from := "INITIATED"
				if typ == domain.TypeCollect || typ == domain.TypeMandateDebit {
					w.event(id, "INITIATED", "PENDING", "api", "request sent to payer PSP", at.Add(300*time.Millisecond))
					from = "PENDING"
				}
				src := "callback"
				if rng.IntN(12) == 0 {
					src = "chktxn"
				}
				w.event(id, from, string(status), src, reason, final)
			}
			ref := fmt.Sprintf("%s-%s-%07d", strings.ToUpper(t.Prefix[:3]), at.Format("0102"), n)
			w.txn(id, t.ID, ref, string(typ), string(status), amt, t.Prefix+"."+m.ID+"@"+p.handle, m.Name, py, m.ID, m.MCC,
				p.id, rrn, code, reason, at, final)
			d := dayOf(stats, at)
			d.total++
			if d.tenantOf[t.ID] == nil {
				d.tenantOf[t.ID] = map[string]int{}
			}
			d.tenantOf[t.ID]["MATCHED"]++
			if status == domain.StatusSuccess {
				s := sampleTxn{id, t.ID, rrn, amt}
				if rng.IntN(50) == 0 {
					d.samples = append(d.samples, s)
				}
				if at.Before(now.Add(-6*time.Hour)) && rng.IntN(40) == 0 {
					refundAt := at.Add(time.Duration(1+rng.IntN(40)) * time.Hour)
					if refundAt.Before(now) {
						ramt := amt
						if rng.IntN(3) == 0 {
							ramt = amt / 2
						}
						rs := "SUCCESS"
						if rng.IntN(30) == 0 {
							rs = "FAILED"
						}
						w.refund(t.ID, id, ramt, rs, p.id, refundAt)
					}
				}
				if rng.IntN(300) == 0 {
					successIDs = append(successIDs, s)
				}
			}
			n++
			if n%50000 == 0 {
				fmt.Printf("  %d payments\n", n)
			}
		}
	}
	must(w.flush())
	fmt.Printf("  %d payments written\n", n)

	fmt.Println("computing T+1 settlements and payouts")
	must(settle(pool, loc, todayStart))
	fmt.Println("recording reconciliation runs")
	must(reconRuns(st, stats, loc, todayStart))
	fmt.Println("raising disputes and webhook history")
	must(disputes(pool, successIDs, now))
	must(webhooks(pool, now))
	for _, a := range []struct{ action, entity, id string }{
		{"psp.upsert", "psp", psps[0].id}, {"recon.run", "recon_run", todayStart.AddDate(0, 0, -1).Format("2006-01-02")},
		{"settlement.run", "settlement", todayStart.AddDate(0, 0, -1).Format("2006-01-02")},
	} {
		must(st.Audit(ctx, actor, a.action, a.entity, a.id, map[string]any{"source": "demo seed"}))
	}
	fmt.Println("done. Sign in to the admin console to explore the data.")
}

func profiles(st *store.Store) ([]pspProfile, error) {
	var ps []store.PSPRecord
	var err error
	for wait := 0; wait < 30; wait++ { // the switch upserts its PSP_SEED banks at startup
		if ps, err = st.ListPSPs(ctx); err != nil || len(ps) > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, errors.New("no sponsor banks configured; start the switch (with PSP_SEED) before seeding")
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Priority < ps[j].Priority })
	shares := []float64{0.54, 0.31, 0.15}
	srs := []float64{0.957, 0.944, 0.929}
	tds := []float64{0.0062, 0.0091, 0.0148}
	var out []pspProfile
	for i, p := range ps {
		j := min(i, 2)
		out = append(out, pspProfile{p.ID, p.Handle, shares[j], srs[j], tds[j]})
	}
	return out, nil
}

func pickPSP(ps []pspProfile) pspProfile {
	var sum float64
	for _, p := range ps {
		sum += p.weight
	}
	x := rng.Float64() * sum
	for _, p := range ps {
		if x -= p.weight; x <= 0 {
			return p
		}
	}
	return ps[0]
}

func pickType(m demodata.Merchant) domain.TxnType {
	recurring := len(m.Fixed) > 0 || m.MCC == "6300" || m.MCC == "4900" || m.MCC == "8299"
	x := rng.Float64()
	switch {
	case recurring && x < 0.35:
		return domain.TypeMandateDebit
	case x < 0.55:
		return domain.TypeIntent
	case x < 0.84:
		return domain.TypeQR
	}
	return domain.TypeCollect
}

func outcome(p pspProfile, td float64, typ domain.TxnType) (domain.TxnStatus, string, string) {
	sr := p.sr * (1 + 0.006*rng.NormFloat64())
	if typ == domain.TypeCollect {
		sr -= 0.04 // collect requests expire or get ignored more often
	}
	x := rng.Float64()
	switch {
	case x < td:
		d := demodata.PickDecline(rng, true)
		return domain.StatusFailed, d.Code, d.Reason
	case x < td+0.0025:
		return domain.StatusDeemed, "RB", "deemed approved; awaiting reconciliation"
	case x < 1-sr:
		if typ == domain.TypeCollect && rng.IntN(3) == 0 {
			return domain.StatusFailed, "U69", "collect request expired"
		}
		d := demodata.PickDecline(rng, false)
		return domain.StatusFailed, d.Code, d.Reason
	}
	return domain.StatusSuccess, "00", ""
}

// latency is the time from initiation to final status: seconds for approvals, longer for timeouts.
func latency(s domain.TxnStatus, code string) time.Duration {
	median, sigma := 1500.0, 0.5
	switch {
	case s == domain.StatusDeemed:
		median, sigma = 31000, 0.2
	case code == "U69":
		median, sigma = 300000, 0.3
	case s == domain.StatusFailed && (code == "U30" || code == "U28" || code == "U90" || code == "XY" || code == "BT" || code == "U09"):
		median, sigma = 9000, 0.6
	case s == domain.StatusFailed:
		median, sigma = 2300, 0.5
	}
	return time.Duration(median*math.Exp(sigma*rng.NormFloat64())) * time.Millisecond
}

func dayOf(m map[string]*dayStats, t time.Time) *dayStats {
	k := t.Format("2006-01-02")
	if m[k] == nil {
		m[k] = &dayStats{tenantOf: map[string]map[string]int{}}
	}
	return m[k]
}

// writer batches rows and streams them with COPY.
type writer struct {
	pool                *pgxpool.Pool
	txns, events, refns [][]any
	refundN             int
}

func newWriter(pool *pgxpool.Pool) *writer { return &writer{pool: pool} }

var (
	txnCols = []string{"id", "tenant_id", "merchant_ref", "type", "status", "amount_paise", "currency", "payee_vpa", "payee_name",
		"payer_vpa_masked", "payer_vpa_token", "payer_vpa_enc", "merchant_id", "mcc", "note", "psp_id", "rrn", "resp_code", "resp_reason",
		"intent_uri", "mandate_id", "expires_at", "check_attempts", "next_check_at", "created_at", "updated_at"}
	eventCols  = []string{"entity_id", "from_status", "to_status", "source", "detail", "at"}
	refundCols = []string{"id", "tenant_id", "txn_id", "merchant_refund_ref", "amount_paise", "status", "mode", "psp_id", "rrn",
		"resp_code", "resp_reason", "check_attempts", "next_check_at", "created_at", "updated_at"}
)

func (w *writer) txn(id, tenant, ref, typ, status string, amt int64, payee, payeeName string, py payer, mid, mcc, psp, rrn, code, reason string, at, final time.Time) {
	w.txns = append(w.txns, []any{id, tenant, ref, typ, status, amt, "INR", payee, payeeName, py.masked, py.token, py.enc, mid, mcc, "",
		psp, rrn, code, reason, "", "", at.Add(15 * time.Minute), 1, nil, at, final})
	if len(w.txns) >= 20000 {
		must(w.flush())
	}
}

func (w *writer) event(id, from, to, source, detail string, at time.Time) {
	w.events = append(w.events, []any{id, from, to, source, detail, at})
}

func (w *writer) refund(tenant, txn string, amt int64, status, psp string, at time.Time) {
	w.refundN++
	code, reason := "00", ""
	if status == "FAILED" {
		code, reason = "U30", "debit failed at remitter bank"
	}
	w.refns = append(w.refns, []any{npci.NewTxnID("RFD"), tenant, txn, fmt.Sprintf("RF-%07d", w.refundN), amt, status, "ONLINE", psp,
		fmt.Sprintf("%d%011d", 4+rng.IntN(5), rng.Int64N(1e11)), code, reason, 1, nil, at, at.Add(2 * time.Second)})
}

func (w *writer) flush() error {
	for _, b := range []struct {
		table string
		cols  []string
		rows  *[][]any
	}{{"transactions", txnCols, &w.txns}, {"txn_events", eventCols, &w.events}, {"refunds", refundCols, &w.refns}} {
		if len(*b.rows) == 0 {
			continue
		}
		if _, err := w.pool.CopyFrom(ctx, pgx.Identifier{b.table}, b.cols, pgx.CopyFromRows(*b.rows)); err != nil {
			return fmt.Errorf("copy %s: %w", b.table, err)
		}
		*b.rows = (*b.rows)[:0]
	}
	return nil
}

// settle writes one settlement line per merchant per business day, as recon.Engine.Settle would,
// marking every line paid with a UTR except today's, which is still awaiting payout.
func settle(pool *pgxpool.Pool, loc *time.Location, todayStart time.Time) error {
	ids := tenantIDs()
	rows, err := pool.Query(ctx, `
		WITH p AS (
			SELECT tenant_id, merchant_id, (created_at AT TIME ZONE $2)::date AS d, COUNT(*) AS n, SUM(amount_paise) AS gross
			FROM transactions WHERE status='SUCCESS' AND tenant_id = ANY($1) AND created_at < $3 GROUP BY 1,2,3
		), r AS (
			SELECT t.tenant_id, t.merchant_id, (r.created_at AT TIME ZONE $2)::date AS d, SUM(r.amount_paise) AS refunded
			FROM refunds r JOIN transactions t ON t.id=r.txn_id
			WHERE r.status='SUCCESS' AND r.tenant_id = ANY($1) AND r.created_at < $3 GROUP BY 1,2,3
		)
		SELECT COALESCE(p.tenant_id, r.tenant_id), COALESCE(p.merchant_id, r.merchant_id), COALESCE(p.d, r.d),
			COALESCE(p.n,0), COALESCE(p.gross,0), COALESCE(r.refunded,0)
		FROM p FULL OUTER JOIN r ON p.tenant_id=r.tenant_id AND p.merchant_id=r.merchant_id AND p.d=r.d`, ids, loc.String(), todayStart)
	if err != nil {
		return err
	}
	fees := map[string]int{}
	for _, t := range demodata.Tenants {
		fees[t.ID] = t.FeeBps
	}
	var lines [][]any
	today := time.Date(todayStart.Year(), todayStart.Month(), todayStart.Day(), 0, 0, 0, 0, time.UTC)
	for rows.Next() {
		var tenant, merchant string
		var d time.Time
		var n int
		var gross, refunded int64
		if err := rows.Scan(&tenant, &merchant, &d, &n, &gross, &refunded); err != nil {
			rows.Close()
			return err
		}
		settleDate := d.AddDate(0, 0, 1)
		fee := gross * int64(fees[tenant]) / 10000
		status, ref := "PAID", fmt.Sprintf("UTR%s%08d", settleDate.Format("060102"), rng.IntN(1e8))
		if !settleDate.Before(today) {
			status, ref = "PENDING_PAYOUT", ""
		}
		created := time.Date(settleDate.Year(), settleDate.Month(), settleDate.Day(), 6, 0, 0, 0, loc)
		lines = append(lines, []any{npci.NewTxnID("STL"), tenant, merchant, settleDate, n, gross, refunded, fee, gross - refunded - fee, status, ref, created})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"settlements"}, []string{"id", "tenant_id", "merchant_id", "settle_date", "txn_count",
		"gross_paise", "refund_paise", "fee_paise", "net_paise", "status", "payout_ref", "created_at"}, pgx.CopyFromRows(lines))
	return err
}

// reconRuns records a completed three-way recon for each past day: almost everything matched,
// deemed payments resolved from the NPCI file, and a handful of exceptions on recent days.
func reconRuns(st *store.Store, stats map[string]*dayStats, loc *time.Location, todayStart time.Time) error {
	for date, d := range stats {
		day, _ := time.ParseInLocation("2006-01-02", date, loc)
		if !day.Before(todayStart) {
			continue
		}
		items := append([]store.ReconItem{}, d.deemed...)
		age := int(todayStart.Sub(day).Hours() / 24)
		if age <= 4 && len(d.samples) >= 3 {
			s := d.samples
			items = append(items,
				store.ReconItem{TenantID: s[0].tenant, TxnID: s[0].id, RRN: s[0].rrn, Kind: "PAYMENT", Category: "MISSING_IN_BANK",
					LedgerStatus: "SUCCESS", NPCIStatus: "SUCCESS", LedgerPaise: s[0].paise, NPCIPaise: s[0].paise, Action: "chase sponsor bank MIS"},
				store.ReconItem{TenantID: s[1].tenant, TxnID: s[1].id, RRN: s[1].rrn, Kind: "PAYMENT", Category: "AMOUNT_MISMATCH",
					LedgerStatus: "SUCCESS", BankStatus: "SUCCESS", NPCIStatus: "SUCCESS", LedgerPaise: s[1].paise, BankPaise: s[1].paise - 100,
					NPCIPaise: s[1].paise, Action: "hold settlement; amount differs"})
			if age <= 1 {
				items = append(items, store.ReconItem{TenantID: s[2].tenant, TxnID: s[2].id, RRN: s[2].rrn, Kind: "PAYMENT", Category: "STATUS_MISMATCH",
					LedgerStatus: "SUCCESS", BankStatus: "SUCCESS", NPCIStatus: "FAILED", LedgerPaise: s[2].paise, BankPaise: s[2].paise,
					Action: "ledger SUCCESS but NPCI FAILED; hold settlement and investigate"})
			}
		}
		cats := map[string]int{}
		for _, it := range items {
			cats[it.Category]++
			d.tenantOf[it.TenantID][it.Category]++
			d.tenantOf[it.TenantID]["MATCHED"]--
		}
		cats["MATCHED"] = d.total - len(items)
		sum, _ := json.Marshal(map[string]any{"date": date, "total": d.total, "categories": cats, "tenants": d.tenantOf,
			"exceptions": len(items) - len(d.deemed)})
		run := &store.ReconRun{ID: npci.NewTxnID("RCN"), Date: day, Status: "COMPLETED", Summary: sum}
		if err := st.SaveReconRun(ctx, run, items); err != nil {
			return err
		}
		created := time.Date(day.Year(), day.Month(), day.Day()+1, 2, 15, 0, 0, loc)
		if _, err := st.Pool().Exec(ctx, `UPDATE recon_runs SET created_at=$2 WHERE id=$1`, run.ID, created); err != nil {
			return err
		}
	}
	return nil
}

func disputes(pool *pgxpool.Pool, txns []sampleTxn, now time.Time) error {
	types := []struct{ typ, reason, remarks string }{
		{"TCC", "U008", "customer debited, merchant not credited"},
		{"RET", "U010", "goods not delivered; refund requested"},
		{"CHARGEBACK", "U022", "customer disputes the payment with remitter bank"},
		{"COMPLAINT", "U005", "duplicate debit reported by customer"},
	}
	var rows [][]any
	for i, t := range txns {
		if i >= 28 {
			break
		}
		k := types[rng.IntN(len(types))]
		created := now.Add(-time.Duration(rng.IntN(20*24)) * time.Hour)
		status, resolution := "RAISED", ""
		switch age := now.Sub(created); {
		case age > 7*24*time.Hour:
			status, resolution = "RESOLVED_ACCEPTED", "refunded to customer"
			if rng.IntN(3) == 0 {
				status, resolution = "RESOLVED_REJECTED", "merchant proof of delivery accepted"
			}
		case age > 2*24*time.Hour:
			status = "UNDER_REVIEW"
		}
		src := "tenant:" + t.tenant
		if k.typ == "TCC" {
			src = "recon"
		}
		rows = append(rows, []any{npci.NewTxnID("DSP"), t.tenant, t.id, k.typ, status, k.reason, k.remarks, t.paise, resolution, src, created, created.Add(time.Hour)})
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"disputes"}, []string{"id", "tenant_id", "txn_id", "type", "status", "reason_code",
		"remarks", "amount_paise", "resolution", "source", "created_at", "updated_at"}, pgx.CopyFromRows(rows))
	return err
}

// webhooks records recent deliveries, plus a short outage at one gateway that left events dead-lettered.
func webhooks(pool *pgxpool.Pool, now time.Time) error {
	rows, err := pool.Query(ctx, `SELECT id, tenant_id, status, created_at, updated_at FROM transactions
		WHERE tenant_id = ANY($1) AND status IN ('SUCCESS','FAILED') ORDER BY created_at DESC LIMIT 400`, tenantIDs())
	if err != nil {
		return err
	}
	var out [][]any
	dead := 0
	for rows.Next() {
		var id, tenant, status string
		var created, updated time.Time
		if err := rows.Scan(&id, &tenant, &status, &created, &updated); err != nil {
			rows.Close()
			return err
		}
		typ := "txn.success"
		if status == "FAILED" {
			typ = "txn.failed"
		}
		payload, _ := json.Marshal(map[string]any{"event_id": "evt_" + id, "type": typ, "created_at": updated.UTC().Format(time.RFC3339),
			"data": map[string]any{"txn_id": id, "status": status}})
		row := []any{tenant, "evt_" + id, typ, payload, "DELIVERED", 1, updated, "", updated, updated.Add(180 * time.Millisecond)}
		if tenant == "pg_orbit" && dead < 6 {
			dead++
			row[4], row[5], row[7], row[9] = "DEAD", 8, "POST https://hooks.orbitpg.example/upi: 503 Service Unavailable", nil
		}
		out = append(out, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"webhook_outbox"}, []string{"tenant_id", "event_id", "event_type", "payload", "status",
		"attempts", "next_attempt_at", "last_error", "created_at", "delivered_at"}, pgx.CopyFromRows(out))
	return err
}

func tenantIDs() []string {
	var ids []string
	for _, t := range demodata.Tenants {
		ids = append(ids, t.ID)
	}
	return ids
}

func atoi(s string) int {
	var n int
	fmt.Sscan(s, &n)
	return n
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "seed:", err)
	os.Exit(1)
}
