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

// cmdRequest implements `paycli request <url>` — a curl-style HTTP wrapper that
// transparently pays L402 challenges using the configured custodial wallet.
//
// On 402, the SDK's L402Doer parses the LSAT challenge, calls PayInvoice on
// agents-pay-service, then replays the original request with the
// `Authorization: LSAT mac:preimage` header. Token reuse is in-process for
// the lifetime of the command.
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
			// We need admin key to actually pay invoices behind the scenes.
			cl, err := clientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), true)
			if err != nil {
				return err
			}
			if cl.KeyType != sdk.KeyAdmin {
				return fail("request: admin key required to auto-pay L402 challenges (run `paycli login --admin-key ...`)")
			}

			doer := sdk.NewL402Doer(cl)
			doer.MaxRetries = c.Int("max-retries")
			if c.Bool("insecure-target") || c.Bool("insecure") {
				doer.HTTPClient = &http.Client{
					Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 — opt-in for local testing
					Timeout:   60 * time.Second,
				}
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
