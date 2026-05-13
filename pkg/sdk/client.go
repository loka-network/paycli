// Package sdk is a Go client for the Loka Payment custodial wallet
// (agents-pay-service / LNbits fork) plus an L402 helper for paying HTTP 402
// challenges issued by Prism / aperture-style gateways.
//
// The SDK is intentionally thin: one HTTP client, plain types, and a small
// L402 state machine. It has no opinions about config storage or CLI UX —
// those live in cmd/lokapay on top of this package.
package sdk

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the hosted Loka Payment endpoint. Override per Client
// for self-hosted deployments.
const DefaultBaseURL = "https://agents-pay.loka.cash"

// KeyType identifies which API key the client is using. Most read-only and
// receive-only operations work with an invoice key; paying invoices and
// creating wallets require an admin key.
type KeyType int

const (
	// KeyUnknown means no key is set (account creation flow).
	KeyUnknown KeyType = iota
	// KeyInvoice gives read + receive permissions.
	KeyInvoice
	// KeyAdmin gives full spend authority over the wallet.
	KeyAdmin
)

// Client is a thread-safe REST client for agents-pay-service.
type Client struct {
	BaseURL    string
	APIKey     string
	KeyType    KeyType
	HTTPClient *http.Client
	UserAgent  string
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithAdminKey sets an admin-scoped X-Api-Key on the client.
func WithAdminKey(k string) Option {
	return func(c *Client) {
		c.APIKey = k
		c.KeyType = KeyAdmin
	}
}

// WithInvoiceKey sets an invoice-scoped X-Api-Key on the client.
func WithInvoiceKey(k string) Option {
	return func(c *Client) {
		c.APIKey = k
		c.KeyType = KeyInvoice
	}
}

// WithHTTPClient swaps the underlying http.Client. Useful for tests or for
// disabling TLS verification when targeting local Prism deployments with
// self-signed certs.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.HTTPClient = h }
}

// WithInsecureTLS disables TLS verification on the underlying client. Only
// safe for local integration testing — never enable against production.
func WithInsecureTLS() Option {
	return func(c *Client) {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 — opt-in for local testing
		}
		c.HTTPClient = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
}

// WithUserAgent customizes the User-Agent header sent on every request.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.UserAgent = ua }
}

// LoginWithPassword exchanges (username, password) for a JWT bearer token via
// POST /api/v1/auth. The returned token authenticates super-user / admin
// operations like topup that aren't available behind X-Api-Key.
//
// On success, the function does NOT persist the token on the Client — that's
// the caller's responsibility (the CLI saves it to config). Use BearerToken
// to inject it on subsequent requests.
func LoginWithPassword(ctx context.Context, baseURL, username, password string, opts ...Option) (string, error) {
	c := New(baseURL, opts...)
	body := map[string]string{"username": username, "password": password}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth", body, &out, false); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("lokapay: auth: empty access_token")
	}
	return out.AccessToken, nil
}

// RegisterAccount creates a NAMED user account on agents-pay-service via
// POST /api/v1/auth/register. Unlike CreateAccount (the anonymous fast path
// at POST /api/v1/account), this route persists a username + bcrypt
// password hash, so the resulting account can also log into the lnbits
// dashboard or call back via LoginWithPassword later.
//
// email is optional ("" allowed). On success the server returns the JWT
// for the just-created session. lnbits auto-creates a default wallet for
// the new user — fetch its keys with ListWalletsByBearer.
//
// Server preconditions:
//   - settings.lnbits_auth_methods must include "username-password"
//     (LNBITS_AUTH_METHODS=...,username-password). Otherwise → 403.
//   - the username must not already exist (case-insensitive on the
//     unique index).
func RegisterAccount(ctx context.Context, baseURL, username, password, email string, opts ...Option) (string, error) {
	c := New(baseURL, opts...)
	body := map[string]string{
		"username":        username,
		"password":        password,
		"password_repeat": password,
	}
	if email != "" {
		body["email"] = email
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/register", body, &out, false); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("lokapay: register: empty access_token")
	}
	return out.AccessToken, nil
}

