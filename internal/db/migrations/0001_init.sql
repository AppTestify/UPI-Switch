-- Config DB: tenants (payment gateways) and sponsor PSP banks.
CREATE TABLE tenants (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'ACTIVE',
    api_key_id     TEXT NOT NULL UNIQUE,
    hmac_secret    TEXT NOT NULL,
    webhook_url    TEXT NOT NULL DEFAULT '',
    webhook_secret TEXT NOT NULL,
    vpa_prefix     TEXT NOT NULL UNIQUE,
    ip_allowlist   TEXT[] NOT NULL DEFAULT '{}',
    cert_cn        TEXT NOT NULL DEFAULT '',
    fee_bps        INT NOT NULL DEFAULT 0,
    limits         JSONB NOT NULL DEFAULT '{}',
    routing        JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE psp_banks (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    endpoint   TEXT NOT NULL,
    handle     TEXT NOT NULL UNIQUE,
    public_key TEXT NOT NULL DEFAULT '',
    cost_bps   INT NOT NULL DEFAULT 0,
    priority   INT NOT NULL DEFAULT 100,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Transaction ledger. Every row is partitioned logically by tenant_id; every query filters on it.
CREATE TABLE transactions (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL REFERENCES tenants(id),
    merchant_ref     TEXT NOT NULL,
    type             TEXT NOT NULL,
    status           TEXT NOT NULL,
    amount_paise     BIGINT NOT NULL CHECK (amount_paise > 0),
    currency         TEXT NOT NULL DEFAULT 'INR',
    payee_vpa        TEXT NOT NULL,
    payee_name       TEXT NOT NULL DEFAULT '',
    payer_vpa_masked TEXT NOT NULL DEFAULT '',
    payer_vpa_token  TEXT NOT NULL DEFAULT '',
    payer_vpa_enc    TEXT NOT NULL DEFAULT '',
    merchant_id      TEXT NOT NULL DEFAULT '',
    mcc              TEXT NOT NULL DEFAULT '',
    note             TEXT NOT NULL DEFAULT '',
    psp_id           TEXT NOT NULL,
    rrn              TEXT NOT NULL DEFAULT '',
    resp_code        TEXT NOT NULL DEFAULT '',
    resp_reason      TEXT NOT NULL DEFAULT '',
    intent_uri       TEXT NOT NULL DEFAULT '',
    mandate_id       TEXT NOT NULL DEFAULT '',
    expires_at       TIMESTAMPTZ NOT NULL,
    check_attempts   INT NOT NULL DEFAULT 0,
    next_check_at    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, merchant_ref)
);
CREATE INDEX transactions_tenant_created ON transactions (tenant_id, created_at);
CREATE INDEX transactions_due_check ON transactions (next_check_at) WHERE next_check_at IS NOT NULL;
CREATE INDEX transactions_payer ON transactions (tenant_id, payer_vpa_token, created_at) WHERE payer_vpa_token <> '';
CREATE INDEX transactions_rrn ON transactions (rrn) WHERE rrn <> '';

CREATE TABLE txn_events (
    id          BIGSERIAL PRIMARY KEY,
    entity_id   TEXT NOT NULL,
    from_status TEXT NOT NULL,
    to_status   TEXT NOT NULL,
    source      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX txn_events_entity ON txn_events (entity_id);

CREATE TABLE idempotency_keys (
    tenant_id     TEXT NOT NULL,
    key           TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'IN_PROGRESS',
    response_code INT NOT NULL DEFAULT 0,
    response_body BYTEA,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);

CREATE TABLE refunds (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL REFERENCES tenants(id),
    txn_id              TEXT NOT NULL REFERENCES transactions(id),
    merchant_refund_ref TEXT NOT NULL,
    amount_paise        BIGINT NOT NULL CHECK (amount_paise > 0),
    status              TEXT NOT NULL,
    mode                TEXT NOT NULL,
    psp_id              TEXT NOT NULL,
    rrn                 TEXT NOT NULL DEFAULT '',
    resp_code           TEXT NOT NULL DEFAULT '',
    resp_reason         TEXT NOT NULL DEFAULT '',
    check_attempts      INT NOT NULL DEFAULT 0,
    next_check_at       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, merchant_refund_ref)
);
CREATE INDEX refunds_txn ON refunds (txn_id);
CREATE INDEX refunds_due_check ON refunds (next_check_at) WHERE next_check_at IS NOT NULL;

CREATE TABLE mandates (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL REFERENCES tenants(id),
    merchant_ref     TEXT NOT NULL,
    status           TEXT NOT NULL,
    payer_vpa_masked TEXT NOT NULL,
    payer_vpa_token  TEXT NOT NULL,
    payer_vpa_enc    TEXT NOT NULL,
    payee_vpa        TEXT NOT NULL,
    merchant_id      TEXT NOT NULL DEFAULT '',
    mcc              TEXT NOT NULL DEFAULT '',
    max_amount_paise BIGINT NOT NULL CHECK (max_amount_paise > 0),
    frequency        TEXT NOT NULL,
    start_date       DATE NOT NULL,
    end_date         DATE NOT NULL,
    umn              TEXT NOT NULL DEFAULT '',
    psp_id           TEXT NOT NULL,
    resp_code        TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, merchant_ref)
);

