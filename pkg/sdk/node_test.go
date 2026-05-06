package sdk

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generateSelfSignedCert spins up a fresh self-signed cert/key pair and
// writes the cert as PEM to a tmp file so we can test WithNodeTLSCertFile.
// Mirrors the way lnd creates its dev cert.
func generateSelfSignedCert(t *testing.T) (certPEM []byte, certPath string, server *httptest.Server, configure func(http.Handler)) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.cert")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{derBytes}, PrivateKey: priv}},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	configure = func(h http.Handler) { mux.Handle("/", h) }
	return certPEM, certPath, srv, configure
}

func TestNodeClient_AddInvoice_HexConversion(t *testing.T) {
	// 32-byte mock r_hash so we can verify base64→hex round-trip.
	rawHash := []byte("01234567890123456789012345678901")
	rHashB64 := base64.StdEncoding.EncodeToString(rawHash)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Grpc-Metadata-macaroon"); got != "deadbeef" {
			t.Errorf("macaroon header = %q, want deadbeef", got)
		}
		if r.URL.Path != "/v1/invoices" || r.Method != http.MethodPost {
			t.Errorf("got %s %s, want POST /v1/invoices", r.Method, r.URL.Path)
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["value"] != float64(1000) {
			t.Errorf("value = %v, want 1000", body["value"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"r_hash":"`+rHashB64+`","payment_request":"lnbc1xyz","add_index":"7"}`)
	}))
	defer srv.Close()

	nc, err := NewNodeClient(srv.URL,
		WithNodeInsecureTLS(),
		WithNodeMacaroonHex("deadbeef"),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := nc.AddInvoice(context.Background(), 1000, "test", 0)
	if err != nil {
		t.Fatalf("AddInvoice: %v", err)
	}
	if resp.PaymentRequest != "lnbc1xyz" {
		t.Errorf("payment_request = %q", resp.PaymentRequest)
	}
	expectedHex := "3031323334353637383930313233343536373839303132333435363738393031"
	if got := resp.PaymentHashHex(); got != expectedHex {
		t.Errorf("PaymentHashHex = %q, want %q", got, expectedHex)
	}
}

func TestNodeClient_PayInvoice_Success(t *testing.T) {
	preimage := []byte("PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP") // 32 bytes
	preimageB64 := base64.StdEncoding.EncodeToString(preimage)
	expectedHex := "5050505050505050505050505050505050505050505050505050505050505050"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/channels/transactions" || r.Method != http.MethodPost {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"payment_error":"","payment_preimage":"`+preimageB64+`","payment_hash":""}`)
	}))
	defer srv.Close()

	nc, _ := NewNodeClient(srv.URL,
		WithNodeInsecureTLS(),
		WithNodeMacaroonHex("aa"),
	)

	// PayInvoice (Wallet interface implementation).
	p, err := nc.PayInvoice(context.Background(), "lnbc1xyz")
	if err != nil {
		t.Fatalf("PayInvoice: %v", err)
	}
	if !p.IsSuccess() {
		t.Fatalf("expected success, got status=%q", p.Status)
	}
	if p.Preimage != expectedHex {
		t.Errorf("preimage = %q, want %q", p.Preimage, expectedHex)
	}
}

func TestNodeClient_PayInvoice_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"payment_error":"unable to find a route to destination","payment_preimage":"","payment_hash":""}`)
	}))
	defer srv.Close()

	nc, _ := NewNodeClient(srv.URL, WithNodeInsecureTLS(), WithNodeMacaroonHex("aa"))

	_, err := nc.PayInvoice(context.Background(), "lnbc1xyz")
	if !errors.Is(err, ErrPaymentFailed) {
		t.Fatalf("want ErrPaymentFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "unable to find a route") {
		t.Errorf("error message lost: %v", err)
	}
}

func TestNodeClient_GrpcGatewayErrorOn200(t *testing.T) {
	// Some lnd-sui devnet states reply 200 OK with the gRPC error envelope
	// {"code": ..., "message": ..., "details": [...]}. NodeClient must
	// surface this as an APIError instead of decoding garbage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"code":2,"message":"chain backend not ready","details":[]}`)
	}))
	defer srv.Close()

	nc, _ := NewNodeClient(srv.URL, WithNodeInsecureTLS(), WithNodeMacaroonHex("aa"))
	_, err := nc.ChannelBalance(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T (%v)", err, err)
	}
	if !strings.Contains(apiErr.Detail, "chain backend not ready") {
		t.Errorf("detail = %q", apiErr.Detail)
	}
}

func TestNodeClient_TLSCertFile(t *testing.T) {
	certPEM, certPath, srv, configure := generateSelfSignedCert(t)
	configure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"identity_pubkey":"abc","alias":"test","num_active_channels":2}`)
	}))

	nc, err := NewNodeClient(srv.URL,
		WithNodeTLSCertFile(certPath),
		WithNodeMacaroonHex("aa"),
	)
	if err != nil {
		t.Fatal(err)
	}
	info, err := nc.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo via pinned cert: %v", err)
	}
	if info.IdentityPubkey != "abc" || info.NumActiveChannels != 2 {
		t.Errorf("unexpected info: %+v", info)
	}
	_ = certPEM
}

func TestBase64ToHex(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"AAA=", "0000"},
		{base64.StdEncoding.EncodeToString([]byte{0xde, 0xad, 0xbe, 0xef}), "deadbeef"},
		{base64.URLEncoding.EncodeToString([]byte{0xde, 0xad, 0xbe, 0xef}), "deadbeef"},
		{"!!!not-base64!!!", ""},
	}
	for _, c := range cases {
		if got := base64ToHex(c.in); got != c.want {
			t.Errorf("base64ToHex(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
