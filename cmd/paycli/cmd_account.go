package main

import (
	"encoding/json"
	"fmt"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

func cmdRegister() *cli.Command {
	return &cli.Command{
		Name:      "register",
		Usage:     "Create a new anonymous account + initial wallet",
		ArgsUsage: "<wallet-name>",
		Description: `Calls POST /api/v1/account on agents-pay-service. The returned
admin and invoice keys are persisted to the local config file so subsequent
commands authenticate automatically.`,
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("wallet name is required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if c.String("base-url") != "" {
				cfg.BaseURL = c.String("base-url")
			}
			if cfg.BaseURL == "" {
				cfg.BaseURL = sdk.DefaultBaseURL
			}
			if c.Bool("insecure") {
				cfg.Insecure = true
			}

			cl, err := clientFromConfig(cfg, "", false, true)
			if err != nil {
				return err
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
		},
	}
}

func cmdLogin() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "Persist an existing admin/invoice key into the local config",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "admin-key", Usage: "wallet admin key"},
			&cli.StringFlag{Name: "invoice-key", Usage: "wallet invoice key"},
			&cli.StringFlag{Name: "wallet-id", Usage: "wallet id (optional)"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if v := c.String("base-url"); v != "" {
				cfg.BaseURL = v
			}
			if c.Bool("insecure") {
				cfg.Insecure = true
			}
			if v := c.String("admin-key"); v != "" {
				cfg.AdminKey = v
			}
			if v := c.String("invoice-key"); v != "" {
				cfg.InKey = v
			}
			if v := c.String("wallet-id"); v != "" {
				cfg.WalletID = v
			}
			if cfg.AdminKey == "" && cfg.InKey == "" {
				return fail("at least one of --admin-key or --invoice-key must be set")
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
		Usage: "Show the wallet attached to the current API key",
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cl, err := clientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false)
			if err != nil {
				return err
			}
			w, err := cl.GetWallet(c.Context)
			if err != nil {
				return fail("whoami: %v", err)
			}
			return printJSON(w)
		},
	}
}

func cmdAddWallet() *cli.Command {
	return &cli.Command{
		Name:      "add-wallet",
		Usage:     "Create an additional sub-wallet under the current account",
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
			cl, err := clientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), true)
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
