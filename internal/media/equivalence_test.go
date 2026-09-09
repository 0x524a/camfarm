package media

import (
	"bytes"
	"os"
	"testing"
)

// isVCL reports whether a NALU carries coded picture data rather than metadata.
//
// H.264 VCL types are 1-5; H.265 reserves 0-31 for VCL and 32-63 for non-VCL.
// This is a test-only distinction, separate from the adapter's parameter-set
// classification, and it exists for the reason documented on
// TestCrossContainerEquivalence.
func isVCL(c Codec, n []byte) bool {
	switch c {
	case CodecH264:
		if len(n) < 1 {
			return false
		}
		t := n[0] & 0x1F
		return t >= 1 && t <= 5
	case CodecH265:
		if len(n) < 2 {
			return false
		}
		return (n[0]>>1)&0b111111 <= 31
	default:
		return false
	}
}

func vclNALUs(c Codec, au AccessUnit) [][]byte {
	var out [][]byte
	for _, n := range au.NALUs {
		if isVCL(c, n) {
			out = append(out, n)
		}
	}
	return out
}

// trimTrailingZeroPad drops trailing 0x00 bytes from a NALU.
//
// H.265/H.264 Annex B defines "trailing_zero_8bits": zero or more 0x00 bytes
// that may follow a NAL unit's rbsp_trailing_bits, before the next start code.
// Per spec these are not part of the NAL unit and a decoder must discard them.
// rbsp_trailing_bits itself mandates a stop bit followed by zero-padding to the
// next byte boundary, which guarantees the true last byte of a NALU's real RBSP
// content is never 0x00 -- so trimming trailing zero bytes can never remove real
// payload, only spec-defined discardable padding.
//
// This exists because ffmpeg's HEVC stream-copy remux (exercised by
// remuxFixture, TS -> MP4) was found by direct byte inspection to carry exactly
// one such byte into the AVCC length-prefixed sample it writes, while
// mediacommon's MPEG-TS Annex-B splitter already strips it on the way in. Both
// paths read their own container correctly; trimming before comparing resolves
// the disagreement by the spec's own definition of a NAL unit's content,
// instead of the two demuxers' incidental treatment of discardable padding.
func trimTrailingZeroPad(n []byte) []byte {
	i := len(n)
	for i > 0 && n[i-1] == 0x00 {
		i--
	}
	return n[:i]
}

// TestCrossContainerEquivalence is the primary invariant of this slice.
//
// One source, reachable three ways, must parse to the same video. The three most
// probable bugs here -- timescale rescaling, AVCC length-prefix splitting, and
// off-by-one accumulation of sample durations -- are all invisible to a
// per-container test, because such a test asserts against numbers derived the
// same wrong way. Comparing two independent paths to identical bytes catches all
// three.
//
// Identity is asserted on VCL NALUs and on the parameter sets, not on whole
// access units. An MP4 muxer relocates parameter sets out-of-band into avcC or
// hvcC and may legitimately add or drop access-unit delimiters and SEI; that is
// a muxer's prerogative, not a difference in the coded video, and asserting
// whole-AU identity would fail for reasons this package does not control.
func TestCrossContainerEquivalence(t *testing.T) {
	cases := map[string]func(*testing.T) []byte{
		"h264": fixtureBytesForTest,
		"h265": fixtureH265BytesForTest,
	}

	for name, srcFn := range cases {
		t.Run(name, func(t *testing.T) {
			src := srcFn(t)

			fromTS, err := parseAny(bytes.NewReader(src))
			if err != nil {
				t.Fatalf("parse MPEG-TS: %v", err)
			}

			for variant, args := range map[string][]string{
				"mp4":  nil,
				"fmp4": fragFlags,
			} {
				t.Run(variant, func(t *testing.T) {
					path := remuxFixture(t, src, "out.mp4", args...)
					f, err := os.Open(path)
					if err != nil {
						t.Fatalf("open: %v", err)
					}
					defer f.Close()
					other, err := parseAny(f)
					if err != nil {
						t.Fatalf("parse %s: %v", variant, err)
					}
					assertEquivalent(t, fromTS, other, variant)
				})
			}
		})
	}
}

func assertEquivalent(t *testing.T, want, got *Media, label string) {
	t.Helper()

	if got.Codec != want.Codec {
		t.Errorf("%s: codec = %q, want %q", label, got.Codec, want.Codec)
	}
	if got.Width != want.Width || got.Height != want.Height {
		t.Errorf("%s: geometry = %dx%d, want %dx%d", label, got.Width, got.Height, want.Width, want.Height)
	}

	// Parameter sets survive a remux byte-for-byte: the encoder produced them
	// once and the muxer only moved them.
	for _, ps := range []struct {
		name      string
		want, got []byte
	}{
		{"VPS", want.VPS, got.VPS},
		{"SPS", want.SPS, got.SPS},
		{"PPS", want.PPS, got.PPS},
	} {
		if !bytes.Equal(ps.want, ps.got) {
			t.Errorf("%s: %s differs across containers:\n want % x\n  got % x", label, ps.name, ps.want, ps.got)
		}
	}

	if len(got.AUs) != len(want.AUs) {
		t.Fatalf("%s: access units = %d, want %d (sample-duration accumulation or fragment handling wrong?)",
			label, len(got.AUs), len(want.AUs))
	}

	for i := range want.AUs {
		wv, gv := vclNALUs(want.Codec, want.AUs[i]), vclNALUs(got.Codec, got.AUs[i])
		if len(wv) != len(gv) {
			t.Fatalf("%s: AU %d has %d VCL NALUs, want %d", label, i, len(gv), len(wv))
		}
		for j := range wv {
			a, b := trimTrailingZeroPad(wv[j]), trimTrailingZeroPad(gv[j])
			if !bytes.Equal(a, b) {
				t.Fatalf("%s: AU %d VCL NALU %d differs (%d vs %d bytes); AVCC splitting wrong?",
					label, i, j, len(gv[j]), len(wv[j]))
			}
		}

		if got.AUs[i].DTS != want.AUs[i].DTS {
			t.Errorf("%s: AU %d DTS = %d, want %d (timescale rescaling wrong?)",
				label, i, got.AUs[i].DTS, want.AUs[i].DTS)
		}
		if got.AUs[i].PTS != want.AUs[i].PTS {
			t.Errorf("%s: AU %d PTS = %d, want %d", label, i, got.AUs[i].PTS, want.AUs[i].PTS)
		}
		if got.AUs[i].RandomAccess != want.AUs[i].RandomAccess {
			t.Errorf("%s: AU %d RandomAccess = %v, want %v",
				label, i, got.AUs[i].RandomAccess, want.AUs[i].RandomAccess)
		}
	}
}
