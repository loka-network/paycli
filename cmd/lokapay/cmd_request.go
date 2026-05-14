package main

import (
	"bytes"
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

// debugTracer is an http.RoundTripper that pretty-prints every L402
// step to its writer. Plugs in front of any existing Transport so the
// L402Doer's HTTP behavior is unchanged — only the side effect of
// each round-trip is observed.
//
// Output format is curl-style "→ request / ← response" pairs grouped
// by attempt number, with a separate "$ paid" line between the 402
// and the retry. Long opaque values (macaroon, preimage, bolt11) are
// truncated to a fixed-width prefix so the log stays readable.
type debugTracer struct {
	base        http.RoundTripper
	w           io.Writer
	mu          sync.Mutex
	attempt     int
	lastReqTime time.Time

	route       string // "hosted" / "node" — used in the wallet annotation
	walletAlias string // sub-wallet alias on hosted route; empty on node
}

func (t *debugTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.attempt++
	attempt := t.attempt
	t.lastReqTime = time.Now()
	t.mu.Unlock()

	t.printRequest(attempt, req)
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(t.w, "\n  ← network error after %s: %v\n", time.Since(t.lastReqTime).Round(time.Millisecond), err)
		return nil, err
	}
	t.printResponse(attempt, resp, time.Since(t.lastReqTime))
	return resp, nil
}

func (t *debugTracer) banner(targetURL string) {
	fmt.Fprintf(t.w, "── L402 debug trace ─────────────────────────────────────────────\n")
	fmt.Fprintf(t.w, "  target:  %s\n", targetURL)
	fmt.Fprintf(t.w, "  wallet:  %s\n", t.walletDesc())
	fmt.Fprintln(t.w)
}

func (t *debugTracer) walletDesc() string {
	if t.walletAlias != "" {
		return fmt.Sprintf("%s route (sub-wallet %q)", t.route, t.walletAlias)
	}
	return t.route + " route"
}

func (t *debugTracer) printRequest(attempt int, req *http.Request) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	fmt.Fprintf(t.w, "[%d] → %s %s\n", attempt, req.Method, req.URL.String())
	fmt.Fprintf(t.w, "      Host: %s\n", host)
	if auth := req.Header.Get("Authorization"); auth != "" {
		fmt.Fprintf(t.w, "      Authorization: %s\n", truncateLSAT(auth))
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		fmt.Fprintf(t.w, "      Content-Type: %s\n", ct)
	}
	if req.ContentLength > 0 {
		fmt.Fprintf(t.w, "      Body: %d bytes\n", req.ContentLength)
	}
}

func (t *debugTracer) printResponse(attempt int, resp *http.Response, elapsed time.Duration) {
	fmt.Fprintf(t.w, "[%d] ← HTTP %s  (%s)\n", attempt, resp.Status, elapsed.Round(time.Millisecond))
	// Pull the L402-relevant headers so a 402 is self-explanatory in the log.
	if wa := resp.Header.Get("Www-Authenticate"); wa != "" {
		fmt.Fprintf(t.w, "      WWW-Authenticate: %s\n", truncateChallenge(wa))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		fmt.Fprintf(t.w, "      Content-Type: %s\n", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		fmt.Fprintf(t.w, "      Content-Length: %s\n", cl)
	}
	fmt.Fprintln(t.w)
}

// paid is called by L402Doer.OnPaid after a successful invoice settle.
// Sits between the 402 response and the LSAT-bearing retry, so we
// place it visually under the prior step.
func (t *debugTracer) paid(ch *sdk.Challenge, paid *sdk.Payment) {
	fmt.Fprintf(t.w, "  $ paying invoice via %s\n", t.walletDesc())
	fmt.Fprintf(t.w, "      invoice:      %s\n", truncate(ch.Invoice, 48))
	fmt.Fprintf(t.w, "      macaroon:     %s\n", truncate(ch.Macaroon, 48))
	fmt.Fprintf(t.w, "      payment_hash: %s\n", paid.PaymentHash)
	if paid.Preimage != "" {
		fmt.Fprintf(t.w, "      preimage:     %s ✓ revealed\n", paid.Preimage)
	}
	if paid.Status != "" {
		fmt.Fprintf(t.w, "      status:       %s\n", paid.Status)
	}
	if paid.Fee != 0 {
		fmt.Fprintf(t.w, "      fee:          %d msat\n", paid.Fee)
	}
	if paid.Amount != 0 {
		fmt.Fprintf(t.w, "      amount:       %d msat\n", paid.Amount)
	}
	fmt.Fprintln(t.w)
}

func (t *debugTracer) summary(ok bool, finalErr error) {
	fmt.Fprintln(t.w, "─────────────────────────────────────────────────────────────────")
	if ok {
		fmt.Fprintf(t.w, "  ✓ L402 cycle complete — %d HTTP round-trip(s)\n", t.attempt)
	} else {
		fmt.Fprintf(t.w, "  ✗ failed after %d HTTP round-trip(s): %v\n", t.attempt, finalErr)
	}
	fmt.Fprintln(t.w)
}

// truncateLSAT collapses `LSAT <macaroonB64>:<preimageHex>` into a
// prefix+suffix view so the auth header is one line.
func truncateLSAT(auth string) string {
	if !strings.HasPrefix(auth, "LSAT ") {
		return auth
	}
	rest := strings.TrimPrefix(auth, "LSAT ")
	colon := strings.LastIndex(rest, ":")
	if colon == -1 {
		return "LSAT " + truncate(rest, 48)
	}
	mac := rest[:colon]
	preimage := rest[colon+1:]
	return "LSAT " + truncate(mac, 24) + ":" + preimage
}

// truncateChallenge keeps the scheme + macaroon/invoice tags but
// shortens each opaque value. Input is the full WWW-Authenticate
// value (`LSAT macaroon="…", invoice="lnbcrt…"`).
func truncateChallenge(s string) string {
	// Buffer up the output piece by piece. We scan for `key="value"`
	// pairs and only truncate the value if it's longer than the limit.
	var out bytes.Buffer
	i := 0
	for i < len(s) {
		eq := strings.IndexByte(s[i:], '=')
		if eq == -1 {
			out.WriteString(s[i:])
			break
		}
		out.WriteString(s[i : i+eq+1]) // include the '='
		i += eq + 1
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end == -1 {
				out.WriteString(s[i:])
				break
			}
			out.WriteByte('"')
			out.WriteString(truncate(s[i+1:i+1+end], 32))
			out.WriteByte('"')
			i += end + 2
		}
	}
	return out.String()
}

// `truncate(s, n)` is defined in cmd_events.go and reused here.
