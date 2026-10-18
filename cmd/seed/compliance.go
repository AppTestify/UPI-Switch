package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/demodata"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// legalNames are the registered names payers see once a merchant's KYC is verified (NPCI OC-101-A).
var legalNames = map[string]string{
	"freshbasket": "FreshBasket Retail Private Limited", "tiffinbox": "Tiffinbox Foods LLP", "voltmart": "VoltMart Electronics Private Limited",
	"yatrago": "YatraGo Travel Services Private Limited", "streamly": "Streamly Media Private Limited", "carepharma": "CarePharma Healthcare Private Limited",
	"fuelpoint": "FuelPoint Energy Retail LLP", "learnly": "Learnly Edutech Private Limited", "brightbills": "BrightBills Payments Private Limited",
	"urbanthreads": "UrbanThreads Apparel Private Limited", "cafecraft": "CafeCraft Hospitality LLP", "playarena": "PlayArena Interactive Private Limited",
	"homenest": "HomeNest Furnishings Private Limited", "quickride": "QuickRide Mobility Private Limited", "kiranaplus": "Sri Lakshmi Kirana Stores",
	"medicheck": "MediCheck Diagnostics Private Limited", "insuresmart": "InsureSmart Insurance Brokers Private Limited",
}

// extraMerchants are onboarded but take no seeded traffic: they show the KYC queue, a rejection and a suspension.
var extraMerchants = []struct {
	tenant, id, name, legal, mcc, kyc, status, remarks string
	ageDays                                            int
}{
	{"pg_kestrel", "glowcare", "GlowCare Cosmetics", "GlowCare Beauty Private Limited", "5977", domain.KYCPending, domain.MerchantActive, "", 1},
	{"pg_bluefin", "goldleaf", "GoldLeaf Jewellers", "GoldLeaf Jewellers LLP", "5944", domain.KYCPending, domain.MerchantActive, "", 2},
	{"pg_monsoon", "citycabs", "CityCabs", "CityCabs Transport Services", "4121", domain.KYCPending, domain.MerchantActive, "", 0},
	{"pg_saffron", "quickloan", "QuickLoan Kiosk", "QuickLoan Kiosk", "6012", domain.KYCRejected, domain.MerchantActive,
		"PAN name does not match the settlement account holder; GSTIN shows as cancelled", 6},
	{"pg_orbit", "luckyspin", "LuckySpin Rewards", "LuckySpin Rewards Private Limited", "7995", domain.KYCVerified, domain.MerchantSuspended,
		"Suspended: chargeback ratio above 1% for two weeks and business model under review", 140},
}

// seedMerchant is a demo merchant as onboarded at a gateway.
type seedMerchant struct {
	domain.Merchant
	created time.Time
}

func (m *seedMerchant) payeeName() string {
	if m.Verified() {
		return m.LegalName
	}
	return m.DisplayName
}

func fakePAN(kind byte) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := []byte{letters[rng.IntN(26)], letters[rng.IntN(26)], letters[rng.IntN(26)], kind, letters[rng.IntN(26)]}
	return fmt.Sprintf("%s%04d%c", b, rng.IntN(10000), letters[rng.IntN(26)])
}

