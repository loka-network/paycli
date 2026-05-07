package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
)

// cmdEvents reads ~/.paycli/events.jsonl and prints a filtered view —
// the audit trail every payment command has been writing to behind the
// scenes since this revision. Default output is one event per line in
// human-friendly form; --json dumps the raw JSONL for machine consumers.
func cmdEvents() *cli.Command {
	return &cli.Command{
		Name:  "events",
		Usage: "Show the local payment event log (~/.paycli/events.jsonl)",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "limit", Aliases: []string{"n"}, Value: 20, Usage: "show last N events (0 = all)"},
			&cli.StringFlag{Name: "type", Aliases: []string{"t"}, Usage: "filter by event type (account_created | invoice_created | pay_sent | l402_paid | topup_credit)"},
			&cli.StringFlag{Name: "route", Aliases: []string{"r"}, Usage: "filter by route (hosted | node)"},
			&cli.StringFlag{Name: "since", Aliases: []string{"s"}, Usage: "RFC3339 timestamp; only events at or after this time"},
			&cli.BoolFlag{Name: "json", Aliases: []string{"j"}, Usage: "raw JSONL output (one event per line, no formatting)"},
			&cli.BoolFlag{Name: "path", Usage: "only print the resolved event log path"},
		},
		Action: func(c *cli.Context) error {
			path, enabled := eventLogPath()
			if c.Bool("path") {
				if !enabled {
					fmt.Println("(disabled via PAYCLI_EVENT_LOG)")
					return nil
				}
				fmt.Println(path)
				return nil
			}
			if !enabled {
				return fail("event log is disabled (PAYCLI_EVENT_LOG=off)")
			}

			events, _, err := readEvents()
			if err != nil {
				return fail("read events: %v", err)
			}
			if len(events) == 0 {
				fmt.Fprintln(cli.ErrWriter, "(no events recorded yet)")
				return nil
			}

			var since time.Time
			if s := c.String("since"); s != "" {
				since, err = time.Parse(time.RFC3339, s)
				if err != nil {
					return fail("--since must be RFC3339 (e.g. 2026-05-07T00:00:00Z): %v", err)
				}
			}

			filtered := events[:0:len(events)]
			for _, e := range events {
				if t := c.String("type"); t != "" && e.Event != t {
					continue
				}
				if r := c.String("route"); r != "" && e.Route != r {
					continue
				}
				if !since.IsZero() {
					ts, perr := time.Parse(time.RFC3339Nano, e.Timestamp)
					if perr != nil || ts.Before(since) {
						continue
					}
				}
				filtered = append(filtered, e)
			}

			if n := c.Int("limit"); n > 0 && len(filtered) > n {
				filtered = filtered[len(filtered)-n:]
			}

			if c.Bool("json") {
				for _, e := range filtered {
					b, _ := json.Marshal(e)
					fmt.Println(string(b))
				}
				return nil
			}

			for _, e := range filtered {
				fmt.Println(formatEvent(e))
			}
			return nil
		},
	}
}

// formatEvent turns one Event into a single human-readable line.
//
// Layout: `<short-ts> <type-tag> [route] amount=… status=… <details>`.
// Times are local-tz so users don't have to mentally convert UTC.
func formatEvent(e Event) string {
	ts := e.Timestamp
	if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
		ts = t.Local().Format("2006-01-02 15:04:05")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-16s", ts, e.Event)
	if e.Route != "" {
		fmt.Fprintf(&b, "  [%s]", e.Route)
	}
	if e.Amount != 0 {
		unit := e.Unit
		if unit == "" {
			unit = "sat"
		}
		fmt.Fprintf(&b, "  amount=%+d %s", e.Amount, unit)
	}
	if e.Status != "" {
		fmt.Fprintf(&b, "  status=%s", e.Status)
	}
	if e.PaymentHash != "" {
		fmt.Fprintf(&b, "  hash=%s..", e.PaymentHash[:min(16, len(e.PaymentHash))])
	}
	switch e.Event {
	case EventL402Paid:
		if e.TargetHost != "" {
			fmt.Fprintf(&b, "  host=%s", e.TargetHost)
		}
		if e.TargetURL != "" {
			fmt.Fprintf(&b, "  url=%s", e.TargetURL)
		}
	case EventInvoiceCreated:
		if e.Memo != "" {
			fmt.Fprintf(&b, "  memo=%q", e.Memo)
		}
	case EventTopupCredit:
		if e.WalletID != "" {
			fmt.Fprintf(&b, "  wallet=%s", e.WalletID)
		}
	case EventAccountCreated:
		if e.Memo != "" {
			fmt.Fprintf(&b, "  name=%q", e.Memo)
		}
		if e.WalletID != "" {
			fmt.Fprintf(&b, "  wallet=%s..", e.WalletID[:min(12, len(e.WalletID))])
		}
	}
	if e.Error != "" {
		fmt.Fprintf(&b, "  error=%q", truncate(e.Error, 80))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
