package main

import (
	"fmt"
	"os"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
	"syscall"
)

// cmdTopup is the operator-side faucet for hosted wallets. It calls
// PUT /users/api/v1/balance on agents-pay-service, which synthesizes a
// successful internal payment on the target wallet — the same path the
// dashboard "credit user" action uses.
//
// Requires a super-user JWT. Login flow:
//
//   1. paycli auth login --username admin
//      (prompts for password, persists Bearer token to config)
//   2. paycli topup --wallet-id <id> --amount 50000
//
// This is intentionally separate from the hosted register/login flow — it
// targets a different role (operator vs end-user) and a different
// auth shape (Bearer JWT vs X-Api-Key).
func cmdTopup() *cli.Command {
	return &cli.Command{
		Name:  "topup",
		Usage: "Credit a hosted wallet via the super-user admin API (operator only)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "wallet-id", Required: true, Usage: "target wallet id"},
			&cli.Int64Flag{Name: "amount", Required: true, Usage: "amount to credit (sats / MIST), negative = debit"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.AdminBearerToken == "" {
				return fail("topup: no super-user token cached — run `paycli auth login --username <name>` first")
			}
			baseURL := cfg.BaseURL
			if v := c.String("base-url"); v != "" {
				baseURL = v
			}
			if baseURL == "" {
				baseURL = sdk.DefaultBaseURL
			}
			opts := []sdk.Option{}
			if c.Bool("insecure") || cfg.Insecure {
				opts = append(opts, sdk.WithInsecureTLS())
			}
			if err := sdk.AdminCreditWallet(c.Context, baseURL, cfg.AdminBearerToken,
				c.String("wallet-id"), c.Int64("amount"), opts...); err != nil {
				return fail("topup: %v", err)
			}
			fmt.Printf("credited %d to wallet %s\n", c.Int64("amount"), c.String("wallet-id"))
			return nil
		},
	}
}

// cmdAuthLogin authenticates as a super-user / admin and stores the JWT in
// the config so subsequent `paycli topup` calls can use it. Separate from
// cmdLogin (which only writes API keys / node creds, no remote call).
func cmdAuthLogin() *cli.Command {
	return &cli.Command{
		Name:  "auth-login",
		Usage: "Log in as a super-user / admin and cache the JWT (for `topup`)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "username", Required: true, Usage: "admin username"},
			&cli.StringFlag{Name: "password", Usage: "password (omit to read from stdin/tty)"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			baseURL := cfg.BaseURL
			if v := c.String("base-url"); v != "" {
				baseURL = v
			}
			if baseURL == "" {
				baseURL = sdk.DefaultBaseURL
			}
			pw := c.String("password")
			if pw == "" {
				fmt.Fprint(os.Stderr, "password: ")
				b, err := term.ReadPassword(int(syscall.Stdin))
				if err != nil {
					return fail("read password: %v", err)
				}
				fmt.Fprintln(os.Stderr)
				pw = string(b)
			}
			opts := []sdk.Option{}
			if c.Bool("insecure") || cfg.Insecure {
				opts = append(opts, sdk.WithInsecureTLS())
			}
			tok, err := sdk.LoginWithPassword(c.Context, baseURL, c.String("username"), pw, opts...)
			if err != nil {
				return fail("auth-login: %v", err)
			}
			cfg.AdminBearerToken = tok
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			fmt.Println("logged in; token cached")
			return nil
		},
	}
}
