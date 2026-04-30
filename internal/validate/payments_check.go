package validate

import (
	"context"
	"math"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/payments"
)

// runPaymentProcessing is Check 12.
//
// Asserts:
//
//	a. payments.jsonl exists (driver ran).
//	b. The realized success rate sits within tolerance of scenario.payments.success_pct.
//	c. Every "succeeded" record carries a Stripe payment_intent_id.
//	d. Every "declined" or "insufficient" record has a non-empty failure_code.
//
// SKIPs cleanly when payments.jsonl isn't present (the run didn't drive
// payments — common for ingestion-only crawl/walk scenarios).
func (v *Validator) runPaymentProcessing(_ context.Context) *CheckResult {
	res := NewCheckResult(CheckPaymentProcessing)
	records, err := payments.Load(v.in.RunOutputDir)
	if err != nil {
		return res.Fail("load payments.jsonl: %v", err)
	}
	if len(records) == 0 {
		if !v.in.Scenario.Payments.Enabled {
			return res.Skip("payments not enabled in scenario")
		}
		return res.Skip("no payments.jsonl in run output — payments driver didn't run")
	}

	var success, decline, insuf, errCount int
	var missingIntent, missingFailureCode int
	for _, r := range records {
		switch r.Outcome {
		case string(payments.OutcomeSucceeded):
			success++
			if r.StripeIntentID == "" {
				missingIntent++
			}
		case string(payments.OutcomeDeclined):
			decline++
			if r.FailureCode == "" {
				missingFailureCode++
			}
		case string(payments.OutcomeInsufficientFunds):
			insuf++
			if r.FailureCode == "" {
				missingFailureCode++
			}
		case string(payments.OutcomeError):
			errCount++
		}
	}
	total := len(records)
	successRate := float64(success) / float64(total)
	declineRate := float64(decline+insuf) / float64(total)

	res.Set("total_records", total)
	res.Set("success_count", success)
	res.Set("decline_count", decline)
	res.Set("insufficient_count", insuf)
	res.Set("error_count", errCount)
	res.Set("success_rate", successRate)
	res.Set("decline_rate", declineRate)
	res.Set("missing_intent_id", missingIntent)
	res.Set("missing_failure_code", missingFailureCode)

	pTol := 0.05 // 5pp tolerance — load tests have small populations
	if v.in.Scenario.Payments.SuccessPct > 0 {
		expected := v.in.Scenario.Payments.SuccessPct
		if math.Abs(successRate-expected) > pTol {
			return res.Fail("success rate %.3f drifts > %.2fpp from scenario %.3f",
				successRate, pTol, expected)
		}
	}
	if missingIntent > 0 {
		return res.Fail("%d successful charges missing stripe payment_intent_id (acceptance criterion violated)", missingIntent)
	}
	if missingFailureCode > 0 {
		return res.Fail("%d declined/insufficient records missing failure_code (acceptance criterion violated)", missingFailureCode)
	}
	return res.Pass()
}
