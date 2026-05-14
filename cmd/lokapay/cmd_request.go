package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdRequest implements `lokapay request <url>` — a curl-style HTTP wrapper that
// transparently pays L402 challenges using whichever wallet backend the user
// has configured (hosted custodial, or their own lnd-sui node).
//
// The L402Doer abstracts over Wallet (interface), so the same code path
// handles both routes — only the spend backend differs.
//
// --debug instruments the doer with an http.RoundTripper wrapper +
// OnPaid trace handler so users can see the full L402 conversation:
// initial request → 402 challenge → wallet PayInvoice → preimage →
// LSAT retry → 200. Output goes to stderr so the response body on
// stdout stays scriptable.
func cmdRequest() *cli.Command {
	return &cli.Command{
		Name:      "request",
		Usage:     "HTTP request with automatic L402 (HTTP 402) payment handling",
		ArgsUsage: "<url>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "method", Aliases: []string{"X"}, Value: "GET"},
			&cli.StringSliceFlag{Name: "header", Aliases: []string{"H"}, Usage: "extra request headers"},
			&cli.StringFlag{Name: "data", Aliases: []string{"d"}, Usage: "request body"},
			&cli.BoolFlag{Name: "insecure-target", Usage: "skip TLS verification on the target URL (e.g. https://localhost:8080 with self-signed cert)"},
			&cli.IntFlag{Name: "max-retries", Value: 1, Usage: "max consecutive 402 cycles per call"},
			&cli.BoolFlag{Name: "include", Aliases: []string{"i"}, Usage: "also print response status + headers"},
			&cli.BoolFlag{Name: "debug", Usage: "visualize the L402 protocol exchange + payment flow to stderr (HTTP requests, 402 challenge, wallet PayInvoice, preimage, retry)"},
		},
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("target url is required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			wallet, err := walletForCurrentRoute(cfg, c.String("base-url"), "", c.Bool("insecure"), c.String("wallet"))
			if err != nil {
				return fail("request: %v", err)
			}
			// Resolve which wallet alias was selected (only meaningful on
			// hosted; node route has no alias). Used to tag the L402 event
			// and label the wallet in debug output.
			var walletAlias string
			if cfg.EffectiveRoute() == RouteHosted {
				walletAlias, _, _ = cfg.Hosted.ResolveWallet(c.String("wallet"))
			}

			doer := sdk.NewL402Doer(wallet)
			doer.MaxRetries = c.Int("max-retries")

			// Build the underlying http.Client. Order matters: insecure-target
			// goes on the base transport; --debug wraps that transport so
			// the debug tracer sees post-TLS plaintext requests.
			baseTransport := http.DefaultTransport
			if c.Bool("insecure-target") {
				baseTransport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 — opt-in for local testing
				}
			}

			var tracer *debugTracer
			if c.Bool("debug") {
				tracer = &debugTracer{
					base:        baseTransport,
					w:           os.Stderr,
					route:       string(cfg.EffectiveRoute()),
					walletAlias: walletAlias,
				}
				baseTransport = tracer
				tracer.banner(c.Args().First())
			}

			doer.HTTPClient = &http.Client{
				Transport: baseTransport,
				Timeout:   60 * time.Second,
			}

			// Audit each L402 settlement. Fires inside doer.Do once per
			// 402 → pay → retry cycle, after a preimage was returned.
			doer.OnPaid = func(r *http.Request, ch *sdk.Challenge, paid *sdk.Payment) {
				targetURL := r.URL.String()
				targetHost := r.Host
				if targetHost == "" {
					targetHost = r.URL.Host
				}
				if tracer != nil {
					tracer.paid(ch, paid)
				}
				LogEvent(Event{
					Event:       EventL402Paid,
					Route:       string(cfg.EffectiveRoute()),
					WalletAlias: walletAlias,
					PaymentHash: paid.PaymentHash,
					Preimage:    paid.Preimage,
					Status:      paid.Status,
					TargetURL:   targetURL,
					TargetHost:  targetHost,
					// Embed the bolt11 in payment_request so operators can
					// reconstruct the amount and routing detail externally.
					PaymentRequest: ch.Invoice,
				})
			}

			var body io.Reader
			if d := c.String("data"); d != "" {
				body = strings.NewReader(d)
			}
			req, err := http.NewRequestWithContext(c.Context, strings.ToUpper(c.String("method")), c.Args().First(), body)
			if err != nil {
				return fail("build request: %v", err)
			}
			for _, h := range c.StringSlice("header") {
				k, v, ok := strings.Cut(h, ":")
				if !ok {
					return fail("invalid header %q (want Key: Value)", h)
				}
				key := strings.TrimSpace(k)
				val := strings.TrimSpace(v)
				// Go's http package treats Host specially — Header.Set("Host",...)
				// is silently ignored; only req.Host is honored on the wire.
				if strings.EqualFold(key, "Host") {
					req.Host = val
					continue
				}
				req.Header.Set(key, val)
			}

			resp, err := doer.Do(c.Context, req)
			if err != nil {
				if tracer != nil {
					tracer.summary(false, err)
				}
				return fail("request: %v", err)
			}
			defer resp.Body.Close()

			if tracer != nil {
				tracer.summary(true, nil)
			}

			if c.Bool("include") {
				fmt.Fprintf(os.Stdout, "HTTP %s\n", resp.Status)
				_ = resp.Header.Write(os.Stdout)
				fmt.Fprintln(os.Stdout)
			}
			if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
				return fail("read response: %v", err)
			}
			return nil
		},
	}
}

