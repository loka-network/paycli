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
// secret=true masks the value on `config show` so admin keys / macaroon
// paths don't get pasted into screenshots and tickets.
//
// validate runs at set time. Returning an error rejects the new value.
type configField struct {
	get      func(*Config) string
	set      func(*Config, string) error
	secret   bool
	help     string
}

var configFields = map[string]configField{
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

	// Hosted route ------------------------------------------------------
	"base_url": {
		get:  func(c *Config) string { return c.BaseURL },
		set:  func(c *Config, v string) error { c.BaseURL = v; return nil },
		help: "[hosted] agents-pay-service base URL, e.g. https://agents-pay.loka.cash",
	},
	"admin_key": {
		get:    func(c *Config) string { return c.AdminKey },
		set:    func(c *Config, v string) error { c.AdminKey = v; return nil },
		secret: true,
		help:   "[hosted] wallet admin key",
	},
	"invoice_key": {
		get:    func(c *Config) string { return c.InKey },
		set:    func(c *Config, v string) error { c.InKey = v; return nil },
		secret: true,
		help:   "[hosted] wallet invoice key",
	},
	"wallet_id": {
		get:  func(c *Config) string { return c.WalletID },
		set:  func(c *Config, v string) error { c.WalletID = v; return nil },
		help: "[hosted] wallet id",
	},
	"user_id": {
		get:  func(c *Config) string { return c.UserID },
		set:  func(c *Config, v string) error { c.UserID = v; return nil },
		help: "[hosted] account id (used by add-wallet)",
	},
	"admin_bearer_token": {
		get:    func(c *Config) string { return c.AdminBearerToken },
		set:    func(c *Config, v string) error { c.AdminBearerToken = v; return nil },
		secret: true,
		help:   "[hosted] super-user JWT cached by `auth-login` (used by topup)",
	},

	// Node route --------------------------------------------------------
	"node_endpoint": {
		get:  func(c *Config) string { return c.NodeEndpoint },
		set:  func(c *Config, v string) error { c.NodeEndpoint = v; return nil },
		help: "[node] lnd REST listener URL, e.g. https://127.0.0.1:8081",
	},
	"node_tls_cert_path": {
		get:  func(c *Config) string { return c.NodeTLSCertPath },
		set:  func(c *Config, v string) error { c.NodeTLSCertPath = v; return nil },
		help: "[node] path to lnd's tls.cert",
	},
	"node_macaroon_path": {
		get:  func(c *Config) string { return c.NodeMacaroonPath },
		set:  func(c *Config, v string) error { c.NodeMacaroonPath = v; return nil },
		help: "[node] path to lnd's admin.macaroon",
	},
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
					out := map[string]string{}
					for k, f := range configFields {
						v := f.get(cfg)
						if f.secret && !c.Bool("reveal") {
							v = maskSecret(v)
						}
						out[k] = v
					}
					b, _ := json.MarshalIndent(struct {
						Path   string            `json:"path"`
						Values map[string]string `json:"values"`
					}{p, out}, "", "  ")
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
					// Print in deterministic order, hosted-then-node grouping
					// so the listing matches the JSON layout the user sees.
					order := []string{
						"route", "insecure_tls",
						"base_url", "admin_key", "invoice_key", "wallet_id", "user_id",
						"admin_bearer_token",
						"node_endpoint", "node_tls_cert_path", "node_macaroon_path",
					}
					for _, k := range order {
						f := configFields[k]
						mark := ""
						if f.secret {
							mark = " (secret)"
						}
						fmt.Printf("  %-22s %s%s\n", k, f.help, mark)
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
