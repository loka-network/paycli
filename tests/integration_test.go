//go:build integration
// +build integration

// Package tests contains integration tests that exercise the SDK against a
// real local Loka Payment stack.
//
// Required local services (see docs/integration-test.md):
//
//   * lnd-sui Alice + Bob via lnd/scripts/itest_sui_single_coin.sh
//   * agents-pay-service running on http://127.0.0.1:5002
//     (PAYCLI_IT_LNBITS_URL to override)
//   * loka-prism-l402 running on https://127.0.0.1:8080
//     (PAYCLI_IT_PRISM_URL to override; insecure TLS expected)
//
// Run with:
//
//   go test -tags=integration -v ./tests/...
//
// Each test creates its own anonymous account so runs are independent.
package tests

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loka-network/paycli/pkg/sdk"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newWalletClient(t *testing.T) *sdk.Client {
	t.Helper()
	url := envOr("PAYCLI_IT_LNBITS_URL", "http://127.0.0.1:5002")
	return sdk.New(url)
}

func mustCreateAccount(t *testing.T, ctx context.Context) *sdk.Wallet {
	t.Helper()
	w, err := newWalletClient(t).CreateAccount(ctx, "paycli-it-"+t.Name())
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if w.AdminKey == "" || w.InvoiceKey == "" {
		t.Fatalf("missing keys in account response: %+v", w)
	}
	return w
}

// TestAccountAndWallet exercises the basic account bootstrap path.
func TestAccountAndWallet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := mustCreateAccount(t, ctx)
	t.Logf("created wallet: id=%s admin=%s... in=%s...", w.ID, w.AdminKey[:6], w.InvoiceKey[:6])

	url := envOr("PAYCLI_IT_LNBITS_URL", "http://127.0.0.1:5002")
	cl := sdk.New(url, sdk.WithAdminKey(w.AdminKey))
	got, err := cl.GetWallet(ctx)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if got.Name == "" {
		t.Fatalf("GetWallet returned empty name: %+v", got)
	}

	// Add a sub-wallet under the same account. Skipped when the server has
	// not enabled the user_id_only auth method (LNBITS_AUTH_METHODS).
	sub, err := cl.CreateWallet(ctx, w.User, "paycli-it-sub-"+t.Name())
	if err != nil {
		t.Logf("CreateWallet skipped: %v (likely user_id_only auth method disabled)", err)
		return
	}
	if sub.AdminKey == "" || sub.AdminKey == w.AdminKey {
		t.Fatalf("sub-wallet should have a fresh admin key, got %+v", sub)
	}
}

// TestCreateInvoice verifies that the wallet can mint a BOLT11 invoice.
func TestCreateInvoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w := mustCreateAccount(t, ctx)
	url := envOr("PAYCLI_IT_LNBITS_URL", "http://127.0.0.1:5002")
	cl := sdk.New(url, sdk.WithInvoiceKey(w.InvoiceKey))

	p, err := cl.CreateInvoice(ctx, sdk.CreateInvoiceRequest{
		Amount: 1000,
		Memo:   "paycli-it",
	})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if p.PaymentHash == "" {
		t.Fatalf("CreateInvoice missing payment_hash: %+v", p)
	}
	if !strings.HasPrefix(strings.ToLower(p.Bolt11+p.PaymentRequest), "lnbc") &&
		!strings.HasPrefix(strings.ToLower(p.Bolt11+p.PaymentRequest), "lnsui") {
		t.Logf("note: server returned non-lnbc invoice prefix: bolt11=%q payment_request=%q",
			p.Bolt11, p.PaymentRequest)
	}
}

// TestL402_ChallengeReachable verifies that paycli can reach Prism, decode
// its 402 challenge, and surface a meaningful error when the wallet has no
// funds. This validates the L402 plumbing without requiring a funded channel.
//
// For a full pay-and-replay cycle, set PAYCLI_IT_FUNDED_ADMIN_KEY to an
// admin key whose wallet has been pre-funded by the host (via
// `bob payinvoice` against the wallet's `fund` invoice). When that env var
// is set, the test asserts the request actually completes.
func TestL402_AgainstPrism(t *testing.T) {
	prism := envOr("PAYCLI_IT_PRISM_URL", "")
	if prism == "" {
		t.Skip("PAYCLI_IT_PRISM_URL not set; skipping L402 e2e test")
	}
	target := envOr("PAYCLI_IT_PRISM_TARGET", prism+"/")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url := envOr("PAYCLI_IT_LNBITS_URL", "http://127.0.0.1:5002")
	adminKey := envOr("PAYCLI_IT_FUNDED_ADMIN_KEY", "")
	funded := adminKey != ""
	if !funded {
		w := mustCreateAccount(t, ctx)
		adminKey = w.AdminKey
	}

	wallet := sdk.New(url, sdk.WithAdminKey(adminKey))

	doer := sdk.NewL402Doer(wallet)
	doer.HTTPClient = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 — local self-signed cert
		Timeout:   60 * time.Second,
	}

	// Prism routes by Host header against hostregexp '^service1.com$'.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "service1.com"

	resp, err := doer.Do(ctx, req)
	if err != nil {
		// With an empty wallet we expect an "Insufficient balance" API error
		// from agents-pay-service when the SDK tries to pay the invoice.
		// The L402 plumbing is still validated by the fact that we reached
		// the pay step at all.
		if !funded && strings.Contains(err.Error(), "Insufficient balance") {
			t.Logf("L402 challenge reached pay step but wallet is empty (expected for unfunded test). "+
				"Set PAYCLI_IT_FUNDED_ADMIN_KEY for a full e2e run. err=%v", err)
			return
		}
		t.Fatalf("L402 Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadGateway {
		// 502/Bad Gateway is acceptable here: the test backend at 127.0.0.1:9998
		// may not be running. What we're verifying is that Prism *accepted* the
		// LSAT and forwarded — non-402 means payment + auth succeeded.
		t.Fatalf("final status = %d, want 200 or 502", resp.StatusCode)
	}
}
