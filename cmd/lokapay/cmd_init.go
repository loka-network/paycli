package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdInit is the interactive setup wizard. Drives a first-time user
// from "I just installed lokapay" to "I can sign payments" without
// having to learn the register/login/wallets/config commands. The
// primary path is hosted route + register, which is what 95%+ of new
// users want; node and login paths are reachable from the same wizard.
//
// The flow doesn't introduce new server semantics — it just composes
// existing SDK / config helpers (sdk.RegisterAccount,
// sdk.ListWalletsByBearer, saveConfig, …). Anything the wizard sets
// up is reproducible by hand via `lokapay register` + `lokapay config
// set`, so the wizard is a convenience layer, not a new abstraction.
func cmdInit() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Interactive setup wizard — pick endpoint, register or log in, save ~/.lokapay/config.json",
		Description: "Walks first-time users through lokapay setup:\n" +
			"  1. choose hosted (custodial) or node (self-hosted) route\n" +
			"  2. pick an endpoint — defaults to https://agents-pay.loka.cash for hosted\n" +
			"  3. register a new named account or persist existing keys\n" +
			"  4. write ~/.lokapay/config.json and probe the endpoint\n\n" +
			"Re-running on an existing config offers backup / abort / overwrite.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "overwrite an existing config without prompting (backup is still written)"},
		},
		Action: func(c *cli.Context) error {
			return runInitWizard(c.Context, c.Bool("yes"))
		},
	}
}

func runInitWizard(ctx context.Context, autoYes bool) error {
	fmt.Fprintln(os.Stderr, "── lokapay setup ────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)

	cfg, err := loadConfig()
	if err != nil {
		return fail("load existing config: %v", err)
	}

	if !autoYes && configHasMeaningfulContent(cfg) {
		choice, err := promptExistingConfigAction(cfg)
		if err != nil {
			return err
		}
		switch choice {
		case "abort":
			fmt.Fprintln(os.Stderr, "Aborted. Nothing changed.")
			return nil
		case "keep_modify":
			// Keep existing config as baseline; user will overwrite
			// fields below as they go through the wizard.
		case "backup_fresh":
			if err := backupConfigOrFail(); err != nil {
				return err
			}
			cfg = &Config{}
		}
	}

	route, err := promptRoute(cfg.EffectiveRoute())
	if err != nil {
		return err
	}
	cfg.Route = route

	switch route {
	case RouteHosted:
		return wizardHosted(ctx, cfg)
	case RouteNode:
		return wizardNode(ctx, cfg)
	}
	return fail("init: unexpected route %q", route)
}

// wizardHosted handles the hosted route — agents-pay-service custodial
// wallet at a configurable URL. It branches into register-new-account
// or persist-existing-keys based on the user's pick.
func wizardHosted(ctx context.Context, cfg *Config) error {
	baseURL, insecure, err := promptHostedEndpoint(defaultIfBlank(cfg.Hosted.BaseURL, sdk.DefaultBaseURL))
	if err != nil {
		return err
	}
	cfg.Hosted.BaseURL = baseURL
	cfg.Insecure = insecure

	mode, err := promptHostedAccountMode()
	if err != nil {
		return err
	}

	switch mode {
	case "register":
		return wizardHostedRegister(ctx, cfg)
	case "login_keys":
		return wizardHostedLoginKeys(ctx, cfg)
	}
	return fail("init: unexpected hosted-mode pick %q", mode)
}

// wizardHostedRegister mirrors `lokapay register --username` but with
// prompts. Backed by sdk.RegisterAccount + sdk.ListWalletsByBearer so
// it produces the same on-disk shape.
func wizardHostedRegister(ctx context.Context, cfg *Config) error {
	username, password, email, err := promptRegisterDetails()
	if err != nil {
		return err
	}

	if err := guardDuplicateWallet(cfg, "default", false); err != nil {
		return fail("init: %v", err)
	}

	var opts []sdk.Option
	if cfg.Insecure {
		opts = append(opts, sdk.WithInsecureTLS())
	}

	fmt.Fprintf(os.Stderr, "→ registering %q at %s …\n", username, cfg.Hosted.BaseURL)
	jwt, err := sdk.RegisterAccount(ctx, cfg.Hosted.BaseURL, username, password, email, opts...)
	if err != nil {
		return fail("register: %v", err)
	}
	wallets, err := sdk.ListWalletsByBearer(ctx, cfg.Hosted.BaseURL, jwt, opts...)
	if err != nil {
		return fail("register: account created but listing wallets failed: %v", err)
	}
	if len(wallets) == 0 {
		return fail("register: account created but no wallets returned")
	}
	w := wallets[0]

	cfg.Hosted.Username = username
	cfg.Hosted.UserID = w.User
	cfg.Hosted.AdminBearerToken = jwt
	cfg.Hosted.ActiveWallet = "default"
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
		Memo:        "init-wizard username=" + username,
	})
	printHostedSummary(cfg, w.Name)
	return nil
}

