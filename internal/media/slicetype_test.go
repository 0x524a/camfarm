package media

import (
	"bytes"
	"testing"
)

func TestIsH264BSlice(t *testing.T) {
	cases := []struct {
		name string
		nalu []byte
		want bool
	}{
		// After the 1-byte NAL header, bits are first_mb_in_slice then slice_type,
		// both unsigned Exp-Golomb. 0xa0 = 1 010 0000 -> first_mb 0, slice_type 1.
		{"B slice_type 1", []byte{0x01, 0xa0}, true},
		// 0x9c = 1 00111 00 -> first_mb 0, slice_type 6.
		{"B slice_type 6", []byte{0x01, 0x9c}, true},
		// 0x9a = 1 00110 10 -> first_mb 0, slice_type 5 (P).
		{"P slice_type 5", []byte{0x41, 0x9a}, false},
		{"IDR slice is never B", []byte{0x65, 0xa0}, false},
		{"SPS is not a slice", []byte{0x67, 0xa0}, false},
		{"empty", nil, false},
		{"header only", []byte{0x01}, false},
		{"exp-golomb runs off the end", []byte{0x01, 0x00}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isH264BSlice(c.nalu); got != c.want {
				t.Errorf("isH264BSlice(% x) = %v, want %v", c.nalu, got, c.want)
			}
		})
	}
}

func TestUnescapeRBSP(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{0, 0, 3, 1}, []byte{0, 0, 1}},
		{[]byte{0, 3}, []byte{0, 3}},
		{[]byte{0, 0, 3, 0, 0, 3}, []byte{0, 0, 0, 0}},
		{nil, []byte{}},
	}
	for _, c := range cases {
		if got := unescapeRBSP(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("unescapeRBSP(% x) = % x, want % x", c.in, got, c.want)
		}
	}
}
