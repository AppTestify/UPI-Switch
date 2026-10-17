# AppTestify UPI Switch

A multi-tenant, multi-bank UPI switch that any payment gateway or aggregator plugs into through one API,
reaching NPCI through sponsor PSP banks. It handles intent, dynamic QR, collect and UPI AutoPay payments,
refunds (online, bulk, offline), T+1 three-way reconciliation, settlement and UDIR disputes.

Built from the solution pack *ATF-UPI-A01 Rev A* (solution architecture, intent/QR sequence, interface schedule).
External networks (sponsor banks, NPCI, payer apps) are replaced by a signed-XML **bank simulator** so the
whole system runs and is tested end to end locally.

## Quick start

```bash
# Docker: PostgreSQL, Redis, the switch and three simulated sponsor banks
docker compose up --build -d
docker compose run --rm demo          # onboard a PG, pay, collect, refund, AutoPay, recon, settle
open http://localhost:8080/admin/     # admin console, token: dev-admin-token-change-me

# Without Docker (needs a local PostgreSQL)
DATABASE_URL=postgres://postgres:postgres@localhost:5432/upiswitch?sslmode=disable ./scripts/run-local.sh
go run ./cmd/demo                     # in another shell

# Tests
make test                             # unit tests
make e2e                              # full system against PostgreSQL (TEST_DATABASE_URL)
```

## How the architecture maps to the code

| Architecture element (sheet 1) | Where |
|---|---|
| Switch API gateway: mTLS, per-PG keys, rate limits, idempotency | `internal/api/middleware.go`, `internal/ratelimit` (Redis-shared or local) |
| Txn orchestrator: state machine, idempotency | `internal/core`, `internal/domain` (INITIATED → PENDING → SUCCESS \| FAILED \| DEEMED) |
| Risk & fraud: velocity, limits, MCC | `internal/risk` |
| Smart PSP router: success rate, cost, failover | `internal/router` (EWMA success/latency, cost, circuit breaker, heartbeat `ReqHbt`) |
| QR / intent generator: signed `upi://pay` | `internal/intent` |
| Collect & mandate: collect, UPI AutoPay | `internal/core/payments.go`, `internal/core/mandates.go` |
| Refund engine: online, bulk, offline | `internal/core/refunds.go` (online falls back to the offline file if the bank is unreachable) |
| Recon & settlement: 3-way match, T+1 | `internal/recon` |
| Disputes / UDIR: TCC, RET, chargeback | `internal/core/disputes.go` (recon auto-raises TCC for unsettled deemed txns) |
| Notify & webhooks: signed events → PG | `internal/webhook` (transactional outbox, backoff, DLQ, replay) |
| UPI message builder: NPCI XML, HSM signing | `internal/npci` (ReqPay, RespPay, ReqChkTxn, ReqValAdd, ReqHbt, ReqMandate, ReqComplaint) |
| Bank callback listener: verify, dedupe, ack | `internal/core/callback.go` (signature, replay window, msgId dedupe, signed Ack) |
| Status & timeout: ReqChkTxn, deemed | `internal/core/poller.go` (60 s first check, exponential backoff, DEEMED after N tries) |
| PostgreSQL txn ledger, config DB | `internal/store`, `internal/db/migrations` |
| Audit log (immutable) | `audit_log` table: hash-chained, update/delete blocked by trigger, `/admin/audit/verify` |
| HSM, key vault, tokenisation | `internal/security` (`Signer` interface with a software HSM; AES-GCM vault; VPA tokens and masking) |
| Observability | `/metrics` (Prometheus), JSON logs with request ids, `/healthz`, `/readyz` |
| Admin console: PG onboarding, RBAC | `/admin/` UI over the admin API; roles `admin` / `ops` / `viewer` |

## Interfaces

**1. Payment gateway → switch** (REST/JSON, HMAC per tenant, `Idempotency-Key` on every POST)

| Method | Path | |
|---|---|---|
| POST | `/v1/upi/intent`, `/v1/upi/qr` | Signed intent URI (+ QR PNG). Steps 3–5. |
| POST | `/v1/upi/collect` | ReqValAdd on the payer, then collect ReqPay with PSP failover |
| GET | `/v1/upi/status/{txn_id}` or `?merchant_ref=` | `&refresh=true` runs ReqChkTxn now |
| POST | `/v1/upi/refund`, `/v1/upi/refund/bulk` | `mode: ONLINE \| OFFLINE` |
| GET | `/v1/upi/refund/{id}` | |
| POST | `/v1/upi/vpa/validate` | ReqValAdd |
| POST | `/v1/upi/mandate`, `/{id}/execute`, `/{id}/revoke`; GET `/{id}` | UPI AutoPay |
| POST/GET | `/v1/upi/disputes`, `/v1/upi/disputes/{id}` | UDIR |
| GET | `/v1/upi/reports/recon?date=`, `/v1/upi/reports/settlements?date=` | Tenant-scoped reports |

