package store

import (
	"context"
	"encoding/json"
	"time"
)

// ReconItem is one line of a reconciliation result.
type ReconItem struct {
	TenantID     string `json:"tenant_id"`
	TxnID        string `json:"txn_id"`
	RRN          string `json:"rrn"`
	Kind         string `json:"kind"` // PAYMENT | REFUND
	Category     string `json:"category"`
	LedgerStatus string `json:"ledger_status"`
	BankStatus   string `json:"bank_status"`
	NPCIStatus   string `json:"npci_status"`
	LedgerPaise  int64  `json:"ledger_paise"`
	BankPaise    int64  `json:"bank_paise"`
	NPCIPaise    int64  `json:"npci_paise"`
	Action       string `json:"action,omitempty"`
}

// ReconRun is a reconciliation run header.
type ReconRun struct {
	ID        string          `json:"run_id"`
	Date      time.Time       `json:"recon_date"`
	Status    string          `json:"status"`
	Summary   json.RawMessage `json:"summary"`
	CreatedAt time.Time       `json:"created_at"`
}

// SaveReconRun writes a run and its items.
func (s *Store) SaveReconRun(ctx context.Context, run *ReconRun, items []ReconItem) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.q.QueryRow(ctx, `INSERT INTO recon_runs (id, recon_date, status, summary) VALUES ($1,$2,$3,$4) RETURNING created_at`,
			run.ID, run.Date, run.Status, []byte(run.Summary)).Scan(&run.CreatedAt); err != nil {
			return err
		}
		for _, it := range items {
			if _, err := tx.q.Exec(ctx, `INSERT INTO recon_items (run_id, tenant_id, txn_id, rrn, kind, category, ledger_status,
				bank_status, npci_status, ledger_paise, bank_paise, npci_paise, action) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
				run.ID, it.TenantID, it.TxnID, it.RRN, it.Kind, it.Category, it.LedgerStatus, it.BankStatus, it.NPCIStatus,
				it.LedgerPaise, it.BankPaise, it.NPCIPaise, it.Action); err != nil {
				return err
			}
		}
		return nil
	})
}

// LatestReconRun returns the newest run for a date.
func (s *Store) LatestReconRun(ctx context.Context, date time.Time) (*ReconRun, error) {
	var r ReconRun
	var sum []byte
	err := s.q.QueryRow(ctx, `SELECT id, recon_date, status, summary, created_at FROM recon_runs WHERE recon_date=$1 ORDER BY created_at DESC LIMIT 1`,
		date).Scan(&r.ID, &r.Date, &r.Status, &sum, &r.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	r.Summary = sum
	return &r, nil
}

// ReconItems returns a run's items, optionally for one tenant and only exceptions.
func (s *Store) ReconItems(ctx context.Context, runID, tenantID string, exceptionsOnly bool) ([]ReconItem, error) {
	rows, err := s.q.Query(ctx, `SELECT tenant_id, txn_id, rrn, kind, category, ledger_status, bank_status, npci_status,
		ledger_paise, bank_paise, npci_paise, action FROM recon_items WHERE run_id=$1 AND ($2='' OR tenant_id=$2)
		AND (NOT $3 OR category <> 'MATCHED') ORDER BY id`, runID, tenantID, exceptionsOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconItem
	for rows.Next() {
		var it ReconItem
		if err := rows.Scan(&it.TenantID, &it.TxnID, &it.RRN, &it.Kind, &it.Category, &it.LedgerStatus, &it.BankStatus,
			&it.NPCIStatus, &it.LedgerPaise, &it.BankPaise, &it.NPCIPaise, &it.Action); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Settlement is a T+1 payout line per tenant merchant.
type Settlement struct {
	ID          string    `json:"settlement_id"`
	TenantID    string    `json:"tenant_id"`
	MerchantID  string    `json:"merchant_id"`
	SettleDate  time.Time `json:"settle_date"`
	TxnCount    int       `json:"txn_count"`
	GrossPaise  int64     `json:"gross_paise"`
	RefundPaise int64     `json:"refund_paise"`
	FeePaise    int64     `json:"fee_paise"`
	NetPaise    int64     `json:"net_paise"`
	Status      string    `json:"status"`
	PayoutRef   string    `json:"payout_ref,omitempty"`
}

// SettlementAgg is the ledger aggregate settlement is built from.
type SettlementAgg struct {
	TenantID    string
	MerchantID  string
	TxnCount    int
	GrossPaise  int64
	RefundPaise int64
}

// AggregateForSettlement sums successful payments and refunds per tenant merchant for [from, to).
func (s *Store) AggregateForSettlement(ctx context.Context, from, to time.Time) ([]SettlementAgg, error) {
	rows, err := s.q.Query(ctx, `
		WITH p AS (
			SELECT tenant_id, merchant_id, COUNT(*) AS n, SUM(amount_paise) AS gross FROM transactions
			WHERE status='SUCCESS' AND created_at >= $1 AND created_at < $2 GROUP BY tenant_id, merchant_id
		), r AS (
			SELECT t.tenant_id, t.merchant_id, SUM(r.amount_paise) AS refunded FROM refunds r JOIN transactions t ON t.id=r.txn_id
			WHERE r.status='SUCCESS' AND r.created_at >= $1 AND r.created_at < $2 GROUP BY t.tenant_id, t.merchant_id
		)
		SELECT COALESCE(p.tenant_id, r.tenant_id), COALESCE(p.merchant_id, r.merchant_id), COALESCE(p.n,0),
			COALESCE(p.gross,0), COALESCE(r.refunded,0)
		FROM p FULL OUTER JOIN r ON p.tenant_id=r.tenant_id AND p.merchant_id=r.merchant_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementAgg
	for rows.Next() {
		var a SettlementAgg
		if err := rows.Scan(&a.TenantID, &a.MerchantID, &a.TxnCount, &a.GrossPaise, &a.RefundPaise); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertSettlement writes or recomputes a pending settlement line (paid lines are left alone).
func (s *Store) UpsertSettlement(ctx context.Context, st *Settlement) error {
	return s.q.QueryRow(ctx, `INSERT INTO settlements (id, tenant_id, merchant_id, settle_date, txn_count, gross_paise, refund_paise, fee_paise, net_paise)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, merchant_id, settle_date) DO UPDATE SET txn_count=EXCLUDED.txn_count, gross_paise=EXCLUDED.gross_paise,
			refund_paise=EXCLUDED.refund_paise, fee_paise=EXCLUDED.fee_paise, net_paise=EXCLUDED.net_paise
			WHERE settlements.status='PENDING_PAYOUT'
		RETURNING id, status`, st.ID, st.TenantID, st.MerchantID, st.SettleDate, st.TxnCount, st.GrossPaise, st.RefundPaise,
		st.FeePaise, st.NetPaise).Scan(&st.ID, &st.Status)
}

// ListSettlements returns settlement lines for a date (and tenant, if given).
func (s *Store) ListSettlements(ctx context.Context, tenantID string, date time.Time) ([]Settlement, error) {
	rows, err := s.q.Query(ctx, `SELECT id, tenant_id, merchant_id, settle_date, txn_count, gross_paise, refund_paise, fee_paise, net_paise,
		status, payout_ref FROM settlements WHERE settle_date=$1 AND ($2='' OR tenant_id=$2) ORDER BY tenant_id, merchant_id`, date, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Settlement
	for rows.Next() {
		var st Settlement
		if err := rows.Scan(&st.ID, &st.TenantID, &st.MerchantID, &st.SettleDate, &st.TxnCount, &st.GrossPaise, &st.RefundPaise,
			&st.FeePaise, &st.NetPaise, &st.Status, &st.PayoutRef); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// MarkSettlementPaid records the payout reference.
func (s *Store) MarkSettlementPaid(ctx context.Context, id, ref string) error {
	ct, err := s.q.Exec(ctx, `UPDATE settlements SET status='PAID', payout_ref=$2 WHERE id=$1 AND status='PENDING_PAYOUT'`, id, ref)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