// wizardHostedLoginKeys persists pre-existing wallet keys. No remote
// call until the user runs `lokapay whoami` — matches `lokapay login`'s
// "credentials in, config out" stance.
func wizardHostedLoginKeys(ctx context.Context, cfg *Config) error {
	adminKey, invoiceKey, walletAlias, walletID, userID, err := promptLoginKeys()
	if err != nil {
		return err
	}
	cfg.Hosted.PutWallet(walletAlias, WalletEntry{
		WalletID:   walletID,
		AdminKey:   adminKey,
		InvoiceKey: invoiceKey,
	})
	cfg.Hosted.ActiveWallet = walletAlias
	if userID != "" {
		cfg.Hosted.UserID = userID
	}
	if err := saveConfig(cfg); err != nil {
		return fail("save config: %v", err)
	}
	printHostedSummary(cfg, walletAlias)
	return nil
}

// wizardNode handles the self-hosted lnd route. Two sub-flows:
//
//	guided — lokapay downloads loka-lnd, starts it against Sui
//	         devnet/testnet, optionally hits the faucet + connects to
//	         Loka seed nodes, then writes everything into the config
//	         (delegates to `lokapay node install` + `lokapay node start`)
//	manual — point lokapay at an already-running lnd by typing the
//	         endpoint + tls cert + macaroon paths
func wizardNode(ctx context.Context, cfg *Config) error {
	mode, err := promptNodeMode()
	if err != nil {
		return err
	}
	if mode == "guided" {
		return wizardNodeGuided(ctx, cfg)
	}
	return wizardNodeManual(ctx, cfg)
}

