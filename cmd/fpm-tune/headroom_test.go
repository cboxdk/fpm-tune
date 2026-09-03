package main

import (
	"math"
	"testing"
)

// TestCheckHeadroom: the host's --cpu-headroom is held to the same range as a
// pool's marker, and refused rather than clamped, so an operator who types
// 1e19 reads an error instead of getting the smallest ceiling for it.
func TestCheckHeadroom(t *testing.T) {
	for _, tc := range []struct {
		in      float64
		wantErr bool
	}{
		{1, false},
		{1.5, false},
		{2, false},
		{100, false},
		{0.5, true},
		{0, true},
		{-1, true},
		{101, true},
		{1e19, true},
		{math.NaN(), true},
		{math.Inf(1), true},
	} {
		err := checkHeadroom(tc.in)
		if tc.wantErr && err == nil {
			t.Errorf("checkHeadroom(%g) = nil, want an error", tc.in)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("checkHeadroom(%g) errored: %v", tc.in, err)
		}
	}
}
