package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/loka-network/paycli/pkg/sdk"
)

// Route enumerates the two custody models paycli supports today. They are
// mutually exclusive per config — a user picks one at `register` / `login`
// time and every subsequent command dispatches against it.
type Route string

const (
	RouteHosted Route = "hosted" // custodial: agents-pay-service over REST
	RouteNode   Route = "node"   // self-custody: the user's own lnd / lnd-sui REST gateway
)

// Config is what we persist at ~/.paycli/config.json. Fields are grouped by
// route — the irrelevant set is empty when the other route is active.
type Config struct {
	Route    Route  `json:"route,omitempty"` // "" → defaults to hosted
	Insecure bool   `json:"insecure_tls,omitempty"`

	// Hosted (agents-pay-service) ----------------------------------------
	BaseURL          string `json:"base_url,omitempty"`
	AdminKey         string `json:"admin_key,omitempty"`
	InKey            string `json:"invoice_key,omitempty"`
	WalletID         string `json:"wallet_id,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	// AdminBearerToken is the super-user / admin JWT cached by `auth-login`,
	// used only by `paycli topup`. Distinct from AdminKey (which is a
	// per-wallet X-Api-Key, not an account-level token).
	AdminBearerToken string `json:"admin_bearer_token,omitempty"`

	// Node (lnd-sui REST gateway) ---------------------------------------
	NodeEndpoint    string `json:"node_endpoint,omitempty"`     // https://127.0.0.1:8081
	NodeTLSCertPath string `json:"node_tls_cert_path,omitempty"`
	NodeMacaroonPath string `json:"node_macaroon_path,omitempty"`
}

// EffectiveRoute resolves the route, defaulting to hosted when unset (so
// configs written by older paycli builds keep working).
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
	return filepath.Join(home, ".paycli", "config.json"), nil
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

// hostedClientFromConfig builds an SDK Client for the hosted route. preferAdmin
// selects the admin key when both are present (needed for spending operations).
//
// Returns an error if the active route is not "hosted".
func hostedClientFromConfig(cfg *Config, baseURLOverride string, insecureOverride bool, preferAdmin bool) (*sdk.Client, error) {
	if cfg.EffectiveRoute() != RouteHosted {
		return nil, fmt.Errorf("active route is %q, this command requires --route hosted", cfg.EffectiveRoute())
	}
	baseURL := cfg.BaseURL
	if baseURLOverride != "" {
		baseURL = baseURLOverride
	}
	if baseURL == "" {
		baseURL = sdk.DefaultBaseURL
	}

	opts := []sdk.Option{}
	if insecureOverride || cfg.Insecure {
		opts = append(opts, sdk.WithInsecureTLS())
	}
	switch {
	case preferAdmin && cfg.AdminKey != "":
		opts = append(opts, sdk.WithAdminKey(cfg.AdminKey))
	case cfg.InKey != "":
		opts = append(opts, sdk.WithInvoiceKey(cfg.InKey))
	case cfg.AdminKey != "":
		opts = append(opts, sdk.WithAdminKey(cfg.AdminKey))
	}

	return sdk.New(baseURL, opts...), nil
}

// nodeClientFromConfig builds an SDK NodeClient for the node route.
func nodeClientFromConfig(cfg *Config, endpointOverride string, insecureOverride bool) (*sdk.NodeClient, error) {
	if cfg.EffectiveRoute() != RouteNode {
		return nil, fmt.Errorf("active route is %q, this command requires --route node", cfg.EffectiveRoute())
	}
	endpoint := cfg.NodeEndpoint
	if endpointOverride != "" {
		endpoint = endpointOverride
	}
	if endpoint == "" {
		return nil, errors.New("node endpoint not configured (run `paycli login --route node --lnd-endpoint ...`)")
	}
	if cfg.NodeMacaroonPath == "" {
		return nil, errors.New("node macaroon path not configured")
	}

	opts := []sdk.NodeOption{
		sdk.WithNodeMacaroonFile(cfg.NodeMacaroonPath),
	}
	switch {
	case insecureOverride || cfg.Insecure:
		opts = append(opts, sdk.WithNodeInsecureTLS())
	case cfg.NodeTLSCertPath != "":
		opts = append(opts, sdk.WithNodeTLSCertFile(cfg.NodeTLSCertPath))
	}
	return sdk.NewNodeClient(endpoint, opts...)
}

// walletForCurrentRoute returns whichever client implements sdk.Wallet for
// the active route. Used by the `request` command so the L402 doer doesn't
// care which backend is in play.
func walletForCurrentRoute(cfg *Config, baseURLOverride, endpointOverride string, insecureOverride bool) (sdk.Wallet, error) {
	switch cfg.EffectiveRoute() {
	case RouteHosted:
		cl, err := hostedClientFromConfig(cfg, baseURLOverride, insecureOverride, true)
		if err != nil {
			return nil, err
		}
		if cl.KeyType != sdk.KeyAdmin {
			return nil, errors.New("admin key required to auto-pay L402 challenges (run `paycli login --admin-key ...`)")
		}
		return cl, nil
	case RouteNode:
		return nodeClientFromConfig(cfg, endpointOverride, insecureOverride)
	default:
		return nil, fmt.Errorf("unsupported route %q", cfg.EffectiveRoute())
	}
}
