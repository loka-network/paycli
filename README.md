# lokapay

Loka Payment CLI + Go SDK. Pay L402-protected (HTTP 402) APIs over
Lightning — settlement chain is whatever your lnd backend speaks
(today: BTC and SUI; EVM-class chains on the roadmap), and the same
one-liner CLI works against two custody models:

| Route | Wallet location | Backend | When to pick it |
|---|---|---|---|
| **node** _(recommended)_ | Your own machine | `lnd-sui` / `lnd-btc` REST gateway + macaroon — `lokapay node` can install + run a loka-lnd for you | "I want self-custody and direct Lightning channels." |
| **hosted** | Loka custodial server | `agents-pay-service` REST + `X-Api-Key` — `https://agents-pay.loka.cash` | "I just want to pay things; don't make me run a node." |

You pick the route once during `lokapay init`; every subsequent
`request` / `fund` / `pay` command dispatches transparently — the
L402Doer takes a `Wallet` interface and both backends satisfy it.

```
┌──────────────┐  hosted: agents-pay.loka.cash   ┌─────────────────────┐
│   lokapay    │ ───────────────────────────────►│  agents-pay-service │
│  (CLI/SDK)   │                                 │   + lnd backend     │
│              │  node:   your own lnd           └─────────────────────┘
│              │ ───────────────────────────────► loka-lnd (BTC / SUI / …)
└──────────────┘
       │   L402 challenge / preimage replay
       └──────► Prism (prism.loka.cash)
```

## Install

```bash
# curl (recommended — works on macOS / Linux)
curl -fsSL https://github.com/loka-network/paycli/releases/latest/download/install.sh | sh

# Homebrew — coming soon (tap repo + PAT not yet provisioned;
# the goreleaser pipeline is already wired so this lights up
# as soon as loka-network/homebrew-tap is published).
# brew install loka-network/tap/lokapay

# Go (installs from main; requires Go 1.25+)
go install github.com/loka-network/paycli/cmd/lokapay@latest
```

## Quick start

```bash
lokapay init                                       # one-shot interactive setup
lokapay services                                   # browse Prism's service catalog
lokapay node faucet                                # (devnet/testnet only) top up test SUI
lokapay request -i --debug <merchant-url>          # pays the 402 challenge, returns the response
```

The `<merchant-url>` is whatever URL the L402 service publishes — Prism
routes by **Host header** (matched against each service's
`host_regexp` in the catalog), not by URL path. In a production
deployment a merchant typically owns a real domain that DNS-resolves
to the gateway, so you just point `lokapay request` at it:

```bash
lokapay request -i https://api.some-merchant.com/v1/data
```

For local dev where everything lives on `127.0.0.1`, override the
Host header explicitly to match whichever service's `host_regexp` you
want (`lokapay services` shows them all):

```bash
# Catalog says service1 has host_regexp=^service1.com$ — fake the Host header.
lokapay request -H "Host: service1.com" --insecure-target -i --debug \
    https://127.0.0.1:8080/data.json
```

`init` handles all the boring stuff in a single wizard: hosted vs
self-hosted route, endpoint URL (defaults to `agents-pay.loka.cash` /
`prism.loka.cash`), account / wallet keys, and — on the self-custody
path — downloads and starts a loka-lnd for you. Re-run `lokapay init`
any time to reconfigure.

Persistent state lives at `~/.lokapay/` (auto-migrated from the legacy
`~/.paycli/` on first run). For everything else — fund / pay / topup /
fiat onramp / managed-node lifecycle / SDK usage — read on:

## Docs

- **[CLI reference](docs/cli.md)** — every command, every flag
- **[SDK reference](docs/sdk.md)** — `github.com/loka-network/paycli/pkg/sdk`
- **[Fiat onramp](docs/fiat-onramp.md)** — Stripe / PayPal operator setup
- **[Manual test playbook](docs/manual-test.md)** — bring up the full local stack and verify

## Layout

```
pkg/sdk/         Go client SDK (importable as github.com/loka-network/paycli/pkg/sdk)
cmd/lokapay/     CLI binary
docs/            CLI + SDK + onramp + integration-test playbooks
scripts/         install.sh + local integration-test runner
.goreleaser.yml  release config — tag-driven multi-platform build
```

## Design

Why a separate binary instead of folding this into `lncli`?

1. **Upstream rebase friction** — `lncli` is vendored from upstream lnd; mixing
   custodial REST commands into `cmd/commands/*.go` makes every upstream
   merge harder.
2. **Different audience, different scope** — `lncli` is a node operator's
   gRPC tool. `lokapay` is an end-user / AI-agent payments CLI with HTTP,
   L402, fiat-onramp, and managed-node lifecycle on top.
3. **Decoupled dependencies** — `lokapay` needs `net/http` + `urfave/cli` +
   `survey/v2`. No lnd build tree, no SUI compiler.

The SDK is the seam: any future Go integration (loka-cloud agents, MCP
servers, dashboards) imports `pkg/sdk` instead of reimplementing
REST + L402 + Stripe + node-install logic.
