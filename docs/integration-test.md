# Integration test playbook

lokapay ships an integration test suite that exercises the SDK against a real
local Loka Payment stack. It's gated behind the `integration` build tag so
unit tests stay hermetic.

## Prerequisite services

Start each in a separate terminal — leave them running.

### 1. lnd-sui (Alice + Bob)

```bash
cd /path/to/lnd
./scripts/itest_sui_single_coin.sh
```

The script starts Alice on `127.0.0.1:10009` and Bob on `127.0.0.1:10010`,
funds them via Sui devnet, and opens a 5 SUI channel. Wait for
`✅ Test workflow completed!` and leave the script paused — it cleans up the
nodes when you press Enter.

The macaroons land in `/tmp/lnd-sui-test/{alice,bob}/data/chain/sui/devnet/`.

### 2. agents-pay-service on `:5002`

The wallet service should be configured to use Alice's lnd-sui as its
funding source. Start it on port 5002:

```bash
cd /path/to/agents-pay-service
PORT=5002 ./venv/bin/lnbits   # or however your env launches lnbits
```

Sanity-check:

```bash
curl http://127.0.0.1:5002/api/v1/health
# {"server_time":...,"up_time":"00:00:01"}
```

### 3. loka-prism-l402 on `:8080`

Use the dev config that points at Alice's macaroon path:

```bash
cd /path/to/loka-prism-l402
./aperture --configfile sample-conf-tmp.yaml
```

Sanity-check (`-k` because the dev cert is self-signed):

```bash
curl -ksI https://127.0.0.1:8080/
# HTTP/2 200
```

## Running the tests

```bash
cd /path/to/paycli
scripts/integration-test.sh
```

The script sets the env vars below and runs `go test -tags=integration` in
the `tests/` package:

| Var | Default | Purpose |
|---|---|---|
| `PAYCLI_IT_LNBITS_URL` | `http://127.0.0.1:5002` | agents-pay-service base URL |
| `PAYCLI_IT_PRISM_URL` | `https://127.0.0.1:8080` | Prism base URL (skipped if unset) |
| `PAYCLI_IT_PRISM_TARGET` | `${PAYCLI_IT_PRISM_URL}/` | Specific URL the L402 test calls |
| `PAYCLI_IT_FUNDED_ADMIN_KEY` | _(unset)_ | Admin key of a pre-funded hosted wallet for full L402 e2e |
| `PAYCLI_IT_NODE_ALICE_DIR` | _(unset)_ | Alice's lnd dir, e.g. `/tmp/lnd-sui-test/alice` (enables node-mode tests) |
| `PAYCLI_IT_NODE_ALICE_REST` | `https://127.0.0.1:8081` | Alice's REST listener |

## What the tests cover

**Hosted route (always runs):**

* `TestAccountAndWallet` — creates a fresh account, fetches it back, then
  attempts to add a sub-wallet (skipped silently if the LNbits server has
  `user_id_only` auth disabled).
* `TestCreateInvoice` — mints a BOLT11 invoice on a fresh wallet via
  `POST /api/v1/payments` and validates the returned `payment_hash`.
* `TestL402_AgainstPrism` — drives the full L402 path against Prism:
  HTTP request → 402 challenge → SDK calls `PayInvoice` →
  surface result.

**Node route (runs when `PAYCLI_IT_NODE_ALICE_DIR` is set):**

* `TestNode_AddInvoice_AgainstAlice` — builds a `NodeClient` from
  Alice's tls.cert + admin.macaroon and exercises `AddInvoice` against
  her live REST gateway.
* `TestNode_ChannelBalance_AgainstAlice` — read-only sanity check; logs
  Alice's local channel balance.

`SendPaymentSync` and `GetInfo` are intentionally not covered by
integration tests because lnd-sui's SUI-RPC layer sometimes returns the
chain backend's pprof page on those endpoints — that's a backend issue,
not an SDK issue.

  When `PAYCLI_IT_FUNDED_ADMIN_KEY` is unset, the test creates a fresh
  empty wallet and accepts an `Insufficient balance` error from
  agents-pay-service as a successful "challenge plumbing works" signal.
  Set `PAYCLI_IT_FUNDED_ADMIN_KEY` to an admin key whose wallet has been
  topped up (e.g. via a `bob payinvoice` of a `lokapay fund` invoice) for
  a true end-to-end pay-and-replay assertion.

## Verified end-to-end flows

These sequences have been run against the local stack and confirmed
to settle real Lightning payments. Use them as smoke tests when you
want to convince yourself the full chain is healthy.

### Hosted route — lokapay paying through agents-pay-service

