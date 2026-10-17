// Package store is the PostgreSQL repository for the ledger, config DB, outbox and audit log.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/security"
)

// ErrNotFound is returned when a row does not exist (or belongs to another tenant).
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on a unique-key clash.
var ErrConflict = errors.New("conflict")

// DBTX is satisfied by both the pool and a transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store wraps a pool or a transaction.
type Store struct {
	pool  *pgxpool.Pool
	q     DBTX
	inTx  bool
	vault *security.Vault
}

// New creates a store on the pool. Secrets (tenant HMAC and webhook keys) are encrypted with vault.
func New(pool *pgxpool.Pool, vault *security.Vault) *Store {
	return &Store{pool: pool, q: pool, vault: vault}
}

// Pool exposes the underlying pool (health checks).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a database transaction. Nested calls reuse the outer transaction.
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.inTx {
		return fn(s)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	ts := &Store{pool: s.pool, q: tx, inTx: true, vault: s.vault}
	if err := fn(ts); err != nil {
		tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrConflict, pe.ConstraintName)
	}
	return err
}

// ---- tenants ----

const tenantCols = `id, name, status, api_key_id, hmac_secret, webhook_url, webhook_secret, vpa_prefix,
	ip_allowlist, cert_cn, fee_bps, limits, routing, created_at`

func (s *Store) scanTenant(row pgx.Row) (*domain.Tenant, error) {
	var t domain.Tenant
	var limits, routing []byte
	var hs, ws string
	if err := row.Scan(&t.ID, &t.Name, &t.Status, &t.APIKeyID, &hs, &t.WebhookURL, &ws, &t.VPAPrefix,
		&t.IPAllowlist, &t.CertCN, &t.FeeBps, &limits, &routing, &t.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if t.HMACSecret, err = s.vault.Decrypt(hs); err != nil {
		return nil, fmt.Errorf("decrypt tenant secret: %w", err)
	}
	if t.WebhookSecret, err = s.vault.Decrypt(ws); err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	if err := json.Unmarshal(limits, &t.Limits); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(routing, &t.Routing); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTenant inserts a tenant.
func (s *Store) CreateTenant(ctx context.Context, t *domain.Tenant) error {
	limits, _ := json.Marshal(t.Limits)
	routing, _ := json.Marshal(t.Routing)
	if t.IPAllowlist == nil {
		t.IPAllowlist = []string{}
	}
	err := s.q.QueryRow(ctx, `INSERT INTO tenants (id, name, status, api_key_id, hmac_secret, webhook_url, webhook_secret,
		vpa_prefix, ip_allowlist, cert_cn, fee_bps, limits, routing)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING created_at`,
		t.ID, t.Name, t.Status, t.APIKeyID, s.vault.Encrypt(t.HMACSecret), t.WebhookURL, s.vault.Encrypt(t.WebhookSecret),
		t.VPAPrefix, t.IPAllowlist, t.CertCN, t.FeeBps, limits, routing).Scan(&t.CreatedAt)
	return mapErr(err)
}

// UpdateTenant saves mutable tenant fields (not keys).
func (s *Store) UpdateTenant(ctx context.Context, t *domain.Tenant) error {
	limits, _ := json.Marshal(t.Limits)
	routing, _ := json.Marshal(t.Routing)
	ct, err := s.q.Exec(ctx, `UPDATE tenants SET name=$2, status=$3, webhook_url=$4, ip_allowlist=$5, cert_cn=$6,
		fee_bps=$7, limits=$8, routing=$9, updated_at=now() WHERE id=$1`,
		t.ID, t.Name, t.Status, t.WebhookURL, t.IPAllowlist, t.CertCN, t.FeeBps, limits, routing)
	if err != nil {
		return mapErr(err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateTenantSecrets replaces the API HMAC secret and webhook secret.
func (s *Store) RotateTenantSecrets(ctx context.Context, id, hmacSecret, webhookSecret string) error {
	ct, err := s.q.Exec(ctx, `UPDATE tenants SET hmac_secret=$2, webhook_secret=$3, updated_at=now() WHERE id=$1`,
		id, s.vault.Encrypt(hmacSecret), s.vault.Encrypt(webhookSecret))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTenant loads a tenant by id.
func (s *Store) GetTenant(ctx context.Context, id string) (*domain.Tenant, error) {
	return s.scanTenant(s.q.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id=$1`, id))
}

// GetTenantByAPIKey loads a tenant by its public API key id.
func (s *Store) GetTenantByAPIKey(ctx context.Context, keyID string) (*domain.Tenant, error) {
	return s.scanTenant(s.q.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE api_key_id=$1`, keyID))
}

// ListTenants returns all tenants.
func (s *Store) ListTenants(ctx context.Context) ([]*domain.Tenant, error) {
	rows, err := s.q.Query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Tenant
	for rows.Next() {
		t, err := s.scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- PSP banks ----

// PSPRecord is a PSP bank row including its PEM public key.
type PSPRecord struct {
	domain.PSPBank
	PublicKeyPEM string `json:"public_key_pem,omitempty"`
}

// UpsertPSP creates or updates a sponsor bank.
func (s *Store) UpsertPSP(ctx context.Context, p PSPRecord) error {
	_, err := s.q.Exec(ctx, `INSERT INTO psp_banks (id, name, endpoint, handle, public_key, cost_bps, priority, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, endpoint=EXCLUDED.endpoint, handle=EXCLUDED.handle,
		public_key=CASE WHEN EXCLUDED.public_key='' THEN psp_banks.public_key ELSE EXCLUDED.public_key END,
		cost_bps=EXCLUDED.cost_bps, priority=EXCLUDED.priority, enabled=EXCLUDED.enabled, updated_at=now()`,
		p.ID, p.Name, p.Endpoint, p.Handle, p.PublicKeyPEM, p.CostBps, p.Priority, p.Enabled)
	return mapErr(err)
}

// ListPSPs returns all sponsor banks.
func (s *Store) ListPSPs(ctx context.Context) ([]PSPRecord, error) {
	rows, err := s.q.Query(ctx, `SELECT id, name, endpoint, handle, public_key, cost_bps, priority, enabled FROM psp_banks ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PSPRecord
	for rows.Next() {
		var p PSPRecord
		if err := rows.Scan(&p.ID, &p.Name, &p.Endpoint, &p.Handle, &p.PublicKeyPEM, &p.CostBps, &p.Priority, &p.Enabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
