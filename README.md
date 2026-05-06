# paycli

Loka Payment custodial wallet CLI + Go SDK. Single binary, single import path,
HTTP only — no LND build dependency.

`paycli` is intentionally separate from `lncli`. Route A (self-custody) users
already have `lncli` for direct LN node operations. `paycli` covers route B —
the **custodial** path against `agents-pay-service`, including the L402 / HTTP
402 auto-payment flow that AI agents need when calling paid APIs behind a
Prism / aperture gateway.

```
┌──────────────┐     X-Api-Key     ┌────────────────────────┐
│   paycli     │  ───────────────► │  agents-pay-service    │ ──► lnd-sui
│   (CLI/SDK)  │                   │   (custodial wallet)   │
└──────────────┘                   └────────────────────────┘
       │                                      ▲
       │   L402 challenge / preimage replay   │
       └──────────────────► Prism ────────────┘
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

```bash
make build                              # → bin/paycli
bin/paycli --base-url http://127.0.0.1:5002 register "main"
bin/paycli whoami
bin/paycli fund --amount 1000 --memo "topup"
bin/paycli request -H "Host: service1.com" \
    --insecure-target -i \
    https://127.0.0.1:8080/freebieservice
```

The `register` command persists the returned admin/invoice keys to
`~/.paycli/config.json` (override with `$PAYCLI_CONFIG`).

## SDK usage

```go
import "github.com/loka-network/paycli/pkg/sdk"

cl := sdk.New("http://127.0.0.1:5002", sdk.WithAdminKey(adminKey))
inv, _ := cl.CreateInvoice(ctx, sdk.CreateInvoiceRequest{Amount: 1000})

doer := sdk.NewL402Doer(cl)
req, _ := http.NewRequest("GET", "https://api.example.com/paid", nil)
resp, _ := doer.Do(ctx, req) // 402 → pay → retry, transparent
```

See [`docs/sdk.md`](docs/sdk.md) and [`docs/cli.md`](docs/cli.md) for full reference.

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
   `paycli` is an end-user / AI-agent payments CLI with HTTP and L402 logic.
   Route A users keep using `lncli` directly.
3. **Decoupled dependencies** — `paycli` only needs `net/http` plus
   `urfave/cli`. No need to drag the whole lnd build tree into the agent
   image.

The SDK is the seam: any future Go integration (loka-cloud agents, MCP
servers, dashboards) imports `pkg/sdk` rather than reimplementing REST + L402
logic.
