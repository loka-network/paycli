package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
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
//
// --debug instruments the doer with an http.RoundTripper wrapper +
// OnPaid trace handler so users can see the full L402 conversation:
// initial request → 402 challenge → wallet PayInvoice → preimage →
// LSAT retry → 200. Output goes to stderr so the response body on
// stdout stays scriptable.
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
			&cli.BoolFlag{Name: "debug", Usage: "visualize the L402 protocol exchange + payment flow to stderr (HTTP requests, 402 challenge, wallet PayInvoice, preimage, retry)"},
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
			// hosted; node route has no alias). Used to tag the L402 event
			// and label the wallet in debug output.
			var walletAlias string
			if cfg.EffectiveRoute() == RouteHosted {
				walletAlias, _, _ = cfg.Hosted.ResolveWallet(c.String("wallet"))
			}

			doer := sdk.NewL402Doer(wallet)
			doer.MaxRetries = c.Int("max-retries")

			// Build the underlying http.Client. Order matters: insecure-target
			// goes on the base transport; --debug wraps that transport so
			// the debug tracer sees post-TLS plaintext requests.
			baseTransport := http.DefaultTransport
			if c.Bool("insecure-target") {
				baseTransport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 — opt-in for local testing
				}
			}

			var tracer *debugTracer
			if c.Bool("debug") {
				tracer = &debugTracer{
					base:        baseTransport,
					w:           os.Stderr,
					route:       string(cfg.EffectiveRoute()),
					walletAlias: walletAlias,
				}
				baseTransport = tracer
				tracer.banner(c.Args().First())
			}

			doer.HTTPClient = &http.Client{
				Transport: baseTransport,
				Timeout:   60 * time.Second,
			}

			// Audit each L402 settlement. Fires inside doer.Do once per
			// 402 → pay → retry cycle, after a preimage was returned.
			doer.OnPaid = func(r *http.Request, ch *sdk.Challenge, paid *sdk.Payment) {
				targetURL := r.URL.String()
				targetHost := r.Host
				if targetHost == "" {
					targetHost = r.URL.Host
				}
				if tracer != nil {
					tracer.paid(ch, paid)
				}
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
				if tracer != nil {
					tracer.summary(false, err)
				}
				return fail("request: %v", err)
			}
			defer resp.Body.Close()

			if tracer != nil {
				tracer.summary(true, nil)
			}

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

// debugTracer is an http.RoundTripper that renders an L402 transaction
// as a UML-style sequence diagram — three vertical lanes (you, the
// merchant, your wallet), arrows between them showing each message,
// time flowing top to bottom.
//
//	          you           merchant         wallet
//	           │              │                │
//	   1.      │── GET ──────►│                │      93ms
//	           │◄── 402 ──────│                │
//	           │              │                │
//	   2.      │── pay ─────────────────────── ►│
//	           │◄── preimage ─────────────────  │      0.01 SUI
//	           │              │                │
//	   3.      │── GET + LSAT ►                 │      52ms
//	           │◄── 200 OK ───│                 │
//	           │              │                │
//
// Lane positions are fixed (laneCol[0..2]) so every arrow draws into
// the same columns regardless of label length. Drawing primitives
// keep the renderer ASCII-only for terminal portability — box drawing
// chars (─ │ ◄ ►) are narrow Unicode (width 1) and consistent across
// mainstream shells; we avoid East-Asian-Ambiguous glyphs like ① that
// rendered at width-2 in some setups.
//
// Plugs in front of any existing Transport so the L402Doer's HTTP
// behavior is unchanged — only the side effect of each round-trip is
// observed.
type debugTracer struct {
	base        http.RoundTripper
	w           io.Writer
	mu          sync.Mutex
	httpStep    int       // counts HTTP round-trips
	startTime   time.Time // first request fired; used for total-elapsed
	lastReqTime time.Time

	route       string // "hosted" / "node" — used in the wallet annotation
	walletAlias string // sub-wallet alias on hosted route; empty on node
}

// Lane column positions (0-indexed). Chosen so:
//   - step label "   N.   " fits in cols 0..7
//   - inter-lane gaps are ~18 chars — wide enough for short labels
//     ("GET", "402", "200 OK", "GET+LSAT") to sit centered between
//     the bars without crowding
//   - lineWidth leaves a 4-char halo past the wallet lane so column
//     headers ("wallet") can be centered without clipping
//   - total fits comfortably in an 80-col terminal even with a
//     side-note ("4.369s") appended
const (
	laneClient   = 12
	laneMerchant = 30
	laneWallet   = 48
	lineWidth    = laneWallet + 4
)

// Friendly column header labels.
var laneLabels = struct{ client, merchant, wallet string }{
	client:   "you",
	merchant: "merchant",
	wallet:   "wallet",
}

