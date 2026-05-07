package main

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
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
			&cli.StringFlag{Name: "wallet-id", Usage: "target wallet id (defaults to active wallet)"},
			&cli.Float64Flag{Name: "amount", Required: true, Usage: "amount to credit in --unit (negative = debit)"},
			&cli.StringFlag{Name: "unit", Value: "sat", Usage: "amount unit: sat | mist | sui (whole, multiplies by 1e9)"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Hosted.AdminBearerToken == "" {
				return fail("topup: no super-user token cached — run `paycli auth login --username <name>` first")
			}
			// topup amount can be negative (= debit), so sign-preserve via
			// resolveAmount on the absolute value then re-apply the sign.
			amt := c.Float64("amount")
			sign := int64(1)
			if amt < 0 {
				sign = -1
				amt = -amt
			}
			subAmount, serverUnit, err := resolveAmount(amt, c.String("unit"))
			if err != nil {
				return fail("topup: %v", err)
			}
			// PUT /users/api/v1/balance only accepts native sub-units;
			// no fiat code makes sense at this admin-side endpoint.
			if serverUnit != "sat" && serverUnit != "mist" {
				return fail("topup: --unit must resolve to sat or mist (got %q)", serverUnit)
			}
			subAmount *= sign
			walletID := c.String("wallet-id")
			var walletAlias string
			if walletID == "" {
				name, w, err := cfg.Hosted.ResolveWallet(c.String("wallet"))
				if err != nil {
					return fail("topup: --wallet-id not given and no active wallet to default to: %v", err)
				}
				walletID = w.WalletID
				walletAlias = name
			} else {
				// Operator passed an explicit id; try to find a matching
				// alias in the local map for nicer logs (best-effort).
				for n, w := range cfg.Hosted.Wallets {
					if w.WalletID == walletID {
						walletAlias = n
						break
					}
				}
			}
			baseURL := cfg.Hosted.BaseURL
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
			if err := sdk.AdminCreditWallet(c.Context, baseURL, cfg.Hosted.AdminBearerToken,
				walletID, subAmount, opts...); err != nil {
				LogEvent(Event{
					Event:       EventTopupCredit,
					Route:       string(RouteHosted),
					Endpoint:    baseURL,
					WalletAlias: walletAlias,
					WalletID:    walletID,
					Amount:      subAmount,
					Unit:        serverUnit,
					Status:      "failed",
					Error:       err.Error(),
				})
				return fail("topup: %v", err)
			}
			LogEvent(Event{
				Event:       EventTopupCredit,
				Route:       string(RouteHosted),
				Endpoint:    baseURL,
				WalletAlias: walletAlias,
				WalletID:    walletID,
				Amount:      subAmount,
				Unit:        serverUnit,
				Status:      "success",
			})
			fmt.Printf("credited %d %s to wallet %s\n", subAmount, serverUnit, walletID)
			return nil
		},
	}
}

// cmdAdminSet PATCHes a single key on agents-pay-service's admin settings.
// The value is parsed as JSON when possible (so `true`, `123`, `"str"`,
// `["a","b"]` all round-trip correctly); otherwise it's sent as a plain
// string.
//
// Most useful for operator tweaks like enabling self-payment:
//
//   paycli admin-set lnd_grpc_allow_self_payment true
//
// Requires a super-user JWT cached via `paycli auth-login`.
func cmdAdminSet() *cli.Command {
	return &cli.Command{
		Name:      "admin-set",
		Usage:     "PATCH a single field on the agents-pay-service admin settings (operator only)",
		ArgsUsage: "<key> <value>",
		Action: func(c *cli.Context) error {
			if c.NArg() < 2 {
				return fail("admin-set: <key> and <value> are required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Hosted.AdminBearerToken == "" {
				return fail("admin-set: no super-user token cached — run `paycli auth-login --username <name>` first")
			}
			baseURL := cfg.Hosted.BaseURL
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
			key, raw := c.Args().Get(0), c.Args().Get(1)
			// Try JSON first (true/false/numbers/quoted-strings/arrays).
			// Fall back to a plain string so `paycli admin-set foo bar`
			// works without quoting.
			var val interface{}
			if err := json.Unmarshal([]byte(raw), &val); err != nil {
				val = raw
			}
			body := map[string]interface{}{key: val}
			if err := sdk.AdminPatchSettings(c.Context, baseURL, cfg.Hosted.AdminBearerToken, body, opts...); err != nil {
				return fail("admin-set: %v", err)
			}
			fmt.Printf("set %s = %v\n", key, val)
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
			baseURL := cfg.Hosted.BaseURL
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
			cfg.Hosted.AdminBearerToken = tok
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			fmt.Println("logged in; token cached")
			return nil
		},
	}
}
