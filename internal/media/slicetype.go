package media

// bitReader reads an RBSP bit by bit. The slice parser only needs the first
// two Exp-Golomb fields of a slice header, so the only error state is ok.
type bitReader struct {
	b   []byte
	pos int // bit position
}

func (r *bitReader) bit() (uint, bool) {
	if r.pos >= len(r.b)*8 {
		return 0, false
	}
	v := uint(r.b[r.pos/8]>>(7-r.pos%8)) & 1
	r.pos++
	return v, true
}

// ue reads one unsigned Exp-Golomb value.
func (r *bitReader) ue() (uint, bool) {
	zeros := 0
	for {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, false
		}
	}
	var v uint
	for i := 0; i < zeros; i++ {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		v = v<<1 | b
	}
	return (uint(1)<<zeros - 1) + v, true
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03 -> 00 00).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// isH264BSlice reports whether nalu is a non-IDR slice whose slice_type is B
// (1 or 6). Only the first 16 payload bytes are read: the two fields it needs
// sit at the very start of the slice header. IDR slices (NAL type 5) cannot be
// B, so they return false without parsing.
func isH264BSlice(nalu []byte) bool {
	if len(nalu) < 2 || nalu[0]&0x1f != 1 {
		return false
	}
	br := bitReader{b: unescapeRBSP(nalu[1:min(len(nalu), 17)])}
	if _, ok := br.ue(); !ok { // first_mb_in_slice
		return false
	}
	st, ok := br.ue() // slice_type
	return ok && (st == 1 || st == 6)
}
