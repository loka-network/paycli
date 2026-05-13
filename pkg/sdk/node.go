package sdk

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// NodeClient talks directly to an lnd / lnd-sui node's grpc-gateway REST
// endpoint. This is "route A" — the user runs their own LN node and lokapay
// is a thin client over its REST surface.
//
// Auth follows lnd's standard convention:
//
//   - HTTPS with the node's auto-generated tls.cert (or skip verify for tests)
//   - Macaroon (admin or invoice-scoped) hex-encoded in the
//     Grpc-Metadata-macaroon header
//
// Only the small subset of REST endpoints lokapay needs is wrapped here.
// For everything else, fall through to the raw HTTP client.
type NodeClient struct {
	// Endpoint is the base URL of the node's REST listener,
	// e.g. https://127.0.0.1:8081 (matches --restlisten on lnd).
	Endpoint string

	// HTTPClient is preconfigured with TLS pinning (or insecure skip) and
	// reasonable timeouts. Created by NewNodeClient.
	HTTPClient *http.Client

	// MacaroonHex is the hex-encoded macaroon to send with every request.
	MacaroonHex string

	UserAgent string
}

// NodeOption configures a NodeClient at construction time.
type NodeOption func(*nodeOpts) error

type nodeOpts struct {
	tlsCertPath string
	tlsCertPEM  []byte
	insecure    bool
	macaroon    string
	timeout     time.Duration
	userAgent   string
}

// WithNodeTLSCertFile pins the TLS cert from a file on disk (default lnd
// places this at ~/.lnd/tls.cert, or in our integration setup
// /tmp/lnd-sui-test/{alice,bob}/tls.cert).
func WithNodeTLSCertFile(path string) NodeOption {
	return func(o *nodeOpts) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("lokapay: read tls cert %s: %w", path, err)
		}
		o.tlsCertPEM = b
		o.tlsCertPath = path
		return nil
	}
}

// WithNodeTLSCertPEM pins the TLS cert via in-memory PEM bytes.
func WithNodeTLSCertPEM(pem []byte) NodeOption {
	return func(o *nodeOpts) error {
		o.tlsCertPEM = pem
		return nil
	}
}

// WithNodeInsecureTLS skips TLS verification entirely. Local testing only.
func WithNodeInsecureTLS() NodeOption {
	return func(o *nodeOpts) error { o.insecure = true; return nil }
}

// WithNodeMacaroonFile reads a binary macaroon from disk and hex-encodes it
// for the Grpc-Metadata-macaroon header.
func WithNodeMacaroonFile(path string) NodeOption {
	return func(o *nodeOpts) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("lokapay: read macaroon %s: %w", path, err)
		}
		o.macaroon = hex.EncodeToString(b)
		return nil
	}
}

// WithNodeMacaroonHex sets the macaroon directly as a hex string. Useful when
// the value comes from an env var rather than a file.
func WithNodeMacaroonHex(macHex string) NodeOption {
	return func(o *nodeOpts) error {
		// Be permissive about whitespace / newlines that creep in via shell.
		o.macaroon = strings.TrimSpace(macHex)
		return nil
	}
}

// WithNodeTimeout overrides the default 30s HTTP timeout.
func WithNodeTimeout(d time.Duration) NodeOption {
	return func(o *nodeOpts) error { o.timeout = d; return nil }
}

// WithNodeUserAgent customizes the User-Agent header.
func WithNodeUserAgent(ua string) NodeOption {
	return func(o *nodeOpts) error { o.userAgent = ua; return nil }
}

// NewNodeClient builds a NodeClient. Endpoint should be a https://host:port
// URL pointing at the node's REST listener.
func NewNodeClient(endpoint string, opts ...NodeOption) (*NodeClient, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("lokapay: NewNodeClient: endpoint required")
	}

	cfg := &nodeOpts{timeout: 30 * time.Second, userAgent: "lokapay-sdk-node/0.1"}
	for _, o := range opts {
		if err := o(cfg); err != nil {
			return nil, err
		}
	}
	if cfg.macaroon == "" {
		return nil, fmt.Errorf("lokapay: NewNodeClient: macaroon is required (use WithNodeMacaroonFile or WithNodeMacaroonHex)")
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.insecure:
		tlsCfg.InsecureSkipVerify = true // #nosec G402 — opt-in for local testing
	case len(cfg.tlsCertPEM) > 0:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.tlsCertPEM) {
			return nil, fmt.Errorf("lokapay: tls cert from %s did not contain a valid PEM block", cfg.tlsCertPath)
		}
		tlsCfg.RootCAs = pool
	}

	return &NodeClient{
		Endpoint:    strings.TrimRight(endpoint, "/"),
		MacaroonHex: cfg.macaroon,
		UserAgent:   cfg.userAgent,
		HTTPClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			Timeout:   cfg.timeout,
		},
	}, nil
}

