package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/loka-network/paycli/pkg/sdk"
)

// Route enumerates the two custody models lokapay supports today. They are
// mutually exclusive per config — a user picks one at `register` / `login`
// time and every subsequent command dispatches against it.
type Route string

const (
	RouteHosted Route = "hosted" // custodial: agents-pay-service over REST
	RouteNode   Route = "node"   // self-custody: the user's own lnd / lnd-sui REST gateway
)

// Config is what we persist at ~/.lokapay/config.json.
//
// Route-specific fields are nested under `Hosted` and `Node`. The hosted
// block now models the LNbits domain accurately:
//
//	one user account (username + JWT for dashboard / operator ops)
//	└── many sub-wallets, each with its own admin / invoice X-Api-Key
//
// `wallets` is the map of locally-known sub-wallets keyed by a friendly
// alias the user picks (e.g. "default", "agent-research", "agent-trading").
// `active_wallet` is the alias lokapay uses by default for fund / pay /
// whoami / request — overridable per-call with the global --wallet flag.
//
// On-wire JSON:
//
//	{
//	  "route": "hosted",
//	  "insecure_tls": false,
//	  "hosted": {
//	    "base_url": "https://...",
//	    "username": "alice",
//	    "user_id": "...",
//	    "admin_bearer_token": "<JWT>",
//	    "active_wallet": "agent-research",
//	    "wallets": {
//	      "default":         { "wallet_id": "...", "admin_key": "...", "invoice_key": "..." },
//	      "agent-research":  { "wallet_id": "...", "admin_key": "...", "invoice_key": "..." }
//	    }
//	  },
//	  "node": { ... }
//	}
//
// Configs written by older lokapay builds are loaded transparently — the
// shim in loadConfig folds legacy flat / pre-multiwallet fields into the
// new shape. Subsequent saveConfig rewrites in the canonical layout.
type Config struct {
	Route    Route `json:"route,omitempty"`
	Insecure bool  `json:"insecure_tls,omitempty"`

	// PrismURL is the L402 gateway lokapay's `services` command (and any
	// future Prism-catalog operations) targets by default. Set by the
	// init wizard to match the agents-pay-service / lnd endpoint locality
	// (local endpoint → local prism, public endpoint → prism.loka.cash);
	// the user can override at any time via `lokapay config set prism_url
	// …` or per-call with `--prism-url`. Empty means "fall back to
	// sdk.DefaultPrismURL".
	PrismURL string `json:"prism_url,omitempty"`

	Hosted HostedConfig `json:"hosted,omitempty"`
	Node   NodeConfig   `json:"node,omitempty"`
}

