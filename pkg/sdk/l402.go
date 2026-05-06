package sdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
)

// L402 challenge format produced by Prism / aperture:
//
//   WWW-Authenticate: LSAT macaroon="<base64>", invoice="lnbc..."
//
// Some servers also emit the newer "L402" scheme with the same shape. After
// paying the invoice and obtaining a preimage, the client retries with:
//
//   Authorization: LSAT <macBase64>:<preimageHex>
//
// challengeRe matches both LSAT and L402 schemes and tolerates either order
// of the macaroon/invoice attributes.
var (
	challengeReMacFirst = regexp.MustCompile(`(?i)(LSAT|L402)\s+macaroon="([^"]+)",\s*invoice="([^"]+)"`)
	challengeReInvFirst = regexp.MustCompile(`(?i)(LSAT|L402)\s+invoice="([^"]+)",\s*macaroon="([^"]+)"`)
)

// Challenge captures the parsed pieces of a 402 LSAT/L402 challenge.
type Challenge struct {
	Scheme   string // "LSAT" or "L402"
	Macaroon string // base64
	Invoice  string // bolt11
}

// AuthHeader builds the Authorization header value for retrying the request
// once the invoice has been paid and a preimage is in hand.
func (ch *Challenge) AuthHeader(preimageHex string) string {
	// Prefer the legacy LSAT scheme on the wire — Prism's own client sends
	// LSAT first for backwards compatibility with older aperture deployments.
	return fmt.Sprintf("LSAT %s:%s", ch.Macaroon, preimageHex)
}

// ParseChallenge extracts the first LSAT/L402 challenge from response headers.
// Returns ErrInvalidChallenge if none of the WWW-Authenticate values match.
func ParseChallenge(h http.Header) (*Challenge, error) {
	for _, v := range h.Values("WWW-Authenticate") {
		if m := challengeReMacFirst.FindStringSubmatch(v); len(m) == 4 {
			return &Challenge{Scheme: m[1], Macaroon: m[2], Invoice: m[3]}, nil
		}
		if m := challengeReInvFirst.FindStringSubmatch(v); len(m) == 4 {
			return &Challenge{Scheme: m[1], Macaroon: m[3], Invoice: m[2]}, nil
		}
	}
	return nil, ErrInvalidChallenge
}

// TokenCache memoizes paid (macaroon, preimage) tuples keyed by request
// origin so callers don't pay twice for the same logical resource. The zero
// value is ready to use.
type TokenCache struct {
	mu     sync.RWMutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	macaroon string
	preimage string
}

// NewTokenCache returns an initialized cache.
func NewTokenCache() *TokenCache {
	return &TokenCache{tokens: map[string]cachedToken{}}
}

// Get looks up a token by key. Returns ok=false if absent.
func (tc *TokenCache) Get(key string) (mac, preimage string, ok bool) {
	if tc == nil || tc.tokens == nil {
		return "", "", false
	}
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	t, ok := tc.tokens[key]
	return t.macaroon, t.preimage, ok
}

// Put stores a token. Replaces any prior entry for the same key.
func (tc *TokenCache) Put(key, mac, preimage string) {
	if tc == nil {
		return
	}
	tc.mu.Lock()
	if tc.tokens == nil {
		tc.tokens = map[string]cachedToken{}
	}
	tc.tokens[key] = cachedToken{macaroon: mac, preimage: preimage}
	tc.mu.Unlock()
}

// Delete removes a cached token. Useful when a 402 reply indicates the
// previously-cached token has expired.
func (tc *TokenCache) Delete(key string) {
	if tc == nil {
		return
	}
	tc.mu.Lock()
	delete(tc.tokens, key)
	tc.mu.Unlock()
}

