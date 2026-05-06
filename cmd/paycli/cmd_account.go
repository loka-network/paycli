package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdRegister handles BOTH routes:
//
//   hosted: calls POST /api/v1/account on agents-pay-service and persists
//           the returned admin/invoice keys.
//
//   node:   no remote call (the wallet is the user's own lnd-sui node);
//           just persists the connection settings to the config file.
func cmdRegister() *cli.Command {
	return &cli.Command{
		Name:      "register",
		Usage:     "Register a wallet (creates an account on hosted, or pins node settings on node)",
		ArgsUsage: "[wallet-name]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "route", Value: string(RouteHosted), Usage: "hosted | node"},
			// node flags
			&cli.StringFlag{Name: "lnd-endpoint", Usage: "[node] REST listener URL, e.g. https://127.0.0.1:8081"},
			&cli.StringFlag{Name: "lnd-tls-cert", Usage: "[node] path to lnd's tls.cert"},
			&cli.StringFlag{Name: "lnd-macaroon", Usage: "[node] path to lnd's admin.macaroon"},
		},
		Action: func(c *cli.Context) error {
			route := Route(c.String("route"))

			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cfg.Route = route
			if c.Bool("insecure") {
				cfg.Insecure = true
			}

			switch route {
			case RouteHosted:
				if c.NArg() < 1 {
					return fail("wallet name is required")
				}
				if c.String("base-url") != "" {
					cfg.BaseURL = c.String("base-url")
				}
				if cfg.BaseURL == "" {
					cfg.BaseURL = sdk.DefaultBaseURL
				}
				cl := sdk.New(cfg.BaseURL)
				if cfg.Insecure {
					cl = sdk.New(cfg.BaseURL, sdk.WithInsecureTLS())
				}
				w, err := cl.CreateAccount(c.Context, c.Args().First())
				if err != nil {
					return fail("register: %v", err)
				}
				cfg.AdminKey = w.AdminKey
				cfg.InKey = w.InvoiceKey
				cfg.WalletID = w.ID
				cfg.UserID = w.User
				if err := saveConfig(cfg); err != nil {
					return fail("save config: %v", err)
				}
				return printJSON(w)

			case RouteNode:
				if c.String("lnd-endpoint") == "" || c.String("lnd-macaroon") == "" {
					return fail("register --route node requires --lnd-endpoint and --lnd-macaroon (--lnd-tls-cert recommended)")
				}
				cfg.NodeEndpoint = c.String("lnd-endpoint")
				cfg.NodeTLSCertPath = c.String("lnd-tls-cert")
				cfg.NodeMacaroonPath = c.String("lnd-macaroon")
				if err := saveConfig(cfg); err != nil {
					return fail("save config: %v", err)
				}
				// Probe the node so the user gets immediate feedback if anything is wrong.
				// GetInfo on lnd-sui devnet sometimes returns a 500 from the SUI chain
				// backend even when the LN-level RPCs (AddInvoice/SendPayment) work — so
				// surface the failure as a warning instead of aborting.
				nc, err := nodeClientFromConfig(cfg, "", false)
				if err != nil {
					return fail("node config invalid: %v", err)
				}
				info, err := nc.GetInfo(c.Context)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: GetInfo failed (%v) — config saved anyway; LN-level RPCs may still work\n", err)
					return nil
				}
				return printJSON(info)
			}
			return fail("unknown --route %q (want hosted | node)", route)
		},
	}
}

// cmdLogin persists existing credentials without making any remote call.
func cmdLogin() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "Persist existing credentials into the local config",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "route", Value: string(RouteHosted), Usage: "hosted | node"},
			// hosted flags
			&cli.StringFlag{Name: "admin-key", Usage: "[hosted] wallet admin key"},
			&cli.StringFlag{Name: "invoice-key", Usage: "[hosted] wallet invoice key"},
			&cli.StringFlag{Name: "wallet-id", Usage: "[hosted] wallet id (optional)"},
			&cli.StringFlag{Name: "user-id", Usage: "[hosted] account id (optional, needed for add-wallet)"},
			// node flags
			&cli.StringFlag{Name: "lnd-endpoint", Usage: "[node] REST listener URL"},
			&cli.StringFlag{Name: "lnd-tls-cert", Usage: "[node] path to tls.cert"},
			&cli.StringFlag{Name: "lnd-macaroon", Usage: "[node] path to macaroon file"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cfg.Route = Route(c.String("route"))
			if v := c.String("base-url"); v != "" {
				cfg.BaseURL = v
			}
			if c.Bool("insecure") {
				cfg.Insecure = true
			}

			switch cfg.Route {
			case RouteHosted:
				if v := c.String("admin-key"); v != "" {
					cfg.AdminKey = v
				}
				if v := c.String("invoice-key"); v != "" {
					cfg.InKey = v
				}
				if v := c.String("wallet-id"); v != "" {
					cfg.WalletID = v
				}
				if v := c.String("user-id"); v != "" {
					cfg.UserID = v
				}
				if cfg.AdminKey == "" && cfg.InKey == "" {
					return fail("login --route hosted requires at least one of --admin-key or --invoice-key")
				}
			case RouteNode:
				if v := c.String("lnd-endpoint"); v != "" {
					cfg.NodeEndpoint = v
				}
				if v := c.String("lnd-tls-cert"); v != "" {
					cfg.NodeTLSCertPath = v
				}
				if v := c.String("lnd-macaroon"); v != "" {
					cfg.NodeMacaroonPath = v
				}
				if cfg.NodeEndpoint == "" || cfg.NodeMacaroonPath == "" {
					return fail("login --route node requires --lnd-endpoint and --lnd-macaroon")
				}
			default:
				return fail("unknown --route %q (want hosted | node)", cfg.Route)
			}
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			fmt.Println("config saved")
			return nil
		},
	}
}

func cmdWhoami() *cli.Command {
	return &cli.Command{
		Name:  "whoami",
		Usage: "Show the wallet attached to the current credentials",
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			switch cfg.EffectiveRoute() {
			case RouteHosted:
				cl, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false)
				if err != nil {
					return err
				}
				w, err := cl.GetWallet(c.Context)
				if err != nil {
					return fail("whoami: %v", err)
				}
				return printJSON(w)
			case RouteNode:
				nc, err := nodeClientFromConfig(cfg, "", c.Bool("insecure"))
				if err != nil {
					return err
				}
				info, err := nc.GetInfo(c.Context)
				if err != nil {
					return fail("whoami: %v", err)
				}
				return printJSON(info)
			}
			return fail("unknown route")
		},
	}
}

func cmdAddWallet() *cli.Command {
	return &cli.Command{
		Name:      "add-wallet",
		Usage:     "Create an additional sub-wallet under the current account (hosted route only)",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "user-id", Usage: "account id (defaults to value cached from `register`)"},
		},
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("wallet name is required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cl, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), true)
			if err != nil {
				return err
			}
			userID := c.String("user-id")
			if userID == "" {
				userID = cfg.UserID
			}
			if userID == "" {
				return fail("add-wallet: --user-id is required (run `paycli register` first to cache it, or pass it explicitly)")
			}
			w, err := cl.CreateWallet(c.Context, userID, c.Args().First())
			if err != nil {
				return fail("add-wallet: %v", err)
			}
			return printJSON(w)
		},
	}
}

func printJSON(v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
