// Package recon runs the T+1 three-way reconciliation (switch ledger × bank MIS × NPCI raw files),
// closes deemed transactions, raises TCC disputes and computes T+1 merchant settlement.
package recon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// File kinds.
const (
	KindBankMIS = "mis"
	KindNPCIRaw = "npci-raw"
)

// Categories of a recon line.
const (
	CatMatched           = "MATCHED"
	CatDeemedResolved    = "DEEMED_RESOLVED"
	CatPendingResolved   = "PENDING_RESOLVED"
	CatAmountMismatch    = "AMOUNT_MISMATCH"
	CatMissingInBank     = "MISSING_IN_BANK"
	CatMissingInNPCI     = "MISSING_IN_NPCI"
	CatStatusMismatch    = "STATUS_MISMATCH"
	CatUnknownInLedger   = "UNKNOWN_IN_LEDGER"
	CatUnresolvedPending = "UNRESOLVED_PENDING"
)

// Row is one line of a bank MIS or NPCI raw file.
// CSV columns: txn_id,rrn,type,amount,status,date (type PAYMENT|REFUND, status SUCCESS|FAILED).
type Row struct {
	TxnID       string
	RRN         string
	Type        string
	AmountPaise int64
	Status      string
	Date        string
}

// ParseCSV parses a settlement file and verifies its SHA-256 checksum when one is given.
func ParseCSV(data []byte, checksum string) ([]Row, error) {
	if checksum != "" {
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(checksum)) {
			return nil, errors.New("recon: file checksum mismatch")
		}
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("recon: csv: %w", err)
	}
	var out []Row
	for i, rec := range recs {
		if i == 0 && len(rec) > 0 && strings.EqualFold(rec[0], "txn_id") {
			continue
		}
		if len(rec) < 5 {
			return nil, fmt.Errorf("recon: line %d has %d columns, want ≥5", i+1, len(rec))
		}
		amt, err := domain.ParseAmount(rec[3])
		if err != nil {
			return nil, fmt.Errorf("recon: line %d: %w", i+1, err)
		}
		row := Row{TxnID: rec[0], RRN: rec[1], Type: strings.ToUpper(rec[2]), AmountPaise: amt, Status: strings.ToUpper(rec[4])}
		if len(rec) > 5 {
			row.Date = rec[5]
		}
		out = append(out, row)
	}
	return out, nil
}

// Resolver applies recon outcomes to the ledger; implemented by core.Service.
type Resolver interface {
	ResolvePayment(ctx context.Context, txnID, result, rrn string) error
	ResolveRefund(ctx context.Context, refundID, result, rrn string) error
	AutoTCC(ctx context.Context, txn *domain.Txn, remarks string) (*domain.Dispute, error)
}

// FileSource fetches settlement files (SFTP in production; HTTP pull from the bank simulator locally).
type FileSource interface {
	Fetch(ctx context.Context, psp store.PSPRecord, kind string, date time.Time) (data []byte, checksum string, err error)
}

// HTTPSource pulls files from {psp endpoint}/files/{kind}?date=YYYY-MM-DD with an X-Checksum-SHA256 header.
type HTTPSource struct{ Client *http.Client }

// Fetch implements FileSource.
func (h HTTPSource) Fetch(ctx context.Context, psp store.PSPRecord, kind string, date time.Time) ([]byte, string, error) {
	url := strings.TrimRight(psp.Endpoint, "/") + "/files/" + kind + "?date=" + date.Format("2006-01-02")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s %s: HTTP %d", psp.ID, kind, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return b, resp.Header.Get("X-Checksum-SHA256"), err
}

// Engine runs reconciliation and settlement.
type Engine struct {
	Store    *store.Store
	Resolver Resolver
	Source   FileSource
	Location *time.Location
	Log      *slog.Logger
}

func (e *Engine) loc() *time.Location {
	if e.Location == nil {
		return time.UTC
	}
	return e.Location
}

