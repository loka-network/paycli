# SDK reference

Import path: `github.com/loka-network/paycli/pkg/sdk`

The SDK has three surfaces:

1. **`Client`** — REST wrapper around `agents-pay-service` (LNbits fork).
   The "hosted" / custodial route.
2. **`NodeClient`** — REST wrapper around the user's own `lnd` / `lnd-sui`
   grpc-gateway. The "node" / self-custody route.
3. **`L402Doer`** — an `http.Client` that transparently pays HTTP 402 LSAT
   challenges. Takes the `Wallet` interface, so it drives either client.

Everything is pure stdlib + `net/http`. No protobuf, no gRPC, no lnd build
dep — `NodeClient` talks to lnd's REST gateway over plain HTTPS.

## Picking a route

```go
// Wallet is satisfied by both Client and NodeClient — anything that
// can settle a BOLT11 invoice and return a preimage.
type Wallet interface {
    PayInvoice(ctx context.Context, bolt11 string) (*Payment, error)
}
```

Generic code that pays L402 challenges should accept `sdk.Wallet`, not a
concrete client.

## Client (hosted route)

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
| `CreateAccount(ctx, name) (*HostedWallet, error)` | `POST /api/v1/account` | none |
| `GetWallet(ctx) (*HostedWalletStatus, error)` | `GET  /api/v1/wallet` | invoice or admin |
| `CreateWallet(ctx, userID, name) (*HostedWallet, error)` | `POST /api/v1/wallet?usr=…` | none — uses `usr` query param |
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

## NodeClient (node route)

`NodeClient` talks to an `lnd` / `lnd-sui` grpc-gateway REST endpoint
directly — no Loka custodial server in the picture.

### Construction

```go
nc, err := sdk.NewNodeClient(
    "https://127.0.0.1:8081",
    sdk.WithNodeTLSCertFile("/tmp/lnd-sui-test/alice/tls.cert"),
    sdk.WithNodeMacaroonFile("/tmp/lnd-sui-test/alice/data/chain/sui/devnet/admin.macaroon"),
    // sdk.WithNodeInsecureTLS(),                // alternative — skip TLS verify
    // sdk.WithNodeMacaroonHex("0201036c6e6402..."), // alternative — pass hex inline
    // sdk.WithNodeTimeout(60 * time.Second),
)
```

The macaroon is read from disk and hex-encoded into the
`Grpc-Metadata-macaroon` header lnd expects. The TLS cert is loaded into
a private cert pool; the system roots are *not* consulted (lnd's dev cert
is self-signed).

### Methods

| Method | HTTP | Returns |
|---|---|---|
| `GetInfo(ctx)` | `GET /v1/getinfo` | `*NodeInfo` |
| `ChannelBalance(ctx)` | `GET /v1/balance/channels` | `*NodeChannelBalance` |
| `AddInvoice(ctx, value, memo, expiry)` | `POST /v1/invoices` | `*NodeAddInvoiceResponse` |
| `SendPaymentSync(ctx, bolt11)` | `POST /v1/channels/transactions` | `*NodeSendPaymentResponse` |
| `PayInvoice(ctx, bolt11)` | (Wallet interface) | `*Payment` (with hex preimage) |
| `ListPayments(ctx, max)` | `GET /v1/payments` | `[]NodePayment` |

### Wire format quirks

* `r_hash`, `payment_hash`, `payment_preimage` come back **base64-encoded**
  on the JSON wire (grpc-gateway default for binary fields). The accessors
  `PaymentHashHex()` / `PreimageHex()` decode and re-encode as lowercase hex.
* int64 fields (`balance`, `value_sat`, `add_index`) are serialized as
  decimal strings. `Amount.SatInt()` / `Amount.MsatInt()` parse them.
* lnd's REST gateway sometimes returns 200 OK with a gRPC error envelope
  (`{"code":N,"message":"..."}`). `NodeClient.do` detects this and surfaces
  it as `*APIError` so callers don't get garbage on the success path.

### Known issue with lnd-sui devnet

Some lnd-sui builds return `JSON decode error: ... <html>/debug/pprof/` from
chain-backend RPCs (`GetInfo`, `WalletBalance`, `SendPaymentSync`) when the
SUI RPC is in a degraded state. `AddInvoice`, `ChannelBalance`, and
`ListPayments` typically remain healthy. The SDK surfaces those as
`*APIError` with the raw body in `.Body` so it's debuggable.

## PrismClient (service catalog)

`PrismClient` calls Prism's admin gRPC-gateway REST. Today the only
endpoint the SDK exposes is `ListServices`; the rest of the admin
surface (CreateService, RevokeToken, stats) is intentionally out of
scope — those are operator dashboard concerns.

```go
pc, _ := sdk.NewPrismClient(
    "https://127.0.0.1:8080",
    sdk.WithPrismMacaroonFile("/path/to/prism/.prism/admin.macaroon"),
    sdk.WithPrismInsecureTLS(),
)
services, _ := pc.ListServices(ctx)
for _, s := range services {
    fmt.Println(s.Name, s.HostRegexp, s.Price)
}
```

## Operator helpers (auth + topup)

Two free functions for super-user / operator flows that aren't tied to
a per-wallet API key:

```go
// Login as super user → JWT
tok, _ := sdk.LoginWithPassword(ctx, baseURL, "admin", "secret")

// Credit a wallet directly (synthesizes an "Admin credit" internal payment)
_ = sdk.AdminCreditWallet(ctx, baseURL, tok, walletID, 50000)
```

Negative amounts debit; LNbits enforces no-overdraft.

## L402Doer

`L402Doer` wraps an `http.Client` and pays through any HTTP 402 challenge
returned by Prism / aperture-style gateways.

### Basic usage

```go
// Either client type works.
wallet := sdk.New(baseURL, sdk.WithAdminKey(adminKey))   // hosted
// wallet, _ := sdk.NewNodeClient(host, ...)             // or node

doer := sdk.NewL402Doer(wallet)
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
