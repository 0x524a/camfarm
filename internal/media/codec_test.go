package media

import (
	"errors"
	"testing"
)

// H.264 NAL headers are one byte: type is the low five bits.
func h264NALU(typ byte, payload ...byte) []byte {
	return append([]byte{typ & 0x1F}, payload...)
}

// H.265 NAL headers are two bytes: type is six bits starting at bit 1 of byte
// 0, and byte 1 carries nuh_layer_id and nuh_temporal_id_plus1. 0x01 is the
// single-layer, temporal-id-0 value every synthetic source here uses.
func h265NALU(typ byte, payload ...byte) []byte {
	return append([]byte{typ << 1, 0x01}, payload...)
}

func TestAdapterCodecsAreDistinct(t *testing.T) {
	if (h264Adapter{}).Codec() != CodecH264 {
		t.Errorf("h264Adapter.Codec() = %q, want %q", h264Adapter{}.Codec(), CodecH264)
	}
	if (h265Adapter{}).Codec() != CodecH265 {
		t.Errorf("h265Adapter.Codec() = %q, want %q", h265Adapter{}.Codec(), CodecH265)
	}
}

// TestH264ClassifyFindsParameterSets drives the adapter with hand-built NALUs:
// no container, no fixture, no demuxer.
func TestH264ClassifyFindsParameterSets(t *testing.T) {
	sps := h264NALU(7, 0xAA)
	pps := h264NALU(8, 0xBB)
	slice := h264NALU(1, 0xCC)

	var ps paramSets
	h264Adapter{}.Classify([][]byte{sps, pps, slice}, &ps)

	if string(ps.SPS) != string(sps) {
		t.Errorf("SPS = % x, want % x", ps.SPS, sps)
	}
	if string(ps.PPS) != string(pps) {
		t.Errorf("PPS = % x, want % x", ps.PPS, pps)
	}
	if ps.VPS != nil {
		t.Errorf("VPS = % x, want nil (H.264 has no VPS)", ps.VPS)
	}
}

func TestH265ClassifyFindsParameterSets(t *testing.T) {
	vps := h265NALU(32, 0xAA)
	sps := h265NALU(33, 0xBB)
	pps := h265NALU(34, 0xCC)

	var ps paramSets
	h265Adapter{}.Classify([][]byte{vps, sps, pps}, &ps)

	if string(ps.VPS) != string(vps) {
		t.Errorf("VPS = % x, want % x", ps.VPS, vps)
	}
	if string(ps.SPS) != string(sps) {
		t.Errorf("SPS = % x, want % x", ps.SPS, sps)
	}
	if string(ps.PPS) != string(pps) {
		t.Errorf("PPS = % x, want % x", ps.PPS, pps)
	}
}

// TestClassifyIgnoresShortNALUs proves neither adapter indexes past the end of
// a truncated NALU. An H.265 header is two bytes, so a one-byte NALU must be
// skipped rather than read.
func TestClassifyIgnoresShortNALUs(t *testing.T) {
	var ps paramSets
	h264Adapter{}.Classify([][]byte{{}}, &ps)
	h265Adapter{}.Classify([][]byte{{}, {0x40}}, &ps)
	if ps.VPS != nil || ps.SPS != nil || ps.PPS != nil {
		t.Fatal("a truncated NALU was classified as a parameter set")
	}
}

func TestValidateRequiresParameterSets(t *testing.T) {
	tests := map[string]struct {
		ad   codecAdapter
		ps   paramSets
		want error
	}{
		"h264 complete": {h264Adapter{}, paramSets{SPS: []byte{1}, PPS: []byte{2}}, nil},
		"h264 no sps":   {h264Adapter{}, paramSets{PPS: []byte{2}}, ErrNoParameterSetsH264},
		"h264 no pps":   {h264Adapter{}, paramSets{SPS: []byte{1}}, ErrNoParameterSetsH264},
		"h265 complete": {h265Adapter{}, paramSets{VPS: []byte{1}, SPS: []byte{2}, PPS: []byte{3}}, nil},
		"h265 no vps":   {h265Adapter{}, paramSets{SPS: []byte{2}, PPS: []byte{3}}, ErrNoParameterSetsH265},
		"h265 no sps":   {h265Adapter{}, paramSets{VPS: []byte{1}, PPS: []byte{3}}, ErrNoParameterSetsH265},
		"h265 no pps":   {h265Adapter{}, paramSets{VPS: []byte{1}, SPS: []byte{2}}, ErrNoParameterSetsH265},
	}
	for name, tc := range tests {
		if err := tc.ad.Validate(tc.ps); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}

// TestH265RandomAccessAcceptsCRA is the regression test for the open-GOP trap.
// libx265 defaults to open GOP, so every keyframe after the first is CRA_NUT
// (21), not an IDR. An implementation that checks only for IDR types finds the
// IDR at the start of a file, looks correct, and then treats every later
// keyframe as non-random-access -- surfacing at the loop boundary, far from
// its cause.
func TestH265RandomAccessAcceptsCRA(t *testing.T) {
	for _, typ := range []byte{19, 20, 21} { // IDR_W_RADL, IDR_N_LP, CRA_NUT
		au := [][]byte{h265NALU(typ, 0x00)}
		if !(h265Adapter{}).IsRandomAccess(au) {
			t.Errorf("NALU type %d not treated as random access", typ)
		}
	}
	if (h265Adapter{}).IsRandomAccess([][]byte{h265NALU(1, 0x00)}) {
		t.Error("a TRAIL_N slice was treated as random access")
	}
}

func TestH264RandomAccess(t *testing.T) {
	if !(h264Adapter{}).IsRandomAccess([][]byte{h264NALU(5, 0x00)}) {
		t.Error("an IDR slice was not treated as random access")
	}
	if (h264Adapter{}).IsRandomAccess([][]byte{h264NALU(1, 0x00)}) {
		t.Error("a non-IDR slice was treated as random access")
	}
}

func TestGeometryRejectsUnparseableSPS(t *testing.T) {
	for name, ad := range map[string]codecAdapter{"h264": h264Adapter{}, "h265": h265Adapter{}} {
		if _, _, _, err := ad.Geometry(paramSets{SPS: []byte{0x00}}); err == nil {
			t.Errorf("%s: expected an error for an unparseable SPS", name)
		}
	}
}