// DayBounds returns [start, end) of a business date in the engine's timezone.
func (e *Engine) DayBounds(date time.Time) (time.Time, time.Time) {
	l := e.loc()
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, l)
	return start, start.AddDate(0, 0, 1)
}

// Files holds the parsed inputs for a run.
type Files struct {
	Bank []Row
	NPCI []Row
}

// FetchFiles pulls the MIS and NPCI raw files from every enabled PSP for a date.
func (e *Engine) FetchFiles(ctx context.Context, date time.Time) (*Files, error) {
	psps, err := e.Store.ListPSPs(ctx)
	if err != nil {
		return nil, err
	}
	f := &Files{}
	for _, p := range psps {
		for _, kind := range []string{KindBankMIS, KindNPCIRaw} {
			data, sum, err := e.Source.Fetch(ctx, p, kind, date)
			if err != nil {
				return nil, fmt.Errorf("fetch %s from %s: %w", kind, p.ID, err)
			}
			rows, err := ParseCSV(data, sum)
			if err != nil {
				return nil, fmt.Errorf("%s from %s: %w", kind, p.ID, err)
			}
			if kind == KindBankMIS {
				f.Bank = append(f.Bank, rows...)
			} else {
				f.NPCI = append(f.NPCI, rows...)
			}
		}
	}
	return f, nil
}

// Summary is a run's counts, overall and per tenant.
type Summary struct {
	Date       string                    `json:"date"`
	Total      int                       `json:"total"`
	Categories map[string]int            `json:"categories"`
	Tenants    map[string]map[string]int `json:"tenants"`
	Exceptions int                       `json:"exceptions"`
}

func isException(cat string) bool {
	switch cat {
	case CatMatched, CatDeemedResolved, CatPendingResolved:
		return false
	}
	return true
}

type entry struct {
	tenant, id, status, rrn, kind string
	amount                        int64
	txn                           *domain.Txn
	refundID                      string
}

// Run reconciles a business date and applies resolutions. It returns the run and its items.
func (e *Engine) Run(ctx context.Context, date time.Time, files *Files) (*store.ReconRun, []store.ReconItem, error) {
	from, to := e.DayBounds(date)
	txns, err := e.Store.TxnsForRecon(ctx, from, to)
	if err != nil {
		return nil, nil, err
	}
	refunds, err := e.Store.RefundsForRecon(ctx, from, to)
	if err != nil {
		return nil, nil, err
	}
	bank := index(files.Bank)
	npciRows := index(files.NPCI)
	var ledger []entry
	for _, t := range txns {
		ledger = append(ledger, entry{tenant: t.TenantID, id: t.ID, status: string(t.Status), rrn: t.RRN, kind: "PAYMENT", amount: t.AmountPaise, txn: t})
	}
	for _, r := range refunds {
		ledger = append(ledger, entry{tenant: r.TenantID, id: r.ID, status: string(r.Status), rrn: r.RRN, kind: "REFUND", amount: r.AmountPaise, refundID: r.ID})
	}
	seen := map[string]bool{}
	var items []store.ReconItem
	for _, l := range ledger {
		seen[l.id] = true
		b, hasB := bank[l.id]
		n, hasN := npciRows[l.id]
		it := store.ReconItem{TenantID: l.tenant, TxnID: l.id, RRN: l.rrn, Kind: l.kind, LedgerStatus: l.status, LedgerPaise: l.amount}
		if hasB {
			it.BankStatus, it.BankPaise = b.Status, b.AmountPaise
			if it.RRN == "" {
				it.RRN = b.RRN
			}
		}
		if hasN {
			it.NPCIStatus, it.NPCIPaise = n.Status, n.AmountPaise
			if it.RRN == "" {
				it.RRN = n.RRN
			}
		}
		it.Category, it.Action = e.classify(ctx, l, it, hasB, hasN, n)
		items = append(items, it)
	}
	for id, r := range npciRows {
		if !seen[id] {
			seen[id] = true
			it := store.ReconItem{TxnID: id, RRN: r.RRN, Kind: r.Type, NPCIStatus: r.Status, NPCIPaise: r.AmountPaise, Category: CatUnknownInLedger, Action: "investigate"}
			if b, ok := bank[id]; ok {
				it.BankStatus, it.BankPaise = b.Status, b.AmountPaise
			}
			items = append(items, it)
		}
	}
	for id, r := range bank {
		if !seen[id] {
			items = append(items, store.ReconItem{TxnID: id, RRN: r.RRN, Kind: r.Type, BankStatus: r.Status, BankPaise: r.AmountPaise, Category: CatUnknownInLedger, Action: "investigate"})
		}
	}
	sum := Summary{Date: date.Format("2006-01-02"), Categories: map[string]int{}, Tenants: map[string]map[string]int{}}
	for _, it := range items {
		sum.Total++
		sum.Categories[it.Category]++
		if isException(it.Category) {
			sum.Exceptions++
		}
		if it.TenantID != "" {
			if sum.Tenants[it.TenantID] == nil {
				sum.Tenants[it.TenantID] = map[string]int{}
			}
			sum.Tenants[it.TenantID][it.Category]++
		}
	}
	sb, _ := json.Marshal(sum)
	run := &store.ReconRun{ID: npci.NewTxnID("RCN"), Date: from, Status: "COMPLETED", Summary: sb}
	if err := e.Store.SaveReconRun(ctx, run, items); err != nil {
		return nil, nil, err
	}
	if err := e.notifyTenants(ctx, run, sum, items); err != nil {
		return run, items, err
	}
	return run, items, nil
}

