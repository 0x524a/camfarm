package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

var sizeUnits = map[string]int64{
	"": 1, "b": 1,
	"kb": 1e3, "mb": 1e6, "gb": 1e9,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30,
}

// parseSize reads a byte count such as "2GiB", "500MB" or "1048576".
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	num, unit := s, ""
	if i := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsDigit(r) && r != '.' }); i >= 0 {
		num, unit = s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	mult, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("unknown unit %q (use B, KB, MB, GB, KiB, MiB or GiB)", unit)
	}
	v := n * float64(mult)
	if v < 1 || v >= math.MaxInt64 {
		return 0, fmt.Errorf("%q must be at least 1 byte and fit in 63 bits", s)
	}
	return int64(v), nil
}
