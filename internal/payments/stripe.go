// Package payments orchestrates the post-invoice payment lifecycle for
// load tests:
//
//   - choose a Stripe test card per scenario.payments mix
//   - drive an Aforo /payments/intent endpoint that proxies to Stripe
//   - record the resulting payment_intent / charge id back onto the
//     invoice (so Check 12 can assert it later)
//   - on decline, drive the dunning sequence to its terminal state
//
// "Stripe integration" here is the THIN HTTP client we use to validate the
// Stripe-side artifact independently of what the platform recorded — the
// platform itself talks to Stripe; we only need to verify the platform's
// claim. When STRIPE_TEST_SECRET_KEY is unset, the stripe client runs in
// "synthesized" mode: it generates plausible test ids and matches them
// against the platform's recorded ids. This keeps CI offline.
package payments

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Stripe test card numbers — sourced directly from
// https://stripe.com/docs/testing#cards.
const (
	TestCardSuccess           = "4242424242424242"
	TestCardDeclineGeneric    = "4000000000000002"
	TestCardDeclineInsufFunds = "4000000000009995"
	TestCardRequires3DS       = "4000002500003155" // future use
)

// PaymentOutcome categorizes the post-call state. The driver maps Stripe
// raw error codes / charge statuses into one of these.
type PaymentOutcome string

const (
	OutcomeSucceeded         PaymentOutcome = "succeeded"
	OutcomeRequiresAction    PaymentOutcome = "requires_action"
	OutcomeDeclined          PaymentOutcome = "declined"
	OutcomeInsufficientFunds PaymentOutcome = "insufficient_funds"
	OutcomeError             PaymentOutcome = "error" // network / API contract failure
)

// StripeClient is the minimal Stripe API shim used by the payment driver.
//
// It supports two modes:
//
//	live      — real Stripe API calls when STRIPE_TEST_SECRET_KEY is set.
//	            ALWAYS uses sk_test_ keys; refuses sk_live_ to avoid
//	            production blast in any environment.
//
//	offline   — synthesizes plausible payment intent ids based on the
//	            requested outcome. Used in CI where no Stripe creds exist.
//	            The validator then compares against the platform's recorded
//	            id by structure, not exact match.
//
// Concurrency: safe for use by many goroutines. The http.Client is shared
// (idle-conn pool); offline-mode synthesis is stateless.
type StripeClient struct {
	apiKey       string
	httpClient   *http.Client
	mode         Mode
	offlineSeed  uint64
}

// Mode selects live vs offline behavior.
type Mode string

const (
	ModeLive    Mode = "live"
	ModeOffline Mode = "offline"
)

// Config configures NewStripeClient.
//
// ForceMode overrides env-var detection — useful in tests. ApiKey, when
// non-empty, overrides the env lookup.
//
// MaxIdleConnsPerHost defaults to 32. Timeout defaults to 15s.
type Config struct {
	APIKey              string
	ForceMode           Mode
	MaxIdleConnsPerHost int
	Timeout             time.Duration
}

// NewStripeClient constructs the client. Returns an error only on
// misconfiguration (live mode requested with no key + invalid key prefix).
//
// The "live" mode REJECTS keys starting with "sk_live_" — this tool never
// targets real Stripe accounts. CI safety: the binary refuses to start
// against a production key by default.
func NewStripeClient(cfg Config) (*StripeClient, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = 32
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	httpClient := &http.Client{Transport: transport, Timeout: cfg.Timeout}

	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("STRIPE_TEST_SECRET_KEY"))
	}

	mode := cfg.ForceMode
	if mode == "" {
		if apiKey == "" {
			mode = ModeOffline
		} else {
			mode = ModeLive
		}
	}

	if mode == ModeLive {
		if apiKey == "" {
			return nil, errors.New("payments: STRIPE_TEST_SECRET_KEY required in live mode")
		}
		if strings.HasPrefix(apiKey, "sk_live_") {
			return nil, errors.New("payments: refusing sk_live_ key — this tool never points at production Stripe")
		}
		if !strings.HasPrefix(apiKey, "sk_test_") {
			return nil, errors.New("payments: STRIPE_TEST_SECRET_KEY must start with sk_test_")
		}
	}

	return &StripeClient{
		apiKey:     apiKey,
		httpClient: httpClient,
		mode:       mode,
	}, nil
}

// Mode returns the active mode.
func (c *StripeClient) Mode() Mode { return c.mode }

