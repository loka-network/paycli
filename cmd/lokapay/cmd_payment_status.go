package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/urfave/cli/v2"
)

// cmdPaymentStatus exposes lnbits' GET /api/v1/payments/{hash} as a
// first-class CLI command. The endpoint internally calls
// update_pending_payment(), which forces lnbits' wallet driver to
// re-query the underlying lnd for the payment's current status — so
// it's the right thing to run when you've previously seen a stuck
// "pending" and want the latest LN-level truth.
//
// Output fields:
//   paid:     true when the payment has SETTLED (preimage revealed)
//   status:   pending | success | failed | unknown
//   preimage: hex (only present when paid=true)
//
// The endpoint accepts an optional X-Api-Key for the wallet, which
// gives back a `details` block with the full Payment record. lokapay
// sends the active wallet's admin key so the operator gets full
// detail when present, and falls back gracefully when it isn't.
func cmdPaymentStatus() *cli.Command {
	return &cli.Command{
		Name:      "payment-status",
		Usage:     "Refresh + show one payment's status (forces lnbits to re-query lnd)",
		ArgsUsage: "<payment_hash>",
		Description: "Useful when a previous fund/pay/request returned status=pending and\n" +
			"you want lnbits to re-query the underlying lnd. Same effect as\n" +
			"hitting GET /api/v1/payments/{hash} directly. lokapay adds the\n" +
			"active wallet's X-Api-Key so the response includes wallet detail.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "raw JSON response"},
		},
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("payment-status: <payment_hash> is required")
			}
			hash := strings.TrimSpace(c.Args().First())
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.EffectiveRoute() != RouteHosted {
				return fail("payment-status: hosted route only")
			}
			cl, _, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false, c.String("wallet"))
			if err != nil {
				return err
			}
			out, err := cl.GetPayment(c.Context, hash)
			if err != nil {
				return fail("payment-status: %v", err)
			}
			if c.Bool("json") {
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			paid, _ := out["paid"].(bool)
			status, _ := out["status"].(string)
			preimage, _ := out["preimage"].(string)
			if status == "" {
				if v, ok := out["status"]; ok && v != nil {
					status = fmt.Sprint(v)
				} else {
					status = "(none)"
				}
			}
			fmt.Printf("hash:     %s\n", hash)
			fmt.Printf("paid:     %t\n", paid)
			fmt.Printf("status:   %s\n", status)
			if preimage != "" {
				fmt.Printf("preimage: %s\n", preimage)
			}
			// `details` is only present when X-Api-Key authenticated against
			// the wallet that minted/paid this hash. Surface a hint for
			// operators eyeballing this command output.
			if details, ok := out["details"].(map[string]interface{}); ok {
				if memo, _ := details["memo"].(string); memo != "" {
					fmt.Printf("memo:     %s\n", memo)
				}
				if amtRaw, ok := details["amount"]; ok {
					// JSON number → float64 in Go's interface{} decode.
					// Use %.0f to keep it readable for whole msat amounts
					// (avoids the scientific-notation default for big values).
					if amt, ok := amtRaw.(float64); ok {
						fmt.Printf("amount:   %.0f msat\n", amt)
					} else {
						fmt.Printf("amount:   %v msat\n", amtRaw)
					}
				}
			}
			return nil
		},
	}
}
