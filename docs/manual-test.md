# Manual end-to-end test playbook

A single document you can run **command by command, top to bottom**, to
bring up the full local Loka stack and verify both paycli routes pay
real Lightning invoices through Prism.

Last verified: 2026-05-07 against the local devnet.

## What you'll end up with

- `lnd-sui` Alice + Bob with two open channels (incl. self-payment-capable
  topology — see step [8/8] of the itest)
- `loka-prism-l402` on `https://127.0.0.1:8080`
- `agents-pay-service` (LNbits fork) on `http://127.0.0.1:5002`,
  super-user provisioned with a known password
- `paycli` binary at `bin/paycli`
- A demo user account with three sub-wallets (one default, two
  per-agent) — exercises the multi-wallet model
- Confirmed L402 payments through Prism on both routes
- Structured payment audit trail at `~/.paycli/events.jsonl`

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

## 6 · Hosted route — register modes

paycli has two `register` modes on the hosted route. Pick the one that
matches your role.

### 6a · Anonymous register (the default — no creds needed)

`paycli register "<wallet-name>"` calls `POST /api/v1/account`, which is
LNbits' anonymous fast path: no username, no password, no email. The DB
row gets only a generated `id`, so this account **cannot** log into the
LNbits dashboard. Designed for AI-agent fleet provisioning where
the `admin_key` is the only auth the caller will ever use.

```bash
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI --base-url $LNBITS_URL register "h-test"
# Output: { id, user, name: "h-test", adminkey, inkey, ... }
# config.json: hosted.wallets["default"] = { admin/invoice key, wallet_id }
```

### 6b · Named register (--username, dashboard-capable, multi-wallet)

When `--username` is set, paycli switches to `POST /api/v1/auth/register`.
The server stores a real username + bcrypt password hash, so the user
can:

- log into the LNbits dashboard (`/wallet` page)
- be re-authenticated by `paycli auth-login`
- act as super-user / operator if their account id is in
  `super_user` / `lnbits_admin_users`

paycli also fetches the auto-created wallet's keys via
`GET /api/v1/wallets` (Bearer JWT) and caches both keys + JWT in the
config — one command, fully ready for `topup` / `admin-set` / multi-wallet
provisioning.

> ⚠️ **Flag ordering**: urfave/cli v2 stops parsing flags at the first
> positional argument. Put `--username` / `--password` / `--email`
> **before** the wallet-name positional. paycli detects the wrong order
> and prints a helpful error, but it's worth knowing.

> ⚠️ **Username regex**: lnbits' `is_valid_username` only accepts
> `[a-zA-Z0-9._]{2,20}` — no dashes / hyphens, no leading/trailing `_`
> or `.`, no `__` / `..`.

```bash
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI --base-url $LNBITS_URL \
    register --username alice --password "alice_pw_2026" --email alice@example.com \
    "main"
# config.json now also has:
#   hosted.username = "alice"
#   hosted.admin_bearer_token = "<JWT>"  ← cached, no re-login needed
#   hosted.wallets["default"] = { ... }
#   hosted.active_wallet = "default"
```

This is the recommended setup for the rest of this playbook because it
unlocks step 6c (multi-wallet) + step 8 (operator topup via
`admin_bearer_token`).

### 6c · Multi-wallet — one user, many agents

A user account holds N sub-wallets. Each gets its own `admin_key` /
`invoice_key`, so different agents can be scoped to different wallets:

```bash
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI wallets list
# Initially: just "default" (the one created at register time)

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI wallets add agentresearch
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI wallets add agenttrading

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI wallets list
# ACT NAME              WALLET_ID                          ADMIN_KEY    INVOICE_KEY
#     agentresearch     f9c7abb6db01...                    9113****     b1ca****
#     agenttrading      eba80c3fd3ae...                    3470****     3819****
# *   default           68146cff5ff7...                    e691****     9917****

# Switch active for a session:
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI wallets use agentresearch

# Override per-call without switching:
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI --wallet default whoami
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI --wallet agenttrading fund --amount 1000
```

The dashboard at `http://127.0.0.1:5002` (login: alice / alice_pw_2026)
sees the same three wallets under one account.

### 6d · Pay through Prism (canonical hosted L402 e2e)

```bash
# Mint a topup invoice on the active wallet (default = "default").
INV=$(PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
        $PAYCLI fund --amount 100000000 --memo topup \
        | jq -r .bolt11)

# Have Bob pay it via REST.
BOB_MAC=$(xxd -ps -u -c 1000 $BOB_DIR/data/chain/sui/devnet/admin.macaroon)
curl -ks --cacert $BOB_DIR/tls.cert \
    -X POST $BOB_REST/v1/channels/transactions \
    -H "Grpc-Metadata-macaroon: $BOB_MAC" \
    -d "{\"payment_request\":\"$INV\"}" \
    | jq '{payment_error, preimage_set: (.payment_preimage != "")}'
# Expect: payment_error="" and preimage_set=true.

sleep 2
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI whoami
# Expect: alias="default", balance ≈ 100000000000 (msat).

# Pay through Prism — service2 routes to Bob's lnd, so this is
# Alice (paycli wallet) → Bob (Prism's per-service backend).
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI request --insecure-target \
    -H 'Host: merchant-bob.local' -i \
    $PRISM_URL/freebieservice
# Expect: HTTP 200 / 404 from the backend (not 402).
# 404 just means the demo backend at 127.0.0.1:9998 has no /freebieservice
# path, but Prism accepted the LSAT and forwarded — that's the L402 win.

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI whoami
# Expect: balance ≈ 90000000000 — exactly 10M sat (service2 price) deducted.
```

---

### 6e · Self-payment (Alice → Alice via service1)

