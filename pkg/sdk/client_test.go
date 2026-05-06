package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer assembles a minimal stand-in for agents-pay-service that records
// the requests it sees and returns a canned response per route. Tests assemble
// one per scenario rather than sharing a global server.
type fakeServer struct {
	t        *testing.T
	gotPath  string
	gotKey   string
	gotBody  []byte
	gotMethod string
	respCode int
	respBody string
}

func (f *fakeServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath = r.URL.Path
		f.gotMethod = r.Method
		f.gotKey = r.Header.Get("X-Api-Key")
		f.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.respCode)
		_, _ = io.WriteString(w, f.respBody)
	})
}

func TestCreateAccount(t *testing.T) {
	f := &fakeServer{
		t:        t,
		respCode: http.StatusOK,
		respBody: `{"id":"w1","user":"u1","name":"main","adminkey":"AAA","inkey":"BBB","balance_msat":0}`,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL)
	w, err := c.CreateAccount(context.Background(), "main")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if w.AdminKey != "AAA" || w.InvoiceKey != "BBB" || w.ID != "w1" {
		t.Fatalf("unexpected wallet: %+v", w)
	}
	if f.gotPath != "/api/v1/account" {
		t.Fatalf("path = %q, want /api/v1/account", f.gotPath)
	}
	if f.gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", f.gotMethod)
	}
	if !strings.Contains(string(f.gotBody), `"name":"main"`) {
		t.Fatalf("body missing name: %s", f.gotBody)
	}
	if f.gotKey != "" {
		t.Fatalf("CreateAccount must not send X-Api-Key, got %q", f.gotKey)
	}
}

func TestPayInvoice_RequiresAdmin(t *testing.T) {
	c := New("http://x", WithInvoiceKey("inv-key"))
	_, err := c.PayInvoice(context.Background(), "lnbc1...")
	if !errors.Is(err, ErrAdminKeyRequired) {
		t.Fatalf("want ErrAdminKeyRequired, got %v", err)
	}
}

func TestPayInvoice_SendsAdminKey(t *testing.T) {
	f := &fakeServer{
		t:        t,
		respCode: http.StatusCreated,
		respBody: `{"checking_id":"c1","payment_hash":"ph","status":"success","preimage":"deadbeef"}`,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, WithAdminKey("admin-key"))
	p, err := c.PayInvoice(context.Background(), "lnbc1xyz")
	if err != nil {
		t.Fatalf("PayInvoice: %v", err)
	}
	if !p.IsSuccess() {
		t.Fatalf("expected success status: %+v", p)
	}
	if f.gotKey != "admin-key" {
		t.Fatalf("X-Api-Key = %q, want admin-key", f.gotKey)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(f.gotBody, &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if sent["out"] != true || sent["bolt11"] != "lnbc1xyz" {
		t.Fatalf("unexpected body: %+v", sent)
	}
}

func TestAPIError_PropagatesDetail(t *testing.T) {
	f := &fakeServer{
		t:        t,
		respCode: http.StatusBadRequest,
		respBody: `{"detail":"Missing BOLT11 invoice"}`,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, WithAdminKey("k"))
	_, err := c.PayInvoice(context.Background(), "lnbc1xyz")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T (%v)", err, err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d", apiErr.Status)
	}
	if !strings.Contains(apiErr.Detail, "Missing BOLT11") {
		t.Fatalf("detail = %q", apiErr.Detail)
	}
}

func TestUnauthorized(t *testing.T) {
	f := &fakeServer{t: t, respCode: http.StatusUnauthorized, respBody: `{}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, WithAdminKey("bad"))
	_, err := c.GetWallet(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}
