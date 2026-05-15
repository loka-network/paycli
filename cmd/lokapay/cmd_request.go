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
				route := string(cfg.EffectiveRoute())
				tracer = &debugTracer{
					base:        baseTransport,
					w:           os.Stderr,
					route:       route,
					walletAlias: walletAlias,
					layout:      laneLayoutFor(route),
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
// as a UML-style sequence diagram — three or four vertical lanes
// (you, prism, merchant, and on hosted route, wallet), arrows between
// them showing each message, time flowing top to bottom.
//
// Lane set depends on custody route:
//
// hosted (4 lanes — wallet is the remote agents-pay-service sub-wallet):
//
//	         you         prism        merchant       wallet
//	          │            │             │             │
//	  1.      │── GET ────►│             │             │
//	          │            │─ ─ proxy ─ ►│             │
//	          │            │◄ ─ ─ 402 ─ ─│             │
//	          │◄── 402 ────│             │             │   needs payment · 89ms
//	  2.      │── pay invoice ────────────────────────►│
//	          │◄── preimage ───────────────────────────│   0.01 SUI
//	  3.      │─ GET+LSAT ►│             │             │
//	          │            │─ ─ proxy ─ ►│             │
//	          │◄── 200 ────│             │             │   52ms
//
// node (3 lanes — "you" IS the wallet, lnd is local; pay step is a
// self-loop annotation instead of a cross-lane arrow):
//
//	      you (lnd)       prism        merchant
//	          │            │             │
//	  1.      │── GET ────►│             │
//	          │            │─ ─ proxy ─ ►│
//	          │◄── 402 ────│             │            needs payment · 89ms
//	  2.      │  ↻ paid via local lnd · 0.01 SUI
//	  3.      │─ GET+LSAT ►│             │
//	          │            │─ ─ proxy ─ ►│
//	          │◄── 200 ────│             │            52ms
//
// Drawing primitives stay narrow-width Unicode only (─ │ ◄ ►). The
// prism↔merchant "forward" hops use the same glyphs but with single-
// space gaps so they read as dashed — paycli never observes them
// directly, they're a structural hint that prism proxies HTTP to the
// L402-protected merchant backend.
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
	layout      laneLayout
}

// laneLayout fixes the column positions + header labels for one custody
// route. Computed once at tracer construction so every arrow renders
// into consistent columns regardless of label length. node mode drops
// the wallet lane and shrinks the canvas accordingly so the diagram
// stays comfortable in an 80-col terminal even with a side-note.
type laneLayout struct {
	client    int
	prism     int
	merchant  int
	wallet    int // 0 when hasWallet is false
	width     int
	hasWallet bool

	labels struct {
		client, prism, merchant, wallet string
	}
}

func laneLayoutFor(route string) laneLayout {
	if route == "node" {
		l := laneLayout{
			client:   9,
			prism:    25,
			merchant: 41,
			width:    47,
		}
		l.labels.client = "you (lnd)"
		l.labels.prism = "prism"
		l.labels.merchant = "merchant"
		return l
	}
	// hosted (default): four lanes with the remote wallet at the far right.
	l := laneLayout{
		client:    9,
		prism:     25,
		merchant:  41,
		wallet:    57,
		width:     62,
		hasWallet: true,
	}
	l.labels.client = "you"
	l.labels.prism = "prism"
	l.labels.merchant = "merchant"
	l.labels.wallet = "wallet"
	return l
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

	// Step 1/3 always show four arrows: you→prism (solid GET / GET+LSAT),
	// prism→merchant (dotted "forward"), merchant→prism (dotted resp),
	// prism→you (solid resp). paycli only observes the outer pair, but
	// the dotted middle pair makes the gateway's proxy role explicit
	// (per L402 — prism gates on macaroon+preimage and forwards on
	// success).
	outLabel := req.Method
	if stepNum == 3 {
		outLabel = req.Method + " + LSAT"
	}
	fmt.Fprintln(t.w, t.arrowRight(stepNum, t.layout.client, t.layout.prism, outLabel, ""))

	if err != nil {
		fmt.Fprintln(t.w, t.arrowLeft(0, t.layout.client, t.layout.prism, "✗ network error", elapsed.String()))
		fmt.Fprintf(t.w, "%s%v\n\n", strings.Repeat(" ", t.layout.width+1), err)
		return nil, err
	}
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
	// Dotted hops: prism ⇢ merchant ⇠ prism. paycli doesn't time these
	// independently (they roll into elapsed on the outer return), so
	// the side-note stays on the outer return arrow.
	fmt.Fprintln(t.w, t.arrowRightDotted(t.layout.prism, t.layout.merchant, "forward"))
	fmt.Fprintln(t.w, t.arrowLeftDotted(t.layout.prism, t.layout.merchant, inLabel))
	fmt.Fprintln(t.w, t.arrowLeft(0, t.layout.client, t.layout.prism, inLabel, inNote))
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
	if t.route == "node" {
		return "node route (local lnd — self-custody)"
	}
	if t.walletAlias != "" {
		return fmt.Sprintf("hosted route (sub-wallet %q on agents-pay-service)", t.walletAlias)
	}
	return "hosted route (agents-pay-service)"
}

