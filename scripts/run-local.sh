#!/usr/bin/env bash
# Runs the switch and two simulated banks against a local PostgreSQL, without Docker.
#   DATABASE_URL=postgres://... ./scripts/run-local.sh
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
go build -o bin/ ./cmd/...
./bin/keygen -dir keys -names switch,bank-a,bank-b,bank-c >/dev/null

export DATABASE_URL="${DATABASE_URL:-postgres://postgres:postgres@localhost:5432/upiswitch?sslmode=disable}"
export DATA_KEY="${DATA_KEY:-dev-data-key-change-me}"
export ADMIN_TOKENS="${ADMIN_TOKENS:-root:admin:dev-admin-token-change-me}"
export FIRST_CHECK_AFTER="${FIRST_CHECK_AFTER:-20s}"
export CONSOLE_USERS="${CONSOLE_USERS:-admin@apptestify.in:admin:Demo@2026,ops@apptestify.in:ops:Demo@2026,viewer@apptestify.in:viewer:Demo@2026}"

pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
SIM_ID=bank-a SIM_ORG=BKA SIM_HANDLE=banka SIM_KEY=keys/bank-a.key SWITCH_PUBLIC_KEY=keys/switch.pub LISTEN_ADDR=:9101 ./bin/pspsim > bank-a.log 2>&1 & pids+=($!)
SIM_ID=bank-b SIM_ORG=BKB SIM_HANDLE=bankb SIM_KEY=keys/bank-b.key SWITCH_PUBLIC_KEY=keys/switch.pub LISTEN_ADDR=:9102 ./bin/pspsim > bank-b.log 2>&1 & pids+=($!)
SIM_ID=bank-c SIM_ORG=BKC SIM_HANDLE=bankc SIM_KEY=keys/bank-c.key SWITCH_PUBLIC_KEY=keys/switch.pub LISTEN_ADDR=:9103 ./bin/pspsim > bank-c.log 2>&1 & pids+=($!)
SIGNING_KEY=keys/switch.key PSP_SEED=deploy/psps.local.json ./bin/switch > switch.log 2>&1 & pids+=($!)

for i in $(seq 1 50); do curl -sf localhost:8080/readyz >/dev/null && break; sleep 0.2; done
echo "switch ready on :8080 (admin console http://localhost:8080/admin/); logs in switch.log, bank-*.log"
echo "sign in as admin@apptestify.in / Demo@2026; seed demo history with: go run ./cmd/seed (DATA_KEY=$DATA_KEY)"
echo "then keep it moving with: go run ./cmd/traffic, or run one scripted flow with: go run ./cmd/demo"
wait