// debugTracer is an http.RoundTripper that narrates an L402 transaction
// as a 3-step story for non-technical users:
//
//	1. Ask the merchant for the resource
//	2. Pay the merchant's invoice (only when step 1 returned 402)
//	3. Show the receipt back to the merchant
//
// Step markers stay ASCII (`1.` / `2.` / `3.`) instead of the
// circled-digit Unicode glyphs (①②③, U+2460..) because those are
// East-Asian-Width "Ambiguous" — some terminals render them at width
// 2, others at width 1, which mis-aligns everything that follows.
// ASCII numbers + a trailing period render identically everywhere.
//
// Protocol-level detail (macaroon bytes, full bolt11, content-type,
// content-length, exact authorization header) is hidden — those exist
// in the lnd / lnbits logs for true debugging. The trace shown here
// is meant to make the *flow* legible, not to be a wire dump.
//
// Plugs in front of any existing Transport so the L402Doer's HTTP
// behavior is unchanged — only the side effect of each round-trip is
// observed.
type debugTracer struct {
	base        http.RoundTripper
	w           io.Writer
	mu          sync.Mutex
	httpStep    int       // counts HTTP round-trips
	startTime   time.Time // first request fired; used for total-elapsed
	lastReqTime time.Time

	route       string // "hosted" / "node" — used in the wallet annotation
	walletAlias string // sub-wallet alias on hosted route; empty on node
}

func (t *debugTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.httpStep++
	step := t.httpStep
	t.lastReqTime = time.Now()
	if t.startTime.IsZero() {
		t.startTime = t.lastReqTime
	}
	t.mu.Unlock()

	// Header for this step. First HTTP call is step 1, any subsequent
	// call is step 3 (3 may repeat if --max-retries > 1 and the merchant
	// rotates challenges; each retry gets its own "3." stanza).
	if step == 1 {
		fmt.Fprintln(t.w, "  1. Asking the merchant for the resource …")
	} else {
		fmt.Fprintln(t.w, "  3. Showing the receipt back to the merchant …")
	}

	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(t.lastReqTime).Round(time.Millisecond)
	if err != nil {
		fmt.Fprintf(t.w, "     ✗ network error after %s: %v\n\n", elapsed, err)
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusPaymentRequired:
		fmt.Fprintf(t.w, "     ← merchant wants payment (402 Payment Required)  · %s\n\n", elapsed)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		fmt.Fprintf(t.w, "     ✓ unlocked: HTTP %s  · %s\n\n", resp.Status, elapsed)
	case resp.StatusCode >= 400:
		fmt.Fprintf(t.w, "     ✗ HTTP %s  · %s\n\n", resp.Status, elapsed)
	default:
		fmt.Fprintf(t.w, "     ← HTTP %s  · %s\n\n", resp.Status, elapsed)
	}
	return resp, nil
}

