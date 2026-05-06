# SDK reference

Import path: `github.com/loka-network/paycli/pkg/sdk`

The SDK has two surfaces:

1. **`Client`** — a thin REST wrapper around `agents-pay-service` (LNbits fork).
2. **`L402Doer`** — an `http.Client` that transparently pays HTTP 402 LSAT
   challenges using a `Client` as the spending wallet.

Everything is pure-stdlib + net/http. No protobuf, no gRPC, no LND build dep.

## Client

### Construction

```go
cl := sdk.New(
    "http://127.0.0.1:5002",
    sdk.WithAdminKey(adminKey),
    // sdk.WithInvoiceKey(inkey),     // alternative — receive-only
    // sdk.WithInsecureTLS(),         // local self-signed prism
    // sdk.WithHTTPClient(custom),    // tracing, retries, timeouts
)
```

Pass an empty `baseURL` to use `sdk.DefaultBaseURL` (`https://agents-pay.loka.cash`).

### Methods

| Method | HTTP | Key required |
|---|---|---|
| `CreateAccount(ctx, name) (*Wallet, error)` | `POST /api/v1/account` | none |
| `GetWallet(ctx) (*WalletStatus, error)` | `GET  /api/v1/wallet` | invoice or admin |
| `CreateWallet(ctx, userID, name) (*Wallet, error)` | `POST /api/v1/wallet?usr=…` | none — uses `usr` query param |
| `CreateInvoice(ctx, CreateInvoiceRequest) (*Payment, error)` | `POST /api/v1/payments` (out=false) | invoice or admin |
| `PayInvoice(ctx, bolt11) (*Payment, error)` | `POST /api/v1/payments` (out=true) | **admin** |
| `ListPayments(ctx, limit, offset) ([]Payment, error)` | `GET /api/v1/payments` | invoice or admin |
| `GetPayment(ctx, hash) (map, error)` | `GET /api/v1/payments/{hash}` | optional |

`CreateAccount` returns the first wallet, which carries:

```go
w.AdminKey   // full spend authority — treat like a service account secret
w.InvoiceKey // receive-only — safe to hand out
w.User       // account id; pass to CreateWallet for sub-wallets
w.ID         // wallet id
```

### Errors

The SDK returns three kinds of errors:

```go
var (
    ErrUnauthorized       = errors.New("paycli: unauthorized — check api key")
    ErrAdminKeyRequired   = errors.New("paycli: admin key required for this operation")
    ErrPaymentFailed      = errors.New("paycli: payment failed")
    ErrInvalidChallenge   = errors.New("paycli: invalid L402 challenge header")
    ErrChallengeExhausted = errors.New("paycli: L402 retry budget exhausted")
)

type APIError struct {
    Status int
    Detail string  // upstream {"detail": "..."} field
    Body   string  // raw body for unstructured errors
}
```

Use `errors.Is` against the sentinels and `errors.As` against `*APIError`.

### Notes on the API surface

* `POST /api/v1/wallet` upstream **does not accept `X-Api-Key`** despite what
  some integration docs imply — it requires a session JWT or, when
  `LNBITS_AUTH_METHODS` includes `user_id_only`, a `usr=<user_id>` query
  param. The SDK uses the latter; expect it to fail with `Missing user ID or
  access token` when the server doesn't allow that auth method.
* `Payment.Time` is decoded as `interface{}` because LNbits serializes it as
  either a Unix int or an ISO-8601 string depending on backend.
* Amounts in the `agents-pay-service` SUI bridge are **MIST**, not sats:
  `1 amount = 1 MIST`. `Wallet.BalanceMsat` is millisat-equivalent — divide
  by 1000 to get MIST/sat.

## L402Doer

`L402Doer` wraps an `http.Client` and pays through any HTTP 402 challenge
returned by Prism / aperture-style gateways.

### Basic usage

```go
wallet := sdk.New(baseURL, sdk.WithAdminKey(adminKey))
doer   := sdk.NewL402Doer(wallet)

req, _ := http.NewRequest("GET", "https://api.example.com/paid", nil)
resp, err := doer.Do(ctx, req)
```

On the first call:

1. `doer.Do` sends the request.
2. Server replies `402 Payment Required` with
   `WWW-Authenticate: LSAT macaroon="…", invoice="lnbc…"`.
3. `Doer` parses the challenge, calls `wallet.PayInvoice(ctx, invoice)`.
4. `Doer` re-sends the original request with
   `Authorization: LSAT <macBase64>:<preimageHex>`.
5. Server replies 200; `doer.Do` returns the final response.

The request body, if any, is fully buffered before the first send so it can
be replayed.

### Token caching

By default the doer carries an in-process cache keyed by
`scheme://host/path`. After a successful payment the
`(macaroon, preimage)` tuple is reused on subsequent calls until the server
returns 402 again. Customize the key via `doer.CacheKey`:

```go
doer.CacheKey = func(r *http.Request) string {
    // one token per service+method, ignoring query string
    return r.URL.Scheme + "://" + r.URL.Host + r.URL.Path + "#" + r.Method
}
```

### Retry budget

`MaxRetries` (default 1) bounds the number of 402 → pay → retry cycles per
call. If the server still demands payment after `MaxRetries`, `Do` returns
`ErrChallengeExhausted`.

### Insecure TLS

For local Prism with self-signed certs:

```go
doer.HTTPClient = &http.Client{
    Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
    Timeout:   60 * time.Second,
}
```

Never enable this against a production gateway.
