package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- idempotency ----

// IdemResult is the state of an idempotency key.
type IdemResult struct {
	New          bool   // key was just claimed; run the handler
	InProgress   bool   // another request with this key is still running
	HashMismatch bool   // key reused with a different request body
	Code         int    // stored response code when completed
	Body         []byte // stored response body when completed
}

// ClaimIdempotency claims (tenant, key) for a request hash, or returns the stored outcome.
func (s *Store) ClaimIdempotency(ctx context.Context, tenantID, key, hash string) (IdemResult, error) {
	ct, err := s.q.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, key, request_hash) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		tenantID, key, hash)
	if err != nil {
		return IdemResult{}, err
	}
	if ct.RowsAffected() == 1 {
		return IdemResult{New: true}, nil
	}
	var r IdemResult
	var storedHash, status string
	err = s.q.QueryRow(ctx, `SELECT request_hash, status, response_code, response_body FROM idempotency_keys WHERE tenant_id=$1 AND key=$2`,
		tenantID, key).Scan(&storedHash, &status, &r.Code, &r.Body)
	if err != nil {
		return IdemResult{}, mapErr(err)
	}
	if storedHash != hash {
		r.HashMismatch = true
	} else if status != "COMPLETED" {
		r.InProgress = true
	}
	return r, nil
}

// CompleteIdempotency stores the response for a claimed key.
func (s *Store) CompleteIdempotency(ctx context.Context, tenantID, key string, code int, body []byte) error {
	_, err := s.q.Exec(ctx, `UPDATE idempotency_keys SET status='COMPLETED', response_code=$3, response_body=$4 WHERE tenant_id=$1 AND key=$2`,
		tenantID, key, code, body)
	return err
}

// ReleaseIdempotency drops a claimed key when the request failed before producing a response worth replaying.
func (s *Store) ReleaseIdempotency(ctx context.Context, tenantID, key string) error {
	_, err := s.q.Exec(ctx, `DELETE FROM idempotency_keys WHERE tenant_id=$1 AND key=$2 AND status='IN_PROGRESS'`, tenantID, key)
	return err
}

// ---- callback replay protection ----

