package main

import (
	"fmt"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

// cmdRate prints the oracle's current view as agents-pay-service sees it,
// for whichever fiat currency the user asks (default USD).
//
// The body lnbits returns is chain-agnostic by name:
//
//	{ "rate": <native units per 1 fiat unit>, "price": <fiat per native unit> }
//
// On a SUI deployment that's MIST/USD + USD/SUI; on BTC it's sat/USD +
// USD/BTC. paycli detects the chain by sniffing the most recent
// fund/pay event's `extra` field — if `wallet_sui_rate` was present the
// chain is SUI, if `wallet_btc_rate` then BTC. When the active wallet
// has never seen a payment we fall back to a generic native-unit label.
func cmdRate() *cli.Command {
	return &cli.Command{
		Name:      "rate",
		Usage:     "Show oracle exchange rate from agents-pay-service",
		ArgsUsage: "[currency]",
		Description: "currency defaults to USD. Hits GET /api/v1/rate/<currency>; the\n" +
			"endpoint is open (no X-Api-Key required), so this works as long as\n" +
			"hosted.base_url points at a reachable agents-pay-service.",
		Action: func(c *cli.Context) error {
			cur := "USD"
			if c.NArg() >= 1 {
				cur = c.Args().First()
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.EffectiveRoute() != RouteHosted {
				return fail("rate: only the hosted route exposes the oracle endpoint")
			}
			baseURL := cfg.Hosted.BaseURL
			if v := c.String("base-url"); v != "" {
				baseURL = v
			}
			if baseURL == "" {
				baseURL = sdk.DefaultBaseURL
			}
			opts := []sdk.Option{}
			if c.Bool("insecure") || cfg.Insecure {
				opts = append(opts, sdk.WithInsecureTLS())
			}
			cl := sdk.New(baseURL, opts...)
			r, err := cl.GetRate(c.Context, cur)
			if err != nil {
				return fail("rate: %v", err)
			}
			native := guessNativeChain()
			fmt.Printf("currency:    %s\n", cur)
			fmt.Printf("price:       %.4f %s per 1 %s\n", r.Price, cur, native.unit)
			fmt.Printf("rate:        %.4f %s per 1 %s\n", r.Rate, native.subunit, cur)
			fmt.Printf("(per quote:  1 %s ≈ %.4f %s; 1 %s ≈ %.4f %s)\n",
				cur, r.Rate, native.subunit,
				native.unit, r.Price, cur)
			return nil
		},
	}
}

// chainUnits is the (display-name, sub-unit-name, sub-units-per-whole)
// triple paycli uses to label numbers. SUI uses MIST as the sub-unit
// (1 SUI = 10^9 MIST); BTC uses sat (1 BTC = 10^8 sat).
type chainUnits struct {
	unit         string  // human-facing whole-unit name
	subunit      string  // sub-unit name (what server-side fields call "amount" / "amount_sat")
	subPerWhole  float64 // 10^9 for SUI, 10^8 for BTC
}

var (
	chainSUI = chainUnits{unit: "SUI", subunit: "MIST", subPerWhole: 1_000_000_000}
	chainBTC = chainUnits{unit: "BTC", subunit: "sat", subPerWhole: 100_000_000}
)

// guessNativeChain returns SUI by default in the absence of evidence,
// since today's Loka deployments are SUI-backed. Caller code that has
// access to a recent payment's `extra` map can pass it to
// chainFromExtra below for a deterministic answer.
func guessNativeChain() chainUnits {
	return chainSUI
}

// chainFromExtra inspects the `extra` dict on a fund/pay response and
// picks the chain from whichever native-rate key the server emitted.
//
//	wallet_sui_rate or sui_rate    → SUI
//	wallet_btc_rate or btc_rate    → BTC
//	(neither present)              → fall back to guessNativeChain()
func chainFromExtra(extra map[string]interface{}) chainUnits {
	if extra == nil {
		return guessNativeChain()
	}
	if _, ok := extra["wallet_sui_rate"]; ok {
		return chainSUI
	}
	if _, ok := extra["sui_rate"]; ok {
		return chainSUI
	}
	if _, ok := extra["wallet_btc_rate"]; ok {
		return chainBTC
	}
	if _, ok := extra["btc_rate"]; ok {
		return chainBTC
	}
	return guessNativeChain()
}
