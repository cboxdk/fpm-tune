package main

import "testing"

func TestParseSizing(t *testing.T) {
	for _, tc := range []struct {
		in      string
		pct     float64
		wantErr bool
	}{
		{"peak", 0, false},
		{"", 0, false},
		{"p95", 0.95, false},
		{"p99", 0.99, false},
		{"95", 0.95, false},
		{"P90", 0.90, false},
		{"p40", 0, true},  // too low to be a sizing percentile
		{"p150", 0, true}, // not a percentile
		{"abc", 0, true},
	} {
		s, err := parseSizing(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSizing(%q) = %+v, want an error", tc.in, s)
			}

			continue
		}
		if err != nil {
			t.Errorf("parseSizing(%q) errored: %v", tc.in, err)

			continue
		}
		if s.Percentile != tc.pct {
			t.Errorf("parseSizing(%q) percentile = %v, want %v", tc.in, s.Percentile, tc.pct)
		}
		if s.Percentile > 0 && s.Margin != defaultSizingMargin {
			t.Errorf("parseSizing(%q) margin = %v, want the default %v", tc.in, s.Margin, defaultSizingMargin)
		}
	}
}
