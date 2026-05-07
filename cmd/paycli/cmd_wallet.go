package main

import (
	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

func cmdFund() *cli.Command {
	return &cli.Command{
		Name:  "fund",
		Usage: "Generate a BOLT11 invoice to receive funds into the active wallet",
		Flags: []cli.Flag{
			&cli.Int64Flag{Name: "amount", Aliases: []string{"a"}, Required: true, Usage: "amount in wallet base unit (sat or MIST)"},
			&cli.StringFlag{Name: "memo", Aliases: []string{"m"}, Usage: "human-readable memo"},
			&cli.StringFlag{Name: "unit", Value: "sat", Usage: "[hosted] amount unit"},
			&cli.IntFlag{Name: "expiry", Usage: "invoice TTL seconds"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			switch cfg.EffectiveRoute() {
			case RouteHosted:
				cl, walletAlias, err := hostedClientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false, c.String("wallet"))
				if err != nil {
					return err
				}
				p, err := cl.CreateInvoice(c.Context, sdk.CreateInvoiceRequest{
					Amount: c.Int64("amount"),
					Memo:   c.String("memo"),
					Unit:   c.String("unit"),
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
					WalletID:       w.WalletID,
					UserID:         cfg.Hosted.UserID,
					Amount:         c.Int64("amount"),
					Unit:           c.String("unit"),
					PaymentHash:    p.PaymentHash,
					Memo:           c.String("memo") + " [wallet=" + walletAlias + "]",
					PaymentRequest: pickNonEmpty(p.Bolt11, p.PaymentRequest),
				})
				return printJSON(p)
			case RouteNode:
				nc, err := nodeClientFromConfig(cfg, "", c.Bool("insecure"))
				if err != nil {
					return err
				}
				resp, err := nc.AddInvoice(c.Context, c.Int64("amount"), c.String("memo"), int64(c.Int("expiry")))
				if err != nil {
					return fail("fund: %v", err)
				}
				LogEvent(Event{
					Event:          EventInvoiceCreated,
					Route:          string(RouteNode),
					Endpoint:       nc.Endpoint,
					Amount:         c.Int64("amount"),
					Unit:           "sat",
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
						Event:    EventPaySent,
						Route:    string(RouteHosted),
						Endpoint: cl.BaseURL,
						WalletID: w.WalletID,
						Status:   "failed",
						Error:    err.Error(),
					})
					return fail("pay: %v", err)
				}
				LogEvent(Event{
					Event:       EventPaySent,
					Route:       string(RouteHosted),
					Endpoint:    cl.BaseURL,
					WalletID:    w.WalletID,
					Amount:      -absInt64(p.Amount),
					Unit:        "msat",
					PaymentHash: p.PaymentHash,
					Preimage:    p.Preimage,
					Status:      p.Status,
					Memo:        "wallet=" + walletAlias,
				})
				return printJSON(p)
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
