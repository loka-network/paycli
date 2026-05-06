package main

import (
	"github.com/loka-network/paycli/pkg/sdk"
	"github.com/urfave/cli/v2"
)

func cmdFund() *cli.Command {
	return &cli.Command{
		Name:  "fund",
		Usage: "Generate a BOLT11 invoice to receive funds into this wallet",
		Flags: []cli.Flag{
			&cli.Int64Flag{Name: "amount", Aliases: []string{"a"}, Required: true, Usage: "amount in wallet base unit (sat or MIST)"},
			&cli.StringFlag{Name: "memo", Aliases: []string{"m"}, Usage: "human-readable memo"},
			&cli.StringFlag{Name: "unit", Value: "sat", Usage: "amount unit"},
			&cli.IntFlag{Name: "expiry", Usage: "invoice TTL seconds"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cl, err := clientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false)
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
			return printJSON(p)
		},
	}
}

func cmdPay() *cli.Command {
	return &cli.Command{
		Name:      "pay",
		Usage:     "Pay a BOLT11 invoice from this wallet",
		ArgsUsage: "<bolt11>",
		Action: func(c *cli.Context) error {
			if c.NArg() < 1 {
				return fail("bolt11 invoice argument is required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cl, err := clientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), true)
			if err != nil {
				return err
			}
			p, err := cl.PayInvoice(c.Context, c.Args().First())
			if err != nil {
				return fail("pay: %v", err)
			}
			return printJSON(p)
		},
	}
}

func cmdHistory() *cli.Command {
	return &cli.Command{
		Name:  "history",
		Usage: "List recent payments on this wallet",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "limit", Value: 20},
			&cli.IntFlag{Name: "offset"},
		},
		Action: func(c *cli.Context) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cl, err := clientFromConfig(cfg, c.String("base-url"), c.Bool("insecure"), false)
			if err != nil {
				return err
			}
			ps, err := cl.ListPayments(c.Context, c.Int("limit"), c.Int("offset"))
			if err != nil {
				return fail("history: %v", err)
			}
			return printJSON(ps)
		},
	}
}
