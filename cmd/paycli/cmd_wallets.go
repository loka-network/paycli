package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdWallets is the wallets subcommand group. paycli's hosted-route
// model is one user account → many sub-wallets, each with its own
// admin/invoice X-Api-Keys an agent talks to agents-pay-service with.
// This group manages the local map of those sub-wallets:
//
//	paycli wallets list                # show local map (active marked)
//	paycli wallets add <name>          # create on server + persist locally
//	paycli wallets use <name>          # switch active
//	paycli wallets show [<name>]       # print one entry's keys
//	paycli wallets remove <name>       # drop from local map (no server delete)
func cmdWallets() *cli.Command {
	return &cli.Command{
		Name:  "wallets",
		Usage: "Manage local sub-wallets under the current hosted account",
		Subcommands: []*cli.Command{
			cmdWalletsList(),
			cmdWalletsAdd(),
			cmdWalletsUse(),
			cmdWalletsShow(),
			cmdWalletsRemove(),
		},
	}
}

func cmdWalletsList() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List sub-wallets recorded in ~/.paycli/config.json",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "raw JSON output"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return printJSON(struct {
					ActiveWallet string                       `json:"active_wallet"`
					Wallets      map[string]WalletEntry       `json:"wallets"`
				}{cfg.Hosted.ActiveWallet, cfg.Hosted.Wallets})
			}
			if len(cfg.Hosted.Wallets) == 0 {
				fmt.Println("(no sub-wallets configured — run `paycli register` or `paycli wallets add <name>`)")
				return nil
			}
			names := make([]string, 0, len(cfg.Hosted.Wallets))
			for n := range cfg.Hosted.Wallets {
				names = append(names, n)
			}
			sort.Strings(names)
			// Compact column widths matched to maskSecretShort's 9-char
			// "4chars…4chars" output, so the header line aligns with the
			// values regardless of how long the underlying keys are.
			const (
				colAct = 3
				colNm  = 16
				colID  = 18
				colKey = 11
			)
			fmt.Printf("%-*s %-*s %-*s %-*s %-*s\n",
				colAct, "ACT",
				colNm, "NAME",
				colID, "WALLET_ID",
				colKey, "ADMIN",
				colKey, "INVOICE")
			for _, n := range names {
				w := cfg.Hosted.Wallets[n]
				active := "  "
				if n == cfg.Hosted.ActiveWallet {
					active = "* "
				}
				fmt.Printf("%-*s %-*s %-*s %-*s %-*s\n",
					colAct, active,
					colNm, truncateMid(n, colNm),
					colID, truncateMid(w.WalletID, colID),
					colKey, maskSecretShort(w.AdminKey),
					colKey, maskSecretShort(w.InvoiceKey))
			}
			return nil
		},
	}
}

func cmdWalletsAdd() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "Create a sub-wallet on the server and persist its keys locally",
		ArgsUsage: "<name>",
		Description: "Calls POST /api/v1/wallet with the cached super-user JWT (run `paycli auth-login` or `paycli register --username`).\n" +
			"The new wallet's admin / invoice X-Api-Keys are saved to ~/.paycli/config.json under the alias <name>.\n" +
			"<name> is the LOCAL alias paycli uses (also sent to the server as the wallet's display name).",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "use", Usage: "also switch active_wallet to the newly-created entry"},
			&cli.BoolFlag{Name: "force", Usage: "overwrite an existing local entry under the same alias (default refuses)"},
		},
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("wallets add: <name> is required")
			}
			name := c.Args().First()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Hosted.AdminBearerToken == "" {
				return fail("wallets add: no super-user JWT cached — register a named account first " +
					"(`paycli register --username NAME --password PW \"main\"`) or run `paycli auth-login`")
			}
			if err := guardDuplicateWallet(cfg, name, c.Bool("force")); err != nil {
				return fail("wallets add: %v", err)
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
			w, err := sdk.CreateWalletByBearer(c.Context, baseURL, cfg.Hosted.AdminBearerToken, name, opts...)
			if err != nil {
				return fail("wallets add: %v", err)
			}
			entry := WalletEntry{
				WalletID:   w.ID,
				AdminKey:   w.AdminKey,
				InvoiceKey: w.InvoiceKey,
			}
			cfg.Hosted.PutWallet(name, entry)
			if c.Bool("use") {
				cfg.Hosted.ActiveWallet = name
			}
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			LogEvent(Event{
				Event:       EventAccountCreated,
				Route:       string(RouteHosted),
				Endpoint:    baseURL,
				WalletAlias: name,
				WalletID:    w.ID,
				UserID:      cfg.Hosted.UserID,
				Memo:        "sub-wallet name=" + w.Name,
			})
			return printJSON(w)
		},
	}
}