// paid is called by L402Doer.OnPaid after a successful invoice settle.
// Renders step 2 differently depending on custody:
//
//   - hosted: cross-lane arrow you ⇄ wallet ("pay invoice" / "preimage").
//   - node:   self-loop annotation on the "you" lane, because the
//     wallet is the same lnd that issued the request — there's no
//     remote counterparty.
func (t *debugTracer) paid(ch *sdk.Challenge, paid *sdk.Payment) {
	chain := chainFromExtra(paid.Extra)
	var amtNote string
	if paid.Amount != 0 {
		amtNote = formatFriendlyAmount(paid.Amount, chain)
	}
	receiptNote := ""
	if paid.Preimage != "" {
		receiptNote = "receipt: " + truncate(paid.Preimage, 16)
	}

	if !t.layout.hasWallet {
		// node mode: render as a self-note on the "you" lane.
		note := "↻ paid via local lnd"
		if amtNote != "" {
			note += " · " + amtNote
		}
		fmt.Fprintln(t.w, t.selfNote(2, t.layout.client, note))
		if receiptNote != "" {
			fmt.Fprintln(t.w, t.laneBarsWithNote(receiptNote))
		}
		fmt.Fprintln(t.w, t.laneBars())
		_ = ch
		return
	}

	// hosted mode: cross-lane arrow you ⇄ wallet (passes over prism +
	// merchant lanes — visually crosses them, semantically bypasses
	// them since the wallet settles independently of the gateway).
	fmt.Fprintln(t.w, t.arrowRight(2, t.layout.client, t.layout.wallet, "pay invoice", ""))
	fmt.Fprintln(t.w, t.arrowLeft(0, t.layout.client, t.layout.wallet, "preimage", amtNote))
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

// laneHeader produces the column header row, e.g. for hosted:
//
//	"        you          prism         merchant       wallet"
func (t *debugTracer) laneHeader() string {
	buf := t.blankLine()
	placeCentered(buf, t.layout.client, t.layout.labels.client)
	placeCentered(buf, t.layout.prism, t.layout.labels.prism)
	placeCentered(buf, t.layout.merchant, t.layout.labels.merchant)
	if t.layout.hasWallet {
		placeCentered(buf, t.layout.wallet, t.layout.labels.wallet)
	}
	return strings.TrimRight(string(buf), " ")
}

// laneBars draws the baseline row showing each active lane as a
// vertical bar without any message.
func (t *debugTracer) laneBars() string {
	buf := t.blankLine()
	buf[t.layout.client] = '│'
	buf[t.layout.prism] = '│'
	buf[t.layout.merchant] = '│'
	if t.layout.hasWallet {
		buf[t.layout.wallet] = '│'
	}
	return string(buf)
}

// blankLine returns a width-sized buffer of spaces ready to overlay.
// Centralised so every primitive matches t.layout.width exactly.
func (t *debugTracer) blankLine() []rune {
	buf := make([]rune, t.layout.width)
	for i := range buf {
		buf[i] = ' '
	}
	return buf
}

// laneCols returns the column positions of all active lanes, so the
// arrow drawers can re-paint lane bars they crossed.
func (t *debugTracer) laneCols() []int {
	if t.layout.hasWallet {
		return []int{t.layout.client, t.layout.prism, t.layout.merchant, t.layout.wallet}
	}
	return []int{t.layout.client, t.layout.prism, t.layout.merchant}
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

// arrowRight draws a solid arrow from fromCol → toCol (fromCol < toCol)
// with label centred over the dashes. stepNum > 0 stamps "  N. " at
// the line start; pass 0 to skip. note (non-empty) appears past the
// last lane as a side annotation.
func (t *debugTracer) arrowRight(stepNum, fromCol, toCol int, label, note string) string {
	buf := t.blankLine()
	// All active lane bars first, then we overwrite with the arrow.
	for _, c := range t.laneCols() {
		buf[c] = '│'
	}
	if stepNum > 0 {
		stepLabel := fmt.Sprintf("  %d.", stepNum)
		for i, r := range stepLabel {
			if i < len(buf) {
				buf[i] = r
			}
		}
	}
	for i := fromCol + 1; i < toCol; i++ {
		buf[i] = '─'
	}
	buf[toCol-1] = '►'
	placeOverlay(buf, fromCol+1, toCol-2, " "+label+" ")
	buf[fromCol] = '│'
	buf[toCol] = '│'
	return appendNote(string(buf), note)
}

// arrowLeft draws a solid arrow from the right lane (toCol) BACK to
// the left lane (fromCol) — response direction. stepNum=0 always.
func (t *debugTracer) arrowLeft(stepNum, fromCol, toCol int, label, note string) string {
	buf := t.blankLine()
	for _, c := range t.laneCols() {
		buf[c] = '│'
	}
	if stepNum > 0 {
		stepLabel := fmt.Sprintf("  %d.", stepNum)
		for i, r := range stepLabel {
			if i < len(buf) {
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

// arrowRightDotted is the same shape as arrowRight but uses spaced
// dashes ("─ ─ ─►") to mark a hop paycli doesn't directly observe —
// specifically the prism → merchant proxy step. Always emits with no
// step prefix, no side note: the surrounding solid arrows own those.
func (t *debugTracer) arrowRightDotted(fromCol, toCol int, label string) string {
	buf := t.blankLine()
	for _, c := range t.laneCols() {
		buf[c] = '│'
	}
	// Paint dash-on-even, space-on-odd inside the arrow span so the
	// glyph reads as dashed against the surrounding solid bars.
	for i := fromCol + 1; i < toCol; i++ {
		if (i-fromCol)%2 == 1 {
			buf[i] = '─'
		}
	}
	buf[toCol-1] = '►'
	placeOverlay(buf, fromCol+1, toCol-2, " "+label+" ")
	buf[fromCol] = '│'
	buf[toCol] = '│'
	return string(buf)
}

// arrowLeftDotted mirrors arrowRightDotted for the reverse hop.
func (t *debugTracer) arrowLeftDotted(fromCol, toCol int, label string) string {
	buf := t.blankLine()
	for _, c := range t.laneCols() {
		buf[c] = '│'
	}
	for i := fromCol + 1; i < toCol; i++ {
		if (i-fromCol)%2 == 1 {
			buf[i] = '─'
		}
	}
	buf[fromCol+1] = '◄'
	placeOverlay(buf, fromCol+2, toCol-1, " "+label+" ")
	buf[fromCol] = '│'
	buf[toCol] = '│'
	return string(buf)
}

// selfNote prints a "self-loop" line — used in node mode for the
// payment step, where the wallet is the same node that's making the
// request so no cross-lane arrow makes sense. Renders as
//
//	"  2.        │  ↻ paid via local lnd · 0.01 SUI"
//
// keeping the lane bars intact for the other lanes so the diagram
// stays grid-aligned.
func (t *debugTracer) selfNote(stepNum, col int, note string) string {
	buf := t.blankLine()
	for _, c := range t.laneCols() {
		buf[c] = '│'
	}
	if stepNum > 0 {
		stepLabel := fmt.Sprintf("  %d.", stepNum)
		for i, r := range stepLabel {
			if i < len(buf) {
				buf[i] = r
			}
		}
	}
	// Write the note immediately to the right of the lane bar.
	start := col + 2
	for i, r := range note {
		p := start + i
		if p < len(buf) {
			buf[p] = r
		}
	}
	return string(buf)
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
