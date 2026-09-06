package media

import (
	"strings"
	"testing"
)

// minimalH264SPS returns a paramSets that Validate accepts, for tests that care
// about assemble's own logic rather than parameter-set handling. The SPS must
// still be a real, parseable one, since assemble always calls Geometry; see
// the comment below for where it comes from.
func minimalH264SPS(t *testing.T) paramSets {
	t.Helper()
	// A real, parseable baseline SPS is needed because assemble calls Geometry.
	// This one comes from the committed fixture, which is the only source of a
	// known-good SPS that does not require hand-encoding one here.
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture for a known-good SPS: %v", err)
	}
	return paramSets{SPS: m.SPS, PPS: m.PPS}
}

func TestAssembleRejectsNoSamples(t *testing.T) {
	_, err := assemble(h264Adapter{}, minimalH264SPS(t), nil)
	if err == nil {
		t.Fatal("expected an error for zero samples")
	}
	const want = "media: source contains no access units"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

// TestAssembleChecksSamplesBeforeParameterSets pins the order the pre-refactor
// code had: a source with no samples reports that, not a missing-SPS error.
func TestAssembleChecksSamplesBeforeParameterSets(t *testing.T) {
	_, err := assemble(h264Adapter{}, paramSets{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no access units") {
		t.Fatalf("err = %v, want the no-access-units error", err)
	}
}

func TestAssembleRebasesTimestamps(t *testing.T) {
	ps := minimalH264SPS(t)
	slice := h264NALU(1, 0x00)

	m, err := assemble(h264Adapter{}, ps, []rawSample{
		{NALUs: [][]byte{slice}, PTS: 9000, DTS: 9000},
		{NALUs: [][]byte{slice}, PTS: 12000, DTS: 12000},
		{NALUs: [][]byte{slice}, PTS: 15000, DTS: 15000},
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	want := []int64{0, 3000, 6000}
	for i, w := range want {
		if m.AUs[i].DTS != w {
			t.Errorf("AU %d DTS = %d, want %d", i, m.AUs[i].DTS, w)
		}
		if m.AUs[i].PTS != w {
			t.Errorf("AU %d PTS = %d, want %d", i, m.AUs[i].PTS, w)
		}
	}
}

// TestAssemblePreservesPTSDTSSkew proves rebasing shifts both series by the same
// offset, so B-frame reordering survives. Shifting them independently would
// silently flatten the skew.
func TestAssemblePreservesPTSDTSSkew(t *testing.T) {
	ps := minimalH264SPS(t)
	slice := h264NALU(1, 0x00)

	m, err := assemble(h264Adapter{}, ps, []rawSample{
		{NALUs: [][]byte{slice}, PTS: 12000, DTS: 9000},
		{NALUs: [][]byte{slice}, PTS: 15000, DTS: 12000},
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	for i, au := range m.AUs {
		if got := au.PTS - au.DTS; got != 3000 {
			t.Errorf("AU %d PTS-DTS = %d, want 3000", i, got)
		}
	}
	if m.AUs[0].DTS != 0 {
		t.Errorf("first DTS = %d, want 0", m.AUs[0].DTS)
	}
}

// TestAssembleRefusesSyncDisagreement covers the deliberate strictness in the
// design: a container that declares a sample non-sync while the bitstream says
// it is random-access is refused, naming both values.
func TestAssembleRefusesSyncDisagreement(t *testing.T) {
	ps := minimalH264SPS(t)
	idr := h264NALU(5, 0x00)

	_, err := assemble(h264Adapter{}, ps, []rawSample{
		{NALUs: [][]byte{idr}, PTS: 0, DTS: 0, Sync: false, SyncKnown: true},
	})
	if err == nil {
		t.Fatal("expected an error when the container contradicts the bitstream")
	}
	for _, want := range []string{"sample 0", "container declares random access"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestAssembleAcceptsSyncAgreement is the companion: the same declaration, this
// time matching, must pass.
func TestAssembleAcceptsSyncAgreement(t *testing.T) {
	ps := minimalH264SPS(t)
	idr := h264NALU(5, 0x00)
	nonIDR := h264NALU(1, 0x00)

	m, err := assemble(h264Adapter{}, ps, []rawSample{
		{NALUs: [][]byte{idr}, DTS: 0, Sync: true, SyncKnown: true},
		{NALUs: [][]byte{nonIDR}, DTS: 3000, Sync: false, SyncKnown: true},
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !m.AUs[0].RandomAccess || m.AUs[1].RandomAccess {
		t.Errorf("RandomAccess = [%v %v], want [true false]", m.AUs[0].RandomAccess, m.AUs[1].RandomAccess)
	}
}

// TestAssembleIgnoresUnknownSyncFlag proves SyncKnown=false means the Sync field
// is not consulted at all, which is how MPEG-TS reaches this code.
func TestAssembleIgnoresUnknownSyncFlag(t *testing.T) {
	ps := minimalH264SPS(t)
	idr := h264NALU(5, 0x00)

	m, err := assemble(h264Adapter{}, ps, []rawSample{
		// Sync deliberately contradicts the bitstream, but SyncKnown is false,
		// so it must be ignored rather than trigger the disagreement error.
		{NALUs: [][]byte{idr}, DTS: 0, Sync: false, SyncKnown: false},
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !m.AUs[0].RandomAccess {
		t.Error("bitstream random access was not honoured when the container declared nothing")
	}
}
