package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

const txnCols = `id, tenant_id, merchant_ref, type, status, amount_paise, currency, payee_vpa, payee_name,
	payer_vpa_masked, payer_vpa_token, payer_vpa_enc, merchant_id, mcc, note, psp_id, rrn, resp_code, resp_reason,
	intent_uri, mandate_id, expires_at, check_attempts, next_check_at, created_at, updated_at`

func scanTxn(row pgx.Row) (*domain.Txn, error) {
	var t domain.Txn
	if err := row.Scan(&t.ID, &t.TenantID, &t.MerchantRef, &t.Type, &t.Status, &t.AmountPaise, &t.Currency,
		&t.PayeeVPA, &t.PayeeName, &t.PayerVPAMasked, &t.PayerVPAToken, &t.PayerVPAEnc, &t.MerchantID, &t.MCC,
		&t.Note, &t.PSPID, &t.RRN, &t.RespCode, &t.RespReason, &t.IntentURI, &t.MandateID, &t.ExpiresAt,
		&t.CheckAttempts, &t.NextCheckAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func scanTxns(rows pgx.Rows, err error) ([]*domain.Txn, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Txn
	for rows.Next() {
		t, err := scanTxn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InsertTxn writes a new ledger row and its first event.
func (s *Store) InsertTxn(ctx context.Context, t *domain.Txn) error {
	err := s.q.QueryRow(ctx, `INSERT INTO transactions (id, tenant_id, merchant_ref, type, status, amount_paise, currency,
		payee_vpa, payee_name, payer_vpa_masked, payer_vpa_token, payer_vpa_enc, merchant_id, mcc, note, psp_id,
		intent_uri, mandate_id, expires_at, next_check_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING created_at, updated_at`,
		t.ID, t.TenantID, t.MerchantRef, t.Type, t.Status, t.AmountPaise, t.Currency, t.PayeeVPA, t.PayeeName,
		t.PayerVPAMasked, t.PayerVPAToken, t.PayerVPAEnc, t.MerchantID, t.MCC, t.Note, t.PSPID, t.IntentURI,
		t.MandateID, t.ExpiresAt, t.NextCheckAt).Scan(&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	return s.AddEvent(ctx, t.ID, "", string(t.Status), "api", "created")
}

// AddEvent appends to the status history of a txn, refund or mandate.
func (s *Store) AddEvent(ctx context.Context, entityID, from, to, source, detail string) error {
	_, err := s.q.Exec(ctx, `INSERT INTO txn_events (entity_id, from_status, to_status, source, detail) VALUES ($1,$2,$3,$4,$5)`,
		entityID, from, to, source, detail)
	return err
}

// Event is one status-history row.
type Event struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Source string    `json:"source"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// Events returns the status history of an entity.
func (s *Store) Events(ctx context.Context, entityID string) ([]Event, error) {
	rows, err := s.q.Query(ctx, `SELECT from_status, to_status, source, detail, at FROM txn_events WHERE entity_id=$1 ORDER BY id`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.From, &e.To, &e.Source, &e.Detail, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetTxn loads a txn scoped to a tenant.
func (s *Store) GetTxn(ctx context.Context, tenantID, id string) (*domain.Txn, error) {
	return scanTxn(s.q.QueryRow(ctx, `SELECT `+txnCols+` FROM transactions WHERE id=$1 AND tenant_id=$2`, id, tenantID))
}

// GetTxnByMerchantRef loads a txn by the PG's own reference.
func (s *Store) GetTxnByMerchantRef(ctx context.Context, tenantID, ref string) (*domain.Txn, error) {
	return scanTxn(s.q.QueryRow(ctx, `SELECT `+txnCols+` FROM transactions WHERE merchant_ref=$1 AND tenant_id=$2`, ref, tenantID))
}

// LockTxn loads a txn by id with a row lock (bank callbacks, poller, recon). Must run in a tx.
func (s *Store) LockTxn(ctx context.Context, id string) (*domain.Txn, error) {
	return scanTxn(s.q.QueryRow(ctx, `SELECT `+txnCols+` FROM transactions WHERE id=$1 FOR UPDATE`, id))
}

// TxnUpdate is the set of fields a status change may write.
type TxnUpdate struct {
	Status         domain.TxnStatus
	RRN            string
	RespCode       string
	RespReason     string
	PSPID          string
	PayerVPAMasked string
	PayerVPAToken  string
	PayerVPAEnc    string
	NextCheckAt    *time.Time
	ClearCheck     bool
	CheckAttempts  *int
}

// UpdateTxn applies u to a locked txn row, enforcing the state machine, and records the event.
func (s *Store) UpdateTxn(ctx context.Context, t *domain.Txn, u TxnUpdate, source, detail string) error {
	from := t.Status
	if u.Status != "" && u.Status != from {
		if !domain.CanTransition(from, u.Status) {
			return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, from, u.Status)
		}
		t.Status = u.Status
	}
	if u.RRN != "" {
		t.RRN = u.RRN
	}
	if u.RespCode != "" {
		t.RespCode = u.RespCode
	}
	if u.RespReason != "" {
		t.RespReason = u.RespReason
	}
	if u.PSPID != "" {
		t.PSPID = u.PSPID
	}
	if u.PayerVPAToken != "" && t.PayerVPAToken == "" {
		t.PayerVPAMasked, t.PayerVPAToken, t.PayerVPAEnc = u.PayerVPAMasked, u.PayerVPAToken, u.PayerVPAEnc
	}
	if u.ClearCheck || t.Status.IsFinal() {
		t.NextCheckAt = nil
	} else if u.NextCheckAt != nil {
		t.NextCheckAt = u.NextCheckAt
	}
	if u.CheckAttempts != nil {
		t.CheckAttempts = *u.CheckAttempts
	}
	err := s.q.QueryRow(ctx, `UPDATE transactions SET status=$2, rrn=$3, resp_code=$4, resp_reason=$5, psp_id=$6,
		payer_vpa_masked=$7, payer_vpa_token=$8, payer_vpa_enc=$9, next_check_at=$10, check_attempts=$11, updated_at=now()
		WHERE id=$1 RETURNING updated_at`,
		t.ID, t.Status, t.RRN, t.RespCode, t.RespReason, t.PSPID, t.PayerVPAMasked, t.PayerVPAToken, t.PayerVPAEnc,
		t.NextCheckAt, t.CheckAttempts).Scan(&t.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if t.Status != from || detail != "" {
		return s.AddEvent(ctx, t.ID, string(from), string(t.Status), source, detail)
	}
	return nil
}

// ClaimDueTxns leases up to limit non-final txns whose status check is due, pushing their
// next_check_at forward by lease so other instances skip them.
func (s *Store) ClaimDueTxns(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]*domain.Txn, error) {
	return scanTxns(s.q.Query(ctx, `UPDATE transactions SET next_check_at = $2
		WHERE id IN (SELECT id FROM transactions WHERE next_check_at IS NOT NULL AND next_check_at <= $1
			ORDER BY next_check_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING `+txnCols, now, now.Add(lease), limit))
}

// TxnFilter narrows ListTxns.
type TxnFilter struct {
	TenantID string
	Status   string
	PSPID    string
	Type     string
	Query    string // exact txn id, merchant ref or RRN
	Merchant string
	From, To time.Time
	Limit    int
	Offset   int
}

const txnFilterWhere = ` WHERE ($1='' OR tenant_id=$1) AND ($2='' OR status=$2)
		AND ($3::timestamptz IS NULL OR created_at >= $3) AND ($4::timestamptz IS NULL OR created_at < $4)
		AND ($5='' OR psp_id=$5) AND ($6='' OR type=$6) AND ($7='' OR id=$7 OR merchant_ref=$7 OR rrn=$7)
		AND ($8='' OR merchant_id=$8)`

func (f TxnFilter) args() []any {
	var from, to *time.Time
	if !f.From.IsZero() {
		from = &f.From
	}
	if !f.To.IsZero() {
		to = &f.To
	}
	return []any{f.TenantID, f.Status, from, to, f.PSPID, f.Type, f.Query, f.Merchant}
}

// ListTxns lists txns for reports and the admin console.
func (s *Store) ListTxns(ctx context.Context, f TxnFilter) ([]*domain.Txn, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	q := `SELECT ` + txnCols + ` FROM transactions` + txnFilterWhere + ` ORDER BY created_at DESC LIMIT $9 OFFSET $10`
	return scanTxns(s.q.Query(ctx, q, append(f.args(), f.Limit, f.Offset)...))
}

// CountTxns counts the txns matching f (Limit and Offset are ignored).
func (s *Store) CountTxns(ctx context.Context, f TxnFilter) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `SELECT COUNT(*) FROM transactions`+txnFilterWhere, f.args()...).Scan(&n)
	return n, err
}

// TxnsForRecon returns all txns created in [from, to) across tenants.
func (s *Store) TxnsForRecon(ctx context.Context, from, to time.Time) ([]*domain.Txn, error) {
	return scanTxns(s.q.Query(ctx, `SELECT `+txnCols+` FROM transactions WHERE created_at >= $1 AND created_at < $2`, from, to))
}

// GetTxnAny loads a txn by id without tenant scoping (internal use only).
func (s *Store) GetTxnAny(ctx context.Context, id string) (*domain.Txn, error) {
	return scanTxn(s.q.QueryRow(ctx, `SELECT `+txnCols+` FROM transactions WHERE id=$1`, id))
}

// TenantVolumeSince implements risk.Stats.
func (s *Store) TenantVolumeSince(ctx context.Context, tenantID string, since time.Time) (int64, error) {
	var v int64
	err := s.q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM transactions
		WHERE tenant_id=$1 AND created_at >= $2 AND status <> 'FAILED'`, tenantID, since).Scan(&v)
	return v, err
}

// PayerCountSince implements risk.Stats.
func (s *Store) PayerCountSince(ctx context.Context, tenantID, token string, since time.Time) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT COUNT(*) FROM transactions WHERE tenant_id=$1 AND payer_vpa_token=$2 AND created_at >= $3`,
		tenantID, token, since).Scan(&n)
	return n, err
}

// SetPayeeVPA changes the payee VPA after a failover to another sponsor bank.
func (s *Store) SetPayeeVPA(ctx context.Context, id, vpa string) error {
	_, err := s.q.Exec(ctx, `UPDATE transactions SET payee_vpa=$2, updated_at=now() WHERE id=$1`, id, vpa)
	return err
}
