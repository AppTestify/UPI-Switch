package store

import (
	"context"
	"time"
)

// StatsFilter scopes the analytics queries.
type StatsFilter struct {
	From, To time.Time
	TenantID string
	Bucket   string   // "hour" or "day"
	TZ       string   // IANA zone buckets are aligned to
	TDCodes  []string // response codes counted as technical declines
}

// Totals are headline counts for a window.
type Totals struct {
	Count        int64 `json:"count"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	Deemed       int64 `json:"deemed"`
	Pending      int64 `json:"pending"`
	VolumePaise  int64 `json:"volume_paise"` // successful payments only
	UniquePayers int64 `json:"unique_payers"`
}

// Bucket is one point of a time series.
type Bucket struct {
	At          time.Time `json:"at"`
	Count       int64     `json:"count"`
	Success     int64     `json:"success"`
	Failed      int64     `json:"failed"`
	Technical   int64     `json:"technical_declines"`
	VolumePaise int64     `json:"volume_paise"`
}

// GroupStat is a breakdown row (by PSP, tenant or payment type).
type GroupStat struct {
	Key         string  `json:"key"`
	Count       int64   `json:"count"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	VolumePaise int64   `json:"volume_paise"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
}

// CodeStat counts declines by sponsor bank and NPCI response code.
type CodeStat struct {
	PSPID  string `json:"psp_id"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
	Paise  int64  `json:"paise"`
}

const statsWhere = ` WHERE created_at >= $1 AND created_at < $2 AND ($3='' OR tenant_id=$3)`

// StatsTotals returns headline counts for [From, To).
func (s *Store) StatsTotals(ctx context.Context, f StatsFilter) (Totals, error) {
	var t Totals
	err := s.q.QueryRow(ctx, `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE status='SUCCESS'), COUNT(*) FILTER (WHERE status='FAILED'),
		COUNT(*) FILTER (WHERE status='DEEMED'), COUNT(*) FILTER (WHERE status IN ('INITIATED','PENDING')),
		COALESCE(SUM(amount_paise) FILTER (WHERE status='SUCCESS'),0),
		COUNT(DISTINCT NULLIF(payer_vpa_token,'')) FROM transactions`+statsWhere, f.From, f.To, f.TenantID).
		Scan(&t.Count, &t.Success, &t.Failed, &t.Deemed, &t.Pending, &t.VolumePaise, &t.UniquePayers)
	return t, err
}

// StatsSeries returns per-bucket counts; empty buckets are omitted.
func (s *Store) StatsSeries(ctx context.Context, f StatsFilter) ([]Bucket, error) {
	bucket := "hour"
	if f.Bucket == "day" {
		bucket = "day"
	}
	rows, err := s.q.Query(ctx, `SELECT date_trunc('`+bucket+`', created_at AT TIME ZONE $4) AT TIME ZONE $4 AS b, COUNT(*),
		COUNT(*) FILTER (WHERE status='SUCCESS'), COUNT(*) FILTER (WHERE status='FAILED'),
		COUNT(*) FILTER (WHERE status='FAILED' AND resp_code = ANY($5)),
		COALESCE(SUM(amount_paise) FILTER (WHERE status='SUCCESS'),0)
		FROM transactions`+statsWhere+` GROUP BY b ORDER BY b`, f.From, f.To, f.TenantID, f.TZ, f.TDCodes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.At, &b.Count, &b.Success, &b.Failed, &b.Technical, &b.VolumePaise); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StatsBy breaks the window down by one of psp_id, tenant_id or type, with time-to-final percentiles.
func (s *Store) StatsBy(ctx context.Context, f StatsFilter, column string) ([]GroupStat, error) {
	switch column {
	case "psp_id", "tenant_id", "type":
	default:
		column = "psp_id"
	}
	rows, err := s.q.Query(ctx, `SELECT `+column+`, COUNT(*),
		COUNT(*) FILTER (WHERE status='SUCCESS'), COUNT(*) FILTER (WHERE status='FAILED'),
		COALESCE(SUM(amount_paise) FILTER (WHERE status='SUCCESS'),0),
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM updated_at-created_at)*1000) FILTER (WHERE status IN ('SUCCESS','FAILED')),0),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM updated_at-created_at)*1000) FILTER (WHERE status IN ('SUCCESS','FAILED')),0)
		FROM transactions`+statsWhere+` GROUP BY 1 ORDER BY 2 DESC`, f.From, f.To, f.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupStat
	for rows.Next() {
		var g GroupStat
		if err := rows.Scan(&g.Key, &g.Count, &g.Success, &g.Failed, &g.VolumePaise, &g.P50Ms, &g.P95Ms); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// StatsDeclines counts failed payments by sponsor bank and response code.
func (s *Store) StatsDeclines(ctx context.Context, f StatsFilter) ([]CodeStat, error) {
	rows, err := s.q.Query(ctx, `SELECT psp_id, resp_code, MAX(resp_reason), COUNT(*), SUM(amount_paise) FROM transactions`+statsWhere+`
		AND status='FAILED' GROUP BY psp_id, resp_code ORDER BY 4 DESC`, f.From, f.To, f.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CodeStat
	for rows.Next() {
		var c CodeStat
		if err := rows.Scan(&c.PSPID, &c.Code, &c.Reason, &c.Count, &c.Paise); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StatsRefunds returns the count and value of successful refunds created in the window.
func (s *Store) StatsRefunds(ctx context.Context, f StatsFilter) (count, paise int64, err error) {
	err = s.q.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(amount_paise),0) FROM refunds`+statsWhere+` AND status='SUCCESS'`,
		f.From, f.To, f.TenantID).Scan(&count, &paise)
	return
}

// StatsOpenExceptions counts what operators still have to act on: DEEMED payments, payments
// pending for more than five minutes, open disputes and dead-lettered webhooks.
func (s *Store) StatsOpenExceptions(ctx context.Context, tenantID string) (deemed, stuck, disputes, deadHooks int64, err error) {
	err = s.q.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM transactions WHERE status='DEEMED' AND ($1='' OR tenant_id=$1)),
		(SELECT COUNT(*) FROM transactions WHERE status IN ('INITIATED','PENDING') AND created_at < now() - interval '5 minutes'
			AND expires_at > now() - interval '1 day' AND ($1='' OR tenant_id=$1)),
		(SELECT COUNT(*) FROM disputes WHERE status IN ('RAISED','UNDER_REVIEW') AND ($1='' OR tenant_id=$1)),
		(SELECT COUNT(*) FROM webhook_outbox WHERE status='DEAD' AND ($1='' OR tenant_id=$1))`, tenantID).
		Scan(&deemed, &stuck, &disputes, &deadHooks)
	return
}

// StatsCompliance counts active bank incidents, merchants awaiting KYC and DEEMED payments created
// before tatCutoff, which are past RBI's reversal deadline.
func (s *Store) StatsCompliance(ctx context.Context, tenantID string, tatCutoff time.Time) (incidents, kycPending, tatBreached int64, err error) {
	err = s.q.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM bank_incidents WHERE status='ACTIVE'),
		(SELECT COUNT(*) FROM merchants WHERE kyc_status='PENDING' AND status='ACTIVE' AND ($1='' OR tenant_id=$1)),
		(SELECT COUNT(*) FROM transactions WHERE status='DEEMED' AND created_at < $2 AND ($1='' OR tenant_id=$1))`, tenantID, tatCutoff).
		Scan(&incidents, &kycPending, &tatBreached)
	return
}