func index(rows []Row) map[string]Row {
	m := make(map[string]Row, len(rows))
	for _, r := range rows {
		m[r.TxnID] = r
	}
	return m
}

// classify decides the category for one ledger entry and applies any resolution.
func (e *Engine) classify(ctx context.Context, l entry, it store.ReconItem, hasB, hasN bool, n Row) (string, string) {
	resolve := func(result string) error {
		if l.kind == "REFUND" {
			return e.Resolver.ResolveRefund(ctx, l.refundID, result, n.RRN)
		}
		return e.Resolver.ResolvePayment(ctx, l.id, result, n.RRN)
	}
	switch domain.TxnStatus(l.status) {
	case domain.StatusSuccess:
		switch {
		case !hasN:
			return CatMissingInNPCI, "investigate with NPCI"
		case n.Status != "SUCCESS":
			return CatStatusMismatch, "ledger SUCCESS but NPCI " + n.Status + "; hold settlement and investigate"
		case !hasB:
			return CatMissingInBank, "chase sponsor bank MIS"
		case it.NPCIPaise != l.amount || it.BankPaise != l.amount:
			return CatAmountMismatch, "hold settlement; amount differs"
		}
		return CatMatched, ""
	case domain.StatusDeemed, domain.StatusInitiated, domain.StatusPending:
		cat := CatDeemedResolved
		if l.status != string(domain.StatusDeemed) {
			cat = CatPendingResolved
		}
		if hasN && n.Status == "SUCCESS" {
			if n.AmountPaise != l.amount {
				return CatAmountMismatch, "NPCI success with different amount; not resolved"
			}
			if err := resolve(npci.ResultSuccess); err != nil {
				return CatUnresolvedPending, "resolve failed: " + err.Error()
			}
			return cat, "marked SUCCESS from NPCI file"
		}
		if hasN || l.status == string(domain.StatusDeemed) || (l.txn != nil && time.Now().After(l.txn.ExpiresAt)) {
			if err := resolve(npci.ResultFailure); err != nil {
				return CatUnresolvedPending, "resolve failed: " + err.Error()
			}
			action := "marked FAILED (no NPCI success)"
			if l.status == string(domain.StatusDeemed) && l.txn != nil {
				if d, err := e.Resolver.AutoTCC(ctx, l.txn, "deemed transaction not settled by NPCI"); err == nil {
					action += "; TCC raised " + d.ID
				} else {
					action += "; TCC failed: " + err.Error()
				}
			}
			return cat, action
		}
		return CatUnresolvedPending, "still awaiting payer; re-run recon later"
	case domain.StatusFailed:
		if hasN && n.Status == "SUCCESS" {
			return CatStatusMismatch, "ledger FAILED but NPCI SUCCESS; refund customer or credit merchant"
		}
		return CatMatched, ""
	case domain.StatusOfflineQueued:
		return CatUnresolvedPending, "offline refund awaiting bank file"
	}
	return CatUnresolvedPending, ""
}

