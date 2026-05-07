package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

// cmdRegister handles BOTH routes:
//
//   hosted (anonymous): calls POST /api/v1/account on agents-pay-service.
//           DB row has empty username; account can't log into the dashboard.
//           Best for AI agents / fleet provisioning.
//
//   hosted (named): when --username is set, switches to
//           POST /api/v1/auth/register so the resulting account has a
//           bcrypt password hash. Dashboard login + paycli auth-login
//           both work afterwards. The auto-created default wallet's
//           keys are fetched via GET /api/v1/wallets and persisted.
//
//   node:   no remote call (the wallet is the user's own lnd-sui node);
//           just persists the connection settings to the config file.
func cmdRegister() *cli.Command {
	return &cli.Command{
		Name:        "register",
		Usage:       "Register a wallet (anonymous on hosted by default, or named with --username)",
		Description: "IMPORTANT: put --flags BEFORE the [wallet-name] positional argument.\nurfave/cli v2 stops flag parsing at the first positional, so\n  paycli register --username alice --password ... \"alice-treasury\"\nis correct;  paycli register \"alice-treasury\" --username alice  is NOT.",
		ArgsUsage:   "[wallet-name]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "route", Value: string(RouteHosted), Usage: "hosted | node"},
			// hosted named-account flags
			&cli.StringFlag{Name: "username", Aliases: []string{"u"}, Usage: "[hosted] register a NAMED account (enables dashboard login + auth-login)"},
			&cli.StringFlag{Name: "password", Aliases: []string{"p"}, Usage: "[hosted] password for --username (omit to read from tty)"},
			&cli.StringFlag{Name: "email", Usage: "[hosted] optional email for the named account"},
			&cli.BoolFlag{Name: "force", Usage: "[hosted] overwrite an existing wallet entry under the same alias (default refuses)"},
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

			// Friendly hint: urfave/cli v2 stops flag parsing at the
			// first positional argument, so flags placed AFTER the
			// wallet-name leak into c.Args() and look like positionals
			// to us. Detect that and tell the user to swap the order.
			for _, a := range c.Args().Slice() {
				if strings.HasPrefix(a, "-") {
					return fail("looks like %q was passed AFTER the wallet name; "+
						"flag parsing stops at the first positional. "+
						"Put --username / --password / --email BEFORE the wallet name.", a)
				}
			}

			switch route {
			case RouteHosted:
				if c.String("base-url") != "" {
					cfg.Hosted.BaseURL = c.String("base-url")
				}
				if cfg.Hosted.BaseURL == "" {
					cfg.Hosted.BaseURL = sdk.DefaultBaseURL
				}

				if c.String("username") != "" {
					return registerNamedHosted(c, cfg)
				}

				// Anonymous fast path.
				if c.NArg() < 1 {
					return fail("wallet name is required (or pass --username to register a named account)")
				}
				if err := guardDuplicateWallet(cfg, "default", c.Bool("force")); err != nil {
					return fail("%v", err)
				}
				cl := sdk.New(cfg.Hosted.BaseURL)
				if cfg.Insecure {
					cl = sdk.New(cfg.Hosted.BaseURL, sdk.WithInsecureTLS())
				}
				w, err := cl.CreateAccount(c.Context, c.Args().First())
				if err != nil {
					return fail("register: %v", err)
				}
				cfg.Hosted.UserID = w.User
				// Anonymous register has no JWT and no concept of multiple
				// wallets — store the single auto-created wallet under the
				// "default" alias.
				cfg.Hosted.PutWallet("default", WalletEntry{
					WalletID:   w.ID,
					AdminKey:   w.AdminKey,
					InvoiceKey: w.InvoiceKey,
				})
				if err := saveConfig(cfg); err != nil {
					return fail("save config: %v", err)
				}
				LogEvent(Event{
					Event:       EventAccountCreated,
					Route:       string(RouteHosted),
					Endpoint:    cfg.Hosted.BaseURL,
					WalletAlias: "default",
					WalletID:    w.ID,
					UserID:      w.User,
					Memo:        "anonymous wallet=" + w.Name,
				})
				return printJSON(w)

			case RouteNode:
				if c.String("lnd-endpoint") == "" || c.String("lnd-macaroon") == "" {
					return fail("register --route node requires --lnd-endpoint and --lnd-macaroon (--lnd-tls-cert recommended)")
				}
				cfg.Node.Endpoint = c.String("lnd-endpoint")
				cfg.Node.TLSCertPath = c.String("lnd-tls-cert")
				cfg.Node.MacaroonPath = c.String("lnd-macaroon")
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
			&cli.StringFlag{Name: "wallet-name", Value: "default", Usage: "[hosted] alias to store the wallet under in the local map"},
			&cli.StringFlag{Name: "user-id", Usage: "[hosted] account id (optional, needed for wallets add)"},
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
				cfg.Hosted.BaseURL = v
			}
			if c.Bool("insecure") {
				cfg.Insecure = true
			}

			switch cfg.Route {
			case RouteHosted:
				adminKey := c.String("admin-key")
				inKey := c.String("invoice-key")
				walletID := c.String("wallet-id")
				walletName := c.String("wallet-name")
				if adminKey == "" && inKey == "" {
					return fail("login --route hosted requires at least one of --admin-key or --invoice-key")
				}
				if v := c.String("user-id"); v != "" {
					cfg.Hosted.UserID = v
				}
				// Merge into the named entry (replacing or creating).
				existing := cfg.Hosted.Wallets[walletName]
				if adminKey != "" {
					existing.AdminKey = adminKey
				}
				if inKey != "" {
					existing.InvoiceKey = inKey
				}
				if walletID != "" {
					existing.WalletID = walletID
				}
				cfg.Hosted.PutWallet(walletName, existing)
			case RouteNode:
				if v := c.String("lnd-endpoint"); v != "" {
					cfg.Node.Endpoint = v
				}
				if v := c.String("lnd-tls-cert"); v != "" {
					cfg.Node.TLSCertPath = v
				}
				if v := c.String("lnd-macaroon"); v != "" {
					cfg.Node.MacaroonPath = v
				}
				if cfg.Node.Endpoint == "" || cfg.Node.MacaroonPath == "" {
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
				cl, name, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false, c.String("wallet"))
				if err != nil {
					return err
				}
				w, err := cl.GetWallet(c.Context)
				if err != nil {
					return fail("whoami: %v", err)
				}
				return printJSON(struct {
					Alias  string                  `json:"alias"`
					Wallet *sdk.HostedWalletStatus `json:"wallet"`
				}{name, w})
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

// registerNamedHosted runs the named-account variant of `paycli register`:
// POST /api/v1/auth/register → JWT, then GET /api/v1/wallets to grab the
// auto-created wallet's keys, then persist everything (keys + JWT) to
// config so subsequent paycli commands work without re-authenticating.
//
// The JWT is the same kind of token paycli auth-login produces, so this
// flow doubles as a one-shot "register + login" — the user can call
// `paycli admin-set` / `topup` immediately if their account has the
// privileges.
func registerNamedHosted(c *cli.Context, cfg *Config) error {
	// Guard the local "default" alias before any remote call so we don't
	// half-create a server-side account whose keys we then refuse to
	// store. The check is identical to the anonymous path's.
	if err := guardDuplicateWallet(cfg, "default", c.Bool("force")); err != nil {
		return fail("%v", err)
	}

	username := c.String("username")
	password := c.String("password")
	if password == "" {
		fmt.Fprint(os.Stderr, "password: ")
		b, err := term.ReadPassword(int(syscall.Stdin))
		if err != nil {
			return fail("read password: %v", err)
		}
		fmt.Fprintln(os.Stderr)
		password = string(b)
		if password == "" {
			return fail("register --username: password cannot be empty")
		}
	}

	opts := []sdk.Option{}
	if cfg.Insecure {
		opts = append(opts, sdk.WithInsecureTLS())
	}

	jwt, err := sdk.RegisterAccount(c.Context, cfg.Hosted.BaseURL, username, password, c.String("email"), opts...)
	if err != nil {
		return fail("register --username %s: %v", username, err)
	}

	// Pull the auto-created default wallet's keys via the JWT.
	wallets, err := sdk.ListWalletsByBearer(c.Context, cfg.Hosted.BaseURL, jwt, opts...)
	if err != nil {
		return fail("register: account created but failed to list wallets: %v", err)
	}
	if len(wallets) == 0 {
		return fail("register: account created but no wallets were returned")
	}
	w := wallets[0]

	// If the user passed a wallet name as positional arg, rename the
	// default wallet to match. (PUT /api/v1/wallet/{new_name} requires the
	// admin_key — which we just got — so this is a single extra call.)
	if c.NArg() >= 1 {
		newName := c.Args().First()
		cl := sdk.New(cfg.Hosted.BaseURL, sdk.WithAdminKey(w.AdminKey))
		if cfg.Insecure {
			cl = sdk.New(cfg.Hosted.BaseURL, sdk.WithAdminKey(w.AdminKey), sdk.WithInsecureTLS())
		}
		if renamed, err := cl.RenameWallet(c.Context, newName); err == nil {
			w.Name = renamed.Name
		} else {
			fmt.Fprintf(os.Stderr, "warning: created account but rename failed (%v) — wallet name stays %q\n", err, w.Name)
		}
	}

	cfg.Hosted.Username = username
	cfg.Hosted.UserID = w.User
	cfg.Hosted.AdminBearerToken = jwt
	cfg.Hosted.PutWallet("default", WalletEntry{
		WalletID:   w.ID,
		AdminKey:   w.AdminKey,
		InvoiceKey: w.InvoiceKey,
	})
	if err := saveConfig(cfg); err != nil {
		return fail("save config: %v", err)
	}
	LogEvent(Event{
		Event:       EventAccountCreated,
		Route:       string(RouteHosted),
		Endpoint:    cfg.Hosted.BaseURL,
		WalletAlias: "default",
		WalletID:    w.ID,
		UserID:      w.User,
		Memo:        "username=" + username + " wallet=" + w.Name,
	})
	return printJSON(w)
}

// guardDuplicateWallet returns an error when the wallets map already
// contains the given alias, unless force is true. Used by both the
// anonymous and named register paths and by `wallets add` to keep the
// "create a new wallet" surface consistent — we never silently
// overwrite an existing key set, since that loses access to the
// previous wallet (the keys live nowhere else).
//
// The error message points at the two recovery paths:
//   - explicit overwrite via --force (operator opted in to the loss)
//   - first remove the old entry via `paycli wallets remove <alias>`
func guardDuplicateWallet(cfg *Config, alias string, force bool) error {
	if force {
		return nil
	}
	existing, ok := cfg.Hosted.Wallets[alias]
	if !ok {
		return nil
	}
	return fmt.Errorf(
		"a wallet alias %q already exists in this config (wallet_id %s) — refusing to overwrite. "+
			"Either remove it first (`paycli wallets remove %s`) or pass --force to drop the old keys",
		alias, existing.WalletID, alias)
}

func printJSON(v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
