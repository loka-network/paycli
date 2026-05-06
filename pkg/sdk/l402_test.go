package sdk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubProtectedService responds 402 with an LSAT challenge on the first call
// and 200 once the request carries an Authorization header. It returns the
// total number of requests it observed so tests can assert retry counts.
type stubProtectedService struct {
	mac     string
	invoice string
	calls   int32
}

func (s *stubProtectedService) handler(t *testing.T, expectedAuth string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		auth := r.Header.Get("Authorization")
		if auth == "" {
			ch := `LSAT macaroon="` + s.mac + `", invoice="` + s.invoice + `"`
			w.Header().Set("WWW-Authenticate", ch)
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		if expectedAuth != "" && auth != expectedAuth {
			t.Errorf("Authorization = %q, want %q", auth, expectedAuth)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func TestParseChallenge_BothOrders(t *testing.T) {
	cases := []string{
		`LSAT macaroon="m1", invoice="inv1"`,
		`L402 macaroon="m1", invoice="inv1"`,
		`LSAT invoice="inv1", macaroon="m1"`,
	}
	for _, raw := range cases {
		h := http.Header{}
		h.Set("WWW-Authenticate", raw)
		ch, err := ParseChallenge(h)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if ch.Macaroon != "m1" || ch.Invoice != "inv1" {
			t.Fatalf("parse %q -> %+v", raw, ch)
		}
	}
}

func TestParseChallenge_Invalid(t *testing.T) {
	h := http.Header{}
	h.Set("WWW-Authenticate", "Basic realm=foo")
	_, err := ParseChallenge(h)
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("want ErrInvalidChallenge, got %v", err)
	}
}

func TestL402Doer_Auto402(t *testing.T) {
	const mac = "bWFjUGxhY2Vob2xkZXI="
	const invoice = "lnbc1...stub"
	const preimage = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	svc := &stubProtectedService{mac: mac, invoice: invoice}
	srv := httptest.NewServer(svc.handler(t, "LSAT "+mac+":"+preimage))
	defer srv.Close()

	// Fake wallet server: PayInvoice returns the canned preimage.
	walletSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/payments" {
			t.Errorf("wallet got %s, want /api/v1/payments", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"checking_id":"c","payment_hash":"h","status":"success","preimage":"`+preimage+`"}`)
	}))
	defer walletSrv.Close()

	wallet := New(walletSrv.URL, WithAdminKey("admin-key"))
	doer := NewL402Doer(wallet)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/protected", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doer.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt32(&svc.calls); got != 2 {
		t.Fatalf("service calls = %d, want 2 (challenge + retry)", got)
	}

	// A second call should reuse the cached token: only ONE service call.
	atomic.StoreInt32(&svc.calls, 0)
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/protected", nil)
	resp2, err := doer.Do(context.Background(), req2)
	if err != nil {
		t.Fatalf("second Do: %v", err)
	}
	resp2.Body.Close()
	if got := atomic.LoadInt32(&svc.calls); got != 1 {
		t.Fatalf("cached call count = %d, want 1", got)
	}
}

func TestL402Doer_RetryBudgetExhausted(t *testing.T) {
	// Service that always replies 402.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `LSAT macaroon="m", invoice="inv"`)
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()

	walletSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","preimage":"`+strings.Repeat("a", 64)+`"}`)
	}))
	defer walletSrv.Close()

	wallet := New(walletSrv.URL, WithAdminKey("k"))
	doer := NewL402Doer(wallet)
	doer.MaxRetries = 1

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := doer.Do(context.Background(), req)
	if !errors.Is(err, ErrChallengeExhausted) {
		t.Fatalf("want ErrChallengeExhausted, got %v", err)
	}
}
