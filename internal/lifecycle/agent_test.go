package lifecycle

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

func TestAgent_Run_FiresTransitionsAndStopsOnContextCancel(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"sub-1","status":"ACTIVE","offeringId":"off-2","sourceSubscriptionId":"sub-1","targetSubscriptionId":"sub-1"}`))
	}))
	t.Cleanup(srv.Close)

	target := aforo.Target{
		Name: "test",
		URLs: map[aforo.Service]string{
			aforo.ServicePricing:       srv.URL,
			aforo.ServiceCatalog:       srv.URL,
			aforo.ServiceCustomer:      srv.URL,
			aforo.ServiceBilling:       srv.URL,
			aforo.ServiceOrganization:  srv.URL,
			aforo.ServiceUsageIngestor: srv.URL,
		},
	}

	mf := &seed.Manifest{
		Tenants: []seed.ManifestTenant{
			{
				TenantID:  "ten-1",
				Archetype: "ar-1",
				Offerings: []seed.ManifestOffering{
					{OfferingID: "off-1"},
					{OfferingID: "off-2"},
				},
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "cust-1",
						Subscriptions: []seed.ManifestSubscription{
							{SubscriptionID: "sub-1", Status: scenario.StateActive},
							{SubscriptionID: "sub-2", Status: scenario.StateActive},
							{SubscriptionID: "sub-3", Status: scenario.StateActive},
						},
					},
				},
			},
		},
	}

	scn := &scenario.Scenario{
		Name:    "test",
		Seed:    1,
		Tenants: scenario.Tenants{Count: 1},
		Lifecycle: scenario.LifecycleProfile{
			Enabled:               true,
			UpgradesPerHourPct:    1.0,
			DowngradesPerHourPct:  1.0,
			PauseResumePerHourPct: 1.0,
		},
	}

	tlog := NewTransitionLogTo(&bytes.Buffer{})
	client, err := NewClient(ClientConfig{Target: target, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	picker := NewPicker(mf, 1)
	agent, err := NewAgent(AgentConfig{
		Scenario:          scn,
		Manifest:          mf,
		Log:               tlog,
		Client:            client,
		Picker:            picker,
		MinTickerInterval: 5 * time.Millisecond,
		MaxTickerInterval: 50 * time.Millisecond,
		PauseResumeDelay:  20 * time.Millisecond,
		ResumeTimeout:     500 * time.Millisecond,
		Logger:            io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := agent.Run(ctx); err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	if hits.Load() == 0 {
		t.Fatal("agent did not fire any transitions")
	}
	if agent.PendingResumeCount() != 0 {
		t.Errorf("PendingResumeCount = %d, want 0", agent.PendingResumeCount())
	}
	snap := agent.LogSnapshot()
	if len(snap.ByKind) == 0 {
		t.Error("snapshot ByKind is empty")
	}
}

func TestAgent_Run_DisabledIdlesAndExits(t *testing.T) {
	mf := &seed.Manifest{}
	scn := &scenario.Scenario{
		Lifecycle: scenario.LifecycleProfile{Enabled: false},
	}
	tlog := NewTransitionLogTo(&bytes.Buffer{})
	client, _ := NewClient(ClientConfig{Target: aforo.LocalTarget, Token: "x"})
	agent, err := NewAgent(AgentConfig{
		Scenario: scn,
		Manifest: mf,
		Log:      tlog,
		Client:   client,
		Picker:   NewPicker(mf, 1),
		Logger:   io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := agent.Run(ctx); err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
}

func TestAgent_RequiresInputs(t *testing.T) {
	if _, err := NewAgent(AgentConfig{}); err == nil {
		t.Fatal("expected error on empty config")
	}
	mf := &seed.Manifest{}
	if _, err := NewAgent(AgentConfig{Scenario: &scenario.Scenario{}, Manifest: mf}); err == nil {
		t.Fatal("expected error: log + client + picker missing")
	}
}
