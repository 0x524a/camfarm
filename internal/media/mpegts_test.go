package media

import (
	"bytes"
	"errors"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
)

// TestCopyNALUsDoesNotAliasInput proves copyNALUs returns independently owned
// memory: it mutates the source buffer after copying and checks the copy is
// unaffected. If copyNALUs is changed to return its input unchanged (no copy),
// this test fails deterministically -- verified by deleting the copy and
// observing the failure.
func TestCopyNALUsDoesNotAliasInput(t *testing.T) {
	src := []byte{0x01, 0x02, 0x03, 0x04}
	au := [][]byte{src[0:2], src[2:4]}

	out := copyNALUs(au)

	// Mutate the source buffer after copying, the way a reused reader buffer
	// would be overwritten by the time a caller inspects the parsed media.
	for i := range src {
		src[i] = 0xFF
	}

	want := [][]byte{{0x01, 0x02}, {0x03, 0x04}}
	for i, n := range out {
		if !bytes.Equal(n, want[i]) {
			t.Fatalf("NALU %d = % x, want % x (copy shares memory with the mutated source)", i, n, want[i])
		}
	}
}

// TestCopyNALUsDropsEmpty proves the empty-NALU guard in copyNALUs, which the
// MPEG-TS pipeline itself never exercises: mediacommon's own Annex-B unmarshal
// already drops zero-length segments before an access unit reaches our
// OnDataH264 callback. This test drives the guard directly.
func TestCopyNALUsDropsEmpty(t *testing.T) {
	out := copyNALUs([][]byte{{0x01}, {}, {0x02}})
	if len(out) != 2 {
		t.Fatalf("copyNALUs returned %d NALUs, want 2 (empty NALU not dropped)", len(out))
	}
	if !bytes.Equal(out[0], []byte{0x01}) || !bytes.Equal(out[1], []byte{0x02}) {
		t.Fatalf("copyNALUs = %v, want [[0x01] [0x02]]", out)
	}
}

// klvTrack and its single write exist only to make the reader see a track that
// is not H.264 -- KLV needs no valid encoding, which is all this test cares
// about.
func writeNonH264Stream(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	track := &mpegts.Track{Codec: &mpegts.CodecKLV{Synchronous: false}}
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{track}}
	if err := w.Initialize(); err != nil {
		t.Fatalf("init writer: %v", err)
	}
	if err := w.WriteKLV(track, 0, []byte("not video")); err != nil {
		t.Fatalf("write KLV: %v", err)
	}
	return buf.Bytes()
}

func TestParseMPEGTSNoH264Track(t *testing.T) {
	_, err := parseMPEGTS(bytes.NewReader(writeNonH264Stream(t)))
	if !errors.Is(err, ErrNoH264Track) {
		t.Fatalf("err = %v, want ErrNoH264Track", err)
	}
}

// writeH264TablesOnly emits PAT/PMT declaring an H.264 track but never writes
// any access unit for it, via the writer's explicit WriteTables (documented for
// exactly this: getting tables out before any media data).
func writeH264TablesOnly(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	track := &mpegts.Track{Codec: &mpegts.CodecH264{}}
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{track}}
	if err := w.Initialize(); err != nil {
		t.Fatalf("init writer: %v", err)
	}
	if _, err := w.WriteTables(); err != nil {
		t.Fatalf("write tables: %v", err)
	}
	return buf.Bytes()
}

func TestParseMPEGTSNoAccessUnits(t *testing.T) {
	_, err := parseMPEGTS(bytes.NewReader(writeH264TablesOnly(t)))
	if err == nil {
		t.Fatal("expected an error for a track with no access units")
	}
	const want = "media: source contains no access units"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

// writeH264WithoutParameterSets writes one access unit containing only a
// non-IDR slice: a real NALU, but neither an SPS nor a PPS.
func writeH264WithoutParameterSets(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	track := &mpegts.Track{Codec: &mpegts.CodecH264{}}
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{track}}
	if err := w.Initialize(); err != nil {
		t.Fatalf("init writer: %v", err)
	}
	// nal_ref_idc = 0, nal_unit_type = 1 (non-IDR slice).
	nonIDR := []byte{0x01, 0x00, 0x00}
	if err := w.WriteH264(track, 0, 0, [][]byte{nonIDR}); err != nil {
		t.Fatalf("write H264: %v", err)
	}
	return buf.Bytes()
}

func TestParseMPEGTSNoParameterSets(t *testing.T) {
	_, err := parseMPEGTS(bytes.NewReader(writeH264WithoutParameterSets(t)))
	if err == nil {
		t.Fatal("expected an error for a stream with no in-band SPS/PPS")
	}
	const want = "media: source carries no in-band SPS/PPS"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}