// HostedConfig groups everything needed for the agents-pay-service custodial route.
type HostedConfig struct {
	BaseURL          string `json:"base_url,omitempty"`
	Username         string `json:"username,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	AdminBearerToken string `json:"admin_bearer_token,omitempty"`

	// ActiveWallet is the alias lokapay targets by default. When unset
	// AND there's exactly one wallet in the map, that single wallet is
	// the implicit active.
	ActiveWallet string `json:"active_wallet,omitempty"`

	// Wallets stores every sub-wallet lokapay has provisioned locally.
	// Each entry holds the per-wallet X-Api-Keys an agent talks to
	// agents-pay-service with.
	Wallets map[string]WalletEntry `json:"wallets,omitempty"`
}

// WalletEntry holds a single sub-wallet's identity + auth keys.
type WalletEntry struct {
	WalletID   string `json:"wallet_id,omitempty"`
	AdminKey   string `json:"admin_key,omitempty"`
	InvoiceKey string `json:"invoice_key,omitempty"`
}

// NodeConfig groups everything needed for the lnd-sui REST gateway route.
//
// Endpoint / TLSCertPath / MacaroonPath are the original "point at an
// already-running lnd" fields. The remaining fields are populated by
// `lokapay node install` + `lokapay node start` when lokapay is managing
// a local lnd itself; they're optional and harmless for the
// external-lnd workflow.
type NodeConfig struct {
	Endpoint     string `json:"endpoint,omitempty"`
	TLSCertPath  string `json:"tls_cert_path,omitempty"`
	MacaroonPath string `json:"macaroon_path,omitempty"`

	// Managed-lnd fields — populated by `lokapay node install/start`.
	LndBinaryPath   string `json:"lnd_binary_path,omitempty"`
	LncliBinaryPath string `json:"lncli_binary_path,omitempty"`
	LndDir          string `json:"lnd_dir,omitempty"`            // --lnddir target (and where logs / PID live)
	LndVersion      string `json:"lnd_version,omitempty"`        // e.g. "v0.21.0", for upgrade tracking
	Network         string `json:"network,omitempty"`            // "devnet" | "testnet" | "mainnet"
	PackageID       string `json:"package_id,omitempty"`         // resolved at start time, cached so lokapay node status doesn't need network
}

// ResolveWallet returns the wallet entry lokapay should use for a command.
//
//	override != ""        → look up by alias; error if missing
//	override == "" + ActiveWallet set → the named active wallet
//	override == "" + exactly one wallet in map → that wallet (implicit)
//	otherwise             → error asking the user to pick
func (h *HostedConfig) ResolveWallet(override string) (string, WalletEntry, error) {
	if override != "" {
		w, ok := h.Wallets[override]
		if !ok {
			return "", WalletEntry{}, fmt.Errorf("no wallet named %q in config (run `lokapay wallets list`)", override)
		}
		return override, w, nil
	}
	if h.ActiveWallet != "" {
		w, ok := h.Wallets[h.ActiveWallet]
		if !ok {
			return "", WalletEntry{}, fmt.Errorf("active_wallet=%q but no entry under wallets — run `lokapay wallets use <name>`", h.ActiveWallet)
		}
		return h.ActiveWallet, w, nil
	}
	if len(h.Wallets) == 1 {
		for name, w := range h.Wallets {
			return name, w, nil
		}
	}
	if len(h.Wallets) == 0 {
		return "", WalletEntry{}, errors.New("no wallets configured (run `lokapay register` or `lokapay wallets add <name>`)")
	}
	return "", WalletEntry{}, fmt.Errorf("multiple wallets configured but no active selected — run `lokapay wallets use <name>` or pass --wallet")
}

// PutWallet adds or replaces an entry in the wallets map. If the map is
// empty, the new entry is also marked as active.
func (h *HostedConfig) PutWallet(name string, w WalletEntry) {
	if h.Wallets == nil {
		h.Wallets = map[string]WalletEntry{}
	}
	if h.ActiveWallet == "" {
		h.ActiveWallet = name
	}
	h.Wallets[name] = w
}

// EffectiveRoute resolves the route, defaulting to hosted when unset (so
// configs written by older lokapay builds keep working).
func (c *Config) EffectiveRoute() Route {
	if c.Route == "" {
		return RouteHosted
	}
	return c.Route
}

// configPath returns the resolved path to the config file, honoring
// $PAYCLI_CONFIG when set.
func configPath() (string, error) {
	if p := os.Getenv("PAYCLI_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	migrateLegacyConfigDirOnce(home)
	return filepath.Join(home, ".lokapay", "config.json"), nil
}

// migrateLegacyConfigDir is the one-shot rename for users upgrading from
// the pre-rename build (binary was `paycli`, config dir was
// `~/.paycli/`). Runs once per process via the sync.Once below.
//
// Conditions for action: old dir present, new dir absent. Anything else
// (both present / new present / neither present) is a no-op so the
// migration never clobbers state, even if the user partially renamed by
// hand. We also rewrite "/.paycli/" → "/.lokapay/" inside the moved
// config.json so cached paths like NodeConfig.LndBinaryPath keep
// resolving after the directory move.
var migrateOnce sync.Once

func migrateLegacyConfigDirOnce(home string) {
	migrateOnce.Do(func() {
		oldDir := filepath.Join(home, ".paycli")
		newDir := filepath.Join(home, ".lokapay")
		if _, err := os.Stat(oldDir); err != nil {
			return
		}
		if _, err := os.Stat(newDir); err == nil {
			return
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not migrate %s → %s: %v\n", oldDir, newDir, err)
			return
		}
		// Patch baked-in paths inside the migrated config.json.
		cfgPath := filepath.Join(newDir, "config.json")
		if b, err := os.ReadFile(cfgPath); err == nil {
			patched := bytes.ReplaceAll(b, []byte("/.paycli/"), []byte("/.lokapay/"))
			if !bytes.Equal(b, patched) {
				_ = os.WriteFile(cfgPath, patched, 0o600)
			}
		}
		fmt.Fprintf(os.Stderr, "→ migrated config dir %s → %s\n", oldDir, newDir)
	})
}

// flatConfigCompat captures the legacy on-disk layouts: both the
// pre-2026-05-07 flat shape and the brief 2026-05-07 single-wallet
// nested shape. We unmarshal the file twice — once into the canonical
// Config, once into this — and fold legacy fields into the new
// multi-wallet shape if the canonical side is empty.
type flatConfigCompat struct {
	// pre-2026-05-07 fully flat
	BaseURL          string `json:"base_url,omitempty"`
	AdminKey         string `json:"admin_key,omitempty"`
	InvoiceKey       string `json:"invoice_key,omitempty"`
	WalletID         string `json:"wallet_id,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	AdminBearerToken string `json:"admin_bearer_token,omitempty"`
	NodeEndpoint     string `json:"node_endpoint,omitempty"`
	NodeTLSCertPath  string `json:"node_tls_cert_path,omitempty"`
	NodeMacaroonPath string `json:"node_macaroon_path,omitempty"`

	// 2026-05-07 single-wallet nested — same field names as the new
	// HostedConfig but with admin/invoice/wallet at the top of `hosted`,
	// before we introduced the wallets map.
	Hosted struct {
		BaseURL          string `json:"base_url,omitempty"`
		AdminKey         string `json:"admin_key,omitempty"`
		InvoiceKey       string `json:"invoice_key,omitempty"`
		WalletID         string `json:"wallet_id,omitempty"`
		UserID           string `json:"user_id,omitempty"`
		AdminBearerToken string `json:"admin_bearer_token,omitempty"`
	} `json:"hosted,omitempty"`
}

