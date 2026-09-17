package main

import "testing"

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"256M":  256 * 1024 * 1024,
		"1G":    1024 * 1024 * 1024,
		"512k":  512 * 1024,
		"1024":  1024,
		"256MB": 256 * 1024 * 1024,
		"20GiB": 20 * 1024 * 1024 * 1024,
		"":      0,
		"abc":   0,
	}
	for in, want := range cases {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
