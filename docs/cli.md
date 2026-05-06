# CLI reference

`paycli` is a single binary covering the full agent-side payment flow:
account creation → invoice issuance → invoice settlement → L402-protected
HTTP requests.

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
`base_url`, `admin_key`, `invoice_key`, `wallet_id`, and `user_id`.

## Commands

### `paycli register <wallet-name>`

Create a fresh anonymous account + first wallet via
`POST /api/v1/account`. Persists keys + user id to the config file.

```bash
paycli --base-url http://127.0.0.1:5002 register "main"
```

### `paycli login --admin-key … [--invoice-key …] [--wallet-id …]`

Persist existing keys (e.g., from a separate provisioning step) without
calling the server.

```bash
paycli login --admin-key 0123abcd... --invoice-key fedcba...
```

### `paycli whoami`

Show the wallet attached to the configured key. Works with either invoice
or admin key.

### `paycli fund --amount N [--memo …] [--unit sat] [--expiry seconds]`

Generate a BOLT11 invoice for receiving funds into the configured wallet.
The full `Payment` object is printed; the `bolt11` / `payment_request`
field is what you hand to the payer.

```bash
paycli fund --amount 1000 --memo "topup"
```

### `paycli pay <bolt11>`

Settle a BOLT11 invoice from the configured wallet. Requires the admin key.

```bash
paycli pay lnbc100u1p3...
```

### `paycli history [--limit N] [--offset M]`

List recent payments on the configured wallet.

### `paycli add-wallet <name> [--user-id …]`

Create an additional sub-wallet. Defaults to the user_id cached by `register`.
Note: the upstream LNbits server must have `user_id_only` enabled in
`LNBITS_AUTH_METHODS` for this to work.

### `paycli request <url> [--method GET] [-H 'Key: Value'] [-d body] [-i] [--insecure-target] [--max-retries 1]`

HTTP request with automatic L402 (HTTP 402) payment handling.

Behind the scenes:

1. Build the request from flags.
2. Send via `sdk.L402Doer`.
3. On 402, parse `WWW-Authenticate: LSAT macaroon="…", invoice="…"`.
4. Call `POST /api/v1/payments` with the invoice → get preimage.
5. Re-send with `Authorization: LSAT <mac>:<preimage>`.

Requires the admin key (only admin keys can spend).

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
