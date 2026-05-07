package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/urfave/cli/v2"
)

// configKeyMap is the source of truth for `paycli config get/set`. Any new
// Config field that should be user-mutable goes here.
//
// Keys use dotted paths matching the on-disk JSON layout
// (e.g. `hosted.admin_key`, `node.endpoint`) so the CLI surface mirrors
// what users see when they cat the file.
//
// secret=true masks the value on `config show` so admin keys / macaroon
// paths don't get pasted into screenshots and tickets.
type configField struct {
	get    func(*Config) string
	set    func(*Config, string) error
	secret bool
	help   string
}

var configFields = map[string]configField{
	// Top-level ----------------------------------------------------------
	"route": {
		get: func(c *Config) string { return string(c.EffectiveRoute()) },
		set: func(c *Config, v string) error {
			r := Route(v)
			if r != RouteHosted && r != RouteNode {
				return fmt.Errorf("route must be %q or %q", RouteHosted, RouteNode)
			}
			c.Route = r
			return nil
		},
		help: "active route: hosted | node",
	},
	"insecure_tls": {
		get: func(c *Config) string { return strconv.FormatBool(c.Insecure) },
		set: func(c *Config, v string) error {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("expected true/false, got %q", v)
			}
			c.Insecure = b
			return nil
		},
		help: "skip TLS verification on the wallet endpoint (true|false)",
	},

	// Hosted route -------------------------------------------------------
	"hosted.base_url": {
		get:  func(c *Config) string { return c.Hosted.BaseURL },
		set:  func(c *Config, v string) error { c.Hosted.BaseURL = v; return nil },
		help: "[hosted] agents-pay-service base URL, e.g. https://agents-pay.loka.cash",
	},
	"hosted.admin_key": {
		get:    func(c *Config) string { return c.Hosted.AdminKey },
		set:    func(c *Config, v string) error { c.Hosted.AdminKey = v; return nil },
		secret: true,
		help:   "[hosted] wallet admin key (X-Api-Key, full spend authority)",
	},
	"hosted.invoice_key": {
		get:    func(c *Config) string { return c.Hosted.InvoiceKey },
		set:    func(c *Config, v string) error { c.Hosted.InvoiceKey = v; return nil },
		secret: true,
		help:   "[hosted] wallet invoice key (X-Api-Key, receive-only)",
	},
	"hosted.wallet_id": {
		get:  func(c *Config) string { return c.Hosted.WalletID },
		set:  func(c *Config, v string) error { c.Hosted.WalletID = v; return nil },
		help: "[hosted] wallet id",
	},
	"hosted.user_id": {
		get:  func(c *Config) string { return c.Hosted.UserID },
		set:  func(c *Config, v string) error { c.Hosted.UserID = v; return nil },
		help: "[hosted] account id (used by add-wallet)",
	},
	"hosted.admin_bearer_token": {
		get:    func(c *Config) string { return c.Hosted.AdminBearerToken },
		set:    func(c *Config, v string) error { c.Hosted.AdminBearerToken = v; return nil },
		secret: true,
		help:   "[hosted] super-user JWT cached by `auth-login` (used by topup, admin-set)",
	},

	// Node route ---------------------------------------------------------
	"node.endpoint": {
		get:  func(c *Config) string { return c.Node.Endpoint },
		set:  func(c *Config, v string) error { c.Node.Endpoint = v; return nil },
		help: "[node] lnd REST listener URL, e.g. https://127.0.0.1:8081",
	},
	"node.tls_cert_path": {
		get:  func(c *Config) string { return c.Node.TLSCertPath },
		set:  func(c *Config, v string) error { c.Node.TLSCertPath = v; return nil },
		help: "[node] path to lnd's tls.cert",
	},
	"node.macaroon_path": {
		get:  func(c *Config) string { return c.Node.MacaroonPath },
		set:  func(c *Config, v string) error { c.Node.MacaroonPath = v; return nil },
		help: "[node] path to lnd's admin.macaroon",
	},
}

// configKeyOrder is the deterministic listing order for `config show` /
// `config keys`. Group hosted-then-node so the listing matches the JSON
// layout the user sees on disk.
var configKeyOrder = []string{
	"route", "insecure_tls",
	"hosted.base_url", "hosted.admin_key", "hosted.invoice_key",
	"hosted.wallet_id", "hosted.user_id", "hosted.admin_bearer_token",
	"node.endpoint", "node.tls_cert_path", "node.macaroon_path",
}

func cmdConfig() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "View and edit ~/.paycli/config.json without hand-editing JSON",
		Subcommands: []*cli.Command{
			{
				Name:  "show",
				Usage: "Print the active config (secrets masked)",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "reveal", Usage: "include unmasked secrets"},
					&cli.BoolFlag{Name: "path", Usage: "only print the resolved config file path"},
				},
				Action: func(c *cli.Context) error {
					p, err := configPath()
					if err != nil {
						return err
					}
					if c.Bool("path") {
						fmt.Println(p)
						return nil
					}
					cfg, err := loadConfig()
					if err != nil {
						return err
					}
					values := make(map[string]string, len(configFields))
					for _, k := range configKeyOrder {
						f := configFields[k]
						v := f.get(cfg)
						if f.secret && !c.Bool("reveal") {
							v = maskSecret(v)
						}
						values[k] = v
					}
					b, _ := json.MarshalIndent(struct {
						Path   string            `json:"path"`
						Values map[string]string `json:"values"`
					}{p, values}, "", "  ")
					fmt.Println(string(b))
					return nil
				},
			},
			{
				Name:      "get",
				Usage:     "Print one config value",
				ArgsUsage: "<key>",
				Action: func(c *cli.Context) error {
					if c.NArg() < 1 {
						return fail("config get: <key> is required")
					}
					key := c.Args().First()
					f, ok := configFields[key]
					if !ok {
						return fail("unknown key %q (try `paycli config keys`)", key)
					}
					cfg, err := loadConfig()
					if err != nil {
						return err
					}
					fmt.Println(f.get(cfg))
					return nil
				},
			},
			{
				Name:      "set",
				Usage:     "Update a config value (writes ~/.paycli/config.json)",
				ArgsUsage: "<key> <value>",
				Action: func(c *cli.Context) error {
					if c.NArg() < 2 {
						return fail("config set: <key> and <value> are required")
					}
					key, val := c.Args().Get(0), c.Args().Get(1)
					f, ok := configFields[key]
					if !ok {
						return fail("unknown key %q (try `paycli config keys`)", key)
					}
					cfg, err := loadConfig()
					if err != nil {
						return err
					}
					if err := f.set(cfg, val); err != nil {
						return fail("config set %s: %v", key, err)
					}
					if err := saveConfig(cfg); err != nil {
						return fail("save config: %v", err)
					}
					fmt.Printf("%s updated\n", key)
					return nil
				},
			},
			{
				Name:  "keys",
				Usage: "List all editable config keys with descriptions",
				Action: func(c *cli.Context) error {
					for _, k := range configKeyOrder {
						f := configFields[k]
						mark := ""
						if f.secret {
							mark = " (secret)"
						}
						fmt.Printf("  %-26s %s%s\n", k, f.help, mark)
					}
					return nil
				},
			},
		},
	}
}

// maskSecret keeps the first 4 chars so users can confirm a key is set
// without exposing the whole value.
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}
