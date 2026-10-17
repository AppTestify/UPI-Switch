package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

const refundCols = `id, tenant_id, txn_id, merchant_refund_ref, amount_paise, status, mode, psp_id, rrn, resp_code,
	resp_reason, check_attempts, next_check_at, created_at, updated_at`

func scanRefund(row pgx.Row) (*domain.Refund, error) {
	var r domain.Refund
	if err := row.Scan(&r.ID, &r.TenantID, &r.TxnID, &r.MerchantRefundRef, &r.AmountPaise, &r.Status, &r.Mode,
		&r.PSPID, &r.RRN, &r.RespCode, &r.RespReason, &r.CheckAttempts, &r.NextCheckAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func scanRefunds(rows pgx.Rows, err error) ([]*domain.Refund, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Refund
	for rows.Next() {
		r, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertRefund writes a new refund.
func (s *Store) InsertRefund(ctx context.Context, r *domain.Refund) error {
	err := s.q.QueryRow(ctx, `INSERT INTO refunds (id, tenant_id, txn_id, merchant_refund_ref, amount_paise, status, mode, psp_id, next_check_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at, updated_at`,
		r.ID, r.TenantID, r.TxnID, r.MerchantRefundRef, r.AmountPaise, r.Status, r.Mode, r.PSPID, r.NextCheckAt).
		Scan(&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	return s.AddEvent(ctx, r.ID, "", string(r.Status), "api", "refund created")
}

// GetRefund loads a refund scoped to a tenant.
func (s *Store) GetRefund(ctx context.Context, tenantID, id string) (*domain.Refund, error) {
	return scanRefund(s.q.QueryRow(ctx, `SELECT `+refundCols+` FROM refunds WHERE id=$1 AND tenant_id=$2`, id, tenantID))
}

// LockRefund loads a refund with a row lock.
func (s *Store) LockRefund(ctx context.Context, id string) (*domain.Refund, error) {
	return scanRefund(s.q.QueryRow(ctx, `SELECT `+refundCols+` FROM refunds WHERE id=$1 FOR UPDATE`, id))
}

// RefundedTotal sums refunds against a txn that are not failed.
func (s *Store) RefundedTotal(ctx context.Context, txnID string) (int64, error) {
	var v int64
	err := s.q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM refunds WHERE txn_id=$1 AND status <> 'FAILED'`, txnID).Scan(&v)
	return v, err
}

// ListRefundsForTxn returns refunds against a txn.
func (s *Store) ListRefundsForTxn(ctx context.Context, txnID string) ([]*domain.Refund, error) {
	return scanRefunds(s.q.Query(ctx, `SELECT `+refundCols+` FROM refunds WHERE txn_id=$1 ORDER BY created_at`, txnID))
}

// ListRefunds lists refunds by tenant/status (admin, offline refund file).
func (s *Store) ListRefunds(ctx context.Context, tenantID, status string, limit int) ([]*domain.Refund, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return scanRefunds(s.q.Query(ctx, `SELECT `+refundCols+` FROM refunds WHERE ($1='' OR tenant_id=$1) AND ($2='' OR status=$2)
		ORDER BY created_at DESC LIMIT $3`, tenantID, status, limit))
}

// RefundsForRecon returns refunds created in [from, to).
func (s *Store) RefundsForRecon(ctx context.Context, from, to time.Time) ([]*domain.Refund, error) {
	return scanRefunds(s.q.Query(ctx, `SELECT `+refundCols+` FROM refunds WHERE created_at >= $1 AND created_at < $2`, from, to))
}

// UpdateRefund changes a locked refund's status and fields.
func (s *Store) UpdateRefund(ctx context.Context, r *domain.Refund, to domain.TxnStatus, rrn, code, reason string, next *time.Time, source string) error {
	from := r.Status
	if to != "" && to != from {
		ok := domain.CanTransition(from, to) || (from == domain.StatusOfflineQueued && to.IsFinal())
		if !ok {
			return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, from, to)
		}
		r.Status = to
	}
	if rrn != "" {
		r.RRN = rrn
	}
	if code != "" {
		r.RespCode = code
	}
	if reason != "" {
		r.RespReason = reason
	}
	if r.Status.IsFinal() {
		r.NextCheckAt = nil
	} else if next != nil {
		r.NextCheckAt = next
	}
	err := s.q.QueryRow(ctx, `UPDATE refunds SET status=$2, rrn=$3, resp_code=$4, resp_reason=$5, next_check_at=$6,
		check_attempts=$7, updated_at=now() WHERE id=$1 RETURNING updated_at`,
		r.ID, r.Status, r.RRN, r.RespCode, r.RespReason, r.NextCheckAt, r.CheckAttempts).Scan(&r.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if r.Status != from {
		return s.AddEvent(ctx, r.ID, string(from), string(r.Status), source, code)
	}
	return nil
}

// ClaimDueRefunds leases pending refunds whose status check is due.
func (s *Store) ClaimDueRefunds(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]*domain.Refund, error) {
	return scanRefunds(s.q.Query(ctx, `UPDATE refunds SET next_check_at = $2
		WHERE id IN (SELECT id FROM refunds WHERE next_check_at IS NOT NULL AND next_check_at <= $1
			ORDER BY next_check_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING `+refundCols, now, now.Add(lease), limit))
}

// ---- mandates ----

const mandateCols = `id, tenant_id, merchant_ref, status, payer_vpa_masked, payer_vpa_token, payer_vpa_enc, payee_vpa,
	merchant_id, mcc, max_amount_paise, frequency, start_date, end_date, umn, psp_id, resp_code, created_at, updated_at`

func scanMandate(row pgx.Row) (*domain.Mandate, error) {
	var m domain.Mandate
	if err := row.Scan(&m.ID, &m.TenantID, &m.MerchantRef, &m.Status, &m.PayerVPAMasked, &m.PayerVPAToken, &m.PayerVPAEnc,
		&m.PayeeVPA, &m.MerchantID, &m.MCC, &m.MaxAmountPaise, &m.Frequency, &m.StartDate, &m.EndDate, &m.UMN, &m.PSPID,
		&m.RespCode, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &m, nil
}

// InsertMandate writes a new mandate.
func (s *Store) InsertMandate(ctx context.Context, m *domain.Mandate) error {
	err := s.q.QueryRow(ctx, `INSERT INTO mandates (id, tenant_id, merchant_ref, status, payer_vpa_masked, payer_vpa_token,
		payer_vpa_enc, payee_vpa, merchant_id, mcc, max_amount_paise, frequency, start_date, end_date, psp_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING created_at, updated_at`,
		m.ID, m.TenantID, m.MerchantRef, m.Status, m.PayerVPAMasked, m.PayerVPAToken, m.PayerVPAEnc, m.PayeeVPA,
		m.MerchantID, m.MCC, m.MaxAmountPaise, m.Frequency, m.StartDate, m.EndDate, m.PSPID).Scan(&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	return s.AddEvent(ctx, m.ID, "", string(m.Status), "api", "mandate created")
}

// GetMandate loads a mandate scoped to a tenant.
func (s *Store) GetMandate(ctx context.Context, tenantID, id string) (*domain.Mandate, error) {
	return scanMandate(s.q.QueryRow(ctx, `SELECT `+mandateCols+` FROM mandates WHERE id=$1 AND tenant_id=$2`, id, tenantID))
}

// LockMandate loads a mandate with a row lock.
func (s *Store) LockMandate(ctx context.Context, id string) (*domain.Mandate, error) {
	return scanMandate(s.q.QueryRow(ctx, `SELECT `+mandateCols+` FROM mandates WHERE id=$1 FOR UPDATE`, id))
}

// UpdateMandate saves status, UMN and response code.
func (s *Store) UpdateMandate(ctx context.Context, m *domain.Mandate, to domain.MandateStatus, umn, code, source string) error {
	from := m.Status
	m.Status = to
	if umn != "" {
		m.UMN = umn
	}
	if code != "" {
		m.RespCode = code
	}
	err := s.q.QueryRow(ctx, `UPDATE mandates SET status=$2, umn=$3, resp_code=$4, updated_at=now() WHERE id=$1 RETURNING updated_at`,
		m.ID, m.Status, m.UMN, m.RespCode).Scan(&m.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if from != to {
		return s.AddEvent(ctx, m.ID, string(from), string(to), source, code)
	}
	return nil
}

// ---- disputes ----

const disputeCols = `id, tenant_id, txn_id, type, status, reason_code, remarks, amount_paise, resolution, source, created_at, updated_at`

func scanDispute(row pgx.Row) (*domain.Dispute, error) {
	var d domain.Dispute
	if err := row.Scan(&d.ID, &d.TenantID, &d.TxnID, &d.Type, &d.Status, &d.ReasonCode, &d.Remarks, &d.AmountPaise,
		&d.Resolution, &d.Source, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &d, nil
}

// InsertDispute writes a new dispute.
func (s *Store) InsertDispute(ctx context.Context, d *domain.Dispute) error {
	err := s.q.QueryRow(ctx, `INSERT INTO disputes (id, tenant_id, txn_id, type, status, reason_code, remarks, amount_paise, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at, updated_at`,
		d.ID, d.TenantID, d.TxnID, d.Type, d.Status, d.ReasonCode, d.Remarks, d.AmountPaise, d.Source).Scan(&d.CreatedAt, &d.UpdatedAt)
	return mapErr(err)
}

// GetDispute loads a dispute scoped to a tenant.
func (s *Store) GetDispute(ctx context.Context, tenantID, id string) (*domain.Dispute, error) {
	return scanDispute(s.q.QueryRow(ctx, `SELECT `+disputeCols+` FROM disputes WHERE id=$1 AND tenant_id=$2`, id, tenantID))
}

// LockDispute loads a dispute with a row lock.
func (s *Store) LockDispute(ctx context.Context, id string) (*domain.Dispute, error) {
	return scanDispute(s.q.QueryRow(ctx, `SELECT `+disputeCols+` FROM disputes WHERE id=$1 FOR UPDATE`, id))
}

// OpenDisputeForTxn returns an unresolved dispute of the given type on a txn, if any.
func (s *Store) OpenDisputeForTxn(ctx context.Context, txnID string, typ domain.DisputeType) (*domain.Dispute, error) {
	return scanDispute(s.q.QueryRow(ctx, `SELECT `+disputeCols+` FROM disputes WHERE txn_id=$1 AND type=$2
		AND status IN ('RAISED','UNDER_REVIEW') LIMIT 1`, txnID, typ))
}

// ListDisputes lists disputes for a tenant (or all, for admin).
func (s *Store) ListDisputes(ctx context.Context, tenantID string, limit int) ([]*domain.Dispute, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.q.Query(ctx, `SELECT `+disputeCols+` FROM disputes WHERE ($1='' OR tenant_id=$1) ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Dispute
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDispute saves dispute status and resolution.
func (s *Store) UpdateDispute(ctx context.Context, d *domain.Dispute, to domain.DisputeStatus, resolution string) error {
	from := d.Status
	d.Status = to
	if resolution != "" {
		d.Resolution = resolution
	}
	err := s.q.QueryRow(ctx, `UPDATE disputes SET status=$2, resolution=$3, updated_at=now() WHERE id=$1 RETURNING updated_at`,
		d.ID, d.Status, d.Resolution).Scan(&d.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if from != to {
		return s.AddEvent(ctx, d.ID, string(from), string(to), "udir", resolution)
	}
	return nil
}

// QueueRefundOffline moves an INITIATED refund that could not reach the bank to the offline file queue.
func (s *Store) QueueRefundOffline(ctx context.Context, r *domain.Refund) error {
	if r.Status != domain.StatusInitiated {
		return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, r.Status, domain.StatusOfflineQueued)
	}
	if _, err := s.q.Exec(ctx, `UPDATE refunds SET status='OFFLINE_QUEUED', next_check_at=NULL, updated_at=now() WHERE id=$1`, r.ID); err != nil {
		return err
	}
	r.Status = domain.StatusOfflineQueued
	return s.AddEvent(ctx, r.ID, string(domain.StatusInitiated), string(domain.StatusOfflineQueued), "router", "bank unreachable")
}

// SetMandatePSP records the sponsor bank that accepted the mandate after a failover.
func (s *Store) SetMandatePSP(ctx context.Context, id, pspID, payee string) error {
	_, err := s.q.Exec(ctx, `UPDATE mandates SET psp_id=$2, payee_vpa=$3, updated_at=now() WHERE id=$1`, id, pspID, payee)
	return err
}
