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

## Global flags

| Flag | Env var | Description |
|---|---|---|
| `--base-url` | `PAYCLI_BASE_URL` | Override the agents-pay-service base URL |
| `--insecure` | `PAYCLI_INSECURE` | Skip TLS verification on the wallet endpoint |
| `--config`   | `PAYCLI_CONFIG`   | Override config file path (default `~/.paycli/config.json`) |

The config file is created on `register` and updated by `login`. It stores
`route` plus the route-specific fields: hosted (`base_url`, `admin_key`,
`invoice_key`, `wallet_id`, `user_id`) or node (`node_endpoint`,
`node_tls_cert_path`, `node_macaroon_path`).

## Commands

### `paycli register [<wallet-name>] [--route hosted|node]`

**Hosted (default)** — creates a fresh anonymous account + first wallet via
`POST /api/v1/account`. Persists keys + user id.

```bash
paycli --base-url http://127.0.0.1:5002 register "main"
```

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

### `paycli fund --amount N [--memo …] [--unit sat] [--expiry seconds]`

Generate a BOLT11 invoice for receiving funds into the configured wallet.

* hosted: `POST /api/v1/payments {out:false, amount:N}` → returns the full
  hosted `Payment` object including `bolt11`.
* node: `POST /v1/invoices {value:N, memo, expiry}` → returns the lnd
  `AddInvoice` response with `payment_request`.

```bash
paycli fund --amount 1000 --memo "topup"
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

Edit a single config field without hand-touching JSON. Run
`paycli config keys` for the full list of editable keys.

```bash
paycli config set base_url http://127.0.0.1:5002
paycli config set route node
paycli config get node_endpoint
```

### `paycli services --prism-url … --prism-macaroon … [-s X] [--insecure]`

List the L402 service catalog from a Prism gateway. The underlying
`GET /api/admin/services` is admin-gated by Prism today, so this command
takes the gateway operator's `admin.macaroon`. With `--search`,
case-insensitive substring filter on name/host/path.

```bash
paycli services \
    --prism-url       https://127.0.0.1:8080 \
    --prism-macaroon  /path/to/prism/.prism/admin.macaroon \
    --insecure
```

### `paycli auth-login --username … [--password …]` (operator)

Exchange super-user / admin credentials for a JWT and cache it in the
config. Required before `paycli topup`. Password may be supplied via
`--password` or read interactively from the tty.

### `paycli topup --wallet-id … --amount N` (operator)

Credit a hosted wallet directly (faucet) via
`PUT /users/api/v1/balance`. Bypasses LN routing — internally
agents-pay-service synthesizes a successful incoming payment, the same
way the dashboard "credit user" button does. Use to onboard test
wallets without round-tripping through a channel.

Negative `--amount` debits the wallet.

### `paycli add-wallet <name> [--user-id …]`

Create an additional sub-wallet. Defaults to the user_id cached by `register`.
Note: the upstream LNbits server must have `user_id_only` enabled in
`LNBITS_AUTH_METHODS` for this to work.

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
