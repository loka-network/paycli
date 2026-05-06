#!/usr/bin/env bash
#
# Run paycli integration tests against the local stack.
#
# Prerequisites (start in this order, leave running in separate terminals):
#
#   1. lnd-sui Alice + Bob channel:
#        cd /path/to/lnd && ./scripts/itest_sui_single_coin.sh
#      Wait for "Test workflow completed" and leave the terminal open.
#
#   2. agents-pay-service on :5002 with funding_source pointed at Alice's lnd-sui:
#        cd /path/to/agents-pay-service && PORT=5002 ./venv/bin/lnbits
#
#   3. loka-prism-l402 on :8080 using sample-conf-tmp.yaml:
#        cd /path/to/loka-prism-l402 && ./aperture --configfile sample-conf-tmp.yaml
#
# Then this script just runs:
#
#   PAYCLI_IT_LNBITS_URL=http://127.0.0.1:5002 \
#   PAYCLI_IT_PRISM_URL=https://127.0.0.1:8080 \
#   go test -tags=integration -v ./tests/...
#
set -euo pipefail

cd "$(dirname "$0")/.."

: "${PAYCLI_IT_LNBITS_URL:=http://127.0.0.1:5002}"
: "${PAYCLI_IT_PRISM_URL:=https://127.0.0.1:8080}"
: "${PAYCLI_IT_PRISM_TARGET:=${PAYCLI_IT_PRISM_URL}/}"

export PAYCLI_IT_LNBITS_URL PAYCLI_IT_PRISM_URL PAYCLI_IT_PRISM_TARGET

echo "[paycli-it] LNbits = $PAYCLI_IT_LNBITS_URL"
echo "[paycli-it] Prism  = $PAYCLI_IT_PRISM_URL"

# Sanity check: agents-pay-service reachable.
if ! curl -fsS "${PAYCLI_IT_LNBITS_URL%/}/api/v1/health" >/dev/null 2>&1; then
    echo "[paycli-it] agents-pay-service health check failed at $PAYCLI_IT_LNBITS_URL"
    echo "  start it first (see top of this script)"
    exit 1
fi

go test -tags=integration -race -v -timeout 5m ./tests/...
