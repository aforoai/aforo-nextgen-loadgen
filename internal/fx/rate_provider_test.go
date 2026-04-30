package fx

import (
	"errors"
	"math"
	"testing"
)

func TestMapProvider_SameCurrency(t *testing.T) {
	p := NewMapProvider(map[string]float64{"USD->EUR": 0.9})
	r, err := p.Rate("USD", "USD")
	if err != nil || r != 1.0 {
		t.Fatalf("USD->USD = %v, err=%v; want 1.0, nil", r, err)
	}
}

func TestMapProvider_PinnedRate(t *testing.T) {
	p := NewMapProvider(map[string]float64{
		"USD->EUR": 0.92,
		"USD->GBP": 0.79,
	})
	tests := []struct {
		from, to string
		want     float64
	}{
		{"USD", "EUR", 0.92},
		{"USD", "GBP", 0.79},
	}
	for _, tt := range tests {
		got, err := p.Rate(tt.from, tt.to)
		if err != nil {
			t.Errorf("%s->%s: %v", tt.from, tt.to, err)
		}
		if got != tt.want {
			t.Errorf("%s->%s = %v; want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestMapProvider_AutoInverse(t *testing.T) {
	// EUR->USD inverse must be 1/0.92 when only USD->EUR is provided.
	p := NewMapProvider(map[string]float64{"USD->EUR": 0.92})
	r, err := p.Rate("EUR", "USD")
	if err != nil {
		t.Fatalf("auto inverse missing: %v", err)
	}
	want := 1.0 / 0.92
	if math.Abs(r-want) > 1e-9 {
		t.Fatalf("EUR->USD = %v; want %v", r, want)
	}
}

func TestMapProvider_ExplicitOverrideAuto(t *testing.T) {
	// When both directions declared, neither is auto-derived.
	p := NewMapProvider(map[string]float64{
		"USD->EUR": 0.92,
		"EUR->USD": 1.10, // intentionally NOT 1/0.92 — bid/ask spread
	})
	r, _ := p.Rate("EUR", "USD")
	if r != 1.10 {
		t.Fatalf("EUR->USD = %v; want explicit 1.10", r)
	}
}

func TestMapProvider_UnknownPair(t *testing.T) {
	p := NewMapProvider(map[string]float64{"USD->EUR": 0.92})
	_, err := p.Rate("USD", "JPY")
	if !errors.Is(err, ErrUnknownPair) {
		t.Fatalf("err = %v; want ErrUnknownPair", err)
	}
}

func TestMapProvider_Convert(t *testing.T) {
	p := NewMapProvider(map[string]float64{"USD->EUR": 0.92})
	got, err := p.Convert(100, "USD", "EUR")
	if err != nil || got != 92.0 {
		t.Fatalf("Convert(100 USD, EUR) = %v, err=%v; want 92.0", got, err)
	}
}

func TestMapProvider_Convert_Rounding(t *testing.T) {
	// 100 * 0.789123 = 78.9123 — 6dp keeps that intact.
	p := NewMapProvider(map[string]float64{"USD->EUR": 0.789123})
	got, _ := p.Convert(100, "USD", "EUR")
	if got != 78.9123 {
		t.Fatalf("rounding: got %v; want 78.9123", got)
	}
}

func TestMapProvider_EmptyCode(t *testing.T) {
	p := NewMapProvider(nil)
	if _, err := p.Rate("", "USD"); err == nil {
		t.Fatal("empty 'from' should error")
	}
	if _, err := p.Rate("USD", ""); err == nil {
		t.Fatal("empty 'to' should error")
	}
}

func TestMapProvider_BadPairKey(t *testing.T) {
	// "USD-EUR" (single dash) is not a valid pair — silently skipped.
	p := NewMapProvider(map[string]float64{"USD-EUR": 0.92})
	if _, err := p.Rate("USD", "EUR"); err == nil {
		t.Fatal("malformed key should not register a rate")
	}
}

func TestSplitPair(t *testing.T) {
	tests := []struct {
		in            string
		fromW, toW    string
		ok            bool
	}{
		{"USD->EUR", "USD", "EUR", true},
		{"usd->eur", "USD", "EUR", true},
		{"USD-EUR", "", "", false},
		{"USDEUR", "", "", false},
		{"USD->", "", "", false},
		{"->EUR", "", "", false},
		{"USD->EURO", "", "", false},
	}
	for _, tt := range tests {
		f, to, ok := splitPair(tt.in)
		if ok != tt.ok || f != tt.fromW || to != tt.toW {
			t.Errorf("splitPair(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tt.in, f, to, ok, tt.fromW, tt.toW, tt.ok)
		}
	}
}