func (t *debugTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.httpStep++
	step := t.httpStep
	t.lastReqTime = time.Now()
	if t.startTime.IsZero() {
		t.startTime = t.lastReqTime
	}
	t.mu.Unlock()

	// First HTTP call = step 1 (initial GET → 402). Subsequent calls =
	// step 3 (retry with LSAT auth → final response).
	stepNum := 1
	if step > 1 {
		stepNum = 3
	}

	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(t.lastReqTime).Round(time.Millisecond)

	// Outgoing arrow: client → merchant
	outLabel := req.Method
	if stepNum == 3 {
		outLabel = req.Method + " + LSAT"
	}
	fmt.Fprintln(t.w, t.arrowRight(stepNum, laneClient, laneMerchant, outLabel, ""))

	// Incoming arrow: merchant → client (or error)
	if err != nil {
		fmt.Fprintln(t.w, t.arrowLeft(0, laneClient, laneMerchant, "✗ network error", elapsed.String()))
		fmt.Fprintf(t.w, "%s%v\n\n", strings.Repeat(" ", laneWallet+5), err)
		return nil, err
	}
	// Keep labels short so they fit in the inter-lane gap; use the
	// side-note column for the explanatory phrase.
	var inLabel, inNote string
	switch {
	case resp.StatusCode == http.StatusPaymentRequired:
		inLabel = "402"
		inNote = "needs payment · " + elapsed.String()
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		inLabel = resp.Status
		inNote = elapsed.String()
	default:
		inLabel = fmt.Sprintf("%d", resp.StatusCode)
		inNote = http.StatusText(resp.StatusCode) + " · " + elapsed.String()
	}
	fmt.Fprintln(t.w, t.arrowLeft(0, laneClient, laneMerchant, inLabel, inNote))
	fmt.Fprintln(t.w, t.laneBars())
	return resp, nil
}

func (t *debugTracer) banner(targetURL string) {
	fmt.Fprintln(t.w, "─── L402 payment ──────────────────────────────────────────────")
	fmt.Fprintf(t.w, "  Resource: %s\n", targetURL)
	fmt.Fprintf(t.w, "  Wallet:   %s\n\n", t.walletDesc())
	// Lane header row + initial baseline.
	fmt.Fprintln(t.w, t.laneHeader())
	fmt.Fprintln(t.w, t.laneBars())
}

func (t *debugTracer) walletDesc() string {
	if t.walletAlias != "" {
		return fmt.Sprintf("%s route (sub-wallet %q)", t.route, t.walletAlias)
	}
	return t.route + " route"
}

// paid is called by L402Doer.OnPaid after a successful invoice settle.
// Renders as step 2: client ↔ wallet round-trip.
func (t *debugTracer) paid(ch *sdk.Challenge, paid *sdk.Payment) {
	chain := chainFromExtra(paid.Extra)
	var amtNote string
	if paid.Amount != 0 {
		amtNote = formatFriendlyAmount(paid.Amount, chain)
	}
	fmt.Fprintln(t.w, t.arrowRight(2, laneClient, laneWallet, "pay invoice", ""))
	receiptNote := ""
	if paid.Preimage != "" {
		// `truncate` already appends "…" when the value is clipped, so
		// don't double it.
		receiptNote = "receipt: " + truncate(paid.Preimage, 16)
	}
	fmt.Fprintln(t.w, t.arrowLeft(0, laneClient, laneWallet, "preimage", amtNote))
	if receiptNote != "" {
		fmt.Fprintln(t.w, t.laneBarsWithNote(receiptNote))
	}
	fmt.Fprintln(t.w, t.laneBars())
	_ = ch
}

func (t *debugTracer) summary(ok bool, finalErr error) {
	total := time.Since(t.startTime).Round(time.Millisecond)
	fmt.Fprintln(t.w, "──────────────────────────────────────────────────────────────")
	if ok {
		fmt.Fprintf(t.w, "  ✓ done in %s — %d HTTP round-trip(s)\n\n", total, t.httpStep)
	} else {
		fmt.Fprintf(t.w, "  ✗ failed after %s: %v\n\n", total, finalErr)
	}
}

// ----------------------------------------------------------------
// Diagram drawing primitives.

// laneHeader produces the column header row.
//
//	"          you           merchant         wallet"
func (t *debugTracer) laneHeader() string {
	buf := make([]rune, lineWidth)
	for i := range buf {
		buf[i] = ' '
	}
	placeCentered(buf, laneClient, laneLabels.client)
	placeCentered(buf, laneMerchant, laneLabels.merchant)
	placeCentered(buf, laneWallet, laneLabels.wallet)
	return strings.TrimRight(string(buf), " ")
}

// laneBars draws the baseline row showing each lane as a vertical bar
// without any message:
//
//	"             │              │                │"
func (t *debugTracer) laneBars() string {
	buf := make([]rune, lineWidth)
	for i := range buf {
		buf[i] = ' '
	}
	buf[laneClient] = '│'
	buf[laneMerchant] = '│'
	buf[laneWallet] = '│'
	return string(buf)
}