func (l *flatConfigCompat) foldInto(c *Config) {
	// Hosted top-level fields (account-level).
	if c.Hosted.BaseURL == "" {
		c.Hosted.BaseURL = pickFirst(l.Hosted.BaseURL, l.BaseURL)
	}
	if c.Hosted.UserID == "" {
		c.Hosted.UserID = pickFirst(l.Hosted.UserID, l.UserID)
	}
	if c.Hosted.AdminBearerToken == "" {
		c.Hosted.AdminBearerToken = pickFirst(l.Hosted.AdminBearerToken, l.AdminBearerToken)
	}

	// Per-wallet legacy fields → fold into a single "default" wallet
	// under the new map, if the new map didn't already have entries.
	if len(c.Hosted.Wallets) == 0 {
		legacyWallet := WalletEntry{
			WalletID:   pickFirst(l.Hosted.WalletID, l.WalletID),
			AdminKey:   pickFirst(l.Hosted.AdminKey, l.AdminKey),
			InvoiceKey: pickFirst(l.Hosted.InvoiceKey, l.InvoiceKey),
		}
		if legacyWallet.AdminKey != "" || legacyWallet.InvoiceKey != "" || legacyWallet.WalletID != "" {
			c.Hosted.PutWallet("default", legacyWallet)
		}
	}

	// Node fields (top-level legacy only — pre 2026-05-07 nesting was
	// single-block, 2026-05-07 already used the nested NodeConfig).
	if c.Node.Endpoint == "" {
		c.Node.Endpoint = l.NodeEndpoint
	}
	if c.Node.TLSCertPath == "" {
		c.Node.TLSCertPath = l.NodeTLSCertPath
	}
	if c.Node.MacaroonPath == "" {
		c.Node.MacaroonPath = l.NodeMacaroonPath
	}
}