CREATE TABLE disputes (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    txn_id       TEXT NOT NULL REFERENCES transactions(id),
    type         TEXT NOT NULL,
    status       TEXT NOT NULL,
    reason_code  TEXT NOT NULL DEFAULT '',
    remarks      TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL,
    resolution   TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX disputes_txn ON disputes (txn_id);

-- Bank callback replay protection.
CREATE TABLE callback_messages (
    msg_id      TEXT PRIMARY KEY,
    psp_id      TEXT NOT NULL,
    api         TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Transactional outbox for signed webhooks to payment gateways.
CREATE TABLE webhook_outbox (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    event_id        TEXT NOT NULL UNIQUE,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'PENDING',
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ
);
CREATE INDEX webhook_outbox_due ON webhook_outbox (next_attempt_at) WHERE status = 'PENDING';

-- Reconciliation and settlement.
CREATE TABLE recon_runs (
    id         TEXT PRIMARY KEY,
    recon_date DATE NOT NULL,
    status     TEXT NOT NULL,
    summary    JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE recon_items (
    id            BIGSERIAL PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES recon_runs(id),
    tenant_id     TEXT NOT NULL DEFAULT '',
    txn_id        TEXT NOT NULL DEFAULT '',
    rrn           TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL,
    category      TEXT NOT NULL,
    ledger_status TEXT NOT NULL DEFAULT '',
    bank_status   TEXT NOT NULL DEFAULT '',
    npci_status   TEXT NOT NULL DEFAULT '',
    ledger_paise  BIGINT NOT NULL DEFAULT 0,
    bank_paise    BIGINT NOT NULL DEFAULT 0,
    npci_paise    BIGINT NOT NULL DEFAULT 0,
    action        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX recon_items_run ON recon_items (run_id, tenant_id);

CREATE TABLE settlements (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants(id),
    merchant_id   TEXT NOT NULL,
    settle_date   DATE NOT NULL,
    txn_count     INT NOT NULL,
    gross_paise   BIGINT NOT NULL,
    refund_paise  BIGINT NOT NULL,
    fee_paise     BIGINT NOT NULL,
    net_paise     BIGINT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'PENDING_PAYOUT',
    payout_ref    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, merchant_id, settle_date)
);

-- Immutable, hash-chained audit log.
CREATE TABLE audit_log (
    id        BIGSERIAL PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor     TEXT NOT NULL,
    action    TEXT NOT NULL,
    entity    TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    detail    JSONB NOT NULL DEFAULT '{}',
    prev_hash TEXT NOT NULL,
    hash      TEXT NOT NULL
);

CREATE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