// laneBarsWithNote draws the baseline row + a side note past the last lane.
func (t *debugTracer) laneBarsWithNote(note string) string {
	return appendNote(t.laneBars(), note)
}

// appendNote tacks "    " + note onto a diagram line. Always-on the
// same code path so notes align identically whether the line was
// built by arrowRight, arrowLeft, or laneBars.
func appendNote(line, note string) string {
	if note == "" {
		return line
	}
	return line + "    " + note
}

// arrowRight draws an arrow from lane at fromCol to lane at toCol,
// where fromCol < toCol. label sits in the middle of the arrow.
// stepNum > 0 puts "  N. " at the line start; pass 0 to skip the step prefix.
// note (if non-empty) appears past the final lane as a side annotation.
//
//	"  1.        │── GET ──────►│                │     93ms"
func (t *debugTracer) arrowRight(stepNum, fromCol, toCol int, label, note string) string {
	buf := make([]rune, lineWidth)
	for i := range buf {
		buf[i] = ' '
	}
	// All three lane bars first, then we overwrite with the arrow.
	buf[laneClient] = '│'
	buf[laneMerchant] = '│'
	buf[laneWallet] = '│'
	if stepNum > 0 {
		stepLabel := fmt.Sprintf("  %d.", stepNum)
		for i, r := range stepLabel {
			if i < lineWidth {
				buf[i] = r
			}
		}
	}
	// Fill the segment between fromCol and toCol with the arrow.
	for i := fromCol + 1; i < toCol; i++ {
		buf[i] = '─'
	}
	buf[toCol-1] = '►'
	placeOverlay(buf, fromCol+1, toCol-2, " "+label+" ")
	buf[fromCol] = '│'
	buf[toCol] = '│'
	return appendNote(string(buf), note)
}

// arrowLeft draws an arrow from the right lane (toCol) BACK to the
// left lane (fromCol) — i.e. response direction. fromCol < toCol.
// stepNum=0 always (responses don't carry a step number).
//
//	"             │◄── 402 ──────│                │"
func (t *debugTracer) arrowLeft(stepNum, fromCol, toCol int, label, note string) string {
	buf := make([]rune, lineWidth)
	for i := range buf {
		buf[i] = ' '
	}
	buf[laneClient] = '│'
	buf[laneMerchant] = '│'
	buf[laneWallet] = '│'
	if stepNum > 0 {
		stepLabel := fmt.Sprintf("  %d.", stepNum)
		for i, r := range stepLabel {
			if i < lineWidth {
				buf[i] = r
			}
		}
	}
	for i := fromCol + 1; i < toCol; i++ {
		buf[i] = '─'
	}
	buf[fromCol+1] = '◄'
	placeOverlay(buf, fromCol+2, toCol-1, " "+label+" ")
	buf[fromCol] = '│'
	buf[toCol] = '│'
	return appendNote(string(buf), note)
}

// placeCentered writes s centered around the given column.
// Clips silently if s won't fit before col 0 or past lineWidth.
func placeCentered(buf []rune, col int, s string) {
	start := col - len(s)/2
	for i, r := range s {
		p := start + i
		if p >= 0 && p < len(buf) {
			buf[p] = r
		}
	}
}

// placeOverlay writes s centered in [start, end] inclusive, overlaying
// whatever's already there. Used to put a label "── GET ──" over the
// arrow's dashes. Truncates s if it's longer than the span — we'd
// rather show a slightly clipped label than silently swallow it.
// All labels we draw are ASCII so a byte-slice truncate is safe here.
func placeOverlay(buf []rune, start, end int, s string) {
	span := end - start + 1
	if span <= 0 {
		return
	}
	if len(s) > span {
		s = s[:span]
	}
	pad := (span - len(s)) / 2
	for i, r := range s {
		p := start + pad + i
		if p >= 0 && p < len(buf) {
			buf[p] = r
		}
	}
}

// formatFriendlyAmount renders a wallet-reported msat amount in the
// chain's natural unit (SUI / BTC), falling back to the sub-unit
// (MIST / sat) for amounts smaller than 0.0001 of the whole-unit. Sign
// is dropped — `paid.Amount` is negative for outgoing on the hosted
// route; for user-facing display the magnitude is what matters.
func formatFriendlyAmount(msat int64, chain chainUnits) string {
	abs := msat
	if abs < 0 {
		abs = -abs
	}
	subUnits := float64(abs) / 1000.0
	whole := subUnits / chain.subPerWhole
	if whole >= 0.0001 {
		// Trim trailing zeros so "0.010000 SUI" → "0.01 SUI".
		s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", whole), "0"), ".")
		return s + " " + chain.unit
	}
	// Sub-unit precision (3 decimals lets fees like 11 msat show as
	// "0.011 MIST" rather than "0 MIST").
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", subUnits), "0"), ".")
	if s == "" {
		s = "0"
	}
	return s + " " + chain.subunit
}

// `truncate(s, n)` is defined in cmd_events.go and reused here.