// L402Doer wraps an http.Client with automatic 402-handling. If a request
// returns 402, Doer parses the LSAT challenge, asks the wallet to pay the
// invoice, and retries the request with the resulting Authorization header.
//
// Wallet is anything that can settle a BOLT11 invoice — the hosted Client
// (agents-pay-service) and NodeClient (lnd-sui REST) both satisfy it.
//
// MaxRetries bounds how many 402 cycles a single Do call may execute.
// Default is 1 (one challenge, one retry). Increase only if a service is
// known to rotate macaroons mid-call.
//
// Cache is optional. When set, Doer attaches a previously-paid LSAT to
// outgoing requests so paid endpoints don't trigger a new payment per call.
type L402Doer struct {
	HTTPClient *http.Client
	Wallet     Wallet
	Cache      *TokenCache
	MaxRetries int

	// CacheKey lets the caller customize how requests are de-duplicated in
	// the token cache. Defaults to scheme://host/path (path included so
	// distinct services on the same host get distinct entries).
	CacheKey func(*http.Request) string
}

// NewL402Doer returns a Doer wired to the given wallet. Pass either a
// hosted *Client or a *NodeClient — both satisfy the Wallet interface.
//
// The HTTPClient is left zero so the doer falls back to http.DefaultClient.
// Callers targeting local Prism with a self-signed cert should set their
// own HTTPClient with InsecureSkipVerify (see docs/sdk.md).
func NewL402Doer(wallet Wallet) *L402Doer {
	return &L402Doer{
		Wallet:     wallet,
		Cache:      NewTokenCache(),
		MaxRetries: 1,
	}
}

// Do executes req, transparently paying any L402 challenge it encounters.
// The body is buffered before the first send so retries can re-deliver it.
func (d *L402Doer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if d.Wallet == nil {
		return nil, errors.New("paycli: L402Doer requires a Wallet client")
	}
	if d.HTTPClient == nil {
		d.HTTPClient = http.DefaultClient
	}
	if d.MaxRetries < 1 {
		d.MaxRetries = 1
	}
	if req.Context() == nil || req.Context() == context.Background() {
		req = req.WithContext(ctx)
	}

	// Buffer the request body so we can replay it on retry. Streaming
	// bodies are not supported with L402 because a 402 always forces at
	// least one replay.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("paycli: read request body: %w", err)
		}
	}
	resetBody := func() {
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		}
	}

	cacheKey := d.cacheKey(req)
	if mac, preimage, ok := d.Cache.Get(cacheKey); ok {
		req.Header.Set("Authorization", fmt.Sprintf("LSAT %s:%s", mac, preimage))
	}

	resetBody()
	resp, err := d.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	for tries := 0; tries < d.MaxRetries && resp.StatusCode == http.StatusPaymentRequired; tries++ {
		ch, parseErr := ParseChallenge(resp.Header)
		if parseErr != nil {
			return resp, parseErr
		}
		// Drain & close the prior 402 body before issuing the retry.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		// Pay the invoice through the custodial wallet.
		paid, payErr := d.Wallet.PayInvoice(ctx, ch.Invoice)
		if payErr != nil {
			return nil, payErr
		}
		if !paid.IsSuccess() {
			return nil, fmt.Errorf("%w: status=%s", ErrPaymentFailed, paid.Status)
		}
		if paid.Preimage == "" {
			return nil, fmt.Errorf("%w: server returned empty preimage", ErrPaymentFailed)
		}

		// Cache and attach the new token, then replay the request.
		d.Cache.Put(cacheKey, ch.Macaroon, paid.Preimage)
		req.Header.Set("Authorization", ch.AuthHeader(paid.Preimage))
		resetBody()

		resp, err = d.HTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
	}

	if resp.StatusCode == http.StatusPaymentRequired {
		// Server still demands payment after MaxRetries. Surface this
		// distinctly so callers can decide whether to bump the budget.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, ErrChallengeExhausted
	}

	return resp, nil
}

func (d *L402Doer) cacheKey(req *http.Request) string {
	if d.CacheKey != nil {
		return d.CacheKey(req)
	}
	return req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
}
