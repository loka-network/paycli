# CLI reference

`paycli` is a single binary covering the full agent-side payment flow:
account creation → invoice issuance → invoice settlement → L402-protected
HTTP requests. Two routes are supported, mutually exclusive per config:

* **`--route hosted`** (default) — talks to `agents-pay-service` via REST
  + `X-Api-Key`. The custodial / Loka-managed wallet.
* **`--route node`** — talks directly to a user-run `lnd` / `lnd-sui` REST
  gateway via TLS + macaroon. The self-custody wallet.

Once configured, every other command (`whoami`, `fund`, `pay`, `request`,
`history`) dispatches automatically based on the saved route — no flag
needed on each call.

## Install

```bash
make build           # → bin/paycli
make install         # → $GOBIN/paycli
```

## Quick start

```bash
paycli init          # interactive setup wizard
```

`init` walks you through endpoint pick (defaults to
`https://agents-pay.loka.cash`), register-new vs import-existing-keys,
and writes `~/.paycli/config.json`. Re-running on an existing config
offers backup / abort / overwrite.

For the **node** route (self-hosted lnd-sui), the wizard's guided path
delegates to `paycli node install` + `paycli node start` — it downloads
the loka-lnd release for your platform, boots it against Sui devnet
or testnet, optionally hits the faucet + connects to the Loka seed
nodes, and registers all the resulting paths in your config. Skip the
wizard and drive it directly when scripting:

```bash
paycli node install                                       # → ~/.paycli/lnd/<version>/bin/{lnd,lncli}
paycli node start --network devnet --faucet --connect-seeds
paycli node status                                        # pid + pubkey + balance + peers
paycli node logs -f                                       # tail lnd.log
paycli node stop                                          # graceful (lncli stop) + SIGTERM fallback
```

Storage layout under `~/.paycli/`: `lnd/<version>/bin/` for binaries,
`lnd-data/` for the data dir (logs, pid, tls.cert, macaroon). Every
step the wizard does is also reachable as an individual command
(`register`, `login`, `node …`, `config set`, …) — see below if you'd
rather drive it manually or script the setup.

## Global flags

| Flag | Env var | Description |
|---|---|---|
| `--base-url` | `PAYCLI_BASE_URL` | Override the agents-pay-service base URL |
| `--insecure` | `PAYCLI_INSECURE` | Skip TLS verification on the wallet endpoint |
| `--config`   | `PAYCLI_CONFIG`   | Override config file path (default `~/.paycli/config.json`) |

## Multi-wallet model (hosted route)

paycli mirrors LNbits' real domain shape: **one user account, many
sub-wallets**. The account holds the dashboard-login identity (username +
password) and a JWT for operator-scope ops. Each sub-wallet has its own
admin / invoice X-Api-Keys an agent talks to agents-pay-service with.

Typical setup:

```bash
# 1. user registers themselves (creates "default" wallet under their account)
paycli --base-url http://127.0.0.1:5002 \
    register --username alice --password "alice-pw" "main"

# 2. user provisions one wallet per agent
paycli wallets add agent_research
paycli wallets add agent_trading
paycli wallets add agent_freebie

# 3. user (in dashboard) sees: main, agent_research, agent_trading, agent_freebie

# 4. each agent gets ONE wallet's keys — export a subset of the config
#    or just hand over wallet_id + admin_key as env vars
paycli wallets show agent_research --reveal
```

`config.json` lives at:

```json
{
  "route": "hosted",
  "hosted": {
    "base_url": "https://...",
    "username": "alice",
    "user_id": "...",
    "admin_bearer_token": "<JWT>",
    "active_wallet": "agent_research",
    "wallets": {
      "default":         { "wallet_id": "...", "admin_key": "...", "invoice_key": "..." },
      "agent_research":  { "wallet_id": "...", "admin_key": "...", "invoice_key": "..." },
      "agent_trading":   { "wallet_id": "...", "admin_key": "...", "invoice_key": "..." }
    }
  }
}
```

Selecting which wallet a command uses:

| | how to pick |
|---|---|
| default for every command | `paycli wallets use <name>` (sets `hosted.active_wallet`) |
| one-off override | `paycli --wallet <name> fund …` (or env `PAYCLI_WALLET=<name>`) |

The config file is created on `register` and updated by `login` /
`wallets`. It stores `route` plus two route-specific sub-objects:

