# lokapay

Loka Payment CLI + Go SDK. Pay L402-protected (HTTP 402) APIs over
Lightning on the Sui chain — either via the hosted Loka custodial
wallet or your own lnd node, with the same one-liner CLI.

```
┌──────────────┐  hosted: agents-pay.loka.cash   ┌─────────────────────┐
│   lokapay    │ ───────────────────────────────►│  agents-pay-service │
│  (CLI/SDK)   │                                 │   + lnd backend     │
│              │  node:   your own loka-lnd      └─────────────────────┘
│              │ ───────────────────────────────► loka-lnd (Sui)
└──────────────┘
       │   L402 challenge / preimage replay
       └──────► Prism (prism.loka.cash)
```

## Install

```bash
# curl (recommended — works on macOS / Linux)
curl -fsSL https://github.com/loka-network/paycli/releases/latest/download/install.sh | sh

# Homebrew
brew install loka-network/tap/lokapay

# Go (installs from main; requires Go 1.25+)
go install github.com/loka-network/paycli/cmd/lokapay@latest
```

## Quick start

```bash
lokapay init                                   # one-shot interactive setup
lokapay services                               # browse Prism's service catalog
lokapay node faucet                            # (devnet/testnet only) top up test SUI
lokapay request -i https://merchant/foo        # pays the 402 challenge, returns the response
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
