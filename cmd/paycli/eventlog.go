package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Event types — keep stable; downstream tools will pattern-match on these.
const (
	EventAccountCreated = "account_created" // hosted register succeeded
	EventInvoiceCreated = "invoice_created" // fund: minted a BOLT11 invoice
	EventPaySent        = "pay_sent"        // pay: settled an external invoice
	EventL402Paid       = "l402_paid"       // request: paid an HTTP 402 challenge
	EventTopupCredit    = "topup_credit"    // operator topup: synthetic credit
)

// Event is one structured row in ~/.paycli/events.jsonl. Fields are
// kept omitempty so a single struct fits every event type without
// littering the log with empty strings.
//
// Amount semantics: positive = funds INTO the wallet (invoice_created
// at mint time is informational; topup_credit credits the wallet).
// Negative = funds LEAVING the wallet (pay_sent, l402_paid).
type Event struct {
	Timestamp      string `json:"ts"`                 // RFC3339Nano
	Event          string `json:"event"`              // see consts above
	Route          string `json:"route,omitempty"`    // hosted | node
	Endpoint       string `json:"endpoint,omitempty"` // base_url / lnd endpoint
	WalletID       string `json:"wallet_id,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	Amount         int64  `json:"amount,omitempty"`         // wallet's base unit (sat / MIST / msat depending on route)
	Unit           string `json:"unit,omitempty"`           // sat / MIST / msat — disambiguates Amount
	PaymentHash    string `json:"payment_hash,omitempty"`   // hex
	Preimage       string `json:"preimage,omitempty"`       // hex (only on settled payments)
	Status         string `json:"status,omitempty"`         // success / failed / pending / SUCCEEDED / etc.
	Memo           string `json:"memo,omitempty"`
	PaymentRequest string `json:"payment_request,omitempty"` // bolt11 (for invoice_created only)
	TargetURL      string `json:"target_url,omitempty"`      // l402_paid: the URL we requested
	TargetHost     string `json:"target_host,omitempty"`     // l402_paid: Host header / hostregexp
	Error          string `json:"error,omitempty"`           // human-readable failure reason when Status != success
}

// eventLogPath returns the on-disk path for the events log. Defaults to
// {dirname(configPath)}/events.jsonl, overridable via $PAYCLI_EVENT_LOG.
//
// Setting $PAYCLI_EVENT_LOG=off (any value matching "off"/"0"/"false")
// disables logging entirely; LogEvent becomes a no-op.
func eventLogPath() (string, bool) {
	switch strings.ToLower(os.Getenv("PAYCLI_EVENT_LOG")) {
	case "off", "0", "false", "no":
		return "", false
	}
	if p := os.Getenv("PAYCLI_EVENT_LOG"); p != "" {
		return p, true
	}
	cp, err := configPath()
	if err != nil {
		return "", true
	}
	return filepath.Join(filepath.Dir(cp), "events.jsonl"), true
}

// LogEvent appends `e` (with Timestamp filled in if empty) as a single
// JSON line to the event log. Best-effort: failure to write is logged
// to stderr but never returned to the caller, since payment commands
// must not fail just because we couldn't write a side-channel log.
//
// Concurrency: the underlying os.File.Write with O_APPEND is atomic for
// writes ≤ PIPE_BUF (≥ 512 on POSIX), and our serialized lines are well
// under that limit, so concurrent paycli invocations interleave safely.
func LogEvent(e Event) {
	path, enabled := eventLogPath()
	if !enabled || path == "" {
		return
	}
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "paycli: event log mkdir failed: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "paycli: event log open failed: %v\n", err)
		return
	}
	defer f.Close()

	line, err := json.Marshal(e)
	if err != nil {
		fmt.Fprintf(os.Stderr, "paycli: event log marshal failed: %v\n", err)
		return
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		fmt.Fprintf(os.Stderr, "paycli: event log write failed: %v\n", err)
	}
}

// readEvents loads the event log from disk in chronological order. The
// `events` CLI reads this and applies whatever filters / limits the user
// asked for in memory. For very large logs we could stream + filter as
// we go, but day-to-day a payment CLI's log stays small.
func readEvents() ([]Event, string, error) {
	path, enabled := eventLogPath()
	if !enabled || path == "" {
		return nil, "", nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	defer f.Close()

	var events []Event
	dec := json.NewDecoder(f)
	for dec.More() {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return events, path, fmt.Errorf("decode event log near offset %d: %w",
				dec.InputOffset(), err)
		}
		events = append(events, e)
	}
	return events, path, nil
}