```json
{
  "route": "hosted",
  "insecure_tls": false,
  "hosted": {
    "base_url": "...",
    "admin_key": "...",
    "invoice_key": "...",
    "wallet_id": "...",
    "user_id": "...",
    "admin_bearer_token": "..."
  },
  "node": {
    "endpoint": "...",
    "tls_cert_path": "...",
    "macaroon_path": "..."
  }
}
```

Older paycli builds wrote a flat schema (`base_url`, `admin_key`,
`node_endpoint`, ... at the top level). Those configs are still loaded
correctly — paycli folds the legacy fields into the new `hosted` /
`node` sub-objects on read, and the next `register` / `login` /
`config set` rewrites the file in the new layout.

Use **dotted keys** like `hosted.admin_key` and `node.endpoint` with
`paycli config get/set` (run `paycli config keys` for the full list).

## Commands

### `paycli register [<wallet-name>] [--route hosted|node] [--username … --password …]`

**Hosted, anonymous (default)** — creates a fresh user + first wallet via
`POST /api/v1/account`. The DB row has empty username and no password
hash, so the account can't log into the LNbits dashboard. Best for AI
agents / automated provisioning where the admin_key is the only auth
the caller will ever use.

> **Duplicate-alias guard:** register and `wallets add` both refuse to
> overwrite an existing entry under the same local alias (silently
> losing the old keys would lock you out of that wallet). When the
> guard fires, run `paycli wallets remove <alias>` first or pass
> `--force` to opt back into overwrite explicitly.

```bash
paycli --base-url http://127.0.0.1:5002 register "main"
```

**Hosted, named** — when `--username` is set, switches to
`POST /api/v1/auth/register`. The DB row gets a real `username` plus a
bcrypt password hash, so the resulting account can:

- log into the LNbits dashboard with username + password
- be re-authenticated via `paycli auth-login`
- act as super-user / operator if its account id matches `super_user`
  or is in `lnbits_admin_users`

`paycli` also fetches the auto-created wallet's keys (via
`GET /api/v1/wallets`, Bearer auth on the just-issued JWT) and caches
them alongside the JWT in the config — one command, fully ready for
subsequent `whoami` / `fund` / `topup` / `admin-set`.

```bash
# Username must match lnbits' is_valid_username regex:
#   ^[a-zA-Z0-9._]{2,20}$, no leading/trailing _ or ., no double __ or ..
# Dashes / hyphens are NOT allowed.
paycli --base-url http://127.0.0.1:5002 \
    register --username alice --password "alice-pw" --email alice@example.com \
    "alice-treasury"
```

If `--password` is omitted the CLI prompts on the tty.

> ⚠️ **Flag-ordering caveat** (urfave/cli v2): the v2 parser stops
> reading flags at the first positional argument. Put **all `--flag`
> options BEFORE** the `[wallet-name]` positional. paycli detects the
> wrong order and prints a helpful error rather than silently
> falling through to the anonymous path.

**Node** — no remote provisioning step; just pins the connection settings
and probes `GetInfo` for sanity. Wallet name is optional in this mode.

```bash
paycli register --route node \
    --lnd-endpoint  https://127.0.0.1:8081 \
    --lnd-tls-cert  /tmp/lnd-sui-test/alice/tls.cert \
    --lnd-macaroon  /tmp/lnd-sui-test/alice/data/chain/sui/devnet/admin.macaroon
```

### `paycli login --route hosted|node …`

Persist existing credentials without making a remote call. Useful when
you've provisioned the wallet/keys elsewhere.

```bash
# hosted
paycli login --admin-key 0123abcd... --invoice-key fedcba...

# node
paycli login --route node \
    --lnd-endpoint  https://127.0.0.1:8081 \
    --lnd-tls-cert  /path/to/tls.cert \
    --lnd-macaroon  /path/to/admin.macaroon
```

### `paycli whoami`

* hosted: `GET /api/v1/wallet` — returns wallet name + balance.
* node: `GET /v1/getinfo` — returns identity_pubkey, alias, channel counts.

