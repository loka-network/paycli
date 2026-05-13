package sdk

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultPrismURL is the hosted Loka Prism gateway. Override per
// NewPrismClient call (or via paycli --prism-url) for self-hosted setups.
const DefaultPrismURL = "https://prism.loka.cash"

// PrismClient calls Prism's admin gRPC-gateway REST API. Auth is the same
// shape as lnd: hex-encoded macaroon in the Grpc-Metadata-Macaroon header.
//
// The relevant endpoint for paycli is GET /api/admin/services — the
// service catalog, which Prism exposes without authentication so end-user
// clients can render a service picker. Other admin operations
// (CreateService, RevokeToken, stats) remain macaroon-gated.
type PrismClient struct {
	Endpoint    string
	HTTPClient  *http.Client
	MacaroonHex string
}

// PrismOption configures a PrismClient.
type PrismOption func(*prismOpts) error

type prismOpts struct {
	insecure bool
	macHex   string
	timeout  time.Duration
}

// WithPrismInsecureTLS skips cert verification. For local dev only.
func WithPrismInsecureTLS() PrismOption {
	return func(o *prismOpts) error { o.insecure = true; return nil }
}

// WithPrismMacaroonFile reads the admin macaroon from disk and hex-encodes it.
func WithPrismMacaroonFile(path string) PrismOption {
	return func(o *prismOpts) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("paycli: read prism macaroon %s: %w", path, err)
		}
		o.macHex = hex.EncodeToString(b)
		return nil
	}
}

// WithPrismMacaroonHex sets the macaroon directly as hex.
func WithPrismMacaroonHex(macHex string) PrismOption {
	return func(o *prismOpts) error { o.macHex = strings.TrimSpace(macHex); return nil }
}

// WithPrismTimeout overrides the 30s default HTTP timeout.
func WithPrismTimeout(d time.Duration) PrismOption {
	return func(o *prismOpts) error { o.timeout = d; return nil }
}

// NewPrismClient builds a client targeting the given Prism admin endpoint
// (typically the same host:port as the proxy itself, e.g. https://127.0.0.1:8080).
// An empty endpoint falls back to DefaultPrismURL.
//
// A macaroon is *optional* — callers that only invoke unauthenticated read
// methods (currently ListServices, GetHealth) can leave it unset. Methods
// that need admin auth will fail on the server with Unauthenticated; the
// SDK doesn't second-guess which method needs what.
func NewPrismClient(endpoint string, opts ...PrismOption) (*PrismClient, error) {
	if endpoint == "" {
		endpoint = DefaultPrismURL
	}
	cfg := &prismOpts{timeout: 30 * time.Second}
	for _, o := range opts {
		if err := o(cfg); err != nil {
			return nil, err
		}
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.insecure {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 — opt-in for local testing
	}
	return &PrismClient{
		Endpoint:    strings.TrimRight(endpoint, "/"),
		MacaroonHex: cfg.macHex,
		HTTPClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			Timeout:   cfg.timeout,
		},
	}, nil
}

// PrismService mirrors adminrpc.Service. PriceSat is the per-call cost in
// the chain's base unit (sats for BTC, MIST for SUI).
type PrismService struct {
	Name       string              `json:"name"`
	Address    string              `json:"address"`
	Protocol   string              `json:"protocol"`
	HostRegexp string              `json:"host_regexp"`
	PathRegexp string              `json:"path_regexp"`
	Price      string              `json:"price"`       // int64 as decimal string
	Auth       string              `json:"auth"`        // "on" / "off" / "" (empty = on)
	AuthScheme string              `json:"auth_scheme"` // AUTH_SCHEME_L402 / _MPP / _L402_MPP
	Payment    *PrismPaymentBackend `json:"payment,omitempty"`
}

// PrismPaymentBackend records the per-merchant lnd override (multi-merchant mode).
type PrismPaymentBackend struct {
	LndHost string `json:"lnd_host"`
	TLSPath string `json:"tls_path"`
	MacPath string `json:"mac_path"`
}

type prismListServicesResp struct {
	Services []PrismService `json:"services"`
}

// ListServices fetches the service catalog. The endpoint is unauthenticated
// on the Prism side (see admin/auth.go's unauthenticatedMethods); the SDK
// still forwards a macaroon when one is configured, in case a deployment
// re-enables auth or future Prism versions add per-service ACLs.
func (p *PrismClient) ListServices(ctx context.Context) ([]PrismService, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Endpoint+"/api/admin/services", nil)
	if err != nil {
		return nil, fmt.Errorf("paycli: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if p.MacaroonHex != "" {
		req.Header.Set("Grpc-Metadata-Macaroon", p.MacaroonHex)
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paycli: http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("paycli: read body: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Body: string(body)}
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &msg)
		apiErr.Detail = msg.Message
		return nil, apiErr
	}
	var out prismListServicesResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("paycli: decode: %w", err)
	}
	return out.Services, nil
}