// RecordCallback stores a callback msgId; it returns false when the message was already seen.
func (s *Store) RecordCallback(ctx context.Context, msgID, pspID, api string) (bool, error) {
	ct, err := s.q.Exec(ctx, `INSERT INTO callback_messages (msg_id, psp_id, api) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, msgID, pspID, api)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

// ---- webhook outbox ----

// OutboxEvent is one queued webhook.
type OutboxEvent struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	EventID     string          `json:"event_id"`
	EventType   string          `json:"event_type"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
}

const outboxCols = `id, tenant_id, event_id, event_type, payload, status, attempts, last_error, created_at, delivered_at`

func scanOutbox(rows pgx.Rows, err error) ([]OutboxEvent, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventID, &e.EventType, &e.Payload, &e.Status, &e.Attempts,
			&e.LastError, &e.CreatedAt, &e.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Enqueue adds a webhook to the outbox; call it in the same transaction as the state change.
func (s *Store) Enqueue(ctx context.Context, tenantID, eventID, eventType string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.q.Exec(ctx, `INSERT INTO webhook_outbox (tenant_id, event_id, event_type, payload) VALUES ($1,$2,$3,$4)
		ON CONFLICT (event_id) DO NOTHING`, tenantID, eventID, eventType, b)
	return err
}

// ClaimDueWebhooks leases pending webhooks due for delivery.
func (s *Store) ClaimDueWebhooks(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxEvent, error) {
	return scanOutbox(s.q.Query(ctx, `UPDATE webhook_outbox SET next_attempt_at = $2
		WHERE id IN (SELECT id FROM webhook_outbox WHERE status='PENDING' AND next_attempt_at <= $1
			ORDER BY next_attempt_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING `+outboxCols, now, now.Add(lease), limit))
}

// MarkWebhook records a delivery attempt. status is DELIVERED, PENDING (retry at next) or DEAD (DLQ).
func (s *Store) MarkWebhook(ctx context.Context, id int64, status string, attempts int, next time.Time, lastErr string) error {
	_, err := s.q.Exec(ctx, `UPDATE webhook_outbox SET status=$2, attempts=$3, next_attempt_at=$4, last_error=$5,
		delivered_at = CASE WHEN $2='DELIVERED' THEN now() ELSE delivered_at END WHERE id=$1`,
		id, status, attempts, next, lastErr)
	return err
}

// ListWebhooks lists outbox rows by status (DLQ view) and tenant.
func (s *Store) ListWebhooks(ctx context.Context, tenantID, status string, limit int) ([]OutboxEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return scanOutbox(s.q.Query(ctx, `SELECT `+outboxCols+` FROM webhook_outbox WHERE ($1='' OR tenant_id=$1) AND ($2='' OR status=$2)
		ORDER BY id DESC LIMIT $3`, tenantID, status, limit))
}

// ReplayWebhook moves a dead or delivered webhook back to PENDING.
func (s *Store) ReplayWebhook(ctx context.Context, id int64) error {
	ct, err := s.q.Exec(ctx, `UPDATE webhook_outbox SET status='PENDING', attempts=0, next_attempt_at=now(), last_error='' WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- audit log ----

// AuditEntry is one immutable audit record.
type AuditEntry struct {
	ID       int64           `json:"id"`
	At       time.Time       `json:"at"`
	Actor    string          `json:"actor"`
	Action   string          `json:"action"`
	Entity   string          `json:"entity"`
	EntityID string          `json:"entity_id"`
	Detail   json.RawMessage `json:"detail"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
}

func auditHash(prev, actor, action, entity, id string, at time.Time, detail []byte) string {
	h := sha256.New()
	for _, p := range []string{prev, actor, action, entity, id, at.UTC().Format(time.RFC3339Nano)} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	h.Write(detail)
	return hex.EncodeToString(h.Sum(nil))
}

// Audit appends a hash-chained entry. Chain order is serialised with a transaction-scoped advisory lock.
func (s *Store) Audit(ctx context.Context, actor, action, entity, entityID string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	// Normalise through jsonb so the stored text and the hashed bytes match on verify.
	return s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, `SELECT pg_advisory_xact_lock(7262002)`); err != nil {
			return err
		}
		var norm []byte
		if err := tx.q.QueryRow(ctx, `SELECT $1::jsonb::text`, string(b)).Scan(&norm); err != nil {
			return err
		}
		prev := ""
		err := tx.q.QueryRow(ctx, `SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		_, err = tx.q.Exec(ctx, `INSERT INTO audit_log (at, actor, action, entity, entity_id, detail, prev_hash, hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, at, actor, action, entity, entityID, string(norm), prev,
			auditHash(prev, actor, action, entity, entityID, at, norm))
		return err
	})
}

// ListAudit returns the most recent audit entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.q.Query(ctx, `SELECT id, at, actor, action, entity, entity_id, detail::text, prev_hash, hash FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var d string
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Entity, &e.EntityID, &d, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		e.Detail = json.RawMessage(d)
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyAudit walks the whole chain and returns the id of the first broken entry (0 if intact) and the count.
func (s *Store) VerifyAudit(ctx context.Context) (brokenAt int64, count int, err error) {
	rows, err := s.q.Query(ctx, `SELECT id, at, actor, action, entity, entity_id, detail::text, prev_hash, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	prev := ""
	for rows.Next() {
		var e AuditEntry
		var d string
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Entity, &e.EntityID, &d, &e.PrevHash, &e.Hash); err != nil {
			return 0, count, err
		}
		count++
		if e.PrevHash != prev || auditHash(prev, e.Actor, e.Action, e.Entity, e.EntityID, e.At, []byte(d)) != e.Hash {
			return e.ID, count, nil
		}
		prev = e.Hash
	}
	return 0, count, rows.Err()
}
