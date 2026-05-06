package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/loka-network/paycli/pkg/sdk"
)

// Config is what we persist at ~/.paycli/config.json. The schema is intentionally
// flat because there is currently only one supported route (custodial). When a
// route A (self-custody / lncli) integration is added, this file will gain a
// routes object and a default_route selector — see docs/cli.md.
type Config struct {
	BaseURL  string `json:"base_url"`
	AdminKey string `json:"admin_key,omitempty"`
	InKey    string `json:"invoice_key,omitempty"`
	WalletID string `json:"wallet_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Insecure bool   `json:"insecure_tls,omitempty"`
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

// clientFromConfig builds an SDK client from the persisted config + CLI flag
// overrides. preferAdmin=true selects the admin key when both are present
// (needed for spending operations).
func clientFromConfig(cfg *Config, baseURLOverride string, insecureOverride bool, preferAdmin bool) (*sdk.Client, error) {
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
