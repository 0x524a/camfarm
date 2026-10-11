package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The caller quotes the input in its own message; parseSize must not repeat it.
func TestParseSizeErrorsDoNotRepeatTheInput(t *testing.T) {
	for _, in := range []string{"lots", "99999999999GiB", "0"} {
		_, err := parseSize(in)
		if err == nil {
			t.Fatalf("parseSize(%q) succeeded", in)
		}
		if strings.Contains(err.Error(), in) {
			t.Errorf("parseSize(%q) error %q repeats the input", in, err)
		}
	}
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"--max-upload", "lots", "--http-addr", "127.0.0.1:0"}, &out, &errb)
	if err == nil || strings.Count(err.Error()+errb.String(), `"lots"`) != 1 {
		t.Errorf("run error %v (stderr %q), want the input quoted exactly once", err, errb.String())
	}
}

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
	for _, in := range []string{"", "0", "-1", "abc", "2XB", "1e3", "1.2.3", "GiB", "99999999999GiB", "NaN", "Inf", "+5", "0x10", "1_000"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}