Request signing: `X-Client-Id`, `X-Timestamp` (unix seconds, ±5 min), and
`X-Signature = hex(HMAC-SHA256(secret, ts + "\n" + METHOD + "\n" + path?query + "\n" + hex(sha256(body))))`.
Tenants can additionally be pinned to an IP allow-list and an mTLS client-certificate CN.

**2/4. Switch ↔ sponsor banks**: signed NPCI XML over HTTPS to `{bank}/upi/{Api}`; bank callbacks to
`POST /v1/npci/callback/{psp_id}`. Bank links can use mTLS (`BANK_TLS_*`) and an IP allow-list (`BANK_IP_ALLOWLIST`).

**5. Switch → payment gateway**: webhooks `txn.success | txn.failed | txn.deemed | refund.* | mandate.* |
dispute.updated | recon.report`, signed `X-ATF-Signature: sha256=hex(HMAC(webhook_secret, ts + "." + body))`
with `X-ATF-Timestamp`, retried with backoff, then dead-lettered (`/admin/webhooks?status=DEAD`, replayable).

**6. Settlement files**: bank MIS and NPCI raw CSVs (`txn_id,rrn,type,amount,status,date`) with SHA-256
checksums, pulled per bank or posted inline to `POST /admin/recon/run`.

## Admin API (Bearer token, RBAC)

`POST /admin/tenants` (onboard, returns secrets once) · `PATCH /admin/tenants/{id}` (limits, routing, IP list,
suspend) · `POST /admin/tenants/{id}/rotate-keys` · `GET/PUT /admin/psps[/{id}]` (with live router health) ·
`GET /admin/txns[/{id}]` · `POST /admin/txns/{id}/check` · `POST /admin/recon/run` · `GET /admin/recon` ·
`POST /admin/settlements/run` · `GET /admin/settlements?format=csv` (payout file) · `POST /admin/settlements/{id}/paid` ·
`GET /admin/refunds/offline` · `POST /admin/refunds/offline-results` · `GET /admin/webhooks` ·
`POST /admin/webhooks/{id}/replay` · `GET /admin/disputes` · `GET /admin/audit[/verify]`.

## Bank simulator

`cmd/pspsim` plays a sponsor bank plus NPCI and the payer's app. `POST /sim/pay {intent_uri, payer_vpa}` pays an
intent the way a customer would. Outcomes follow keywords in the payer VPA: `nofunds`, `badpin`, `decline`,
`invalid` (ValAdd fails), `deemed` (DEEMED now, SUCCESS in the NPCI file), `timeout` (no callback; ReqChkTxn
says SUCCESS), `lost` (no callback, stays pending, FAILED in the NPCI file). `POST /sim/down {"Down":true}` takes
the bank offline to exercise failover; `GET /files/mis|npci-raw?date=` serves settlement files.

## Configuration

Environment variables (see `internal/config`): `DATABASE_URL`, `REDIS_URL`, `DATA_KEY` (vault key, required),
`SIGNING_KEY`/`SIGNING_KEY_ID`, `ADMIN_TOKENS` (`name:role:token,...`), `PSP_SEED`, `TLS_CERT`/`TLS_KEY`/`TLS_CLIENT_CA`,
`BANK_TLS_CERT`/`BANK_TLS_KEY`/`BANK_TLS_CA`, `BANK_IP_ALLOWLIST`, `TRUST_PROXY`, `RECON_TZ` (default Asia/Kolkata),
`FIRST_CHECK_AFTER` (60s), `MAX_CHECK_ATTEMPTS` (6), `INTENT_EXPIRY` (15m), `BLOCKED_MCC`, `POLL_EVERY`, `WEBHOOK_EVERY`.

## Choices made where the pack left them open

- **Go + PostgreSQL**, one stateless binary that scales horizontally; workers coordinate through row leases
  (`FOR UPDATE SKIP LOCKED`), so any number of instances can run active-active.
- **Event bus**: a PostgreSQL transactional outbox feeds webhooks, so a state change and its event commit together.
  Kafka and the data lake are not wired in yet.
- **HSM**: a software signer behind the `security.Signer` interface; a PKCS#11 HSM drops in behind the same interface.
- **XML signature**: enveloped RSA-SHA256 over the exact document bytes. Real NPCI certification needs W3C XML-DSig
  with C14N and NPCI's certificates; that is the first thing to swap when a sponsor bank shares its spec.
- **Message shapes** follow NPCI element names, simplified. ReqChkTxn and ReqValAdd are answered synchronously.
- **Settlement files** are pulled over HTTP from the simulator in place of SFTP; checksums are verified, PGP is not yet.
- **Bank names, handles, endpoints and service levels** are placeholders, as the pack itself notes.

## Not yet production-ready

Certification against a real sponsor bank (XML-DSig/C14N, NPCI schemas, PGP/SFTP), a hardware HSM, Kafka,
the data lake/MIS, SIEM/CERT-In integration, DR replication and load testing toward the 3,000 TPS / p99 < 150 ms
targets are outside this first build.
