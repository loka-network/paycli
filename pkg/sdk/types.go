package sdk

// HostedWallet mirrors the agents-pay-service Wallet response object — i.e.
// a sub-wallet record on the custodial LNbits side. It is distinct from the
// node-mode case where the "wallet" lives inside the user's own lnd-sui.
// Only fields the SDK and CLI consume are modeled; the upstream payload
// carries additional bookkeeping that callers may inspect via raw JSON.
type HostedWallet struct {
	ID          string `json:"id"`
	User        string `json:"user"`
	Name        string `json:"name"`
	AdminKey    string `json:"adminkey"`
	InvoiceKey  string `json:"inkey"`
	BalanceMsat int64  `json:"balance_msat"`
	Currency    string `json:"currency,omitempty"`
}

// CreateAccountRequest is the body for POST /api/v1/account.
type CreateAccountRequest struct {
	Name string `json:"name"`
}

// CreateWalletRequest is the body for POST /api/v1/wallet.
type CreateWalletRequest struct {
	Name string `json:"name"`
}

// CreateInvoiceRequest is the body for POST /api/v1/payments when receiving.
// Set Out=false. Amount is in the wallet's base unit (sat for BTC, MIST for SUI).
type CreateInvoiceRequest struct {
	Out             bool                   `json:"out"`
	Amount          int64                  `json:"amount"`
	Memo            string                 `json:"memo,omitempty"`
	Unit            string                 `json:"unit,omitempty"`
	Expiry          int                    `json:"expiry,omitempty"`
	Webhook         string                 `json:"webhook,omitempty"`
	DescriptionHash string                 `json:"description_hash,omitempty"`
	Internal        bool                   `json:"internal,omitempty"`
	FiatProvider    string                 `json:"fiat_provider,omitempty"`
	Extra           map[string]interface{} `json:"extra,omitempty"`
}

// PayInvoiceRequest is the body for POST /api/v1/payments when spending.
// Set Out=true. Requires admin_key.
type PayInvoiceRequest struct {
	Out    bool                   `json:"out"`
	Bolt11 string                 `json:"bolt11"`
	Extra  map[string]interface{} `json:"extra,omitempty"`
}

// Payment is the response object from create/pay invoice and the listing endpoints.
// Only the fields the SDK consumes are modeled; everything else is preserved
// in Extra so callers can introspect server-side detail.
//
// Time is left untyped because LNbits serializes it as either a Unix epoch
// integer or an ISO-8601 string depending on the database backend. Callers
// that need a typed timestamp should re-parse it themselves.
type Payment struct {
	CheckingID     string                 `json:"checking_id"`
	PaymentHash    string                 `json:"payment_hash"`
	WalletID       string                 `json:"wallet_id"`
	Amount         int64                  `json:"amount"`
	Fee            int64                  `json:"fee"`
	Status         string                 `json:"status"`
	Memo           string                 `json:"memo"`
	Time           interface{}            `json:"time,omitempty"`
	Bolt11         string                 `json:"bolt11"`
	Preimage       string                 `json:"preimage"`
	PaymentRequest string                 `json:"payment_request"`
	Extra          map[string]interface{} `json:"extra,omitempty"`
}

// IsSuccess reports whether the payment was settled.
func (p *Payment) IsSuccess() bool { return p.Status == "success" }

// IsFailed reports whether the payment terminated unsuccessfully.
func (p *Payment) IsFailed() bool { return p.Status == "failed" }

// IsPending reports whether the payment is still in flight.
func (p *Payment) IsPending() bool {
	return p.Status == "pending" || p.Status == ""
}

// HostedWalletStatus is the lightweight response from GET /api/v1/wallet
// (invoice key). Distinct from HostedWallet which is the full wallet record
// returned by account/wallet creation.
type HostedWalletStatus struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Balance int64  `json:"balance"`
}
