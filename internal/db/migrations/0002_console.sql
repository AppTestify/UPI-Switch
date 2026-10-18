-- Console analytics scan the ledger by time across all tenants.
CREATE INDEX transactions_created ON transactions (created_at);
CREATE INDEX webhook_outbox_status ON webhook_outbox (status, created_at);
