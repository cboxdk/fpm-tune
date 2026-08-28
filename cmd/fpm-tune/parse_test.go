package main

import "testing"

// TestParseBytes covers the failure that made this the tool's sharpest edge:
// Sscanf stopped at the first non-digit and reported success, so --reserve 512MB
// parsed as 512 BYTES. That does not fail loudly — it reserves nothing and hands
// the whole host to workers, which is the one outcome this tool exists to
// prevent, produced by a spelling of the unit that looks entirely reasonable.
func TestParseBytes(t *testing.T) {
	const (
		k  = 1 << 10
		m  = 1 << 20
		g  = 1 << 30
		tb = 1 << 40
	)

	valid := map[string]int64{
		"512":  512,
		"8K":   8 * k,
		"8k":   8 * k,
		"512M": 512 * m,
		"8G":   8 * g,
		"2T":   2 * tb,

		// The spellings that used to parse as a bare number.
		"8GB":   8 * g,
		"512MB": 512 * m,
		"8Gb":   8 * g,

		// The tool PRINTS these, so it has to read them. A program that will not
		// accept its own output is a trap.
		"8GiB":   8 * g,
		"512MiB": 512 * m,
		"8Gi":    8 * g,

		" 8G ": 8 * g,
	}

	for in, want := range valid {
		got, err := parseBytes(in)
		if err != nil {
			t.Errorf("parseBytes(%q) errored: %v", in, err)

			continue
		}
		if got != want {
			t.Errorf("parseBytes(%q) = %d, want %d", in, got, want)
		}
	}

	// Anything not fully understood must be refused, not silently truncated.
	invalid := []string{
		"", "garbage", "-5G", "0", "0G",
		"1e9", // parsed as 1
		"8GG",
		"G",
		"8G8",
		"9223372036854775807G", // would overflow
	}

	for _, in := range invalid {
		if got, err := parseBytes(in); err == nil {
			t.Errorf("parseBytes(%q) = %d with no error; a size that is not fully "+
				"understood must be refused rather than truncated", in, got)
		}
	}
}
