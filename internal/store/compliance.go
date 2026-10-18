package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

// ---- merchants ----

const merchantCols = `tenant_id, id, mid, legal_name, display_name, mcc, business_type, kyc_status, kyc_method, kyc_ref,
	pan_masked, pan_enc, gstin, status, declared_paise, source, remarks, verified_at, created_at, updated_at`

func scanMerchant(row pgx.Row) (*domain.Merchant, error) {
	var m domain.Merchant
	if err := row.Scan(&m.TenantID, &m.ID, &m.MID, &m.LegalName, &m.DisplayName, &m.MCC, &m.BusinessType, &m.KYCStatus,
		&m.KYCMethod, &m.KYCRef, &m.PANMasked, &m.PANEnc, &m.GSTIN, &m.Status, &m.DeclaredPaise, &m.Source, &m.Remarks,
		&m.VerifiedAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &m, nil
}

// InsertMerchant registers a sub-merchant. With ignoreExisting, a merchant that is already registered
// is left as it is (ErrNotFound is not returned; the caller re-reads it).
func (s *Store) InsertMerchant(ctx context.Context, m *domain.Merchant, ignoreExisting bool) error {
	q := `INSERT INTO merchants (tenant_id, id, mid, legal_name, display_name, mcc, business_type, kyc_status, kyc_method,
		kyc_ref, pan_masked, pan_enc, gstin, status, declared_paise, source, remarks, verified_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`
	if ignoreExisting {
		q += ` ON CONFLICT (tenant_id, id) DO NOTHING`
	}
	_, err := s.q.Exec(ctx, q, m.TenantID, m.ID, m.MID, m.LegalName, m.DisplayName, m.MCC, m.BusinessType, m.KYCStatus,
		m.KYCMethod, m.KYCRef, m.PANMasked, m.PANEnc, m.GSTIN, m.Status, m.DeclaredPaise, m.Source, m.Remarks, m.VerifiedAt)
	return mapErr(err)
}

// GetMerchant loads a tenant's merchant.
func (s *Store) GetMerchant(ctx context.Context, tenantID, id string) (*domain.Merchant, error) {
	return scanMerchant(s.q.QueryRow(ctx, `SELECT `+merchantCols+` FROM merchants WHERE tenant_id=$1 AND id=$2`, tenantID, id))
}

// LockMerchant loads a merchant with a row lock.
func (s *Store) LockMerchant(ctx context.Context, tenantID, id string) (*domain.Merchant, error) {
	return scanMerchant(s.q.QueryRow(ctx, `SELECT `+merchantCols+` FROM merchants WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, id))
}

// UpdateMerchant saves the KYC and status fields of a locked merchant.
func (s *Store) UpdateMerchant(ctx context.Context, m *domain.Merchant) error {
	return mapErr(s.q.QueryRow(ctx, `UPDATE merchants SET legal_name=$3, display_name=$4, kyc_status=$5, kyc_method=$6, kyc_ref=$7,
		status=$8, remarks=$9, verified_at=$10, updated_at=now() WHERE tenant_id=$1 AND id=$2 RETURNING updated_at`,
		m.TenantID, m.ID, m.LegalName, m.DisplayName, m.KYCStatus, m.KYCMethod, m.KYCRef, m.Status, m.Remarks, m.VerifiedAt).
		Scan(&m.UpdatedAt))
}

// MerchantFilter selects merchants for the console.
type MerchantFilter struct {
	TenantID, KYCStatus, Status, Query string
	Since                              time.Time // start of the activity window for the stats
	Limit, Offset                      int
}

// MerchantRow is a merchant with its recent activity.
type MerchantRow struct {
	*domain.Merchant
	Count   int64 `json:"count"`
	Success int64 `json:"success"`
	Paise   int64 `json:"-"`
}

// ListMerchants returns merchants with their payment counts and volume since f.Since, busiest first.
func (s *Store) ListMerchants(ctx context.Context, f MerchantFilter) ([]MerchantRow, int64, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	where := ` WHERE ($1='' OR m.tenant_id=$1) AND ($2='' OR m.kyc_status=$2) AND ($3='' OR m.status=$3)
		AND ($4='' OR m.id ILIKE '%'||$4||'%' OR m.legal_name ILIKE '%'||$4||'%' OR m.display_name ILIKE '%'||$4||'%' OR m.mid=$4)`
	args := []any{f.TenantID, f.KYCStatus, f.Status, f.Query}
	var total int64
	if err := s.q.QueryRow(ctx, `SELECT COUNT(*) FROM merchants m`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	cols := ""
	for _, c := range []string{"tenant_id", "id", "mid", "legal_name", "display_name", "mcc", "business_type", "kyc_status", "kyc_method", "kyc_ref",
		"pan_masked", "pan_enc", "gstin", "status", "declared_paise", "source", "remarks", "verified_at", "created_at", "updated_at"} {
		cols += "m." + c + ", "
	}
	rows, err := s.q.Query(ctx, `SELECT `+cols+`COALESCE(a.n,0), COALESCE(a.ok,0), COALESCE(a.paise,0) FROM merchants m
		LEFT JOIN (SELECT tenant_id, merchant_id, COUNT(*) n, COUNT(*) FILTER (WHERE status='SUCCESS') ok,
			COALESCE(SUM(amount_paise) FILTER (WHERE status='SUCCESS'),0) paise
			FROM transactions WHERE created_at >= $5 GROUP BY tenant_id, merchant_id) a
		ON a.tenant_id=m.tenant_id AND a.merchant_id=m.id`+where+`
		ORDER BY COALESCE(a.paise,0) DESC, m.created_at DESC LIMIT $6 OFFSET $7`, append(args, f.Since, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []MerchantRow
	for rows.Next() {
		var m domain.Merchant
		var r MerchantRow
		if err := rows.Scan(&m.TenantID, &m.ID, &m.MID, &m.LegalName, &m.DisplayName, &m.MCC, &m.BusinessType, &m.KYCStatus,
			&m.KYCMethod, &m.KYCRef, &m.PANMasked, &m.PANEnc, &m.GSTIN, &m.Status, &m.DeclaredPaise, &m.Source, &m.Remarks,
			&m.VerifiedAt, &m.CreatedAt, &m.UpdatedAt, &r.Count, &r.Success, &r.Paise); err != nil {
			return nil, 0, err
		}
		r.Merchant = &m
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// MerchantKYCCounts counts merchants by KYC status (and suspended ones under "SUSPENDED").
func (s *Store) MerchantKYCCounts(ctx context.Context, tenantID string) (map[string]int64, error) {
	rows, err := s.q.Query(ctx, `SELECT CASE WHEN status='SUSPENDED' THEN 'SUSPENDED' ELSE kyc_status END, COUNT(*)
		FROM merchants WHERE ($1='' OR tenant_id=$1) GROUP BY 1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// PayerCategoryVolumeSince sums a customer's non-failed payments to merchants in the given MCCs since t,
// across every gateway on the switch (NPCI category limits are per customer per day).
func (s *Store) PayerCategoryVolumeSince(ctx context.Context, token string, mccs []string, since time.Time) (int64, error) {
	var v int64
	err := s.q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM transactions
		WHERE payer_vpa_token=$1 AND created_at >= $2 AND status <> 'FAILED' AND ($3::text[] IS NULL OR mcc = ANY($3))`,
		token, since, mccs).Scan(&v)
	return v, err
}

// ---- AutoPay pre-debit notifications ----

const notificationCols = `id, mandate_id, tenant_id, seq, amount_paise, debit_date, status, attempts, txn_id, notified_at`

func scanNotification(row pgx.Row) (*domain.MandateNotification, error) {
	var n domain.MandateNotification
	if err := row.Scan(&n.ID, &n.MandateID, &n.TenantID, &n.Seq, &n.AmountPaise, &n.DebitDate, &n.Status, &n.Attempts,
		&n.TxnID, &n.NotifiedAt); err != nil {
		return nil, mapErr(err)
	}
	return &n, nil
}

func scanNotifications(rows pgx.Rows, err error) ([]*domain.MandateNotification, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.MandateNotification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InsertNotification records a pre-debit notification with the next sequence number for the mandate.
func (s *Store) InsertNotification(ctx context.Context, n *domain.MandateNotification) error {
	return mapErr(s.q.QueryRow(ctx, `INSERT INTO mandate_notifications (id, mandate_id, tenant_id, seq, amount_paise, debit_date, notified_at)
		VALUES ($1,$2,$3,(SELECT COALESCE(MAX(seq),0)+1 FROM mandate_notifications WHERE mandate_id=$2),$4,$5,COALESCE($6,now()))
		RETURNING seq, status, notified_at`,
		n.ID, n.MandateID, n.TenantID, n.AmountPaise, n.DebitDate, nullTime(n.NotifiedAt)).Scan(&n.Seq, &n.Status, &n.NotifiedAt))
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// LockOpenNotification returns the mandate's latest notification that has not been executed yet, locked.
func (s *Store) LockOpenNotification(ctx context.Context, mandateID string) (*domain.MandateNotification, error) {
	return scanNotification(s.q.QueryRow(ctx, `SELECT `+notificationCols+` FROM mandate_notifications
		WHERE mandate_id=$1 AND status='NOTIFIED' ORDER BY seq DESC LIMIT 1 FOR UPDATE`, mandateID))
}

// MarkNotificationAttempt links a debit attempt to its notification.
func (s *Store) MarkNotificationAttempt(ctx context.Context, id, txnID string) error {
	_, err := s.q.Exec(ctx, `UPDATE mandate_notifications SET attempts=attempts+1, txn_id=$2, updated_at=now() WHERE id=$1`, id, txnID)
	return err
}

// SetNotificationStatus changes a notification's status.
func (s *Store) SetNotificationStatus(ctx context.Context, id, status string) error {
	_, err := s.q.Exec(ctx, `UPDATE mandate_notifications SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}

// SettleNotification closes the notification behind a mandate debit: EXECUTED on success, FAILED once a
// failed debit has used up maxTries. Otherwise it stays open for a retry.
func (s *Store) SettleNotification(ctx context.Context, txnID string, success bool, maxTries int) error {
	_, err := s.q.Exec(ctx, `UPDATE mandate_notifications SET updated_at=now(),
		status = CASE WHEN $2 THEN 'EXECUTED' WHEN attempts >= $3 THEN 'FAILED' ELSE status END
		WHERE txn_id=$1 AND status='NOTIFIED'`, txnID, success, maxTries)
	return err
}

// ListNotifications lists a mandate's notifications, newest first.
func (s *Store) ListNotifications(ctx context.Context, mandateID string) ([]*domain.MandateNotification, error) {
	return scanNotifications(s.q.Query(ctx, `SELECT `+notificationCols+` FROM mandate_notifications WHERE mandate_id=$1 ORDER BY seq DESC`, mandateID))
}

// MandateFilter selects mandates for the console.
type MandateFilter struct {
	TenantID, Status, Query string
	Limit, Offset           int
}

// MandateRow is a mandate with its debit history summary.
type MandateRow struct {
	*domain.Mandate
	Debits       int64      `json:"debits"`
	DebitedPaise int64      `json:"-"`
	LastDebitAt  *time.Time `json:"last_debit_at,omitempty"`
	NextDebit    *time.Time `json:"-"`
}

// ListMandates returns mandates with their successful debit counts, newest first.
func (s *Store) ListMandates(ctx context.Context, f MandateFilter) ([]MandateRow, int64, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	where := ` WHERE ($1='' OR m.tenant_id=$1) AND ($2='' OR m.status=$2)
		AND ($3='' OR m.id=$3 OR m.merchant_ref=$3 OR m.umn=$3 OR m.merchant_id ILIKE '%'||$3||'%')`
	args := []any{f.TenantID, f.Status, f.Query}
	var total int64
	if err := s.q.QueryRow(ctx, `SELECT COUNT(*) FROM mandates m`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	cols := ""
	for _, c := range []string{"id", "tenant_id", "merchant_ref", "status", "payer_vpa_masked", "payer_vpa_token", "payer_vpa_enc",
		"payee_vpa", "merchant_id", "mcc", "max_amount_paise", "frequency", "start_date", "end_date", "umn", "psp_id", "resp_code", "created_at", "updated_at"} {
		cols += "m." + c + ", "
	}
	// mandate_id <> '' lets the planner use the partial index transactions_mandate.
	rows, err := s.q.Query(ctx, `SELECT `+cols+`
		(SELECT COUNT(*) FROM transactions t WHERE t.mandate_id <> '' AND t.mandate_id=m.id AND t.status='SUCCESS'),
		(SELECT COALESCE(SUM(amount_paise),0) FROM transactions t WHERE t.mandate_id <> '' AND t.mandate_id=m.id AND t.status='SUCCESS'),
		(SELECT MAX(created_at) FROM transactions t WHERE t.mandate_id <> '' AND t.mandate_id=m.id),
		(SELECT MIN(debit_date)::timestamptz FROM mandate_notifications n WHERE n.mandate_id=m.id AND n.status='NOTIFIED')
		FROM mandates m`+where+` ORDER BY m.created_at DESC LIMIT $4 OFFSET $5`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []MandateRow
	for rows.Next() {
		var m domain.Mandate
		var r MandateRow
		if err := rows.Scan(&m.ID, &m.TenantID, &m.MerchantRef, &m.Status, &m.PayerVPAMasked, &m.PayerVPAToken, &m.PayerVPAEnc,
			&m.PayeeVPA, &m.MerchantID, &m.MCC, &m.MaxAmountPaise, &m.Frequency, &m.StartDate, &m.EndDate, &m.UMN, &m.PSPID,
			&m.RespCode, &m.CreatedAt, &m.UpdatedAt, &r.Debits, &r.DebitedPaise, &r.LastDebitAt, &r.NextDebit); err != nil {
			return nil, 0, err
		}
		r.Mandate = &m
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetMandateAny loads a mandate without tenant scoping (admin console).
func (s *Store) GetMandateAny(ctx context.Context, id string) (*domain.Mandate, error) {
	return scanMandate(s.q.QueryRow(ctx, `SELECT `+mandateCols+` FROM mandates WHERE id=$1`, id))
}

// MandateDebits lists the debits of a mandate, newest first.
func (s *Store) MandateDebits(ctx context.Context, mandateID string) ([]*domain.Txn, error) {
	return scanTxns(s.q.Query(ctx, `SELECT `+txnCols+` FROM transactions WHERE mandate_id <> '' AND mandate_id=$1 ORDER BY created_at DESC LIMIT 100`, mandateID))
}

// MandateCounts counts mandates by status.
func (s *Store) MandateCounts(ctx context.Context, tenantID string) (map[string]int64, error) {
	rows, err := s.q.Query(ctx, `SELECT status, COUNT(*) FROM mandates WHERE ($1='' OR tenant_id=$1) GROUP BY status`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ---- refunds (console) ----

// RefundFilter selects refunds for the console.
type RefundFilter struct {
	TenantID, Status, Mode, Query string
	Limit, Offset                 int
}

// RefundRow is a refund with the payment it reverses.
type RefundRow struct {
	*domain.Refund
	MerchantID   string    `json:"merchant_id"`
	PayeeName    string    `json:"payee_name"`
	PayerVPA     string    `json:"payer_vpa"`
	TxnPaise     int64     `json:"-"`
	TxnRRN       string    `json:"txn_rrn"`
	TxnCreatedAt time.Time `json:"txn_created_at"`
}

const refundFilterWhere = ` WHERE ($1='' OR r.tenant_id=$1) AND ($2='' OR r.status=$2) AND ($3='' OR r.mode=$3)
	AND ($4='' OR r.id=$4 OR r.txn_id=$4 OR r.merchant_refund_ref=$4 OR r.rrn=$4 OR t.rrn=$4)`

// ListRefundsAdmin lists refunds with their original payments, newest first.
func (s *Store) ListRefundsAdmin(ctx context.Context, f RefundFilter) ([]RefundRow, int64, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	args := []any{f.TenantID, f.Status, f.Mode, f.Query}
	var total int64
	if err := s.q.QueryRow(ctx, `SELECT COUNT(*) FROM refunds r JOIN transactions t ON t.id=r.txn_id`+refundFilterWhere, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	cols := ""
	for _, c := range []string{"id", "tenant_id", "txn_id", "merchant_refund_ref", "amount_paise", "status", "mode", "psp_id", "rrn", "resp_code",
		"resp_reason", "check_attempts", "next_check_at", "created_at", "updated_at"} {
		cols += "r." + c + ", "
	}
	rows, err := s.q.Query(ctx, `SELECT `+cols+`t.merchant_id, t.payee_name, t.payer_vpa_masked, t.amount_paise, t.rrn, t.created_at
		FROM refunds r JOIN transactions t ON t.id=r.txn_id`+refundFilterWhere+` ORDER BY r.created_at DESC LIMIT $5 OFFSET $6`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []RefundRow
	for rows.Next() {
		var r domain.Refund
		var row RefundRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.TxnID, &r.MerchantRefundRef, &r.AmountPaise, &r.Status, &r.Mode, &r.PSPID, &r.RRN,
			&r.RespCode, &r.RespReason, &r.CheckAttempts, &r.NextCheckAt, &r.CreatedAt, &r.UpdatedAt,
			&row.MerchantID, &row.PayeeName, &row.PayerVPA, &row.TxnPaise, &row.TxnRRN, &row.TxnCreatedAt); err != nil {
			return nil, 0, err
		}
		row.Refund = &r
		out = append(out, row)
	}
	return out, total, rows.Err()
}

// RefundSummary totals refunds by status since t.
func (s *Store) RefundSummary(ctx context.Context, tenantID string, since time.Time) (map[string][2]int64, error) {
	rows, err := s.q.Query(ctx, `SELECT status, COUNT(*), COALESCE(SUM(amount_paise),0) FROM refunds
		WHERE ($1='' OR tenant_id=$1) AND created_at >= $2 GROUP BY status`, tenantID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var k string
		var n, p int64
		if err := rows.Scan(&k, &n, &p); err != nil {
			return nil, err
		}
		out[k] = [2]int64{n, p}
	}
	return out, rows.Err()
}

// ---- bank incidents ----

const incidentCols = `id, psp_id, severity, status, reason, td_rate, peak_td_rate, affected, started_at, resolved_at`

func scanIncident(row pgx.Row) (*domain.Incident, error) {
	var i domain.Incident
	if err := row.Scan(&i.ID, &i.PSPID, &i.Severity, &i.Status, &i.Reason, &i.TDRate, &i.PeakTDRate, &i.Affected, &i.StartedAt, &i.ResolvedAt); err != nil {
		return nil, mapErr(err)
	}
	return &i, nil
}

// OpenIncident starts an incident for a bank unless one is already active (then ErrConflict).
func (s *Store) OpenIncident(ctx context.Context, i *domain.Incident) error {
	return mapErr(s.q.QueryRow(ctx, `INSERT INTO bank_incidents (id, psp_id, severity, reason, td_rate, peak_td_rate, affected, started_at)
		VALUES ($1,$2,$3,$4,$5,$5,$6,COALESCE($7,now())) RETURNING status, started_at`,
		i.ID, i.PSPID, i.Severity, i.Reason, i.TDRate, i.Affected, nullTime(i.StartedAt)).Scan(&i.Status, &i.StartedAt))
}

// ActiveIncident returns the bank's active incident, locked.
func (s *Store) ActiveIncident(ctx context.Context, pspID string) (*domain.Incident, error) {
	return scanIncident(s.q.QueryRow(ctx, `SELECT `+incidentCols+` FROM bank_incidents WHERE psp_id=$1 AND status='ACTIVE' FOR UPDATE`, pspID))
}

// UpdateIncident saves the live numbers of an active incident, or resolves it.
func (s *Store) UpdateIncident(ctx context.Context, i *domain.Incident) error {
	_, err := s.q.Exec(ctx, `UPDATE bank_incidents SET severity=$2, status=$3, reason=$4, td_rate=$5,
		peak_td_rate=GREATEST(peak_td_rate,$5), affected=$6, resolved_at=$7, updated_at=now() WHERE id=$1`,
		i.ID, i.Severity, i.Status, i.Reason, i.TDRate, i.Affected, i.ResolvedAt)
	return err
}

// ListIncidents lists incidents that were active at any point since t, newest first.
func (s *Store) ListIncidents(ctx context.Context, since time.Time, limit int) ([]*domain.Incident, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.q.Query(ctx, `SELECT `+incidentCols+` FROM bank_incidents WHERE status='ACTIVE' OR resolved_at >= $1
		ORDER BY started_at DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Incident
	for rows.Next() {
		i, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// BankWindow is one bank's outcome counts over a recent window.
type BankWindow struct {
	PSPID     string
	Count     int64
	Technical int64
}

// BankWindows counts final outcomes per bank since t, with technical declines split out.
func (s *Store) BankWindows(ctx context.Context, since time.Time, tdCodes []string) (map[string]BankWindow, error) {
	rows, err := s.q.Query(ctx, `SELECT psp_id, COUNT(*), COUNT(*) FILTER (WHERE status='FAILED' AND resp_code = ANY($2))
		FROM transactions WHERE updated_at >= $1 AND status IN ('SUCCESS','FAILED','DEEMED') GROUP BY psp_id`, since, tdCodes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BankWindow{}
	for rows.Next() {
		var w BankWindow
		if err := rows.Scan(&w.PSPID, &w.Count, &w.Technical); err != nil {
			return nil, err
		}
		out[w.PSPID] = w
	}
	return out, rows.Err()
}

// ---- RBI turnaround (TAT) cases ----

// TATCase is a payment that went DEEMED: the customer may have been debited without the merchant
// being confirmed, so RBI's turnaround clock for failed transactions applies.
type TATCase struct {
	TxnID      string     `json:"txn_id"`
	TenantID   string     `json:"tenant_id"`
	MerchantID string     `json:"merchant_id"`
	PayeeName  string     `json:"payee_name"`
	PayerVPA   string     `json:"payer_vpa"`
	Paise      int64      `json:"-"`
	PSPID      string     `json:"psp"`
	RRN        string     `json:"rrn,omitempty"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	DeemedAt   time.Time  `json:"deemed_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	DisputeID  string     `json:"dispute_id,omitempty"`
}

// TATCases returns payments created since t that went DEEMED, plus every still-unresolved one however old (its
// compensation keeps accruing), oldest unresolved first.
func (s *Store) TATCases(ctx context.Context, tenantID string, since time.Time) ([]TATCase, error) {
	rows, err := s.q.Query(ctx, `SELECT t.id, t.tenant_id, t.merchant_id, t.payee_name, t.payer_vpa_masked, t.amount_paise, t.psp_id, t.rrn,
			t.status, t.created_at, d.at,
			(SELECT MIN(at) FROM txn_events r WHERE r.entity_id=t.id AND r.from_status='DEEMED' AND r.to_status IN ('SUCCESS','FAILED')),
			COALESCE((SELECT id FROM disputes x WHERE x.txn_id=t.id ORDER BY created_at LIMIT 1),'')
		FROM (SELECT entity_id, MIN(at) at FROM txn_events WHERE to_status='DEEMED'
			AND (at >= $2 OR entity_id IN (SELECT id FROM transactions WHERE status='DEEMED')) GROUP BY entity_id) d
		JOIN transactions t ON t.id=d.entity_id
		WHERE ($1='' OR t.tenant_id=$1) AND (t.created_at >= $2 OR t.status='DEEMED')
		ORDER BY (t.status='DEEMED') DESC, t.created_at`, tenantID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TATCase
	for rows.Next() {
		var c TATCase
		if err := rows.Scan(&c.TxnID, &c.TenantID, &c.MerchantID, &c.PayeeName, &c.PayerVPA, &c.Paise, &c.PSPID, &c.RRN, &c.Status,
			&c.CreatedAt, &c.DeemedAt, &c.ResolvedAt, &c.DisputeID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