// ListWalletsByBearer returns every wallet the bearer JWT has access to
// via GET /api/v1/wallets. Used after RegisterAccount to grab the
// auto-created default wallet's admin/invoice keys without needing any
// X-Api-Key (the JWT auths via the user-existence dependency).
func ListWalletsByBearer(ctx context.Context, baseURL, bearerToken string, opts ...Option) ([]HostedWallet, error) {
	c := New(baseURL, opts...)
	var out []HostedWallet
	if err := c.doWithBearer(ctx, http.MethodGet, "/api/v1/wallets", nil, &out, bearerToken); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateWalletByBearer adds a sub-wallet under the account identified by
// the bearer JWT via POST /api/v1/wallet (the route's auth dependency
// is check_account_id_exists, which honors Authorization: Bearer in
// addition to the ?usr= query param).
//
// This is the route lokapay uses to provision per-agent wallets after
// `register --username`: the JWT is what the user already has cached,
// and Bearer works regardless of whether the server has the
// user_id_only auth method enabled.
func CreateWalletByBearer(ctx context.Context, baseURL, bearerToken, name string, opts ...Option) (*HostedWallet, error) {
	c := New(baseURL, opts...)
	body := CreateWalletRequest{Name: name}
	var w HostedWallet
	if err := c.doWithBearer(ctx, http.MethodPost, "/api/v1/wallet", body, &w, bearerToken); err != nil {
		return nil, err
	}
	return &w, nil
}

// AdminPatchSettings PATCHes /admin/api/v1/settings with a partial dict
// of fields to update. Requires a super-user JWT (LoginWithPassword).
//
// Use this to flip server-level toggles like lnd_grpc_allow_self_payment
// without going through the dashboard. The body is merged on the server
// side — only the keys you pass are touched.
func AdminPatchSettings(ctx context.Context, baseURL, bearerToken string, partial map[string]interface{}, opts ...Option) error {
	c := New(baseURL, opts...)
	return c.doWithBearer(ctx, http.MethodPatch, "/admin/api/v1/settings", partial, nil, bearerToken)
}

// AdminCreditWallet credits (or debits, with negative amount) a wallet via
// PUT /users/api/v1/balance. Requires a super-user JWT — see
// LoginWithPassword.
//
// This is the "faucet" path on the agents-pay-service: it bypasses LN routing
// and synthesizes a successful internal payment on the target wallet, which
// is exactly how the dashboard "credit user" action works under the hood.
func AdminCreditWallet(ctx context.Context, baseURL, bearerToken, walletID string, amount int64, opts ...Option) error {
	c := New(baseURL, opts...)
	body := map[string]interface{}{"id": walletID, "amount": amount}
	return c.doWithBearer(ctx, http.MethodPut, "/users/api/v1/balance", body, nil, bearerToken)
}

// doWithBearer is a one-shot variant of do that sends Authorization: Bearer ...
// instead of X-Api-Key. Kept private so the package's public surface stays
// X-Api-Key-centric for the common (per-wallet) case.
func (c *Client) doWithBearer(ctx context.Context, method, path string, body, out interface{}, token string) error {
	saved := c.APIKey
	c.APIKey = ""
	defer func() { c.APIKey = saved }()

	rdr, err := jsonReader(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("lokapay: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("lokapay: http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("lokapay: read body: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Body: string(respBody)}
		var detail struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &detail)
		apiErr.Detail = detail.Detail
		return apiErr
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func jsonReader(body interface{}) (io.Reader, error) {
	if body == nil {
		return nil, nil
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("lokapay: marshal body: %w", err)
	}
	return bytes.NewReader(buf), nil
}

// New constructs a Client. baseURL may be empty to use DefaultBaseURL.
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		UserAgent:  "lokapay-sdk/0.1",
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// CreateAccount provisions a fresh anonymous user + first wallet.
// No API key is required for this call.
//
// POST /api/v1/account
func (c *Client) CreateAccount(ctx context.Context, name string) (*HostedWallet, error) {
	body := CreateAccountRequest{Name: name}
	var w HostedWallet
	if err := c.do(ctx, http.MethodPost, "/api/v1/account", body, &w, false); err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateWallet adds a sub-wallet under the account identified by userID.
// userID is the `user` field returned by CreateAccount (the LNbits account
// id, not a wallet id).
//
// LNbits' POST /api/v1/wallet does NOT accept X-Api-Key — it requires either
// a session JWT or, when the server has user_id_only auth enabled, a
// `?usr=<user_id>` query parameter. The SDK uses the latter because that's
// what's available to callers who only persisted the admin key + user id at
// account-creation time.
//
// POST /api/v1/wallet?usr={userID}
func (c *Client) CreateWallet(ctx context.Context, userID, name string) (*HostedWallet, error) {
	if userID == "" {
		return nil, fmt.Errorf("lokapay: CreateWallet requires userID")
	}
	body := CreateWalletRequest{Name: name}
	path := "/api/v1/wallet?usr=" + url.QueryEscape(userID)
	var w HostedWallet
	// withAuth=false: this endpoint does not consume X-Api-Key.
	if err := c.do(ctx, http.MethodPost, path, body, &w, false); err != nil {
		return nil, err
	}
	return &w, nil
}

// FiatRate is the response shape of GET /api/v1/rate/<currency>:
//
//	{ "rate":  993048659.38,   // native units per 1 fiat unit
//	                            //   MIST/USD on a SUI deployment,
//	                            //   sat/USD on a BTC deployment
//	  "price": 1.007 }          // fiat per 1 whole native unit (USD/SUI or USD/BTC)
//
// Whether the deployment is SUI or BTC isn't reported in the body —
// callers that need the chain label should infer from a fund/pay
// response's `extra.wallet_sui_rate` vs `extra.wallet_btc_rate`, or
// from the wallet's own context.
type FiatRate struct {
	Rate  float64 `json:"rate"`
	Price float64 `json:"price"`
}

// GetRate fetches the oracle's view for the given fiat currency.
//
// GET /api/v1/rate/{currency}
func (c *Client) GetRate(ctx context.Context, currency string) (*FiatRate, error) {
	var r FiatRate
	if err := c.do(ctx, http.MethodGet, "/api/v1/rate/"+url.PathEscape(currency), nil, &r, false); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetWallet returns the wallet referenced by the configured API key.
// Works with either invoice or admin key, but admin sees more fields.
//
// GET /api/v1/wallet
func (c *Client) GetWallet(ctx context.Context) (*HostedWalletStatus, error) {
	var w HostedWalletStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/wallet", nil, &w, true); err != nil {
		return nil, err
	}
	return &w, nil
}

// RenameWallet renames the wallet authenticated by the current admin key.
//
// PUT /api/v1/wallet/{new_name}
func (c *Client) RenameWallet(ctx context.Context, newName string) (*HostedWalletStatus, error) {
	if c.KeyType != KeyAdmin {
		return nil, ErrAdminKeyRequired
	}
	var w HostedWalletStatus
	if err := c.do(ctx, http.MethodPut, "/api/v1/wallet/"+url.PathEscape(newName), nil, &w, true); err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateInvoice generates a BOLT11 payment_request bound to the configured
// wallet. Either key type is accepted by the upstream API.
//
// POST /api/v1/payments  with Out=false
func (c *Client) CreateInvoice(ctx context.Context, req CreateInvoiceRequest) (*Payment, error) {
	req.Out = false
	if req.Unit == "" {
		req.Unit = "sat"
	}
	var p Payment
	if err := c.do(ctx, http.MethodPost, "/api/v1/payments", req, &p, true); err != nil {
		return nil, err
	}
	return &p, nil
}

// PayInvoice settles an external or internal BOLT11 invoice from the
// configured wallet. Requires an admin key.
//
// POST /api/v1/payments  with Out=true
func (c *Client) PayInvoice(ctx context.Context, bolt11 string) (*Payment, error) {
	if c.KeyType != KeyAdmin {
		return nil, ErrAdminKeyRequired
	}
	body := PayInvoiceRequest{Out: true, Bolt11: bolt11}
	var p Payment
	if err := c.do(ctx, http.MethodPost, "/api/v1/payments", body, &p, true); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPayments returns payments for the configured wallet. Pass 0 limit/offset
// to use server defaults.
//
// GET /api/v1/payments
func (c *Client) ListPayments(ctx context.Context, limit, offset int) ([]Payment, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/api/v1/payments"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var ps []Payment
	if err := c.do(ctx, http.MethodGet, path, nil, &ps, true); err != nil {
		return nil, err
	}
	return ps, nil
}

// GetPayment returns the status of a single payment by hash. Works without
// an API key (fewer fields), or with the wallet's key for full detail.
//
// GET /api/v1/payments/{payment_hash}
func (c *Client) GetPayment(ctx context.Context, paymentHash string) (map[string]interface{}, error) {
	var out map[string]interface{}
	auth := c.APIKey != ""
	if err := c.do(ctx, http.MethodGet, "/api/v1/payments/"+paymentHash, nil, &out, auth); err != nil {
		return nil, err
	}
	return out, nil
}

// do is the single HTTP entry point. It serializes JSON, sets headers,
// inspects the response, and unmarshals on success.
func (c *Client) do(ctx context.Context, method, path string, body, out interface{}, withAuth bool) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("lokapay: marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("lokapay: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if withAuth {
		if c.APIKey == "" {
			return ErrUnauthorized
		}
		req.Header.Set("X-Api-Key", c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("lokapay: http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("lokapay: read body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Body: string(respBody)}
		var detail struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &detail)
		apiErr.Detail = detail.Detail
		return apiErr
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("lokapay: decode response: %w", err)
	}
	return nil
}
