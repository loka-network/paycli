package main

import (
	"fmt"
	"os"

	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

func cmdFund() *cli.Command {
	return &cli.Command{
		Name:  "fund",
		Usage: "Generate a BOLT11 invoice to receive funds into the active wallet",
		Flags: []cli.Flag{
			&cli.Float64Flag{Name: "amount", Aliases: []string{"a"}, Required: true, Usage: "amount in --unit (e.g. `--amount 0.1 --unit sui` or `--amount 1000 --unit sat`)"},
			&cli.StringFlag{Name: "memo", Aliases: []string{"m"}, Usage: "human-readable memo"},
			&cli.StringFlag{Name: "unit", Value: "sat", Usage: "amount unit: sat | mist (sub-unit), sui (whole, multiplies by 1e9), or a fiat code (USD/EUR/...) for server-side oracle conversion"},
			&cli.IntFlag{Name: "expiry", Usage: "invoice TTL seconds"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			subAmount, serverUnit, err := resolveAmount(c.Float64("amount"), c.String("unit"))
			if err != nil {
				return fail("fund: %v", err)
			}
			switch cfg.EffectiveRoute() {
			case RouteHosted:
				cl, walletAlias, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false, c.String("wallet"))
				if err != nil {
					return err
				}
				p, err := cl.CreateInvoice(c.Context, sdk.CreateInvoiceRequest{
					Amount: subAmount,
					Memo:   c.String("memo"),
					Unit:   serverUnit,
					Expiry: c.Int("expiry"),
				})
				if err != nil {
					return fail("fund: %v", err)
				}
				_, w, _ := cfg.Hosted.ResolveWallet(walletAlias)
				LogEvent(Event{
					Event:          EventInvoiceCreated,
					Route:          string(RouteHosted),
					Endpoint:       cl.BaseURL,
					WalletAlias:    walletAlias,
					WalletID:       w.WalletID,
					UserID:         cfg.Hosted.UserID,
					Amount:         subAmount,
					Unit:           serverUnit,
					PaymentHash:    p.PaymentHash,
					Memo:           c.String("memo"),
					PaymentRequest: pickNonEmpty(p.Bolt11, p.PaymentRequest),
				})
				if err := printJSON(p); err != nil {
					return err
				}
				printPaymentSummary(p.Amount, p.Extra)
				return nil
			case RouteNode:
				nc, err := nodeClientFromConfig(cfg, "", c.Bool("insecure"))
				if err != nil {
					return err
				}
				// lnd-rest's value field is sub-unit (sat or MIST). For
				// fiat units we can't talk to lnd directly, so reject —
				// node mode has no oracle.
				if serverUnit != "sat" && serverUnit != "mist" {
					return fail("fund: node route only accepts sat/mist/sui units (no oracle on the node side)")
				}
				resp, err := nc.AddInvoice(c.Context, subAmount, c.String("memo"), int64(c.Int("expiry")))
				if err != nil {
					return fail("fund: %v", err)
				}
				LogEvent(Event{
					Event:          EventInvoiceCreated,
					Route:          string(RouteNode),
					Endpoint:       nc.Endpoint,
					Amount:         subAmount,
					Unit:           serverUnit,
					PaymentHash:    resp.PaymentHashHex(),
					Memo:           c.String("memo"),
					PaymentRequest: resp.PaymentRequest,
				})
				return printJSON(resp)
			}
			return fail("unknown route")
		},
	}
}

func cmdPay() *cli.Command {
	return &cli.Command{
		Name:      "pay",
		Usage:     "Pay a BOLT11 invoice from the active wallet",
		ArgsUsage: "<bolt11>",
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("bolt11 invoice argument is required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			bolt11 := c.Args().First()
			switch cfg.EffectiveRoute() {
			case RouteHosted:
				cl, walletAlias, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), true, c.String("wallet"))
				if err != nil {
					return err
				}
				_, w, _ := cfg.Hosted.ResolveWallet(walletAlias)
				p, err := cl.PayInvoice(c.Context, bolt11)
				if err != nil {
					LogEvent(Event{
						Event:       EventPaySent,
						Route:       string(RouteHosted),
						Endpoint:    cl.BaseURL,
						WalletAlias: walletAlias,
						WalletID:    w.WalletID,
						Status:      "failed",
						Error:       err.Error(),
					})
					return fail("pay: %v", err)
				}
				LogEvent(Event{
					Event:       EventPaySent,
					Route:       string(RouteHosted),
					Endpoint:    cl.BaseURL,
					WalletAlias: walletAlias,
					WalletID:    w.WalletID,
					Amount:      -absInt64(p.Amount),
					Unit:        "msat",
					PaymentHash: p.PaymentHash,
					Preimage:    p.Preimage,
					Status:      p.Status,
				})
				if err := printJSON(p); err != nil {
					return err
				}
				printPaymentSummary(p.Amount, p.Extra)
				return nil
			case RouteNode:
				nc, err := nodeClientFromConfig(cfg, "", c.Bool("insecure"))
				if err != nil {
					return err
				}
				resp, err := nc.SendPaymentSync(c.Context, bolt11)
				if err != nil {
					LogEvent(Event{
						Event:    EventPaySent,
						Route:    string(RouteNode),
						Endpoint: nc.Endpoint,
						Status:   "failed",
						Error:    err.Error(),
					})
					return fail("pay: %v", err)
				}
				status := "SUCCEEDED"
				if resp.PaymentError != "" || resp.PreimageHex() == "" {
					status = "failed"
				}
				LogEvent(Event{
					Event:       EventPaySent,
					Route:       string(RouteNode),
					Endpoint:    nc.Endpoint,
					Unit:        "sat",
					PaymentHash: resp.PaymentHashHex(),
					Preimage:    resp.PreimageHex(),
					Status:      status,
					Error:       resp.PaymentError,
				})
				return printJSON(resp)
			}
			return fail("unknown route")
		},
	}
}