// --- Response types (only fields lokapay consumes) -------------------------

// NodeInfo is a slim subset of lnrpc.GetInfoResponse.
type NodeInfo struct {
	IdentityPubkey      string  `json:"identity_pubkey"`
	Alias               string  `json:"alias"`
	NumActiveChannels   int64   `json:"num_active_channels"`
	NumPendingChannels  int64   `json:"num_pending_channels"`
	BlockHeight         int64   `json:"block_height"`
	SyncedToChain       bool    `json:"synced_to_chain"`
	Version             string  `json:"version"`
	Chains              []Chain `json:"chains"`
}

// Chain mirrors lnrpc.Chain ({chain, network}).
type Chain struct {
	Chain   string `json:"chain"`
	Network string `json:"network"`
}

// NodeChannelBalance is a subset of lnrpc.ChannelBalanceResponse.
//
// lnd's grpc-gateway serializes int64 fields as decimal strings, so we
// keep the wire shape and convert in helper accessors.
type NodeChannelBalance struct {
	Balance       string `json:"balance"`
	LocalBalance  Amount `json:"local_balance"`
	RemoteBalance Amount `json:"remote_balance"`
}

// Amount mirrors lnrpc.Amount.
type Amount struct {
	Sat  string `json:"sat"`
	Msat string `json:"msat"`
}

// AsInt64 returns the parsed integer value of an amount string. Empty values
// decode as zero so callers don't have to guard.
func (a Amount) SatInt() int64  { return parseInt64(a.Sat) }
func (a Amount) MsatInt() int64 { return parseInt64(a.Msat) }

// NodeAddInvoiceResponse is the relevant subset of lnrpc.AddInvoiceResponse.
// `r_hash` and `payment_addr` come back as base64; `payment_request` is the
// BOLT11 string we hand to a payer.
type NodeAddInvoiceResponse struct {
	RHashB64       string `json:"r_hash"`
	PaymentRequest string `json:"payment_request"`
	AddIndex       string `json:"add_index"`
	PaymentAddrB64 string `json:"payment_addr"`
}

// PaymentHashHex returns the lowercase hex of r_hash.
func (r *NodeAddInvoiceResponse) PaymentHashHex() string {
	return base64ToHex(r.RHashB64)
}

// NodeSendPaymentResponse is the subset of lnrpc.SendResponse from
// /v1/channels/transactions (SendPaymentSync).
type NodeSendPaymentResponse struct {
	PaymentError      string `json:"payment_error"`
	PaymentPreimageB64 string `json:"payment_preimage"`
	PaymentHashB64    string `json:"payment_hash"`
}

// PreimageHex / PaymentHashHex return hex-encoded versions for callers
// (notably the L402 LSAT Authorization header).
func (r *NodeSendPaymentResponse) PreimageHex() string {
	return base64ToHex(r.PaymentPreimageB64)
}
func (r *NodeSendPaymentResponse) PaymentHashHex() string {
	return base64ToHex(r.PaymentHashB64)
}

// NodePayment is the subset of lnrpc.Payment used in ListPayments responses.
type NodePayment struct {
	PaymentHash    string `json:"payment_hash"` // already hex on this endpoint
	PaymentRequest string `json:"payment_request"`
	Status         string `json:"status"` // "SUCCEEDED" / "FAILED" / "IN_FLIGHT" / "INITIATED"
	ValueSat       string `json:"value_sat"`
	FeeSat         string `json:"fee_sat"`
	CreationTimeNS string `json:"creation_time_ns"`
	PaymentPreimage string `json:"payment_preimage"`
	FailureReason  string `json:"failure_reason"`
}

type nodeListPaymentsResp struct {
	Payments         []NodePayment `json:"payments"`
	FirstIndexOffset string        `json:"first_index_offset"`
	LastIndexOffset  string        `json:"last_index_offset"`
	TotalNumPayments string        `json:"total_num_payments"`
}

// --- Methods --------------------------------------------------------------

