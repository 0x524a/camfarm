package main

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"1048576": 1048576, "2GiB": 2 << 30, "512MiB": 512 << 20, "500MB": 500_000_000,
		"1.5GiB": 3 << 29, "10kb": 10_000, " 4 mib ": 4 << 20, "7B": 7, "1KiB": 1024,
	}
	for in, want := range ok {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "abc", "2XB", "1e3", "1.2.3", "GiB", "99999999999GiB"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}
