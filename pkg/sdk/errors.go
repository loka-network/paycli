package sdk

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the SDK so callers can branch on common
// conditions without parsing message strings.
var (
	ErrUnauthorized       = errors.New("paycli: unauthorized — check api key")
	ErrAdminKeyRequired   = errors.New("paycli: admin key required for this operation")
	ErrPaymentFailed      = errors.New("paycli: payment failed")
	ErrInvalidChallenge   = errors.New("paycli: invalid L402 challenge header")
	ErrChallengeExhausted = errors.New("paycli: L402 retry budget exhausted")
)

// APIError wraps a non-2xx response from agents-pay-service. The Detail
// field carries the upstream `detail` JSON field when present.
type APIError struct {
	Status int
	Detail string
	Body   string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("paycli: api error %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("paycli: api error %d: %s", e.Status, e.Body)
}