// merchantPlan decides each demo merchant's KYC state: almost all verified, one still pending.
func merchantPlan(vault *security.Vault, now time.Time) map[string]*seedMerchant {
	plan := map[string]*seedMerchant{}
	add := func(tenant, id, name, legal, mcc, kyc, status, remarks, source string, created time.Time) {
		kind := byte('C')
		if strings.HasSuffix(legal, "LLP") {
			kind = 'F'
		} else if !strings.Contains(legal, "Limited") {
			kind = 'P'
		}
		pan := fakePAN(kind)
		m := &seedMerchant{created: created, Merchant: domain.Merchant{TenantID: tenant, ID: id, MID: "ATFM" + strings.ToUpper(security.RandomToken(5)),
			LegalName: legal, DisplayName: name, MCC: mcc, BusinessType: "REGULAR", KYCStatus: kyc, Status: status, Remarks: remarks, Source: source,
			PANMasked: pan[:3] + "XXXX" + pan[7:], PANEnc: vault.Encrypt(pan)}}
		if kind == 'P' {
			m.BusinessType = "SMALL"
		} else {
			m.GSTIN = fmt.Sprintf("%02d%s1Z%d", []int{27, 29, 7, 33, 24, 9}[rng.IntN(6)], pan, rng.IntN(10))
		}
		switch {
		case kyc == domain.KYCVerified && rng.IntN(4) == 0:
			m.KYCMethod, m.KYCRef = "PAN", "NSDL-"+strings.ToUpper(security.RandomToken(4))
		case kyc == domain.KYCVerified || rng.IntN(2) == 0:
			m.KYCMethod, m.KYCRef = "CKYC", fmt.Sprintf("XXXXXXXXXX%04d", rng.IntN(10000))
		default:
			m.KYCMethod = "PAN"
		}
		if kyc == domain.KYCVerified {
			v := created.Add(time.Duration(20+rng.IntN(50)) * time.Hour)
			m.VerifiedAt = &v
		}
		plan[tenant+"/"+id] = m
	}
	for _, t := range demodata.Tenants {
		for _, dm := range t.Merchants {
			kyc, source := domain.KYCVerified, "API"
			created := now.AddDate(0, 0, -(45 + rng.IntN(400)))
			if t.ID == "pg_monsoon" && dm.ID == "kiranaplus" {
				// started taking payments before the gateway sent its KYC: registered on first payment, in review
				kyc, source, created = domain.KYCPending, "FIRST_PAYMENT", now.AddDate(0, 0, -9)
			}
			add(t.ID, dm.ID, dm.Name, legalNames[dm.ID], dm.MCC, kyc, domain.MerchantActive, "", source, created)
		}
	}
	for _, x := range extraMerchants {
		add(x.tenant, x.id, x.name, x.legal, x.mcc, x.kyc, x.status, x.remarks, "API", now.AddDate(0, 0, -x.ageDays).Add(-time.Duration(rng.IntN(20))*time.Hour))
	}
	return plan
}

// writeMerchants stores the plan, backdated, with declared monthly volumes close to what each merchant actually does
// (one under-declared, so profile monitoring has something to flag).
func writeMerchants(st *store.Store, pool *pgxpool.Pool, plan map[string]*seedMerchant, now time.Time) error {
	for _, m := range plan {
		if err := st.InsertMerchant(ctx, &m.Merchant, true); err != nil {
			return fmt.Errorf("merchant %s/%s: %w", m.TenantID, m.ID, err)
		}
		updated := m.created
		if m.VerifiedAt != nil {
			updated = *m.VerifiedAt
		}
		if m.Status == domain.MerchantSuspended || m.KYCStatus == domain.KYCRejected {
			updated = now.Add(-time.Duration(30+rng.IntN(60)) * time.Hour)
		}
		if _, err := pool.Exec(ctx, `UPDATE merchants SET created_at=$3, updated_at=$4 WHERE tenant_id=$1 AND id=$2`, m.TenantID, m.ID, m.created, updated); err != nil {
			return err
		}
		if err := st.Audit(ctx, actor, "merchant.register", "merchant", m.TenantID+"/"+m.ID, map[string]any{"mid": m.MID, "mcc": m.MCC, "source": "demo seed"}); err != nil {
			return err
		}
	}
	rows, err := pool.Query(ctx, `SELECT tenant_id, merchant_id, SUM(amount_paise) FROM transactions
		WHERE tenant_id = ANY($1) AND status='SUCCESS' AND created_at >= $2 GROUP BY 1, 2`, tenantIDs(), now.AddDate(0, 0, -30))
	if err != nil {
		return err
	}
	vol := map[string]int64{}
	for rows.Next() {
		var t, m string
		var v int64
		if err := rows.Scan(&t, &m, &v); err != nil {
			rows.Close()
			return err
		}
		vol[t+"/"+m] = v
	}
	rows.Close()
	for k, v := range vol {
		f := 1.15 + rng.Float64()*0.45
		if k == "pg_kestrel/voltmart" {
			f = 0.4 // declared ₹ much lower than it now processes
		}
		lakh := int64(100_000 * 100)
		declared := int64(math.Ceil(float64(v)*f/float64(lakh))) * lakh
		t, m, _ := strings.Cut(k, "/")
		if _, err := pool.Exec(ctx, `UPDATE merchants SET declared_paise=$3 WHERE tenant_id=$1 AND id=$2`, t, m, declared); err != nil {
			return err
		}
	}
	return nil
}