func (t *debugTracer) banner(targetURL string) {
	fmt.Fprintln(t.w, "─── L402 payment ──────────────────────────────────────────────")
	fmt.Fprintf(t.w, "  Resource: %s\n", targetURL)
	fmt.Fprintf(t.w, "  Wallet:   %s\n\n", t.walletDesc())
}

func (t *debugTracer) walletDesc() string {
	if t.walletAlias != "" {
		return fmt.Sprintf("%s route (sub-wallet %q)", t.route, t.walletAlias)
	}
	return t.route + " route"
}

// paid is called by L402Doer.OnPaid after a successful invoice settle.
// Sits between the 402 response and the LSAT-bearing retry, so it
// renders as step 2 between step 1 and step 3.
func (t *debugTracer) paid(ch *sdk.Challenge, paid *sdk.Payment) {
	chain := chainFromExtra(paid.Extra)
	fmt.Fprintln(t.w, "  2. Paying the merchant's invoice via your wallet …")
	if paid.Amount != 0 {
		amt := formatFriendlyAmount(paid.Amount, chain)
		if paid.Fee != 0 {
			fee := formatFriendlyAmount(paid.Fee, chain)
			fmt.Fprintf(t.w, "     ✓ paid %s  (fee %s)\n", amt, fee)
		} else {
			fmt.Fprintf(t.w, "     ✓ paid %s\n", amt)
		}
	} else {
		fmt.Fprintln(t.w, "     ✓ payment settled")
	}
	if paid.Preimage != "" {
		fmt.Fprintf(t.w, "     ✓ receipt: %s…  (proof you can show the merchant)\n", truncate(paid.Preimage, 16))
	}
	fmt.Fprintln(t.w)
	// Silence the unused param warning — keep the signature stable so the
	// SDK's OnPaid hook still aligns; we don't need ch here.
	_ = ch
}

func (t *debugTracer) summary(ok bool, finalErr error) {
	total := time.Since(t.startTime).Round(time.Millisecond)
	fmt.Fprintln(t.w, "──────────────────────────────────────────────────────────────")
	if ok {
		fmt.Fprintf(t.w, "  ✓ done in %s — %d HTTP round-trip(s)\n\n", total, t.httpStep)
	} else {
		fmt.Fprintf(t.w, "  ✗ failed after %s: %v\n\n", total, finalErr)
	}
}

// formatFriendlyAmount renders a wallet-reported msat amount in the
// chain's natural unit (SUI / BTC), falling back to the sub-unit
// (MIST / sat) for amounts smaller than 0.0001 of the whole-unit. Sign
// is dropped — `paid.Amount` is negative for outgoing on the hosted
// route; for user-facing display the magnitude is what matters.
func formatFriendlyAmount(msat int64, chain chainUnits) string {
	abs := msat
	if abs < 0 {
		abs = -abs
	}
	subUnits := float64(abs) / 1000.0
	whole := subUnits / chain.subPerWhole
	if whole >= 0.0001 {
		// Trim trailing zeros so "0.010000 SUI" → "0.01 SUI".
		s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", whole), "0"), ".")
		return s + " " + chain.unit
	}
	// Sub-unit precision (3 decimals lets fees like 11 msat show as
	// "0.011 MIST" rather than "0 MIST").
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", subUnits), "0"), ".")
	if s == "" {
		s = "0"
	}
	return s + " " + chain.subunit
}

// `truncate(s, n)` is defined in cmd_events.go and reused here.