func pickFirst(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	var legacy flatConfigCompat
	_ = json.Unmarshal(b, &legacy)
	legacy.foldInto(&c)
	return &c, nil
}

func saveConfig(c *Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// hostedClientFromConfig builds an SDK Client for the hosted route using
// the resolved active wallet's keys. preferAdmin selects the admin key
// when both admin + invoice are available (needed for spending).
//
// `walletOverride` is the value of the global --wallet flag (empty for
// "use the active one"). Returns an error if the active route is not
// "hosted" or if no wallet matches.
func hostedClientFromConfig(cfg *Config, baseURLOverride string, insecureOverride bool, preferAdmin bool, walletOverride string) (*sdk.Client, string, error) {
	if cfg.EffectiveRoute() != RouteHosted {
		return nil, "", fmt.Errorf("active route is %q, this command requires --route hosted", cfg.EffectiveRoute())
	}
	baseURL := cfg.Hosted.BaseURL
	if baseURLOverride != "" {
		baseURL = baseURLOverride
	}
	if baseURL == "" {
		baseURL = sdk.DefaultBaseURL
	}

	name, w, err := cfg.Hosted.ResolveWallet(walletOverride)
	if err != nil {
		return nil, "", err
	}

	opts := []sdk.Option{}
	if insecureOverride || cfg.Insecure {
		opts = append(opts, sdk.WithInsecureTLS())
	}
	switch {
	case preferAdmin && w.AdminKey != "":
		opts = append(opts, sdk.WithAdminKey(w.AdminKey))
	case w.InvoiceKey != "":
		opts = append(opts, sdk.WithInvoiceKey(w.InvoiceKey))
	case w.AdminKey != "":
		opts = append(opts, sdk.WithAdminKey(w.AdminKey))
	}

	return sdk.New(baseURL, opts...), name, nil
}

// nodeClientFromConfig builds an SDK NodeClient for the node route.
func nodeClientFromConfig(cfg *Config, endpointOverride string, insecureOverride bool) (*sdk.NodeClient, error) {
	if cfg.EffectiveRoute() != RouteNode {
		return nil, fmt.Errorf("active route is %q, this command requires --route node", cfg.EffectiveRoute())
	}
	endpoint := cfg.Node.Endpoint
	if endpointOverride != "" {
		endpoint = endpointOverride
	}
	if endpoint == "" {
		return nil, errors.New("node endpoint not configured (run `lokapay login --route node --lnd-endpoint ...`)")
	}
	if cfg.Node.MacaroonPath == "" {
		return nil, errors.New("node macaroon path not configured")
	}

	opts := []sdk.NodeOption{
		sdk.WithNodeMacaroonFile(cfg.Node.MacaroonPath),
	}
	switch {
	case insecureOverride || cfg.Insecure:
		opts = append(opts, sdk.WithNodeInsecureTLS())
	case cfg.Node.TLSCertPath != "":
		opts = append(opts, sdk.WithNodeTLSCertFile(cfg.Node.TLSCertPath))
	}
	return sdk.NewNodeClient(endpoint, opts...)
}

// walletForCurrentRoute returns whichever client implements sdk.Wallet for
// the active route, using the resolved active wallet on the hosted side.
func walletForCurrentRoute(cfg *Config, baseURLOverride, endpointOverride string, insecureOverride bool, walletOverride string) (sdk.Wallet, error) {
	switch cfg.EffectiveRoute() {
	case RouteHosted:
		cl, _, err := hostedClientFromConfig(cfg, baseURLOverride, insecureOverride, true, walletOverride)
		if err != nil {
			return nil, err
		}
		if cl.KeyType != sdk.KeyAdmin {
			return nil, errors.New("admin key required to auto-pay L402 challenges (run `lokapay wallets use <name>` to switch to an admin-key-bearing wallet)")
		}
		return cl, nil
	case RouteNode:
		return nodeClientFromConfig(cfg, endpointOverride, insecureOverride)
	default:
		return nil, fmt.Errorf("unsupported route %q", cfg.EffectiveRoute())
	}
}
