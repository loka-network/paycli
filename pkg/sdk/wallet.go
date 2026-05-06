package sdk

import "context"

// Wallet abstracts the spend-side surface that L402Doer depends on. Both
// the hosted Client (agents-pay-service REST) and NodeClient (lnd-sui REST
// gateway) satisfy it, so L402Doer can drive either route transparently.
//
// The returned Payment must, on success, populate Status="success" and
// Preimage with the 32-byte preimage hex-encoded — that is what gets folded
// into the LSAT Authorization header.
type Wallet interface {
	PayInvoice(ctx context.Context, bolt11 string) (*Payment, error)
}

// Compile-time guards: the two concrete clients satisfy Wallet.
var (
	_ Wallet = (*Client)(nil)
	_ Wallet = (*NodeClient)(nil)
)