// wizardNodeGuided downloads + starts a managed lnd. Mirrors the steps
// in loka-agentic-payment SKILL.md, gated by a few prompts so the user
// confirms network + bootstrap actions.
func wizardNodeGuided(ctx context.Context, cfg *Config) error {
	network, doFaucet, doSeeds, err := promptNodeGuided()
	if err != nil {
		return err
	}

	destRoot, err := lokapayLndRoot()
	if err != nil {
		return err
	}
	res, err := sdk.DownloadAndExtractLnd(ctx, destRoot, sdk.DefaultLndVersion, false, os.Stderr)
	if err != nil {
		return fail("init: download lnd: %v", err)
	}
	cfg.Node.LndBinaryPath = res.LndPath
	cfg.Node.LncliBinaryPath = res.LncliPath
	cfg.Node.LndVersion = res.Version
	if err := saveConfig(cfg); err != nil {
		return fail("save config: %v", err)
	}

	pkgID, err := sdk.FetchSuiPackageID(ctx, network, res.Version)
	if err != nil {
		return fail("init: resolve package id: %v", err)
	}
	fmt.Fprintf(os.Stderr, "  sui %s package_id: %s\n", network, pkgID)

	lndDir, err := lokapayLndDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(lndDir, 0o700); err != nil {
		return fail("mkdir %s: %v", lndDir, err)
	}
	if alreadyRunning(lndDir) {
		fmt.Fprintf(os.Stderr, "ℹ lnd already running at %s — skipping spawn; will reuse\n", lndDir)
	} else {
		netCfg := sdk.SuiNetworkConfigs[network]
		if err := spawnManagedLnd(ctx, cfg, lndDir, network, netCfg, pkgID, defaultManagedLndPorts, 30*time.Second); err != nil {
			return fail("init: spawn lnd: %v", err)
		}
	}

	cfg.Node.Endpoint = "https://127.0.0.1:8081"
	cfg.Node.TLSCertPath = filepath.Join(lndDir, "tls.cert")
	cfg.Node.MacaroonPath = filepath.Join(lndDir, "data", "chain", "sui", string(network), "admin.macaroon")
	cfg.Node.LndDir = lndDir
	cfg.Node.Network = string(network)
	cfg.Node.PackageID = pkgID
	cfg.Insecure = true
	if err := saveConfig(cfg); err != nil {
		return fail("save config: %v", err)
	}

	if doFaucet {
		if err := fundWalletFromFaucet(ctx, cfg, sdk.SuiNetworkConfigs[network]); err != nil {
			fmt.Fprintf(os.Stderr, "warning: faucet bootstrap failed: %v\n", err)
		}
	}
	if doSeeds {
		connectSeeds(ctx, cfg)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "✓ node ready")
	fmt.Fprintf(os.Stderr, "  network:   %s\n  endpoint:  %s\n  lnddir:    %s\n", network, cfg.Node.Endpoint, lndDir)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Next:")
	fmt.Fprintln(os.Stderr, "  lokapay node status        # confirm pubkey + balance")
	fmt.Fprintln(os.Stderr, "  lokapay whoami             # via the saved node config")
	return nil
}

func wizardNodeManual(ctx context.Context, cfg *Config) error {
	endpoint, tlsCert, macaroon, insecure, err := promptNodeEndpoint(cfg.Node.Endpoint, cfg.Node.TLSCertPath, cfg.Node.MacaroonPath)
	if err != nil {
		return err
	}
	cfg.Node.Endpoint = endpoint
	cfg.Node.TLSCertPath = tlsCert
	cfg.Node.MacaroonPath = macaroon
	if insecure {
		cfg.Insecure = true
	}
	if err := saveConfig(cfg); err != nil {
		return fail("save config: %v", err)
	}

	// Probe — same semantics as `lokapay register --route node`.
	fmt.Fprintln(os.Stderr, "→ probing node with GetInfo …")
	nc, err := nodeClientFromConfig(cfg, "", false)
	if err != nil {
		return fail("init: node config invalid: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if info, err := nc.GetInfo(probeCtx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: GetInfo failed (%v) — config saved anyway; LN-level RPCs may still work\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "✓ node responded — alias=%s pubkey=%s\n", info.Alias, info.IdentityPubkey)
	}
	fmt.Fprintln(os.Stderr, "Done. Try: lokapay whoami")
	return nil
}

// ----------------------------------------------------------------
// Prompts. Each helper is a single survey question (or a small group)
// so the wizard's flow above reads as plain prose.

func promptExistingConfigAction(cfg *Config) (string, error) {
	p, _ := configPath()
	label := "Existing config detected (" + p + ")"
	walletCount := len(cfg.Hosted.Wallets)
	if walletCount > 0 {
		label = fmt.Sprintf("%s — %d wallet(s), route=%s", label, walletCount, cfg.EffectiveRoute())
	}
	choice := ""
	err := survey.AskOne(&survey.Select{
		Message: label + ". What now?",
		Options: []string{
			"backup_fresh", // recommended for "I want a clean slate"
			"keep_modify",
			"abort",
		},
		Default: "backup_fresh",
		Description: func(value string, _ int) string {
			switch value {
			case "backup_fresh":
				return "copy current → config.json.bak.<ts>, start clean"
			case "keep_modify":
				return "keep current entries; only overwrite what the wizard touches"
			case "abort":
				return "exit without touching the file"
			}
			return ""
		},
	}, &choice)
	return choice, err
}

func promptRoute(current Route) (Route, error) {
	def := string(RouteHosted)
	if current == RouteNode {
		def = string(RouteNode)
	}
	choice := ""
	err := survey.AskOne(&survey.Select{
		Message: "Custody mode:",
		Options: []string{string(RouteHosted), string(RouteNode)},
		Default: def,
		Description: func(value string, _ int) string {
			switch value {
			case string(RouteHosted):
				return "custodial — agents-pay-service holds your funds (convenient: no lnd, no channel ops)"
			case string(RouteNode):
				return "self-hosted — you run your own lnd / lnd-sui REST gateway"
			}
			return ""
		},
	}, &choice)
	return Route(choice), err
}

func promptHostedEndpoint(current string) (string, bool, error) {
	endpoint := ""
	if err := survey.AskOne(&survey.Input{
		Message: "agents-pay-service URL:",
		Default: current,
	}, &endpoint, survey.WithValidator(survey.Required), survey.WithValidator(validateURL)); err != nil {
		return "", false, err
	}
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	insecure := shouldSkipTLSFor(endpoint)
	if insecure {
		fmt.Fprintf(os.Stderr, "  (local / non-TLS endpoint — TLS verification will be skipped)\n")
	}
	return endpoint, insecure, nil
}

func promptHostedAccountMode() (string, error) {
	choice := ""
	err := survey.AskOne(&survey.Select{
		Message: "Account:",
		Options: []string{"register", "login_keys"},
		Default: "register",
		Description: func(value string, _ int) string {
			switch value {
			case "register":
				return "create a new named account (username + password)"
			case "login_keys":
				return "I already have wallet keys (admin_key / invoice_key) to import"
			}
			return ""
		},
	}, &choice)
	return choice, err
}

func promptRegisterDetails() (username, password, email string, err error) {
	if err = survey.AskOne(&survey.Input{
		Message: "Username (2-20 chars, letters/digits/./_; no leading/trailing . or _ and no .. or __):",
	}, &username, survey.WithValidator(validateUsername)); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Password{
		Message: "Password (at least 8 characters):",
	}, &password, survey.WithValidator(validatePassword)); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Input{
		Message: "Email (optional, press enter to skip):",
	}, &email, survey.WithValidator(validateOptionalEmail)); err != nil {
		return
	}
	return
}