(node-mode `GetInfo` against lnd-sui devnet sometimes errors with the
chain-backend's pprof page — see `docs/sdk.md#known-issue-with-lnd-sui-devnet`.)

### `paycli fund --amount N [--unit sat|mist|sui|<fiat>] [--memo …] [--expiry seconds] [--via stripe|paypal] [--open]`

Generate a BOLT11 invoice (or fiat checkout URL) for receiving funds
into the active wallet.

`--unit` controls how `--amount` is interpreted:

| --unit | meaning | example |
|---|---|---|
| `sat` (default) | sub-unit, integer | `--amount 1000` (1000 sat / MIST under SUI bridge) |
| `mist` | sub-unit, integer (alias of sat under SUI) | `--amount 1000 --unit mist` |
| `sui` | **whole SUI** — paycli multiplies by 1e9 to MIST on the wire | `--amount 0.1 --unit sui` (100M MIST) |
| `USD` / `EUR` / … | **fiat** — server's oracle does the conversion (hosted only) | `--amount 0.10 --unit USD` |

Why no `--unit btc`? 1 BTC ≈ $100k+, so day-to-day LN amounts are
typed in sat directly. There's no ergonomic gain from a whole-BTC
flag, and adding one risks ambiguous reads. Stay in sat for BTC
deployments. SUI is different — 1 SUI ≈ $1, so users naturally
think in SUI.

* hosted: `POST /api/v1/payments {out:false, amount:N, unit:U}` → full
  hosted `Payment` object including `bolt11`. Fractional sub-units are
  rejected client-side; sub-MIST resolution is rejected for `--unit sui`.
* node: `POST /v1/invoices {value:N, memo, expiry}`. Node mode has no
  oracle, so fiat units are rejected.

```bash
paycli fund --amount 0.1 --unit sui --memo "topup"   # 0.1 SUI
paycli fund --amount 100000000 --memo "topup"        # 100M MIST (same)
paycli fund --amount 0.10 --unit USD --memo "topup"  # hosted-route oracle: USD → sat → BOLT11
```

**Fiat onramp (Stripe / PayPal): `--via {stripe,paypal}`**

When `--via` is set, the server skips minting a BOLT11 and instead asks
the configured fiat provider to create a hosted checkout session. The
response's `payment_request` field is the **checkout URL** (not a
BOLT11). After the user pays at the provider's page, the server's
webhook (`/api/v1/callback/{stripe,paypal}`) verifies the event and
credits the active sub-wallet without ever touching lnd.

Constraints:
- Hosted route only (node mode has no fiat backend).
- `--unit` must be a fiat code; `sat` / `mist` / `sui` are rejected
  client-side.
- The named provider must be enabled in the server's admin settings —
  see [`docs/fiat-onramp.md`](./fiat-onramp.md) for the operator setup
  (enabling the provider, getting API + webhook secrets, registering
  the webhook URL).

```bash
paycli fund --amount 5    --unit USD --via stripe              # → Stripe checkout URL
paycli fund --amount 9.99 --unit USD --via paypal --open       # → PayPal approve_url, opens browser
```

### `paycli pay <bolt11>`

Settle a BOLT11 invoice from the configured wallet.

* hosted: `POST /api/v1/payments {out:true, bolt11}` — requires admin key.
* node: `POST /v1/channels/transactions {payment_request}` — synchronous
  send via the macaroon.

```bash
paycli pay lnbc100u1p3...
```

### `paycli history [--limit N] [--offset M]`

List recent payments on the configured wallet.

### `paycli config show [--reveal] [--path]`

Print the active config. Secrets (admin key, invoice key, bearer token)
are masked unless `--reveal` is passed. `--path` prints just the resolved
config file path.

### `paycli config get <key>` / `paycli config set <key> <value>`

Edit a single config field without hand-touching JSON. Keys are dotted
paths matching the on-disk JSON layout. Run `paycli config keys` for
the full list.

```bash
paycli config set hosted.base_url http://127.0.0.1:5002
paycli config set route node
paycli config get node.endpoint
```

### `paycli services [--prism-url …] [--prism-macaroon …] [-s X] [--insecure]`

