# Manual end-to-end test playbook

A single document you can run **command by command, top to bottom**, to
bring up the full local Loka stack and verify both paycli routes pay
real Lightning invoices through Prism.

Last verified: 2026-05-06 against the local devnet.

## What you'll end up with

- `lnd-sui` Alice + Bob with an open channel (Bob: 5 SUI local capacity)
- `loka-prism-l402` on `https://127.0.0.1:8080`
- `agents-pay-service` (LNbits fork) on `http://127.0.0.1:5002`,
  super-user provisioned with a known password
- `paycli` binary at `bin/paycli`
- Two confirmed L402 payments through Prism — one hosted, one node

## Prerequisites

- Go 1.23+ (for paycli + lnd build) and Python 3.12+ (for lnbits)
- `sui` CLI installed and on PATH
- A clean shell — every block below assumes you're at the repo root
  (`/Users/blake/work/nagara/code/chain/loka-payment`)

Set these once, all subsequent blocks reference them:

```bash
LOKA=/Users/blake/work/nagara/code/chain/loka-payment
LNCLI=$LOKA/lnd/lncli-debug
ALICE_DIR=/tmp/lnd-sui-test/alice
BOB_DIR=/tmp/lnd-sui-test/bob
ALICE_REST=https://127.0.0.1:8081
BOB_REST=https://127.0.0.1:8082
PRISM_URL=https://127.0.0.1:8080
LNBITS_URL=http://127.0.0.1:5002
mkdir -p /tmp/paycli-itest
```

---

## 1 · Build paycli

```bash
( cd $LOKA/paycli && make build )
PAYCLI=$LOKA/paycli/bin/paycli
$PAYCLI --version
```

---

## 2 · Start lnd-sui (Alice + Bob, local SUI node)

The script spins up a fresh SUI localnet, regenerates Alice/Bob lnd dirs,
funds them, and opens a 5 SUI channel. The trailing `read -p ""` would
exit on stdin EOF — feeding it a stdin that never closes makes it stay
running in the background without blocking interactively.

```bash
( cd $LOKA/lnd && \
    ./scripts/itest_sui_single_coin.sh localnet \
        < <(while true; do sleep 3600; done) \
        > /tmp/paycli-itest/itest.log 2>&1 ) &

# Wait for the channel to come up (~30-60s on a fresh box).
until grep -q "Channel is fully operational" /tmp/paycli-itest/itest.log; do sleep 5; done
echo "lnd-sui ready"
```

Sanity check:

```bash
MAC=$(xxd -ps -u -c 1000 $ALICE_DIR/data/chain/sui/devnet/admin.macaroon)
curl -ks --cacert $ALICE_DIR/tls.cert \
    $ALICE_REST/v1/getinfo \
    -H "Grpc-Metadata-macaroon: $MAC" | jq '{alias, block_height, num_active_channels}'
# Expect: alias is the pubkey prefix, block_height > 0, num_active_channels == 1
```

If `getinfo` returns `<html>...debug/pprof/...` the SUI localnet has
degraded — see `docs/integration-test.md` § "When SUI chain RPC drifts".

---

## 3 · Start Prism

```bash
( cd $LOKA/loka-prism-l402 && \
    ./prism --configfile=./sample-conf-tmp.yaml \
        < /dev/null > /tmp/paycli-itest/prism.log 2>&1 ) &

until nc -z 127.0.0.1 8080; do sleep 1; done
echo "prism ready"
```

`sample-conf-tmp.yaml` is configured to use Alice's macaroon as the
authenticator. It exposes three services:

| Service | Host header | Backend lnd | Notes |
|---|---|---|---|
| `service1` | `service1.com` | Alice (authenticator's lnd) | invoices land in Alice's wallet |
| `service2` | `merchant-bob.local` | Bob (per-service backend) | invoices land in Bob's wallet — multi-merchant mode |
| `service3` | n/a | n/a | demo, not used here |

---

## 4 · Bootstrap agents-pay-service (fresh install)

Wipe any prior state so we start clean and own the super-user password:

```bash
TS=$(date +%s)
mv $LOKA/agents-pay-service/data $LOKA/agents-pay-service/data.bak.$TS 2>/dev/null || true
mkdir -p $LOKA/agents-pay-service/data
```

Start the service with the self-payment env flag (it gets persisted to
DB on first install — see § "Why env vars don't override at boot" below):

```bash
( cd $LOKA/agents-pay-service && \
    LND_GRPC_ALLOW_SELF_PAYMENT=true \
    ./.venv/bin/lnbits --port 5002 \
    < /dev/null > /tmp/paycli-itest/lnbits.log 2>&1 ) &

until nc -z 127.0.0.1 5002; do sleep 1; done
sleep 3
curl -s $LNBITS_URL/api/v1/health
```

Claim the install and set a known password:

```bash
ADMIN_PASSWORD=paycli-test-2026
curl -s -X PUT $LNBITS_URL/api/v1/auth/first_install \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"admin\",\"password\":\"$ADMIN_PASSWORD\",\"password_repeat\":\"$ADMIN_PASSWORD\"}" \
    | jq '{token_type, has_token: (.access_token != null)}'
# Expect: {"token_type":"bearer","has_token":true}
```

Login via paycli (caches the JWT for `topup` and `admin-set`):

```bash
$PAYCLI --base-url $LNBITS_URL auth-login \
    --username admin --password $ADMIN_PASSWORD
# → "logged in; token cached"
```

Switch the funding source from VoidWallet → LndWallet so the wallet
actually talks to Alice's lnd-sui. Then restart so the new class is
instantiated:

```bash
$PAYCLI admin-set lnbits_backend_wallet_class LndWallet

pkill -f 'lnbits --port 5002'
sleep 2
( cd $LOKA/agents-pay-service && \
    LND_GRPC_ALLOW_SELF_PAYMENT=true \
    ./.venv/bin/lnbits --port 5002 \
    < /dev/null > /tmp/paycli-itest/lnbits.log 2>&1 ) &
until nc -z 127.0.0.1 5002; do sleep 1; done
sleep 4

grep -E "Funding source|connected" /tmp/paycli-itest/lnbits.log | tail -3
# Expect: "Funding source: LndWallet" + "Backend LndWallet connected and with a balance of … msat"
```

---

## 5 · Verify Prism catalog (paycli services)

```bash
$PAYCLI services \
    --prism-url       $PRISM_URL \
    --prism-macaroon  $LOKA/loka-prism-l402/.prism/admin.macaroon \
    --insecure | jq '.[].name'
# Expect: "service1", "service2", "service3"
```

---

## 6 · Hosted route — pay through service2 (Alice → Bob)

This is the **canonical end-to-end hosted-route test** because it
exercises a real cross-wallet payment.

```bash
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI --base-url $LNBITS_URL register "h-test"

# Mint a topup invoice and have Bob pay it via REST.
INV=$(PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
        $PAYCLI fund --amount 100000000 --memo topup \
        | jq -r .bolt11)

BOB_MAC=$(xxd -ps -u -c 1000 $BOB_DIR/data/chain/sui/devnet/admin.macaroon)
curl -ks --cacert $BOB_DIR/tls.cert \
    -X POST $BOB_REST/v1/channels/transactions \
    -H "Grpc-Metadata-macaroon: $BOB_MAC" \
    -d "{\"payment_request\":\"$INV\"}" \
    | jq '{payment_error, preimage_set: (.payment_preimage != "")}'
# Expect: payment_error="" and preimage_set=true.

sleep 2
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI whoami
# Expect: balance ≈ 100000000000 (msat).

# Pay through Prism — service2 routes invoices to Bob's lnd, so this is
# Alice (paycli wallet) → Bob (Prism's per-service backend).
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI request --insecure-target \
    -H 'Host: merchant-bob.local' -i \
    $PRISM_URL/freebieservice
# Expect: HTTP 200 / 404 from the backend service (not 402).
# 404 just means the demo backend at 127.0.0.1:9998 has no /freebieservice path,
# but Prism HAS accepted the LSAT and forwarded — that's the L402 win.

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI whoami
# Expect: balance ≈ 90000000000 — exactly 10M sat (service2 price) was deducted.
```

---

## 7 · Node route — Bob's lnd-sui pays Alice via service1

This is the canonical cross-wallet **node-route** test.

```bash
PAYCLI_CONFIG=/tmp/paycli-cfg-n.json $PAYCLI register --route node \
    --lnd-endpoint  $BOB_REST \
    --lnd-tls-cert  $BOB_DIR/tls.cert \
    --lnd-macaroon  $BOB_DIR/data/chain/sui/devnet/admin.macaroon \
    | jq '{alias, num_active_channels, block_height}'
# Expect: num_active_channels=1, block_height>0, no pprof error.

# Capture Bob's channel local balance before the payment.
BOB_MAC=$(xxd -ps -u -c 1000 $BOB_DIR/data/chain/sui/devnet/admin.macaroon)
BEFORE=$(curl -ks --cacert $BOB_DIR/tls.cert \
    $BOB_REST/v1/balance/channels \
    -H "Grpc-Metadata-macaroon: $BOB_MAC" \
    | jq -r '.local_balance.sat')
echo "before: $BEFORE sat"

# Drive L402 against service1 — invoices land in Alice's wallet, paid by Bob.
PAYCLI_CONFIG=/tmp/paycli-cfg-n.json \
    $PAYCLI request --insecure-target \
    -H 'Host: service1.com' -i \
    $PRISM_URL/freebieservice
# Expect: HTTP 200 / 404 from backend.

AFTER=$(curl -ks --cacert $BOB_DIR/tls.cert \
    $BOB_REST/v1/balance/channels \
    -H "Grpc-Metadata-macaroon: $BOB_MAC" \
    | jq -r '.local_balance.sat')
echo "after:  $AFTER sat"
echo "delta:  $((BEFORE - AFTER)) sat"
# Expect delta ≈ 10000000 (service price) + small routing fee.
```

---

## 8 · Operator topup (skip the LN channel)

Useful when channels are unavailable or you want a deterministic credit.

```bash
WALLET_ID=$(jq -r .wallet_id /tmp/paycli-cfg-h.json)

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI topup --wallet-id $WALLET_ID --amount 50000000
# → "credited 50000000 to wallet …"

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI whoami
# Balance increased by 50,000,000 (note: msat — this is 50,000 sats).
```

Negative `--amount` debits.

---

## 9 · Cleanup

```bash
pkill -f 'lnbits --port 5002'
pkill -f 'prism --configfile'
# To stop the lnd-sui itest cleanly: send the script SIGTERM so its EXIT
# trap kills alice/bob/sui:
pkill -f 'itest_sui_single_coin.sh'
rm -f /tmp/paycli-cfg-h.json /tmp/paycli-cfg-n.json
```

---

## Caveats discovered during this verification

### Why env vars don't override at boot (use `paycli admin-set` instead)

`LND_GRPC_ALLOW_SELF_PAYMENT=true`, `LNBITS_BACKEND_WALLET_CLASS=LndWallet`,
and similar env vars *do* get loaded by Pydantic at startup — but lnbits'
boot path then calls `update_cached_settings(settings_db.dict())`, where
`settings_db` is a `SuperSettings` constructed via `dict_to_model`. The
constructor fills in **every editable field** with its class default for
keys not present in the DB, then writes that "DB + defaults" dict back
to the in-memory settings. Net effect: any editable setting not
explicitly persisted to the `system_settings` table reverts to its
class default at boot.

Workarounds:
- **Fresh install path** (the one this guide uses): on a brand-new DB,
  `init_admin_settings()` snapshots `settings.dict()` *after* env load
  and persists every field. So env vars seed the DB once, then live
  there durably.
- **Existing install path**: use `paycli admin-set <key> <value>`
  (which PATCHes `/admin/api/v1/settings`) to update the DB row.

### lnd-sui self-payment doesn't complete on this devnet

When the same lnd both issues an invoice and pays it (the exact case for
`paycli (Alice) → service1 (Alice authenticator)`), `SendPaymentV2`
returns `IN_FLIGHT` and never resolves to `SUCCEEDED` — the corresponding
invoice stays `OPEN`. lnbits' wallet driver returns "pending" with an
empty preimage, so the L402Doer correctly refuses to fabricate an LSAT
token. The wallet balance is still optimistically debited because lnbits
treats `IN_FLIGHT` as not-yet-failed.

This is an **lnd-sui upstream behavior**, not a paycli or lnbits bug.
Standard lnd handles self-payments via a routing short-circuit; the
SUI chain-backend integration apparently doesn't wire that path through
to the payment-completion state machine.

For e2e testing always use a **cross-wallet** route — service2 in this
playbook (Alice paywall, Bob backend) is the canonical hosted-route
e2e, and the node route (Bob → service1 = Alice) covers the other
direction.

### Bob's `payinvoice --force` via the lnd CLI hangs

The `lncli payinvoice` flow stays on `IN_FLIGHT` because of the same
chain-backend issue when used against the lnd-sui REST gateway under
some chain states. The REST `POST /v1/channels/transactions` path used
in this guide works reliably on a fresh `itest_sui_single_coin.sh`
restart and is the recommended way to fund hosted wallets from Bob.

If REST also stalls, the SUI localnet has drifted — restart per § 2.