func (e *Engine) notifyTenants(ctx context.Context, run *store.ReconRun, sum Summary, items []store.ReconItem) error {
	byTenant := map[string][]store.ReconItem{}
	for _, it := range items {
		if it.TenantID != "" && isException(it.Category) {
			byTenant[it.TenantID] = append(byTenant[it.TenantID], it)
		}
	}
	for tenant, counts := range sum.Tenants {
		payload := map[string]any{
			"event_id": "recon:" + run.ID + ":" + tenant, "type": "recon.report", "created_at": time.Now().UTC().Format(time.RFC3339),
			"data": map[string]any{"run_id": run.ID, "date": sum.Date, "counts": counts, "exceptions": byTenant[tenant]},
		}
		if err := e.Store.Enqueue(ctx, tenant, "recon:"+run.ID+":"+tenant, "recon.report", payload); err != nil {
			return err
		}
	}
	return nil
}

// Settle computes T+1 payouts for payments made on date: gross − refunds − MDR fee per tenant merchant. Payments
// reconciliation flagged as differing from the bank or NPCI are held back until resolved.
func (e *Engine) Settle(ctx context.Context, date time.Time) ([]store.Settlement, error) {
	from, to := e.DayBounds(date)
	aggs, err := e.Store.AggregateForSettlement(ctx, from, to)
	if err != nil {
		return nil, err
	}
	settleDate := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	fees := map[string]int{}
	var out []store.Settlement
	for _, a := range aggs {
		bps, ok := fees[a.TenantID]
		if !ok {
			t, err := e.Store.GetTenant(ctx, a.TenantID)
			if err != nil {
				return nil, err
			}
			bps = t.FeeBps
			fees[a.TenantID] = bps
		}
		fee := a.GrossPaise * int64(bps) / 10000
		st := store.Settlement{
			ID: npci.NewTxnID("STL"), TenantID: a.TenantID, MerchantID: a.MerchantID, SettleDate: settleDate, TxnCount: a.TxnCount,
			GrossPaise: a.GrossPaise, RefundPaise: a.RefundPaise, FeePaise: fee, HeldCount: a.HeldCount, HeldPaise: a.HeldPaise, NetPaise: a.GrossPaise - a.RefundPaise - fee,
		}
		if err := e.Store.UpsertSettlement(ctx, &st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// PayoutFile renders pending settlement lines for a settle date as CSV for the payout bank.
func PayoutFile(lines []store.Settlement) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.Write([]string{"settlement_id", "tenant_id", "merchant_id", "settle_date", "txn_count", "gross", "refunds", "fees", "net", "status"})
	for _, s := range lines {
		w.Write([]string{s.ID, s.TenantID, s.MerchantID, s.SettleDate.Format("2006-01-02"), fmt.Sprint(s.TxnCount),
			domain.FormatAmount(s.GrossPaise), domain.FormatAmount(s.RefundPaise), domain.FormatAmount(s.FeePaise), signed(s.NetPaise), s.Status})
	}
	w.Flush()
	return b.Bytes()
}

func signed(p int64) string {
	if p < 0 {
		return "-" + domain.FormatAmount(-p)
	}
	return domain.FormatAmount(p)
}
