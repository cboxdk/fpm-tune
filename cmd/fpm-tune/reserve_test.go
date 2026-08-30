package main

import "testing"

func TestParseReserve(t *testing.T) {
	const gib = int64(1) << 30

	for _, tc := range []struct {
		in       string
		bytes    int64
		fraction float64
		wantErr  bool
	}{
		{"2G", 2 * gib, 0, false},
		{"512MB", 512 * 1024 * 1024, 0, false},
		{"20%", 0, 0.20, false},
		{" 15 % ", 0, 0.15, false},
		{"150%", 0, 0, true}, // a reserve of the whole host and more
		{"0%", 0, 0, true},   // nothing reserved is not a reserve
		{"100%", 0, 0, true}, // leaves nothing for workers
		{"abc", 0, 0, true},
	} {
		bytes, fraction, err := parseReserve(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseReserve(%q) = (%d, %v), want an error", tc.in, bytes, fraction)
			}

			continue
		}
		if err != nil {
			t.Errorf("parseReserve(%q) errored: %v", tc.in, err)

			continue
		}
		if bytes != tc.bytes || fraction != tc.fraction {
			t.Errorf("parseReserve(%q) = (%d, %v), want (%d, %v)", tc.in, bytes, fraction, tc.bytes, tc.fraction)
		}
	}
}