// GetInfo calls GET /v1/getinfo. Returns whatever lnd-sui populates; some
// fields may be null on an underdeveloped chain backend.
func (n *NodeClient) GetInfo(ctx context.Context) (*NodeInfo, error) {
	var out NodeInfo
	if err := n.do(ctx, http.MethodGet, "/v1/getinfo", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChannelBalance calls GET /v1/balance/channels.
func (n *NodeClient) ChannelBalance(ctx context.Context) (*NodeChannelBalance, error) {
	var out NodeChannelBalance
	if err := n.do(ctx, http.MethodGet, "/v1/balance/channels", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddInvoice calls POST /v1/invoices to mint a new BOLT11 invoice.
//
// `value` is in satoshis (or MIST under lnd-sui — same wire field, just
// different chain unit).
func (n *NodeClient) AddInvoice(ctx context.Context, value int64, memo string, expirySeconds int64) (*NodeAddInvoiceResponse, error) {
	body := map[string]interface{}{"value": value}
	if memo != "" {
		body["memo"] = memo
	}
	if expirySeconds > 0 {
		body["expiry"] = strconv.FormatInt(expirySeconds, 10)
	}
	var out NodeAddInvoiceResponse
	if err := n.do(ctx, http.MethodPost, "/v1/invoices", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendPaymentSync settles a BOLT11 invoice synchronously via
// POST /v1/channels/transactions.
//
// On success, PaymentPreimageB64 carries a 32-byte preimage in base64; on
// payment failure, PaymentError contains lnd's failure string and the
// other fields may be empty.
func (n *NodeClient) SendPaymentSync(ctx context.Context, bolt11 string) (*NodeSendPaymentResponse, error) {
	body := map[string]interface{}{"payment_request": bolt11}
	var out NodeSendPaymentResponse
	if err := n.do(ctx, http.MethodPost, "/v1/channels/transactions", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PayInvoice satisfies the Wallet interface so a NodeClient can drive an
// L402Doer just like the hosted Client.
//
// The returned Payment maps lnd's send response onto lokapay's common shape:
//   - Status="success" + Preimage=<hex> on a paid invoice
//   - Status="failed" + payment_error preserved in Extra on failure
func (n *NodeClient) PayInvoice(ctx context.Context, bolt11 string) (*Payment, error) {
	resp, err := n.SendPaymentSync(ctx, bolt11)
	if err != nil {
		return nil, err
	}
	p := &Payment{
		Bolt11: bolt11,
		Extra:  map[string]interface{}{},
	}
	if resp.PaymentError != "" {
		p.Status = "failed"
		p.Extra["payment_error"] = resp.PaymentError
		return p, fmt.Errorf("%w: %s", ErrPaymentFailed, resp.PaymentError)
	}
	preimageHex := resp.PreimageHex()
	if preimageHex == "" {
		p.Status = "failed"
		return p, fmt.Errorf("%w: empty preimage from node", ErrPaymentFailed)
	}
	p.Status = "success"
	p.Preimage = preimageHex
	p.PaymentHash = resp.PaymentHashHex()
	return p, nil
}

// ListPayments calls GET /v1/payments.
func (n *NodeClient) ListPayments(ctx context.Context, maxPayments int) ([]NodePayment, error) {
	q := url.Values{}
	if maxPayments > 0 {
		q.Set("max_payments", strconv.Itoa(maxPayments))
		q.Set("reversed", "true")
	}
	path := "/v1/payments"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out nodeListPaymentsResp
	if err := n.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Payments, nil
}

// --- Internals ------------------------------------------------------------

func (n *NodeClient) do(ctx context.Context, method, path string, body, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("lokapay: marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.Endpoint+path, rdr)
	if err != nil {
		return fmt.Errorf("lokapay: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", n.UserAgent)
	req.Header.Set("Grpc-Metadata-macaroon", n.MacaroonHex)

	resp, err := n.HTTPClient.Do(req)
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
		// lnd's grpc-gateway returns errors as
		//   {"code": <grpc>, "message": "...", "details": [...]}
		// Surface message into APIError.Detail when present.
		apiErr := &APIError{Status: resp.StatusCode, Body: string(respBody)}
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &msg)
		apiErr.Detail = msg.Message
		return apiErr
	}

	// Even on 200, lnd's REST gateway sometimes returns
	//   {"code":N,"message":"...","details":[...]}
	// for SendPaymentSync when the payment can't be routed. Detect and
	// surface so callers don't have to treat 200 + error JSON as success.
	var maybeErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &maybeErr); err == nil &&
		maybeErr.Code != 0 && maybeErr.Message != "" {
		// Distinguish from a real success that happens to have a
		// "message" field (none of the lnrpc responses lokapay wraps do).
		// The Code field on lnd error objects is non-zero (gRPC status code).
		return &APIError{Status: resp.StatusCode, Detail: maybeErr.Message, Body: string(respBody)}
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("lokapay: decode response: %w", err)
	}
	return nil
}

// base64ToHex converts standard-encoded base64 → lowercase hex. Used to
// translate lnd grpc-gateway binary fields onto the L402 wire format.
func base64ToHex(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// URL-safe fallback (some grpc-gateway versions emit this).
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
	}
	return hex.EncodeToString(b)
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