func cmdWalletsUse() *cli.Command {
	return &cli.Command{
		Name:      "use",
		Usage:     "Switch the active wallet (paycli's default target for fund/pay/whoami)",
		ArgsUsage: "<name>",
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("wallets use: <name> is required")
			}
			name := c.Args().First()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Hosted.Wallets[name]; !ok {
				return fail("wallets use: no wallet named %q in config (run `paycli wallets list`)", name)
			}
			cfg.Hosted.ActiveWallet = name
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			fmt.Printf("active_wallet = %s\n", name)
			return nil
		},
	}
}

func cmdWalletsShow() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "Print one wallet's stored keys (defaults to the active wallet)",
		ArgsUsage: "[<name>]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "reveal", Usage: "include unmasked keys"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := c.Args().First()
			resolved, w, err := cfg.Hosted.ResolveWallet(name)
			if err != nil {
				return fail("wallets show: %v", err)
			}
			out := struct {
				Name       string `json:"name"`
				WalletID   string `json:"wallet_id"`
				AdminKey   string `json:"admin_key"`
				InvoiceKey string `json:"invoice_key"`
				Active     bool   `json:"active"`
			}{
				Name:     resolved,
				WalletID: w.WalletID,
				Active:   resolved == cfg.Hosted.ActiveWallet,
			}
			if c.Bool("reveal") {
				out.AdminKey = w.AdminKey
				out.InvoiceKey = w.InvoiceKey
			} else {
				out.AdminKey = maskSecret(w.AdminKey)
				out.InvoiceKey = maskSecret(w.InvoiceKey)
			}
			return printJSON(out)
		},
	}
}

func cmdWalletsRemove() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "Drop a wallet entry from the local config (no server-side delete)",
		ArgsUsage: "<name>",
		Description: "Removes the alias from ~/.paycli/config.json. The wallet keeps existing on the server and could be re-imported via `paycli wallets add` (which would re-create it with new keys, not the same ones — there's no server-side rename API in lnbits).",
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("wallets remove: <name> is required")
			}
			name := c.Args().First()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Hosted.Wallets[name]; !ok {
				return fail("wallets remove: no wallet named %q in config", name)
			}
			delete(cfg.Hosted.Wallets, name)
			if cfg.Hosted.ActiveWallet == name {
				// Pick a deterministic fallback: alphabetical first, or empty.
				cfg.Hosted.ActiveWallet = ""
				names := make([]string, 0, len(cfg.Hosted.Wallets))
				for n := range cfg.Hosted.Wallets {
					names = append(names, n)
				}
				sort.Strings(names)
				if len(names) > 0 {
					cfg.Hosted.ActiveWallet = names[0]
				}
			}
			if err := saveConfig(cfg); err != nil {
				return fail("save config: %v", err)
			}
			suffix := ""
			if cfg.Hosted.ActiveWallet != "" {
				suffix = fmt.Sprintf(", active_wallet now %q", cfg.Hosted.ActiveWallet)
			}
			fmt.Printf("removed %s%s\n", name, suffix)
			return nil
		},
	}
}

// maskSecretShort produces a fixed-shape "first-4 … last-4" preview,
// suitable for tabular display where the full mask from maskSecret
// (which preserves the original length) blows column alignment.
//
//	"" → "(none)"
//	≤8 → all asterisks
//	>8 → "abcd…wxyz"   (9 chars total)
func maskSecretShort(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// truncateMid keeps the start + end and drops the middle, so wallet
// ids / aliases that exceed the column don't wrap or push everything
// out of alignment. n is the target column width.
func truncateMid(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	keep := n - 1 // room for the single "…"
	left := keep / 2
	right := keep - left
	return s[:left] + "…" + s[len(s)-right:]
}
