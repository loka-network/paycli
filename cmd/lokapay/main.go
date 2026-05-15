package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/urfave/cli/v2"
)

// version is overridden at link-time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	app := &cli.App{
		Name:    "lokapay",
		Usage:   "Loka Payment CLI — self-custody or hosted L402 payments for AI agents",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "base-url",
				Usage:   "agents-pay-service base URL (overrides config)",
				EnvVars: []string{"PAYCLI_BASE_URL"},
			},
			&cli.BoolFlag{
				Name:    "insecure",
				Usage:   "skip TLS verification (local testing only)",
				EnvVars: []string{"PAYCLI_INSECURE"},
			},
			&cli.StringFlag{
				Name:    "config",
				Usage:   "path to config file",
				EnvVars: []string{"PAYCLI_CONFIG"},
			},
			&cli.StringFlag{
				Name:    "wallet",
				Usage:   "[hosted] sub-wallet alias to target (default: active_wallet from config)",
				EnvVars: []string{"PAYCLI_WALLET"},
			},
		},
		Before: func(c *cli.Context) error {
			if p := c.String("config"); p != "" {
				_ = os.Setenv("PAYCLI_CONFIG", p)
			}
			return nil
		},
		Commands: []*cli.Command{
			cmdInit(),
			cmdNode(),
			cmdRegister(),
			cmdLogin(),
			cmdWhoami(),
			cmdFund(),
			cmdPay(),
			cmdRequest(),
			cmdHistory(),
			cmdWallets(),
			cmdConfig(),
			cmdServices(),
			cmdAuthLogin(),
			cmdTopup(),
			cmdAdminSet(),
			cmdEvents(),
			cmdRate(),
			cmdPaymentStatus(),
		},
	}

	if err := app.Run(reorderArgs(app, os.Args)); err != nil {
		log.Fatal(err)
	}
}

// fail prints a user-facing error and exits with status 1.
func fail(format string, args ...interface{}) error {
	return cli.Exit(fmt.Sprintf(format, args...), 1)
}

// reorderArgs hoists any flag tokens that appear after a positional
// argument to the front of their subcommand's arg list. urfave/cli v2
// (which we're pinned to) stops flag parsing at the first non-flag
// token, so `lokapay request URL --debug` would silently drop --debug.
// Most CLIs (curl, git, …) accept flags anywhere; users expect the same
// from lokapay.
//
// The walker is flag-type-aware: it inspects the global and per-command
// flag definitions to know whether `-x` consumes the next arg (string
// / int / duration / slice flags) or is standalone (bool flag). For
// `--flag=value` the equals form keeps it as a single token.
func reorderArgs(app *cli.App, args []string) []string {
	if len(args) <= 1 {
		return args
	}
	globalBools := collectBoolFlagNames(app.Flags)

	// Pass 1: skip past global flags to find the subcommand.
	i := 1
	for i < len(args) {
		tok := args[i]
		if !strings.HasPrefix(tok, "-") {
			break // first non-flag = subcommand
		}
		// Flag tokens at the global level. Treat == form as standalone.
		if strings.Contains(tok, "=") {
			i++
			continue
		}
		name := strings.TrimLeft(tok, "-")
		if globalBools[name] {
			i++
			continue
		}
		// Non-bool flag consumes the next arg as its value.
		i += 2
	}
	if i >= len(args) {
		return args // no subcommand → nothing to reorder
	}
	subcmdIdx := i
	subcmdName := args[subcmdIdx]
	var cmd *cli.Command
	for _, c := range app.Commands {
		if c.HasName(subcmdName) {
			cmd = c
			break
		}
	}
	if cmd == nil {
		return args
	}

	cmdBools := collectBoolFlagNames(cmd.Flags)
	mergedBools := make(map[string]bool, len(globalBools)+len(cmdBools))
	for k := range globalBools {
		mergedBools[k] = true
	}
	for k := range cmdBools {
		mergedBools[k] = true
	}

	var flagTokens, posTokens []string
	j := subcmdIdx + 1
	for j < len(args) {
		tok := args[j]
		if !strings.HasPrefix(tok, "-") {
			posTokens = append(posTokens, tok)
			j++
			continue
		}
		flagTokens = append(flagTokens, tok)
		j++
		if strings.Contains(tok, "=") {
			continue // value is bundled
		}
		name := strings.TrimLeft(tok, "-")
		if mergedBools[name] {
			continue // bool flag, no value
		}
		// Value-bearing flag: take the next token as its value if it
		// doesn't itself look like a flag.
		if j < len(args) && !strings.HasPrefix(args[j], "-") {
			flagTokens = append(flagTokens, args[j])
			j++
		}
	}

	out := make([]string, 0, len(args))
	out = append(out, args[:subcmdIdx+1]...)
	out = append(out, flagTokens...)
	out = append(out, posTokens...)
	return out
}

func collectBoolFlagNames(flags []cli.Flag) map[string]bool {
	m := map[string]bool{}
	for _, f := range flags {
		if _, ok := f.(*cli.BoolFlag); !ok {
			continue
		}
		for _, n := range f.Names() {
			m[n] = true
		}
	}
	return m
}
