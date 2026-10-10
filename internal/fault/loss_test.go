package fault

import (
	"reflect"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

func TestKeyframeStarvationDeliversEveryNthRandomAccessUnit(t *testing.T) {
	e, err := New(seed.Seed(1), []Spec{{Kind: KindKeyframeStarvation, KeepEvery: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var withheld []int
	for i := 0; i < 60; i++ {
		ra := i%5 == 0 // random-access units: frames 0,5,10,...,55 -> k = 0..11
		for _, d := range e.DecideFrame(i, ra) {
			if d.Kind != KindKeyframeStarvation || d.Suppressed {
				t.Fatalf("frame %d: unexpected decision %+v", i, d)
			}
			withheld = append(withheld, i)
		}
	}
	// k mod 3 != 0 is withheld: k = 1,2,4,5,7,8,10,11 -> frames 5,10,20,25,35,40,50,55.
	want := []int{5, 10, 20, 25, 35, 40, 50, 55}
	if !reflect.DeepEqual(withheld, want) {
		t.Fatalf("withheld frames = %v, want %v", withheld, want)
	}
}

func TestKeyframeStarvationHasNoRandomness(t *testing.T) {
	run := func(s seed.Seed) []int {
		e, _ := New(s, []Spec{{Kind: KindKeyframeStarvation, KeepEvery: 4}})
		var out []int
		for i := 0; i < 100; i++ {
			if len(e.DecideFrame(i, i%7 == 0)) > 0 {
				out = append(out, i)
			}
		}
		return out
	}
	if !reflect.DeepEqual(run(1), run(999)) {
		t.Fatal("keyframe_starvation depended on the seed")
	}
}

func lossIndices(t *testing.T, s seed.Seed, spec Spec, frames, perFrame int, ra func(int) bool) [][]PacketDecision {
	t.Helper()
	e, err := New(s, []Spec{spec})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out [][]PacketDecision
	for i := 0; i < frames; i++ {
		out = append(out, e.DecidePackets(i, perFrame, ra(i)))
	}
	return out
}

func never(int) bool { return false }

func TestPacketLossRateBounds(t *testing.T) {
	for _, d := range lossIndices(t, 1, Spec{Kind: KindPacketLoss, Rate: 0}, 50, 4, never) {
		if len(d) != 0 {
			t.Fatalf("rate 0 withheld %v", d)
		}
	}
	for i, d := range lossIndices(t, 1, Spec{Kind: KindPacketLoss, Rate: 1}, 50, 4, never) {
		if len(d) != 4 {
			t.Fatalf("frame %d: rate 1 withheld %d of 4 packets", i, len(d))
		}
	}
}

func TestPacketLossIsSeedDeterministic(t *testing.T) {
	spec := Spec{Kind: KindPacketLoss, Rate: 0.3}
	a := lossIndices(t, 42, spec, 200, 3, never)
	b := lossIndices(t, 42, spec, 200, 3, never)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different loss")
	}
	if reflect.DeepEqual(a, lossIndices(t, 43, spec, 200, 3, never)) {
		t.Fatal("different seeds produced identical loss")
	}
}

func TestPacketLossIndependentRateConverges(t *testing.T) {
	const frames, per = 20000, 5
	lost := 0
	for _, d := range lossIndices(t, 7, Spec{Kind: KindPacketLoss, Rate: 0.2}, frames, per, never) {
		lost += len(d)
	}
	got := float64(lost) / float64(frames*per)
	if got < 0.19 || got > 0.21 {
		t.Fatalf("measured loss %.4f, want 0.20 +/- 0.01", got)
	}
}

// Burst mode must hit both the configured long-run rate and the configured
// mean run length. The seed is fixed, so this is an exact, repeatable
// measurement; the tolerances are about 7 standard deviations wide.
func TestPacketLossBurstModeMatchesRateAndMeanBurst(t *testing.T) {
	const frames, per = 100000, 4
	spec := Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 4}
	lost, runs, run := 0, 0, 0
	for _, d := range lossIndices(t, 11, spec, frames, per, never) {
		hit := map[int]bool{}
		for _, p := range d {
			hit[p.Index] = true
		}
		for j := 0; j < per; j++ { // walk packets in order to find consecutive runs
			if hit[j] {
				lost++
				run++
				continue
			}
			if run > 0 {
				runs++
				run = 0
			}
		}
	}
	if run > 0 {
		runs++
	}
	total := frames * per
	if rate := float64(lost) / float64(total); rate < 0.09 || rate > 0.11 {
		t.Fatalf("measured loss %.4f, want 0.10 +/- 0.01", rate)
	}
	if mean := float64(lost) / float64(runs); mean < 3.75 || mean > 4.25 {
		t.Fatalf("measured mean burst %.3f, want 4 +/- 0.25", mean)
	}
}

func TestPacketLossDrawsDoNotDependOnProtectKeyframes(t *testing.T) {
	ra := func(i int) bool { return i%4 == 0 }
	plain := lossIndices(t, 5, Spec{Kind: KindPacketLoss, Rate: 0.4}, 200, 3, ra)
	protected := lossIndices(t, 5, Spec{Kind: KindPacketLoss, Rate: 0.4, ProtectKeyframes: true}, 200, 3, ra)
	for i := range plain {
		if len(plain[i]) != len(protected[i]) {
			t.Fatalf("frame %d: protecting keyframes changed which packets were drawn", i)
		}
		for j := range plain[i] {
			if plain[i][j].Index != protected[i][j].Index {
				t.Fatalf("frame %d: packet index moved", i)
			}
			if got, want := protected[i][j].Suppressed, ra(i); got != want {
				t.Fatalf("frame %d: Suppressed = %v, want %v", i, got, want)
			}
			if plain[i][j].Suppressed {
				t.Fatalf("frame %d: unprotected loss was marked suppressed", i)
			}
		}
	}
}

func TestPacketLossDoesNotShiftFrameDrop(t *testing.T) {
	drops := func(specs []Spec, withPackets bool) []int {
		e, err := New(seed.Seed(99), specs)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for i := 0; i < 300; i++ {
			for _, d := range e.DecideFrame(i, false) {
				if d.Kind == KindFrameDrop {
					out = append(out, i)
				}
			}
			if withPackets {
				e.DecidePackets(i, 3, false)
			}
		}
		return out
	}
	base := drops([]Spec{{Kind: KindFrameDrop, Rate: 0.4}}, false)
	both := drops([]Spec{{Kind: KindFrameDrop, Rate: 0.4}, {Kind: KindPacketLoss, Rate: 0.4}}, true)
	if !reflect.DeepEqual(base, both) {
		t.Fatal("adding packet_loss moved frame_drop decisions")
	}
}