func cmdHistory() *cli.Command {
	return &cli.Command{
		Name:  "history",
		Usage: "List recent payments on the active wallet",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "limit", Value: 20},
			&cli.IntFlag{Name: "offset"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			switch cfg.EffectiveRoute() {
			case RouteHosted:
				cl, _, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false, c.String("wallet"))
				if err != nil {
					return err
				}
				ps, err := cl.ListPayments(c.Context, c.Int("limit"), c.Int("offset"))
				if err != nil {
					return fail("history: %v", err)
				}
				return printJSON(ps)
			case RouteNode:
				nc, err := nodeClientFromConfig(cfg, "", c.Bool("insecure"))
				if err != nil {
					return err
				}
				ps, err := nc.ListPayments(c.Context, c.Int("limit"))
				if err != nil {
					return fail("history: %v", err)
				}
				return printJSON(ps)
			}
			return fail("unknown route")
		},
	}
}

// pickNonEmpty returns the first non-empty string. Used because the
// hosted Payment object exposes both Bolt11 and PaymentRequest depending
// on the LNbits version, but they hold the same value.
func pickNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// printPaymentSummary writes a one-line "≈ X SUI ≈ Y USD" hint to
// stderr after a hosted-route fund / pay, so the operator doesn't have
// to manually divide msat by 10^9 and look up extra.wallet_fiat_amount.
//
// amountMsat is what agents-pay-service stores (1 unit = 1 msat for BTC,
// or — confusingly — 1 unit = 1 millisat-but-actually-1-MIST under the
// SUI bridge, since lnd-sui keeps the lnrpc field name). chainFromExtra
// disambiguates by sniffing extra.wallet_(sui|btc)_rate.
//
// Best-effort and silent on missing data — never disrupts the user's
// JSON-parsing pipeline. Any failure path just skips the line.
func printPaymentSummary(amountMsat int64, extra map[string]interface{}) {
	chain := chainFromExtra(extra)
	// agents-pay-service's amount is reported in msat (millisat). The
	// "whole sub-unit" is sat (BTC) or MIST (SUI), so we divide by 1000
	// to land in the chain's sub-unit, and again by subPerWhole to land
	// in the whole unit.
	subUnits := float64(absInt64(amountMsat)) / 1000.0
	whole := subUnits / chain.subPerWhole

	var fiat float64
	var fiatCur string
	if v, ok := extra["wallet_fiat_amount"].(float64); ok {
		fiat = v
	}
	if v, ok := extra["wallet_fiat_currency"].(string); ok {
		fiatCur = v
	}

	if fiatCur != "" && fiat > 0 {
		fmt.Fprintf(os.Stderr, "≈ %.6f %s  (%.0f %s ≈ %.4f %s)\n",
			whole, chain.unit, subUnits, chain.subunit, fiat, fiatCur)
	} else {
		fmt.Fprintf(os.Stderr, "≈ %.6f %s  (%.0f %s)\n", whole, chain.unit, subUnits, chain.subunit)
	}
}
