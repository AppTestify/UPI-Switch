-- Sub-merchants of each payment gateway, with the due diligence the RBI PA Directions require
-- (RBI/DPSS/2025-26/141 para 13): KYC route and status, PA-assigned MCC and MID, registered name.
CREATE TABLE merchants (
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    id              TEXT NOT NULL,
    mid             TEXT NOT NULL UNIQUE,
    legal_name      TEXT NOT NULL,
    display_name    TEXT NOT NULL,
    mcc             TEXT NOT NULL,
    business_type   TEXT NOT NULL DEFAULT 'SMALL',
    kyc_status      TEXT NOT NULL DEFAULT 'PENDING',
    kyc_method      TEXT NOT NULL DEFAULT '',
    kyc_ref         TEXT NOT NULL DEFAULT '',
    pan_masked      TEXT NOT NULL DEFAULT '',
    pan_enc         TEXT NOT NULL DEFAULT '',
    gstin           TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'ACTIVE',
    declared_paise  BIGINT NOT NULL DEFAULT 0, -- declared monthly volume, for profile monitoring
    source          TEXT NOT NULL DEFAULT 'API',
    remarks         TEXT NOT NULL DEFAULT '',
    verified_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX merchants_kyc ON merchants (kyc_status);

-- AutoPay pre-debit notifications (RBI e-mandate framework: at least 24 h before each debit).
CREATE TABLE mandate_notifications (
    id           TEXT PRIMARY KEY,
    mandate_id   TEXT NOT NULL REFERENCES mandates(id),
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    seq          INT NOT NULL,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    debit_date   DATE NOT NULL,
    status       TEXT NOT NULL DEFAULT 'NOTIFIED',
    attempts     INT NOT NULL DEFAULT 0,
    txn_id       TEXT NOT NULL DEFAULT '',
    notified_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (mandate_id, seq)
);
CREATE INDEX mandate_notifications_txn ON mandate_notifications (txn_id) WHERE txn_id <> '';

-- Sponsor bank downtime, detected from live technical-decline rates and the router's circuit breaker.
CREATE TABLE bank_incidents (
    id           TEXT PRIMARY KEY,
    psp_id       TEXT NOT NULL,
    severity     TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'ACTIVE',
    reason       TEXT NOT NULL DEFAULT '',
    td_rate      DOUBLE PRECISION NOT NULL DEFAULT 0,
    peak_td_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    affected     BIGINT NOT NULL DEFAULT 0,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX bank_incidents_one_active ON bank_incidents (psp_id) WHERE status = 'ACTIVE';
CREATE INDEX bank_incidents_started ON bank_incidents (started_at);

-- Turnaround tracking starts from the moment a payment went DEEMED.
CREATE INDEX txn_events_deemed ON txn_events (at) WHERE to_status = 'DEEMED';
CREATE INDEX transactions_mandate ON transactions (mandate_id) WHERE mandate_id <> '';
CREATE INDEX refunds_created ON refunds (created_at);
-- NPCI category limits are per customer per day across every gateway on the switch.
CREATE INDEX transactions_payer_day ON transactions (payer_vpa_token, created_at) WHERE payer_vpa_token <> '';