// validateUsername mirrors lnbits' is_valid_username regex
// (^[a-zA-Z0-9._]{2,20}$ + no leading/trailing _ or ., no double __ or ..)
// so the wizard rejects bad input locally instead of round-tripping a
// 400 from the server.
func validateUsername(ans interface{}) error {
	s, ok := ans.(string)
	if !ok {
		return fmt.Errorf("expected string")
	}
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 20 {
		return fmt.Errorf("must be 2-20 characters")
	}
	if !usernameAllowedChars.MatchString(s) {
		return fmt.Errorf("only letters, digits, '.' and '_' allowed (no '-' or other punctuation)")
	}
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, "_") {
		return fmt.Errorf("cannot start or end with '.' or '_'")
	}
	if strings.Contains(s, "..") || strings.Contains(s, "__") {
		return fmt.Errorf("cannot contain '..' or '__'")
	}
	return nil
}

// validatePassword enforces lnbits' min-length rule (8 chars). The
// server also accepts longer passwords with any characters; we don't
// duplicate complexity rules beyond what the server checks.
func validatePassword(ans interface{}) error {
	s, ok := ans.(string)
	if !ok {
		return fmt.Errorf("expected string")
	}
	if len(s) < 8 {
		return fmt.Errorf("must be at least 8 characters (got %d)", len(s))
	}
	return nil
}