// Charge represents the Stripe-side outcome of a single payment attempt.
// Mirrors the subset of PaymentIntent + Charge fields we need.
type Charge struct {
	PaymentIntentID string         `json:"payment_intent_id"`
	ChargeID        string         `json:"charge_id"`
	Status          string         `json:"status"`         // raw Stripe status
	Outcome         PaymentOutcome `json:"outcome"`        // normalized
	AmountUSD       float64        `json:"amount_usd"`
	Currency        string         `json:"currency"`
	IdempotencyKey  string         `json:"idempotency_key"`
	FailureCode     string         `json:"failure_code,omitempty"`
	FailureMessage  string         `json:"failure_message,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

// CreatePaymentIntent posts /v1/payment_intents. In offline mode, returns a
// deterministic synthesized result keyed off (idempotencyKey, card).
//
// Idempotency-Key is REQUIRED on every call (acceptance criterion: "All
// Stripe API calls have idempotency keys"). Caller must supply one.
func (c *StripeClient) CreatePaymentIntent(
	ctx context.Context,
	amountUSD float64,
	currency, card, idempotencyKey, customerHint string,
) (*Charge, error) {
	if idempotencyKey == "" {
		return nil, errors.New("payments: idempotency_key is required for every Stripe API call")
	}
	if amountUSD <= 0 {
		return nil, fmt.Errorf("payments: amount_usd %v must be > 0", amountUSD)
	}
	if c.mode == ModeOffline {
		return c.offlineCharge(amountUSD, currency, card, idempotencyKey), nil
	}
	return c.liveCharge(ctx, amountUSD, currency, card, idempotencyKey, customerHint)
}

func (c *StripeClient) liveCharge(
	ctx context.Context,
	amountUSD float64,
	currency, card, idempotencyKey, customerHint string,
) (*Charge, error) {
	form := url.Values{}
	form.Set("amount", fmt.Sprintf("%d", int64(amountUSD*100))) // Stripe wants cents
	form.Set("currency", strings.ToLower(orDefault(currency, "usd")))
	form.Set("payment_method_data[type]", "card")
	form.Set("payment_method_data[card][number]", card)
	form.Set("payment_method_data[card][exp_month]", "12")
	form.Set("payment_method_data[card][exp_year]", fmt.Sprintf("%d", time.Now().Year()+1))
	form.Set("payment_method_data[card][cvc]", "123")
	form.Set("confirm", "true")
	form.Set("description", fmt.Sprintf("aforo-loadgen %s", customerHint))

	endpoint := "https://api.stripe.com/v1/payment_intents"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("payments: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	httpReq.Header.Set("Stripe-Version", "2024-04-10")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("payments: stripe transport: %w", err)
	}
	defer resp.Body.Close()

	var raw stripePaymentIntent
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("payments: stripe decode: %w", err)
	}

	chg := &Charge{
		PaymentIntentID: raw.ID,
		ChargeID:        firstChargeID(raw),
		Status:          raw.Status,
		Outcome:         classifyStripeStatus(raw.Status, raw.LastPaymentError),
		AmountUSD:       amountUSD,
		Currency:        currency,
		IdempotencyKey:  idempotencyKey,
		CreatedAt:       time.Now().UTC(),
	}
	if raw.LastPaymentError != nil {
		chg.FailureCode = raw.LastPaymentError.Code
		chg.FailureMessage = raw.LastPaymentError.Message
	}
	return chg, nil
}

// offlineCharge returns a deterministic synthetic outcome derived from the
// card number — the test cards are chosen by the driver to express intent.
// The validator can match by Outcome alone in offline mode.
func (c *StripeClient) offlineCharge(amountUSD float64, currency, card, idempotencyKey string) *Charge {
	pi := "pi_offline_" + base64.RawURLEncoding.EncodeToString([]byte(idempotencyKey))
	if len(pi) > 28 {
		pi = pi[:28]
	}
	chargeID := strings.Replace(pi, "pi_offline_", "ch_offline_", 1)
	switch card {
	case TestCardDeclineGeneric:
		return &Charge{
			PaymentIntentID: pi,
			ChargeID:        chargeID,
			Status:          "requires_payment_method",
			Outcome:         OutcomeDeclined,
			AmountUSD:       amountUSD, Currency: currency,
			IdempotencyKey: idempotencyKey,
			FailureCode:    "card_declined",
			FailureMessage: "Your card was declined.",
			CreatedAt:      time.Now().UTC(),
		}
	case TestCardDeclineInsufFunds:
		return &Charge{
			PaymentIntentID: pi,
			ChargeID:        chargeID,
			Status:          "requires_payment_method",
			Outcome:         OutcomeInsufficientFunds,
			AmountUSD:       amountUSD, Currency: currency,
			IdempotencyKey: idempotencyKey,
			FailureCode:    "insufficient_funds",
			FailureMessage: "Your card has insufficient funds.",
			CreatedAt:      time.Now().UTC(),
		}
	case TestCardRequires3DS:
		return &Charge{
			PaymentIntentID: pi,
			ChargeID:        chargeID,
			Status:          "requires_action",
			Outcome:         OutcomeRequiresAction,
			AmountUSD:       amountUSD, Currency: currency,
			IdempotencyKey: idempotencyKey,
			CreatedAt:      time.Now().UTC(),
		}
	default:
		// Default + TestCardSuccess succeed.
		return &Charge{
			PaymentIntentID: pi,
			ChargeID:        chargeID,
			Status:          "succeeded",
			Outcome:         OutcomeSucceeded,
			AmountUSD:       amountUSD, Currency: currency,
			IdempotencyKey: idempotencyKey,
			CreatedAt:      time.Now().UTC(),
		}
	}
}

// classifyStripeStatus turns raw Stripe statuses into one of our normalized
// outcomes. Mapping:
//
//	succeeded                  → OutcomeSucceeded
//	requires_action            → OutcomeRequiresAction
//	requires_payment_method +
//	  err.code=insufficient_funds → OutcomeInsufficientFunds
//	requires_payment_method    → OutcomeDeclined
//	canceled / failed / *      → OutcomeError
func classifyStripeStatus(status string, lastErr *stripeError) PaymentOutcome {
	switch status {
	case "succeeded":
		return OutcomeSucceeded
	case "requires_action":
		return OutcomeRequiresAction
	case "requires_payment_method":
		if lastErr != nil && lastErr.Code == "insufficient_funds" {
			return OutcomeInsufficientFunds
		}
		return OutcomeDeclined
	}
	return OutcomeError
}

type stripePaymentIntent struct {
	ID               string        `json:"id"`
	Status           string        `json:"status"`
	Charges          stripeCharges `json:"charges"`
	LastPaymentError *stripeError  `json:"last_payment_error"`
}

type stripeCharges struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type stripeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func firstChargeID(p stripePaymentIntent) string {
	if len(p.Charges.Data) > 0 {
		return p.Charges.Data[0].ID
	}
	return ""
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
