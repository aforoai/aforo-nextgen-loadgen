package tax

import (
	"context"
	"math"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestMockEngine_HappyPath(t *testing.T) {
	tx := scenario.Tax{
		Jurisdictions: map[string]float64{
			"US-CA": 0.0925, // California sales tax
			"EU-DE": 0.19,   // German VAT
			"UK":    0.20,   // UK VAT
		},
		JurisdictionByCurrency: map[string]string{
			"USD": "US-CA",
			"EUR": "EU-DE",
			"GBP": "UK",
		},
	}
	engine := NewMockEngine(tx)
	if engine.Name() != "mock" {
		t.Fatalf("name=%s", engine.Name())
	}

	tests := []struct {
		name     string
		req      Request
		wantJur  string
		wantRate float64
		wantTax  float64
	}{
		{
			name:     "USD invoice → US-CA",
			req:      Request{InvoiceID: "i1", SubtotalUSD: 100, Currency: "USD"},
			wantJur:  "US-CA",
			wantRate: 0.0925,
			wantTax:  9.25,
		},
		{
			name:     "EUR invoice → EU-DE",
			req:      Request{InvoiceID: "i2", SubtotalUSD: 200, Currency: "EUR"},
			wantJur:  "EU-DE",
			wantRate: 0.19,
			wantTax:  38.0,
		},
		{
			name:     "explicit jurisdiction overrides currency",
			req:      Request{InvoiceID: "i3", SubtotalUSD: 100, Currency: "USD", Jurisdiction: "UK"},
			wantJur:  "UK",
			wantRate: 0.20,
			wantTax:  20.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := engine.Calculate(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if resp.JurisdictionCode != tt.wantJur {
				t.Errorf("jur = %s; want %s", resp.JurisdictionCode, tt.wantJur)
			}
			if math.Abs(resp.Rate-tt.wantRate) > 1e-9 {
				t.Errorf("rate = %v; want %v", resp.Rate, tt.wantRate)
			}
			if math.Abs(resp.TaxAmountUSD-tt.wantTax) > 1e-6 {
				t.Errorf("tax = %v; want %v", resp.TaxAmountUSD, tt.wantTax)
			}
		})
	}
}

func TestMockEngine_NoJurisdictionMatched(t *testing.T) {
	engine := NewMockEngine(scenario.Tax{})
	resp, err := engine.Calculate(context.Background(),
		Request{InvoiceID: "i1", SubtotalUSD: 100, Currency: "USD"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.JurisdictionCode != "" {
		t.Errorf("expected empty jurisdiction; got %q", resp.JurisdictionCode)
	}
	if resp.TaxAmountUSD != 0 {
		t.Errorf("expected zero tax; got %v", resp.TaxAmountUSD)
	}
	if resp.Note == "" {
		t.Error("expected note explaining no match")
	}
}

func TestMockEngine_DefaultJurisdiction(t *testing.T) {
	tx := scenario.Tax{
		Jurisdictions:       map[string]float64{"US-CA": 0.0925},
		DefaultJurisdiction: "US-CA",
	}
	engine := NewMockEngine(tx)
	resp, _ := engine.Calculate(context.Background(),
		Request{InvoiceID: "i1", SubtotalUSD: 100, Currency: "JPY"})
	if resp.JurisdictionCode != "US-CA" || resp.TaxAmountUSD != 9.25 {
		t.Errorf("default jurisdiction not applied: %+v", resp)
	}
}

func TestMockEngine_UnknownJurisdiction(t *testing.T) {
	// User passes Jurisdiction="ZZ" but it isn't in the table — engine must
	// not silently zero out without a note.
	tx := scenario.Tax{Jurisdictions: map[string]float64{"US-CA": 0.0925}}
	engine := NewMockEngine(tx)
	resp, _ := engine.Calculate(context.Background(),
		Request{InvoiceID: "i1", SubtotalUSD: 100, Jurisdiction: "ZZ"})
	if resp.TaxAmountUSD != 0 {
		t.Errorf("unknown jurisdiction should produce zero tax")
	}
	if resp.Note == "" {
		t.Error("expected explanatory note")
	}
}

func TestMockEngine_BadInput(t *testing.T) {
	engine := NewMockEngine(scenario.Tax{})
	if _, err := engine.Calculate(context.Background(),
		Request{SubtotalUSD: 100, Currency: "USD"}); err == nil {
		t.Error("missing invoice id should error")
	}
	if _, err := engine.Calculate(context.Background(),
		Request{InvoiceID: "i1", SubtotalUSD: -1}); err == nil {
		t.Error("negative subtotal should error")
	}
}

func TestBuild(t *testing.T) {
	tests := []struct {
		engine scenario.TaxEngine
		want   string
	}{
		{scenario.TaxMock, "mock"},
		{scenario.TaxAvalara, "avalara-mock"}, // no creds → fallback name
		{scenario.TaxVertex, "vertex-mock"},
		{"", "mock"},
	}
	for _, tt := range tests {
		e, err := Build(scenario.Tax{Engine: tt.engine})
		if err != nil {
			t.Fatalf("Build(%q): %v", tt.engine, err)
		}
		if e.Name() != tt.want {
			t.Errorf("Build(%q).Name()=%q; want %q", tt.engine, e.Name(), tt.want)
		}
	}
	if _, err := Build(scenario.Tax{Engine: "bogus"}); err == nil {
		t.Error("Build with unknown engine should fail")
	}
}

func TestResolve(t *testing.T) {
	byCur := map[string]string{"USD": "US-CA", "EUR": "EU-DE"}
	tests := []struct {
		name string
		req  Request
		def  string
		want string
	}{
		{"explicit wins", Request{Jurisdiction: "ZZ", Currency: "USD"}, "DEFAULT", "ZZ"},
		{"by currency", Request{Currency: "USD"}, "DEFAULT", "US-CA"},
		{"default fallback", Request{Currency: "JPY"}, "DEFAULT", "DEFAULT"},
		{"empty", Request{}, "", ""},
	}
	for _, tt := range tests {
		got := Resolve(tt.req, byCur, tt.def)
		if got != tt.want {
			t.Errorf("%s: got %q; want %q", tt.name, got, tt.want)
		}
	}
}

func TestMultiplyAndRound(t *testing.T) {
	cases := []struct {
		amount, rate, want float64
	}{
		{100, 0.0925, 9.25},
		{0, 0.10, 0},
		{100, 0, 0},
		{-100, 0.10, 0}, // negative amount returns 0
		{99.99, 0.0925, 9.249075},
	}
	for _, c := range cases {
		got := MultiplyAndRound(c.amount, c.rate)
		if math.Abs(got-c.want) > 1e-6 {
			t.Errorf("MultiplyAndRound(%v, %v) = %v; want %v", c.amount, c.rate, got, c.want)
		}
	}
}
