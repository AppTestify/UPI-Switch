-- Console sessions are recorded server-side so sign-out, user removal and idle timeouts take effect at once.
CREATE TABLE console_sessions (
    id           TEXT PRIMARY KEY,
    email        TEXT NOT NULL,
    role         TEXT NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX console_sessions_email ON console_sessions (email) WHERE revoked_at IS NULL;

-- Failed sign-ins per account are counted from the audit log to lock the account out.
CREATE INDEX audit_log_actor_action ON audit_log (actor, action, at);

-- Payments reconciliation flagged (amount or status differs from the bank or NPCI) are held out of settlement.
CREATE INDEX recon_items_held ON recon_items (txn_id) WHERE category IN ('AMOUNT_MISMATCH', 'STATUS_MISMATCH');
ALTER TABLE settlements ADD COLUMN held_count INT NOT NULL DEFAULT 0, ADD COLUMN held_paise BIGINT NOT NULL DEFAULT 0;
CREATE INDEX transactions_deemed ON transactions (id) WHERE status = 'DEEMED';
