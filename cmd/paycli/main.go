package main

import (
	"fmt"
	"log"
	"os"

	"github.com/urfave/cli/v2"
)

// version is overridden at link-time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	app := &cli.App{
		Name:    "paycli",
		Usage:   "Loka Payment CLI — custodial wallet + L402 payments for AI agents",
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

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

// fail prints a user-facing error and exits with status 1.
func fail(format string, args ...interface{}) error {
	return cli.Exit(fmt.Sprintf(format, args...), 1)
}