// validateOptionalEmail accepts empty input (the field is optional)
// and otherwise does a minimal sanity check — exactly one '@' with
// non-empty local-part and domain-part. Server still has the final
// say; this is just to catch obvious typos before a network round-trip.
func validateOptionalEmail(ans interface{}) error {
	s, ok := ans.(string)
	if !ok {
		return fmt.Errorf("expected string")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	at := strings.IndexByte(s, '@')
	if at < 1 || at == len(s)-1 || strings.IndexByte(s[at+1:], '@') >= 0 {
		return fmt.Errorf("not a valid email (want user@host)")
	}
	if !strings.Contains(s[at+1:], ".") {
		return fmt.Errorf("email host must contain '.'")
	}
	return nil
}

// usernameAllowedChars matches the character class lnbits permits:
// letters, digits, dot, underscore. Anchored so the whole input must
// be in the class — partial matches don't satisfy validateUsername.
var usernameAllowedChars = regexp.MustCompile(`^[a-zA-Z0-9._]+$`)

func promptLoginKeys() (adminKey, invoiceKey, walletAlias, walletID, userID string, err error) {
	if err = survey.AskOne(&survey.Input{
		Message: "Wallet alias to store under:",
		Default: "default",
	}, &walletAlias, survey.WithValidator(survey.Required)); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Password{
		Message: "admin_key:",
	}, &adminKey, survey.WithValidator(survey.Required)); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Password{
		Message: "invoice_key:",
	}, &invoiceKey, survey.WithValidator(survey.Required)); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Input{
		Message: "wallet_id (optional):",
	}, &walletID); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Input{
		Message: "user_id (optional):",
	}, &userID); err != nil {
		return
	}
	return
}

func promptNodeMode() (string, error) {
	choice := ""
	err := survey.AskOne(&survey.Select{
		Message: "Node setup:",
		Options: []string{"guided", "manual"},
		Default: "guided",
		Description: func(value string, _ int) string {
			switch value {
			case "guided":
				return "lokapay downloads loka-lnd, starts it against Sui, configures itself"
			case "manual":
				return "I already have an lnd running — just enter endpoint + paths"
			}
			return ""
		},
	}, &choice)
	return choice, err
}

func promptNodeGuided() (sdk.SuiNetwork, bool, bool, error) {
	netChoice := ""
	if err := survey.AskOne(&survey.Select{
		Message: "Sui chain:",
		Options: []string{string(sdk.NetworkDevnet), string(sdk.NetworkTestnet)},
		Default: string(sdk.NetworkDevnet),
		Description: func(value string, _ int) string {
			switch value {
			case string(sdk.NetworkDevnet):
				return "fastest reset cadence; daily resets, faucet available"
			case string(sdk.NetworkTestnet):
				return "longer-lived; faucet available; closer to mainnet semantics"
			}
			return ""
		},
	}, &netChoice); err != nil {
		return "", false, false, err
	}
	doFaucet := false
	if err := survey.AskOne(&survey.Confirm{
		Message: "Request test SUI from the faucet after start?",
		Default: true,
	}, &doFaucet); err != nil {
		return "", false, false, err
	}
	doSeeds := false
	if err := survey.AskOne(&survey.Confirm{
		Message: "Connect to Loka EU + US seed nodes after start?",
		Default: true,
	}, &doSeeds); err != nil {
		return "", false, false, err
	}
	return sdk.SuiNetwork(netChoice), doFaucet, doSeeds, nil
}

func promptNodeEndpoint(curEndpoint, curTLS, curMac string) (endpoint, tlsCert, macaroon string, insecure bool, err error) {
	if err = survey.AskOne(&survey.Input{
		Message: "lnd REST endpoint:",
		Default: defaultIfBlank(curEndpoint, "https://127.0.0.1:8081"),
	}, &endpoint, survey.WithValidator(survey.Required), survey.WithValidator(validateURL)); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Input{
		Message: "Path to tls.cert (leave empty if the endpoint is local / self-signed):",
		Default: curTLS,
	}, &tlsCert); err != nil {
		return
	}
	if err = survey.AskOne(&survey.Input{
		Message: "Path to admin.macaroon:",
		Default: curMac,
	}, &macaroon, survey.WithValidator(survey.Required)); err != nil {
		return
	}
	// Auto-flip insecure when there's no cert AND the endpoint is local —
	// 99% of the time that combo means "self-signed local lnd". Public
	// remote endpoints without a cert keep insecure=false so the user
	// gets a clear TLS error rather than silent MITM exposure.
	if tlsCert == "" && shouldSkipTLSFor(endpoint) {
		insecure = true
		fmt.Fprintf(os.Stderr, "  (local endpoint with no cert — TLS verification will be skipped)\n")
	}
	return
}

