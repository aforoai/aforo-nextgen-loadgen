package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/spf13/cobra"
)

type BillRunRequest struct {
	BillingPeriodStart string `json:"billingPeriodStart"`
	BillingPeriodEnd   string `json:"billingPeriodEnd"`
	BillingPeriodLabel string `json:"billingPeriodLabel"`
	TargetScope        string `json:"targetScope"`
	AutoFinalize       bool   `json:"autoFinalize"`
	Environment        string `json:"environment"`
	Notes              string `json:"notes"`
}

type BillRunResponse struct {
	Success bool `json:"success"`
	Data    struct {
		ID                 string    `json:"id"`
		Status             string    `json:"status"`
		InvoicesGenerated  *int      `json:"invoicesGenerated"`
		CustomersProcessed int       `json:"customersProcessed"`
		ProgressPct        int       `json:"progressPct"`
		DurationMs         *int64    `json:"durationMs"`
		StartedAt          time.Time `json:"startedAt"`
		CompletedAt        time.Time `json:"completedAt"`
	} `json:"data"`
}

func newBillRunCmd() *cobra.Command {
	var (
		target         string
		tenantID       string
		periodStart    string
		periodEnd      string
		periodLabel    string
		targetScope    string
		autoFinalize   bool
		environment    string
		notes          string
		tokenEnv       string
		pollInterval   int
		maxWaitSeconds int
	)

	cmd := &cobra.Command{
		Use:   "billrun",
		Short: "Create and execute a bill run to generate invoices",
		Long: `Create and execute a bill run for a tenant to generate invoices from usage events.

This command:
1. Creates a bill run for the specified billing period
2. Executes the bill run asynchronously
3. Polls for completion
4. Reports invoice generation results

Examples:
  aforo-loadgen billrun --tenant-id demo123 \
    --period-start 2026-06-01T00:00:00Z \
    --period-end 2026-06-30T23:59:59Z \
    --target http://100.27.159.112

  aforo-loadgen billrun --tenant-id demo123 \
    --period-start 2026-06-01T00:00:00Z \
    --period-end 2026-06-30T23:59:59Z \
    --period-label "June 2026" \
    --target-scope ALL \
    --environment LIVE \
    --target local`,
		RunE: func(cmd *cobra.Command, args []string) error {
			token := os.Getenv(tokenEnv)
			if token == "" {
				return fmt.Errorf("env var %s is not set", tokenEnv)
			}

			targetURL, err := aforo.ResolveTarget(target)
			if err != nil {
				return fmt.Errorf("resolve target: %w", err)
			}
			baseURL, err := targetURL.URL(aforo.ServiceBilling)
			if err != nil {
				return fmt.Errorf("get billing URL: %w", err)
			}

			fmt.Printf("Creating bill run for tenant %s...\n", tenantID)
			fmt.Printf("Period: %s to %s\n", periodStart, periodEnd)
			fmt.Printf("Target: %s\n", baseURL)

			// Step 1: Create bill run
			billRunID, err := createBillRun(baseURL, token, tenantID, BillRunRequest{
				BillingPeriodStart: periodStart,
				BillingPeriodEnd:   periodEnd,
				BillingPeriodLabel: periodLabel,
				TargetScope:        targetScope,
				AutoFinalize:       autoFinalize,
				Environment:        environment,
				Notes:              notes,
			})
			if err != nil {
				return fmt.Errorf("create bill run: %w", err)
			}

			fmt.Printf("✓ Bill run created: %s\n", billRunID)

			// Step 2: Execute bill run
			fmt.Println("Executing bill run...")
			if err := executeBillRun(baseURL, token, tenantID, billRunID); err != nil {
				return fmt.Errorf("execute bill run: %w", err)
			}

			fmt.Println("✓ Bill run executing (async)")

			// Step 3: Poll for completion
			fmt.Printf("Polling for completion (interval: %ds, max wait: %ds)...\n", pollInterval, maxWaitSeconds)
			result, err := pollBillRun(baseURL, token, tenantID, billRunID, pollInterval, maxWaitSeconds)
			if err != nil {
				return fmt.Errorf("poll bill run: %w", err)
			}

			// Step 4: Report results
			fmt.Println("\n" + strings.Repeat("=", 60))
			fmt.Printf("Bill Run Complete: %s\n", billRunID)
			fmt.Println(strings.Repeat("=", 60))
			fmt.Printf("Status:              %s\n", result.Data.Status)
			fmt.Printf("Customers Processed: %d\n", result.Data.CustomersProcessed)
			if result.Data.InvoicesGenerated != nil {
				fmt.Printf("Invoices Generated:  %d\n", *result.Data.InvoicesGenerated)
			}
			if result.Data.DurationMs != nil {
				fmt.Printf("Duration:            %dms\n", *result.Data.DurationMs)
			}
			fmt.Printf("Progress:            %d%%\n", result.Data.ProgressPct)
			fmt.Println(strings.Repeat("=", 60))

			if result.Data.Status != "COMPLETED" {
				fmt.Printf("\n⚠️  Bill run status: %s (not COMPLETED)\n", result.Data.Status)
			} else {
				fmt.Println("\n✅ Bill run completed successfully!")
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "local", "target environment: local, staging, prod, or full URL")
	cmd.Flags().StringVar(&tenantID, "tenant-id", "", "tenant ID to run billing for (required)")
	cmd.Flags().StringVar(&periodStart, "period-start", "", "billing period start (ISO 8601, required)")
	cmd.Flags().StringVar(&periodEnd, "period-end", "", "billing period end (ISO 8601, required)")
	cmd.Flags().StringVar(&periodLabel, "period-label", "", "human-readable period label (e.g., 'June 2026')")
	cmd.Flags().StringVar(&targetScope, "target-scope", "ALL", "scope: ALL, CUSTOMER, SUBSCRIPTION, OFFERING")
	cmd.Flags().BoolVar(&autoFinalize, "auto-finalize", false, "automatically finalize generated invoices")
	cmd.Flags().StringVar(&environment, "environment", "LIVE", "environment: LIVE or TEST")
	cmd.Flags().StringVar(&notes, "notes", "Generated by aforo-loadgen billrun", "notes for the bill run")
	cmd.Flags().StringVar(&tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding the bearer token")
	cmd.Flags().IntVar(&pollInterval, "poll-interval", 2, "seconds between status polls")
	cmd.Flags().IntVar(&maxWaitSeconds, "max-wait", 300, "maximum seconds to wait for completion")

	_ = cmd.MarkFlagRequired("tenant-id")
	_ = cmd.MarkFlagRequired("period-start")
	_ = cmd.MarkFlagRequired("period-end")

	return cmd
}

func createBillRun(baseURL, token, tenantID string, req BillRunRequest) (string, error) {
	url := fmt.Sprintf("%s/api/v1/bill-runs", baseURL)
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("X-Tenant-Id", tenantID)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result BillRunResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}

	return result.Data.ID, nil
}

func executeBillRun(baseURL, token, tenantID, billRunID string) error {
	url := fmt.Sprintf("%s/api/v1/bill-runs/%s/execute", baseURL, billRunID)

	httpReq, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("X-Tenant-Id", tenantID)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 202 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("HTTP %d (failed to read body: %w)", resp.StatusCode, err)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func pollBillRun(baseURL, token, tenantID, billRunID string, intervalSec, maxWaitSec int) (*BillRunResponse, error) {
	url := fmt.Sprintf("%s/api/v1/bill-runs/%s", baseURL, billRunID)
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()

	timeout := time.After(time.Duration(maxWaitSec) * time.Second)
	consecutiveErrors := 0
	maxConsecutiveErrors := 5

	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("timeout after %ds", maxWaitSec)
		case <-ticker.C:
			httpReq, err := http.NewRequest("GET", url, nil)
			if err != nil {
				return nil, fmt.Errorf("create request: %w", err)
			}
			httpReq.Header.Set("Authorization", "Bearer "+token)
			httpReq.Header.Set("X-Tenant-Id", tenantID)

			resp, err := http.DefaultClient.Do(httpReq)
			if err != nil {
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					return nil, fmt.Errorf("too many consecutive errors: %w", err)
				}
				fmt.Printf("  ⚠️  Network error (retry %d/%d): %v\n", consecutiveErrors, maxConsecutiveErrors, err)
				continue
			}

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("read response: %w", err)
			}
			_ = resp.Body.Close()

			// Workaround for billing service controller mapping bug (same as ingestor had)
			// The GET endpoint sometimes returns 500 "No static resource" error
			// Retry a few times before giving up
			if resp.StatusCode == 500 {
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					return nil, fmt.Errorf("too many 500 errors - billing service may have controller mapping bug: %s", string(respBody))
				}
				fmt.Printf("  ⚠️  Server error 500 (retry %d/%d) - known billing service bug\n", consecutiveErrors, maxConsecutiveErrors)
				continue
			}

			if resp.StatusCode != 200 {
				return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}

			// Reset error counter on success
			consecutiveErrors = 0

			var result BillRunResponse
			if err := json.Unmarshal(respBody, &result); err != nil {
				return nil, err
			}

			status := result.Data.Status
			progress := result.Data.ProgressPct

			fmt.Printf("  Status: %s, Progress: %d%%\n", status, progress)

			if status == "COMPLETED" || status == "FAILED" || status == "PARTIALLY_FAILED" {
				return &result, nil
			}
		}
	}
}