```bash
# 1. fresh wallet via lokapay
lokapay --base-url http://127.0.0.1:5002 register "h-test"

# 2. mint a fund invoice and have Bob pay it via REST (proves Bob's
#    SUI chain backend is healthy — see "When SUI chain RPC drifts" below)
INV=$(lokapay fund --amount 100000000 --memo topup | jq -r .bolt11)
curl -ks --cacert /tmp/lnd-sui-test/bob/tls.cert \
    -X POST https://127.0.0.1:8082/v1/channels/transactions \
    -H "Grpc-Metadata-macaroon: $(xxd -ps -u -c 1000 /tmp/lnd-sui-test/bob/data/chain/sui/devnet/admin.macaroon)" \
    -d "{\"payment_request\":\"$INV\"}"

# 3. drive an L402 request. Use a service whose payment backend is NOT the
#    same lnd as agents-pay-service's funding source — otherwise lnd will
#    refuse with "self-payments not allowed".
lokapay request --insecure-target \
    -H "Host: merchant-bob.local" -i \
    https://127.0.0.1:8080/freebieservice
# Expected: HTTP 200 from the backend (or 404 if no path), wallet balance
# decreased by 10M sat (service2's price = 10000000 MIST).
```

### Node route — lokapay driving the user's own lnd-sui

```bash
# 1. point lokapay at Bob's REST gateway
lokapay register --route node \
    --lnd-endpoint  https://127.0.0.1:8082 \
    --lnd-tls-cert  /tmp/lnd-sui-test/bob/tls.cert \
    --lnd-macaroon  /tmp/lnd-sui-test/bob/data/chain/sui/devnet/admin.macaroon

# 2. drive L402 — same command as hosted, dispatches by saved route.
#    Bob has 5 SUI in the channel, well above service prices.
lokapay request --insecure-target -H "Host: service1.com" -i \
    https://127.0.0.1:8080/freebieservice
# Expected: HTTP 200 / 404 from backend, Bob's channel local_balance
# decreased by ~10M sat.
```

## When SUI chain RPC drifts (the pprof page error)

Symptom on chain-touching REST calls (`/v1/getinfo`,
`/v1/balance/blockchain`, `/v1/channels/transactions`) — lnd returns:

```
{"code":2,"message":"unable to get best block info: JSON decode error:
 invalid character '<' ... <html>...debug/pprof/..."}
```

What's happening: lnd-sui's chain client is hitting the SUI JSON-RPC
on port 9000, but the SUI process has degraded into a state where
**every HTTP request 303-redirects to `/debug/pprof`**. lnd treats
the HTML body as JSON and surfaces this nested error.

Channel-only RPCs (`/v1/balance/channels`, `/v1/invoices`,
`/v1/payments`) keep working because they don't query chain state —
that's why `lokapay fund` and `lokapay history` may keep working even
when `lokapay pay` and `lokapay whoami` are broken.

Fix: full restart of the integration stack.

```bash
# 1. kill the existing itest process (or its child lnd/sui)
kill -INT <itest_pid>

# 2. restart with non-closing stdin so the script's final `read -p ""`
#    doesn't immediately exit when stdin EOFs:
( cd /path/to/lnd && \
    ./scripts/itest_sui_single_coin.sh localnet \
        < <(while true; do sleep 3600; done) \
        > /tmp/itest.log 2>&1 ) &

# 3. restart prism + agents-pay-service so they reconnect with the
#    newly-generated macaroons (paths stay the same, contents change):
pkill -f "prism --configfile|aperture --configfile"
pkill -f "lnbits --port 5002"
( cd /path/to/loka-prism-l402 && \
    ./prism --configfile=./sample-conf-tmp.yaml < /dev/null > /tmp/prism.log 2>&1 ) &
( cd /path/to/agents-pay-service && \
    ./.venv/bin/lnbits --port 5002 < /dev/null > /tmp/lnbits.log 2>&1 ) &
```

After restart, `curl https://127.0.0.1:8081/v1/getinfo` should return a
real `block_height` instead of the pprof body.

## Funding a wallet for full e2e

```bash
# 1. create a fresh wallet
lokapay --base-url http://127.0.0.1:5002 register "it-funded"
ADMIN=$(jq -r .admin_key ~/.lokapay/config.json)

# 2. issue a topup invoice
INV=$(lokapay fund --amount 1000000 --memo it-topup | jq -r .bolt11)

# 3. pay it from Bob's lnd-sui (the channel-funded side)
LNCLI_BIN=/path/to/lnd/lncli-debug
LNDDIR=/tmp/lnd-sui-test/bob
$LNCLI_BIN --lnddir=$LNDDIR --rpcserver=localhost:10010 \
    --macaroonpath=$LNDDIR/data/chain/sui/devnet/admin.macaroon \
    payinvoice --pay_req="$INV" --force

# 4. run the e2e test against this funded wallet
PAYCLI_IT_FUNDED_ADMIN_KEY=$ADMIN scripts/integration-test.sh
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `dial tcp 127.0.0.1:5002: connection refused` | agents-pay-service not started on 5002 |
| `Missing user ID or access token` on `add-wallet` | `LNBITS_AUTH_METHODS` doesn't include `user_id_only` |
| 402 loop / `ErrChallengeExhausted` | wallet has no funds OR Prism's macaroon is rejected by itself; check `--max-retries` and channel balance |
| `Insufficient balance.` from `lokapay pay` / `request` | wallet not funded; see "Funding a wallet" above |
| `cannot unmarshal string into Go struct field Payment.time of type int64` | SDK out of sync with server schema; `Payment.Time` is `interface{}` since v0.1 — rebuild `lokapay` |
