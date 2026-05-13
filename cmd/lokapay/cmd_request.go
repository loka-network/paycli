package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
			// hosted; node route has no alias). Used to tag the L402 event.
			var walletAlias string
			if cfg.EffectiveRoute() == RouteHosted {
				walletAlias, _, _ = cfg.Hosted.ResolveWallet(c.String("wallet"))
			}

			doer := sdk.NewL402Doer(wallet)
			doer.MaxRetries = c.Int("max-retries")
			if c.Bool("insecure-target") {
				doer.HTTPClient = &http.Client{
					Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 — opt-in for local testing
					Timeout:   60 * time.Second,
				}
			}
			// Audit each L402 settlement. Fires inside doer.Do once per
			// 402 → pay → retry cycle, after a preimage was returned.
			doer.OnPaid = func(r *http.Request, ch *sdk.Challenge, paid *sdk.Payment) {
				targetURL := r.URL.String()
				targetHost := r.Host
				if targetHost == "" {
					targetHost = r.URL.Host
				}
				// Best-effort amount: parse the bolt11 prefix to grab a
				// human-friendly value. The wallet's returned Payment may
				// not carry a normalized Amount (hosted/node disagree on
				// units), but the invoice itself is canonical.
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
				return fail("request: %v", err)
			}
			defer resp.Body.Close()

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
