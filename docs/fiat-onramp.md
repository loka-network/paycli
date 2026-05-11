# Fiat onramp (Stripe / PayPal)

When users top up via credit card or PayPal instead of Lightning, the
flow is:

```
paycli fund --amount 5 --unit USD --via stripe
       │
       ▼
agents-pay-service POST /api/v1/payments {fiat_provider: "stripe", ...}
       │
       ▼  (no lnd, no BOLT11)
fiat provider create_invoice() → Stripe Checkout Session URL
       │
       ▼
user opens URL, pays card / PayPal
       │
       ▼
provider POSTs webhook to /api/v1/callback/{stripe,paypal}
       │
       ▼
agents-pay-service verifies signature, credits the sub-wallet
```

The Lightning side (alice's lnd, channels, MIST) is **not involved at
all**. Fiat providers credit balance directly into the sub-wallet
ledger. Settlement is internal to lnbits.

This doc covers operator setup. For the CLI flag itself see
[`cli.md` → `paycli fund --via`](./cli.md#paycli-fund--amount-n---unit-satmistsuifiat---memo----expiry-seconds---via-stripepaypal---open).

---

## Stripe

### 1. Get API + webhook secrets

In the [Stripe Dashboard](https://dashboard.stripe.com):

1. **Activate test mode** (toggle top-right). Use this for staging /
   smoke tests — *never* point production at test keys.
2. **Developers → API keys** — copy the **Secret key**
   (`sk_test_...` for test, `sk_live_...` for prod). This is what the
   server uses to mint Checkout Sessions.
3. **Developers → Webhooks → Add endpoint**:
   - Endpoint URL: `https://<your-agents-pay-service-domain>/api/v1/callback/stripe`
   - Events to listen for: at minimum
     - `checkout.session.completed`
     - `payment_intent.succeeded`
     - `invoice.paid` (if you'll use subscriptions)
   - After saving, click into the endpoint, **Reveal signing secret** →
     copy `whsec_...`.
4. *(optional)* Note the **Publishable key** if you ever embed Stripe
   Elements directly; the paycli flow doesn't need it.

### 2. Configure agents-pay-service

Easiest is to PATCH the admin settings via the REST API. You need an
admin JWT — a super-user account's bearer token (see
[`paycli auth-login` / register flow](./cli.md)).

```bash
ADMIN_JWT='eyJhbGciOi...'  # super-user JWT from /api/v1/auth/login
BASE=http://127.0.0.1:5002

curl -X PATCH "$BASE/admin/api/v1/settings" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "stripe_enabled": true,
    "stripe_api_secret_key": "sk_test_51XXXXXXXXXXXXXXXXX",
    "stripe_webhook_signing_secret": "whsec_XXXXXXXXXXXXXXXXX",
    "stripe_payment_webhook_url": "https://<your-domain>/api/v1/callback/stripe",
    "stripe_payment_success_url": "https://<your-domain>/payments?paid=1"
  }'
```

Equivalent settings in the admin UI: **Settings → Fiat → Stripe**.

If running locally, expose `:5002` to Stripe via a tunnel
(`stripe listen --forward-to localhost:5002/api/v1/callback/stripe` for
test mode, or `cloudflared tunnel`/`ngrok http 5002` for arbitrary
endpoints) — Stripe can't reach 127.0.0.1.

### 3. Verify

```bash
# server reports the provider as enabled (uses admin JWT)
curl -s "$BASE/api/v1/fiat/test/stripe" -H "Authorization: Bearer $ADMIN_JWT"
# → {"success": true, ...}
```

Then drive a real onramp from the user side:

```bash
paycli fund --amount 5 --unit USD --via stripe --memo "agent budget" --open
# → JSON response (payment_hash, payment_request=<checkout_url>, ...)
# → STRIPE checkout URL: https://checkout.stripe.com/c/pay/cs_test_...
# Browser opens to the Stripe-hosted card form.
```

Pay with the test card `4242 4242 4242 4242`, any future expiry, any
CVC. Stripe POSTs the webhook back; check the wallet balance:

```bash
paycli whoami
# active wallet's balance includes the credited amount minus any
# configured service fee.
```

### Test mode vs live mode

- Test secrets (`sk_test_…`, `whsec_…` from a test endpoint) only accept
  test cards and never charge money.
- Live secrets (`sk_live_…`, live webhook secret) accept real cards.
  You must verify your business in Stripe before live mode unlocks.
- Stripe lets you have multiple webhook endpoints — register a
  *separate* one per environment (staging vs prod) so you can rotate
  the staging signing secret without touching prod.

---

## PayPal

### 1. Get app credentials + webhook ID

In the [PayPal Developer Dashboard](https://developer.paypal.com/dashboard/applications):

1. **Apps & Credentials → Sandbox** (for test) or **Live**.
2. **Create App** (type: "Merchant"). After creation copy:
   - **Client ID** — `Aa…`
   - **Client Secret** — click "Show" then copy
3. Same app's settings → **Webhooks** → **Add Webhook**:
   - URL: `https://<your-domain>/api/v1/callback/paypal`
   - Event types: at minimum
     - `CHECKOUT.ORDER.APPROVED`
     - `PAYMENT.CAPTURE.COMPLETED`
     - `BILLING.SUBSCRIPTION.PAYMENT.SUCCEEDED` (only if subscriptions)
4. After save, the new webhook entry shows a **Webhook ID** (`WH-…`).
   PayPal uses this ID to look up the public cert needed for signature
   verification.

### 2. Configure agents-pay-service

```bash
curl -X PATCH "$BASE/admin/api/v1/settings" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "paypal_enabled": true,
    "paypal_api_endpoint": "https://api-m.sandbox.paypal.com",
    "paypal_client_id": "Aa…",
    "paypal_client_secret": "EH…",
    "paypal_webhook_id": "WH-…",
    "paypal_payment_webhook_url": "https://<your-domain>/api/v1/callback/paypal",
    "paypal_payment_success_url": "https://<your-domain>/payments?paid=1"
  }'
```

For production, change `paypal_api_endpoint` to
`https://api-m.paypal.com` and use Live credentials.

### 3. Verify

```bash
paycli fund --amount 9.99 --unit USD --via paypal --memo "agent budget" --open
# → PayPal approve_url: https://www.sandbox.paypal.com/checkoutnow?token=...
```

In sandbox, log in with a sandbox personal-account email/password from
the developer console (Apps → Sandbox Accounts) to complete the
checkout.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `Fiat provider 'stripe' is not enabled.` | `stripe_enabled` is false in admin settings, or the admin JWT used to PATCH wasn't a super-user. |
| `Cannot create payment request for 'stripe'.` | API secret rejected by Stripe (wrong key, test key in live mode, or live key in test mode). Check the Stripe dashboard logs. |
| `Webhook signature verification failed.` | `stripe_webhook_signing_secret` doesn't match the endpoint. Re-copy from Stripe dashboard → Webhooks → endpoint → Reveal signing secret. |
| paycli prints checkout URL but wallet doesn't credit after payment | Webhook isn't reaching the server. From the provider dashboard, look at the most recent delivery attempt (Stripe shows HTTP status + response body). For local dev, make sure your tunnel (`stripe listen` / `ngrok` / `cloudflared`) is running and points at the right port. |
| `Fiat provider cannot be used with satoshis.` | You passed `--unit sat`. Use a fiat code: `--unit USD` / `EUR` / etc. paycli's client guard catches this before it hits the server. |
| `--via requires hosted route` | Your active route is `node`. Run `paycli config set route hosted` to switch (and `paycli config set route node` to flip back when done). Fiat onramp only exists on the hosted route. |

## Operational notes

- **Reconciliation**: every fiat-funded payment row gets
  `fiat_provider="stripe"` (or `paypal`) and `extra.fiat_checking_id`
  pointing back at the provider's session/order ID, so you can
  cross-reference lnbits ledger ↔ Stripe dashboard 1:1.
- **Service fees**: `FiatProviderLimits.service_fee_percent` /
  `service_max_fee_sats` in the admin settings let you carve out a per-tx
  fee that goes to a designated wallet (`service_fee_wallet_id`).
- **Per-user gating**: `FiatProviderLimits.allowed_users` is a
  user-id allow-list. Empty list = anyone can use the provider; set it
  to lock fiat onramp down to specific accounts.
- **Subscriptions**: `POST /api/v1/fiat/subscriptions` (not yet exposed
  from paycli) drives recurring billing through the same provider
  config. Implement on top of `paycli fund --via` when needed.
