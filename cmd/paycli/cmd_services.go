package main

import (
	"strings"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdServices lists the L402 service catalog from a Prism gateway.
//
// Important: the underlying API (`GET /api/admin/services`) is currently
// admin-gated by Prism — there is no public/anon catalog endpoint. This
// command therefore requires the admin macaroon, making it primarily
// useful for Prism operators and integration testing rather than
// end-user discovery. Once Prism exposes a public catalog, this command
// will switch to it transparently.
func cmdServices() *cli.Command {
	return &cli.Command{
		Name:  "services",
		Usage: "List services exposed by a Prism gateway (requires its admin macaroon)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "prism-url", Required: true, Usage: "Prism base URL, e.g. https://127.0.0.1:8080"},
			&cli.StringFlag{Name: "prism-macaroon", Required: true, Usage: "path to Prism's admin.macaroon"},
			&cli.BoolFlag{Name: "insecure", Usage: "skip TLS verification (local dev)"},
			&cli.StringFlag{Name: "search", Aliases: []string{"s"}, Usage: "filter services by name / host / path substring (case-insensitive)"},
		},
		Action: func(c *cli.Context) error {
			opts := []sdk.PrismOption{
				sdk.WithPrismMacaroonFile(c.String("prism-macaroon")),
			}
			if c.Bool("insecure") {
				opts = append(opts, sdk.WithPrismInsecureTLS())
			}
			pc, err := sdk.NewPrismClient(c.String("prism-url"), opts...)
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