// ----------------------------------------------------------------
// Small helpers.

func configHasMeaningfulContent(c *Config) bool {
	return c.Hosted.BaseURL != "" || len(c.Hosted.Wallets) > 0 ||
		c.Node.Endpoint != "" || c.Hosted.AdminBearerToken != ""
}

func backupConfigOrFail() error {
	p, err := configPath()
	if err != nil {
		return fail("init: configPath: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		// Nothing to back up; fine.
		return nil
	}
	bak := fmt.Sprintf("%s.bak.%s", p, time.Now().Format("20060102_150405"))
	in, err := os.ReadFile(p)
	if err != nil {
		return fail("init: read config for backup: %v", err)
	}
	if err := os.WriteFile(bak, in, 0o600); err != nil {
		return fail("init: write backup %s: %v", bak, err)
	}
	fmt.Fprintf(os.Stderr, "→ existing config backed up to %s\n", bak)
	return nil
}

func defaultIfBlank(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

// shouldSkipTLSFor decides whether TLS verification should be skipped
// for the given endpoint URL, based purely on the host. Used to drop a
// redundant "skip TLS verification?" prompt during init — the answer
// is auto-deterministic from the URL:
//
//   - http:// scheme               → true (no TLS to verify anyway)
//   - https:// to a loopback or
//     *.local mDNS host            → true (self-signed cert is the norm)
//   - https:// to a public domain  → false (real cert expected)
//
// Caller is welcome to override via paycli's global --insecure flag if
// they're in some weird middle case (e.g. a private VPN with no cert);
// this helper just picks a sensible default so the wizard stops asking.
func shouldSkipTLSFor(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	if u.Scheme == "http" {
		return true
	}
	host := u.Hostname()
	switch host {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
		return true
	}
	// Common dev / homelab patterns: foo.local (mDNS), foo.localhost
	// (which RFC 6761 reserves as loopback). Treat both as local.
	if strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") {
		return true
	}
	// IPv4 private ranges 10.x.x.x and 192.168.x.x and 172.16-31.x.x —
	// also typical for in-LAN lnd/agents-pay-service deployments where
	// a real cert is rare.
	if strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") {
		return true
	}
	if strings.HasPrefix(host, "172.") {
		// 172.16.0.0/12 → second octet 16-31
		if dot := strings.IndexByte(host[4:], '.'); dot > 0 {
			second := host[4 : 4+dot]
			if n, err := strconv.Atoi(second); err == nil && n >= 16 && n <= 31 {
				return true
			}
		}
	}
	return false
}

// validateURL is a survey validator that accepts http(s)://host[:port][/path].
// It rejects obviously broken input early so the user fixes typos before
// the wizard tries (and fails) to talk to the server.
func validateURL(ans interface{}) error {
	s, ok := ans.(string)
	if !ok {
		return fmt.Errorf("expected string")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("URL is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must start with http:// or https://")
	}
	if u.Host == "" {
		return fmt.Errorf("URL is missing host")
	}
	return nil
}

func printHostedSummary(cfg *Config, walletName string) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "✓ setup complete")
	fmt.Fprintf(os.Stderr, "  endpoint:      %s\n", cfg.Hosted.BaseURL)
	fmt.Fprintf(os.Stderr, "  route:         %s\n", cfg.EffectiveRoute())
	fmt.Fprintf(os.Stderr, "  active wallet: %s (server name: %s)\n", cfg.Hosted.ActiveWallet, walletName)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Next:")
	fmt.Fprintln(os.Stderr, "  lokapay whoami            # confirm the wallet")
	fmt.Fprintln(os.Stderr, "  lokapay services          # browse Prism services")
	fmt.Fprintln(os.Stderr, "  lokapay fund --amount 5 --unit USD --via stripe   # top up via Stripe")
}