If you want to verify the upstream-lnd self-payment story is wired up
end-to-end (see § "lnd-sui self-payment" caveat below), this is the
test. It needs `lnd_grpc_allow_self_payment=true` on the lnbits side:

```bash
$PAYCLI admin-set lnd_grpc_allow_self_payment true
pkill -f 'lnbits --port 5002'                       # restart so the funding
( cd $LOKA/agents-pay-service && \                  # source picks up the flag
    LND_GRPC_ALLOW_SELF_PAYMENT=true \
    ./.venv/bin/lnbits --port 5002 \
    < /dev/null > /tmp/paycli-itest/lnbits.log 2>&1 ) &
until nc -z 127.0.0.1 5002; do sleep 1; done

# Now `request` against service1 (authenticator = Alice = paycli's
# funding source) settles via the alice ↔ bob ↔ alice cycle that step
# [8/8] of the itest also exercises.
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI request --insecure-target -H 'Host: service1.com' -i \
    $PRISM_URL/freebieservice
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
Requires the cached super-user JWT — present after step 6b's named
register (the JWT was issued to the `alice` account; whether `topup`
works depends on `alice`'s id matching `super_user` / `lnbits_admin_users`,
which the bootstrap admin from step 4 always satisfies).

If `alice` doesn't have super-user rights, log in explicitly:

```bash
$PAYCLI --base-url $LNBITS_URL auth-login \
    --username admin --password $ADMIN_PASSWORD
```

Then topup. Without `--wallet-id`, paycli targets the active sub-wallet
of the current config:

```bash
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI topup --amount 50000000
# → credited 50000000 to wallet <id>

# Or pick a specific sub-wallet:
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI --wallet agentresearch topup --amount 50000000

# Or pass wallet-id directly (legacy):
WALLET_ID=$(jq -r '.hosted.wallets.default.wallet_id' /tmp/paycli-cfg-h.json)
PAYCLI_CONFIG=/tmp/paycli-cfg-h.json \
    $PAYCLI topup --wallet-id $WALLET_ID --amount 50000000

PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI whoami
# Balance increased.  Negative --amount debits.
```

---

## 9 · Inspect the local payment event log

Every state-changing payment command (`register`, `wallets add`,
`fund`, `pay`, `request` per L402 settlement, `topup`) appends a
structured row to `~/.paycli/events.jsonl` (or wherever `$PAYCLI_CONFIG`
points; the log lives next to the config). After running steps 6–8 you
should see a clean audit trail:

```bash
$ PAYCLI_CONFIG=/tmp/paycli-cfg-h.json $PAYCLI events
2026-05-07 …  account_created   [hosted]  wallet=default        ...  memo="username=alice ..."
2026-05-07 …  account_created   [hosted]  wallet=agentresearch  ...
2026-05-07 …  account_created   [hosted]  wallet=agenttrading   ...
2026-05-07 …  invoice_created   [hosted]  wallet=default        amount=+100000000 sat
2026-05-07 …  l402_paid         [hosted]  wallet=default        status=success ... host=merchant-bob.local
2026-05-07 …  topup_credit      [hosted]  wallet=default        amount=+50000000 sat status=success
```

Filtering options:

```bash
$PAYCLI events --json | jq                 # raw JSONL for tooling
$PAYCLI events -t l402_paid                # just L402 settlements
$PAYCLI events -r hosted -s 2026-05-07T00:00:00Z   # today's hosted events
$PAYCLI events --path                      # print the log path
PAYCLI_EVENT_LOG=off $PAYCLI fund …        # disable for one call
```

Cross-tab agents: `paycli events --json | jq -s 'group_by(.wallet_alias) |
map({alias: .[0].wallet_alias, total: map(.amount) | add})'`.

---

## 10 · Cleanup

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

### lnd-sui self-payment requires `--allow-circular-route` + two channels

When the same lnd both issues an invoice and pays it (the exact case for
`paycli (Alice) → service1 (Alice authenticator)`), `SendPaymentV2` is a
self-payment. Two upstream-lnd preconditions must be met:

1. lnd is started with `--allow-circular-route`. Without it,
   `htlcswitch.go:1161` rejects the return hop with
   `OutgoingFailureCircularRoute → TemporaryChannelFailure` and the
   payment fails with `FAILURE_REASON_NO_ROUTE` before any HTLC is
   attempted.

2. There are TWO channels between alice and bob. With one channel,
   `getOutgoingBalance` reports `insufficient_balance` even with raw
   capacity available, because the same channel's balance is consumed
   on both hops of the cycle.

Both are now wired into `lnd/scripts/itest_sui_single_coin.sh`:
`--allow-circular-route` is passed to both alice and bob, and step
[5b/8] opens a second alice → bob channel. Step [8/8] of the itest
script asserts a successful Alice → Alice payment with a non-zero
preimage, so this stays regression-tested.

For paycli's hosted route, this means an Alice-funded wallet *can*
pay a service whose payment backend is also Alice (e.g.
`paycli request -H "Host: service1.com"`) once
`lnd_grpc_allow_self_payment=true` is also set in agents-pay-service
(via `paycli admin-set lnd_grpc_allow_self_payment true`).

### Bob's `payinvoice --force` via the lnd CLI sometimes hangs

`lncli payinvoice` against the lnd-sui REST gateway can stall on
`IN_FLIGHT` when the SUI chain RPC has drifted (see § "When SUI chain
RPC drifts"). The REST `POST /v1/channels/transactions` path used
throughout this guide works reliably on a fresh
`itest_sui_single_coin.sh` restart and is the recommended way to fund
hosted wallets from Bob.

If REST also stalls, the SUI localnet has drifted — restart per § 2.
