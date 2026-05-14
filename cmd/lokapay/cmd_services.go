package main

import (
	"strings"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdServices lists the L402 service catalog from a Prism gateway.
//
// GET /api/admin/services is exposed without authentication by Prism
// (see loka-prism-l402/admin/auth.go), so end users can render a service
// picker before paying. The --prism-macaroon flag is still accepted for
// deployments that re-enable auth or hand out limited-scope macaroons.
//
// Default Prism URL resolution (first non-empty wins):
//
//  1. `--prism-url` flag on the command
//  2. `prism_url` in the config (set by `lokapay init` based on the wallet
//     endpoint's locality, or manually via `lokapay config set prism_url …`)
//  3. sdk.DefaultPrismURL (https://prism.loka.cash)
//
// The auto-insecure logic from `lokapay init` is mirrored: if the
// resolved URL is local (loopback / RFC 1918 / .local), TLS verification
// is skipped without requiring `--insecure`.
func cmdServices() *cli.Command {
	return &cli.Command{
		Name:  "services",
		Usage: "List services exposed by a Prism gateway",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "prism-url", Usage: "Prism base URL (defaults to config's prism_url, else https://prism.loka.cash)"},
			&cli.StringFlag{Name: "prism-macaroon", Usage: "path to Prism's admin.macaroon (optional; only needed on auth-gated deployments)"},
			&cli.BoolFlag{Name: "insecure", Usage: "skip TLS verification (auto-true for local prism URLs)"},
			&cli.StringFlag{Name: "search", Aliases: []string{"s"}, Usage: "filter services by name / host / path substring (case-insensitive)"},
		},
		Action: func(c *cli.Context) error {
			prismURL := c.String("prism-url")
			if prismURL == "" {
				if cfg, err := loadConfig(); err == nil && cfg.PrismURL != "" {
					prismURL = cfg.PrismURL
				} else {
					prismURL = sdk.DefaultPrismURL
				}
			}
			var opts []sdk.PrismOption
			if mac := c.String("prism-macaroon"); mac != "" {
				opts = append(opts, sdk.WithPrismMacaroonFile(mac))
			}
			if c.Bool("insecure") || shouldSkipTLSFor(prismURL) {
				opts = append(opts, sdk.WithPrismInsecureTLS())
			}
			pc, err := sdk.NewPrismClient(prismURL, opts...)
			if err != nil {
				return fail("services: %v", err)
			}
			services, err := pc.ListServices(c.Context)
			if err != nil {
				return fail("services: %v", err)
			}
			if needle := strings.ToLower(c.String("search")); needle != "" {
				services = filterServices(services, needle)
			}
			return printJSON(services)
		},
	}
}

func filterServices(in []sdk.PrismService, needle string) []sdk.PrismService {
	out := make([]sdk.PrismService, 0, len(in))
	for _, s := range in {
		hay := strings.ToLower(s.Name + " " + s.Address + " " + s.HostRegexp + " " + s.PathRegexp)
		if strings.Contains(hay, needle) {
			out = append(out, s)
		}
	}
	return out
}