List the L402 service catalog from a Prism gateway. The underlying
`GET /api/admin/services` is unauthenticated on Prism (see
[`loka-prism-l402/admin/auth.go`'s `unauthenticatedMethods`][prism-auth]),
so neither flag is required — defaults target the hosted gateway and
no macaroon. `--prism-macaroon` is still accepted for deployments that
re-enable auth or hand out limited-scope macaroons. `--search` is a
case-insensitive substring filter on name / host / path.

```bash
# default — talk to the hosted gateway
paycli services -s freebie

# self-hosted local dev
paycli services \
    --prism-url       https://127.0.0.1:8080 \
    --insecure
```

[prism-auth]: https://github.com/loka-network/aperture/blob/loka/admin/auth.go

### `paycli auth-login --username … [--password …]` (operator)

Exchange super-user / admin credentials for a JWT and cache it in the
config. Required before `paycli topup`. Password may be supplied via
`--password` or read interactively from the tty.

### `paycli events [-n N] [-t TYPE] [-r ROUTE] [-s RFC3339] [--json] [--path]`

Show the local payment event log at `~/.paycli/events.jsonl`. Every
state-changing payment command — `register` (hosted), `fund`, `pay`,
`request` (per L402 settlement), `topup` — appends one structured row
to this file as it runs.

```bash
paycli events                                    # last 20, human-readable
paycli events -n 100 -t l402_paid                # last 100 L402 settlements
paycli events -r hosted -s 2026-05-07T00:00:00Z  # hosted-route events from today
paycli events --json | jq                        # raw JSONL for tooling
paycli events --path                             # just print the log path
```

Set `PAYCLI_EVENT_LOG=off` (or `0` / `false`) to disable logging
entirely, or set it to a file path to redirect.

The schema fields:

| Field | Notes |
|---|---|
| `ts` | RFC3339Nano UTC |
| `event` | `account_created` \| `invoice_created` \| `pay_sent` \| `l402_paid` \| `topup_credit` |
| `route` | `hosted` or `node` |
| `endpoint` | base_url (hosted) or lnd REST URL (node) |
| `wallet_alias` | hosted only — the local alias under `hosted.wallets[…]` (e.g. `agentresearch`) |
| `wallet_id`, `user_id` | populated when known |
| `amount`, `unit` | positive = inflow, negative = outflow |
| `payment_hash`, `preimage`, `status` | settlement detail |
| `target_url`, `target_host` | L402-only: where the request went |
| `payment_request` | bolt11 (for invoice_created and l402_paid) |
| `error` | populated on failure |

### `paycli payment-status <hash> [--wallet …] [--json]`

Refresh + print one payment's current state. Hits
`GET /api/v1/payments/{hash}` on agents-pay-service, which forces
lnbits' wallet driver to re-query the underlying lnd before answering.
Useful when a previous fund / pay / request returned `status=pending`
and you want the latest LN-level truth without grepping curl.

```bash
$ paycli payment-status 93a6642ff4e67795...
hash:     93a6642ff4e67795860f059f1d43b1523a2bf6967434bcabe76a4df33f6ff159
paid:     false
status:   pending
memo:     L402
amount:   -10000000000 msat
```

`paid` reflects LN settlement (preimage revealed); `status` mirrors
lnbits' apipayments table (pending → success/failed once the wallet
driver sees the underlying lnd's terminal state).

When a payment stays stuck on `pending` indefinitely, that's the
lnd-sui devnet caveat documented in `docs/manual-test.md` —
SendPaymentV2's state machine doesn't always flip IN_FLIGHT →
SUCCEEDED/FAILED on this fork. The wallet balance has been
optimistically pre-debited; if lnd eventually reports FAILED the
balance gets refunded automatically.

### `paycli rate [<currency=USD>]`

Hits `GET /api/v1/rate/<currency>` on agents-pay-service and prints
the oracle's chain-aware view. On a SUI deployment the labels read
"USD per SUI" / "MIST per USD"; on BTC they'd read "USD per BTC" /
"sat per USD". Open endpoint — no key required.

```bash
$ paycli rate
currency:    USD
price:       1.0070 USD per 1 SUI
rate:        993048659.3843 MIST per 1 USD
(per quote:  1 USD ≈ 993048659.3843 MIST; 1 SUI ≈ 1.0070 USD)

$ paycli rate EUR
currency:    EUR
price:       0.8557 EUR per 1 SUI
rate:        1168648889.9589 MIST per 1 EUR
```

After every successful `paycli fund` / `paycli pay`, paycli also
prints a one-line summary on stderr decoding the msat amount into
the chain's whole unit + the configured fiat:

```
$ paycli fund --amount 50000000 --memo demo
{ ...JSON... }
≈ 0.050000 SUI  (50000000 MIST ≈ 0.0500 USD)
```

The chain (SUI vs BTC) is auto-detected from the response's
`extra.wallet_sui_rate` / `extra.wallet_btc_rate` field, so a single
paycli binary works against both chain modes of agents-pay-service.

### `paycli admin-set <key> <value>` (operator)

PATCH a single field on `agents-pay-service`'s admin settings via
`PATCH /admin/api/v1/settings`. The value is JSON-parsed when possible
(so `true` / `42` / `"str"` / `["a","b"]` round-trip), otherwise sent
as a plain string.

Most common use: enable LND self-payment so a wallet can pay invoices
issued by the same lnd it's funded from (e.g. paying a Prism paywall
that's authenticated by the same node):

```bash
paycli auth-login --username admin                       # one-time
paycli admin-set lnd_grpc_allow_self_payment true        # persists to DB
pkill -f 'lnbits --port 5002' && lnbits --port 5002 &    # restart so the
                                                         # wallet driver
                                                         # picks up the flag
```

Note: env vars like `LND_GRPC_ALLOW_SELF_PAYMENT=true` look like they
should work but are silently overridden at boot — lnbits' admin-DB
contents shadow editable env values. Use `admin-set` instead.

### `paycli topup --wallet-id … --amount N` (operator)

Credit a hosted wallet directly (faucet) via
`PUT /users/api/v1/balance`. Bypasses LN routing — internally
agents-pay-service synthesizes a successful incoming payment, the same
way the dashboard "credit user" button does. Use to onboard test
wallets without round-tripping through a channel.

Negative `--amount` debits the wallet.

### `paycli wallets list | add | use | show | remove`

The `wallets` group manages the local map of sub-wallets the active
account knows about. See § "Multi-wallet model" above for the
end-to-end shape.

```bash
paycli wallets list                        # show all entries (active marked *)
paycli wallets add <alias> [--use]         # create on server + persist locally
paycli wallets use <alias>                 # switch active_wallet
paycli wallets show [<alias>] [--reveal]   # print one entry's keys
paycli wallets remove <alias>              # drop from local config (no server delete)
```

`wallets add` calls `POST /api/v1/wallet` with the cached super-user
JWT — set up via `paycli register --username` or `paycli auth-login`.
The new wallet's keys are auto-saved into `hosted.wallets[<alias>]`.

`wallets show` masks keys by default; pass `--reveal` to print plaintext
when you need to copy them into an agent's environment.

### `paycli request <url> [--method GET] [-H 'Key: Value'] [-d body] [-i] [--insecure-target] [--max-retries 1]`

HTTP request with automatic L402 (HTTP 402) payment handling.

Behind the scenes:

1. Build the request from flags.
2. Send via `sdk.L402Doer` wired to the active route's wallet.
3. On 402, parse `WWW-Authenticate: LSAT macaroon="…", invoice="…"`.
4. Pay the invoice via the active route:
   * hosted → `POST /api/v1/payments {out:true,bolt11}` (admin key).
   * node → `POST /v1/channels/transactions {payment_request}` (macaroon).
5. Re-send with `Authorization: LSAT <mac>:<preimage>`.

Hosted mode requires an admin key (invoice keys can't spend). Node mode
just needs whatever macaroon is configured — usually `admin.macaroon`.

```bash
# Local Prism (self-signed cert, hostregexp routes by Host header):
paycli request \
    -H "Host: service1.com" \
    --insecure-target -i \
    https://127.0.0.1:8080/freebieservice
```

Notes:

* The `Host` header is special — `paycli` recognizes it and sets
  `req.Host` rather than `req.Header["Host"]` (which `net/http` ignores).
* Tokens are cached in-process for the lifetime of the command; running the
  command twice in one shell still pays twice. Long-lived processes that
  re-use the same `L402Doer` get free token reuse.
* `--insecure-target` disables TLS verification on the **target** URL only.
  `--insecure` (global) disables TLS verification on the **wallet** URL.

## Exit codes

* `0` — success
* `1` — any error (API error, network, build error, etc.). The error
  message is printed to stderr.

## Examples

End-to-end flow:

```bash
# 1. provision an agent wallet
paycli --base-url http://127.0.0.1:5002 register "agent-001"

# 2. top it up — print the invoice, have an external wallet pay it
paycli fund --amount 100000 --memo "agent budget"
# (pay the printed bolt11 from another wallet/lncli)

# 3. consume a paid API behind Prism
paycli request \
    -H "Host: service1.com" \
    --insecure-target -i \
    https://127.0.0.1:8080/api/some-endpoint
```
