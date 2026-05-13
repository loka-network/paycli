# paycli

Loka Payment CLI + Go SDK. Single binary, single import path, HTTP-only —
no LND build dependency.

paycli supports **two routes** out of one binary:

| Route | Wallet location | Backend |
|---|---|---|
| **hosted** | Loka custodial server | `agents-pay-service` REST + `X-Api-Key` |
| **node** | User's own LN node | `lnd-sui` REST gateway + macaroon |

The same `paycli request` command transparently pays L402 (HTTP 402)
challenges via whichever route is configured — the L402Doer takes a
`Wallet` interface and both backends satisfy it.

```
┌──────────────┐  hosted: X-Api-Key   ┌────────────────────────┐
│              │  ──────────────────► │  agents-pay-service    │ ──► lnd-sui
│   paycli     │                      │   (custodial wallet)   │
│  (CLI/SDK)   │                      └────────────────────────┘
│              │
│              │  node: macaroon + TLS  ┌────────────────────────┐
│              │  ────────────────────► │  lnd-sui REST gateway  │
└──────────────┘                        └────────────────────────┘
       │                                       ▲
       │   L402 challenge / preimage replay    │
       └──────────────────► Prism ─────────────┘
```

## Layout

```
pkg/sdk/        Go client SDK (importable as github.com/loka-network/paycli/pkg/sdk)
cmd/paycli/     CLI binary built from the SDK
docs/           SDK + CLI reference, integration-test playbook
scripts/        Local integration-test runner
tests/          Build-tag-gated integration tests against a live local stack
```

## Quick start

### Hosted route (custodial)

```bash
make build                              # → bin/lokapay
bin/lokapay --base-url http://127.0.0.1:5002 register "main"
bin/lokapay whoami
bin/lokapay fund --amount 1000 --memo "topup"
bin/lokapay request -H "Host: service1.com" \
    --insecure-target -i \
    https://127.0.0.1:8080/freebieservice
```

### Node route (self-custody)

```bash
make build
bin/lokapay register --route node \
    --lnd-endpoint  https://127.0.0.1:8081 \
    --lnd-tls-cert  /tmp/lnd-sui-test/alice/tls.cert \
    --lnd-macaroon  /tmp/lnd-sui-test/alice/data/chain/sui/devnet/admin.macaroon
bin/lokapay whoami
bin/lokapay fund --amount 1000 --memo "node-mode invoice"
bin/lokapay request -H "Host: service1.com" --insecure-target -i \
    https://127.0.0.1:8080/freebieservice    # same command, different backend
```

Config lives at `~/.lokapay/config.json` (override with `$PAYCLI_CONFIG`).
Switching routes is just `paycli login --route ...` — config remembers
which one is active.

## SDK usage

```go
import "github.com/loka-network/paycli/pkg/sdk"

// Hosted route
hosted := sdk.New("http://127.0.0.1:5002", sdk.WithAdminKey(adminKey))

// Node route — talks to the user's own lnd-sui REST gateway
node, _ := sdk.NewNodeClient("https://127.0.0.1:8081",
    sdk.WithNodeTLSCertFile("/path/to/tls.cert"),
    sdk.WithNodeMacaroonFile("/path/to/admin.macaroon"),
)

// L402Doer takes anything that satisfies sdk.Wallet — both clients do.
doer := sdk.NewL402Doer(node) // or hosted
req, _ := http.NewRequest("GET", "https://api.example.com/paid", nil)
resp, _ := doer.Do(ctx, req) // 402 → pay → retry, transparent
```

See [`docs/sdk.md`](docs/sdk.md) and [`docs/cli.md`](docs/cli.md) for full
reference. For a step-by-step end-to-end verification (build, bring up
the local stack from scratch, run both routes, observe wallet balance
deltas), follow [`docs/manual-test.md`](docs/manual-test.md).

## Tests

```bash
make test                # unit tests (httptest mocks, no external deps)
scripts/integration-test.sh  # full e2e against local lnd-sui + agents-pay-service + prism
```

Read [`docs/integration-test.md`](docs/integration-test.md) for the local
service prerequisites.

## Design

Why not embed this into `lnd/cmd/lncli`? Three reasons:

1. **Upstream rebase friction** — `lncli` is vendored from upstream lnd; mixing
   Loka custodial REST commands into `cmd/commands/*.go` makes every upstream
   merge harder.
2. **Different audience and scope** — `lncli` is a node operator's gRPC tool;
   `lokapay` is an end-user / AI-agent payments CLI with HTTP and L402 logic.
   Route A users keep using `lncli` directly.
3. **Decoupled dependencies** — `lokapay` only needs `net/http` plus
   `urfave/cli`. No need to drag the whole lnd build tree into the agent
   image.

The SDK is the seam: any future Go integration (loka-cloud agents, MCP
servers, dashboards) imports `pkg/sdk` rather than reimplementing REST + L402
logic.