var (
	mandateCols = []string{"id", "tenant_id", "merchant_ref", "status", "payer_vpa_masked", "payer_vpa_token", "payer_vpa_enc", "payee_vpa",
		"merchant_id", "mcc", "max_amount_paise", "frequency", "start_date", "end_date", "umn", "psp_id", "resp_code", "created_at", "updated_at"}
	noticeCols = []string{"id", "mandate_id", "tenant_id", "seq", "amount_paise", "debit_date", "status", "attempts", "txn_id", "notified_at", "updated_at"}
)

// offPeak moves an AutoPay debit out of NPCI's peak hours (10:00–13:00 and 17:00–21:30), keeping its day.
func offPeak(at time.Time, loc *time.Location) time.Time {
	if ok, _ := core.OffPeak(at, loc); ok {
		return at
	}
	l := at.In(loc)
	h := []int{6, 7, 8, 9, 14, 15, 16, 22}[rng.IntN(8)]
	return time.Date(l.Year(), l.Month(), l.Day(), h, rng.IntN(60), rng.IntN(60), 0, loc)
}

// mandate writes the AutoPay mandate behind a seeded debit, the pre-debit notification that preceded it
// and, for mandates due again in the next two days, the notification already sent for that debit.
// It returns the mandate id and the debit amount (capped at what the payer may be debited without a PIN).
func (w *writer) mandate(tenant string, m demodata.Merchant, py payer, payee, psp, handle, txnID string, amt int64, status domain.TxnStatus, at, now time.Time) (string, int64) {
	amt = min(amt, core.AFACap(m.MCC))
	freq, months := "MONTHLY", 1
	if m.MCC == "6300" && rng.IntN(3) == 0 {
		freq, months = "YEARLY", 12
	}
	cycles := 1 + rng.IntN(11)
	start := at.AddDate(0, -cycles*months, 0)
	if freq == "YEARLY" {
		cycles = 1
		start = at.AddDate(-1, 0, 0)
	}
	maxAmt := amt
	if len(m.Fixed) == 0 {
		maxAmt = min(int64(math.Ceil(float64(amt)*1.5/10000))*10000, core.AFACap(m.MCC))
	}
	id := npci.NewTxnID("MDT")
	mstatus := "ACTIVE"
	updated := start
	if rng.IntN(35) == 0 && at.Before(now.Add(-48*time.Hour)) {
		mstatus, updated = "REVOKED", at.Add(time.Duration(1+rng.IntN(40))*time.Hour)
	}
	w.mandateN++
	ref := fmt.Sprintf("SUB-%s-%06d", strings.ToUpper(m.ID[:4]), w.mandateN)
	umn := strings.ToLower(security.RandomToken(16)) + "@" + handle
	w.mandates = append(w.mandates, []any{id, tenant, ref, mstatus, py.masked, py.token, py.enc, payee, m.ID, m.MCC, maxAmt, freq,
		start, start.AddDate(3+rng.IntN(3), 0, 0), umn, psp, "00", start, updated})
	nstatus, attempts := "EXECUTED", 1
	if status == domain.StatusFailed {
		nstatus, attempts = "FAILED", 4
	}
	notified := at.Add(-time.Duration(26+rng.IntN(22)) * time.Hour)
	w.notices = append(w.notices, []any{npci.NewTxnID("PDN"), id, tenant, cycles + 1, amt, at, nstatus, attempts, txnID, notified, at})
	if next := at.AddDate(0, months, 0); mstatus == "ACTIVE" && next.After(now) && next.Before(now.Add(48*time.Hour)) {
		if n := next.Add(-30 * time.Hour); n.Before(now) {
			w.notices = append(w.notices, []any{npci.NewTxnID("PDN"), id, tenant, cycles + 2, amt, next, "NOTIFIED", 0, "", n, n})
		}
	}
	return id, amt
}

// incidents records the sponsor-bank downtime the seeded traffic shows, as the bank monitor would have.
func incidents(pool *pgxpool.Pool, list []seedIncident) error {
	var rows [][]any
	for _, i := range list {
		rows = append(rows, []any{npci.NewTxnID("INC"), i.psp, i.severity, "RESOLVED", i.reason, i.td * 0.1, i.td, i.affected, i.from, i.to, i.to})
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"bank_incidents"}, []string{"id", "psp_id", "severity", "status", "reason", "td_rate",
		"peak_td_rate", "affected", "started_at", "resolved_at", "updated_at"}, pgx.CopyFromRows(rows))
	return err
}

type seedIncident struct {
	psp, severity, reason string
	td                    float64
	from, to              time.Time
	affected              int64
}
