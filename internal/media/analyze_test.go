package media

import (
	"slices"
	"testing"
)

var (
	pSlice = []byte{0x41, 0x9a} // H.264 non-IDR slice, slice_type 5 (P)
	bSlice = []byte{0x01, 0xa0} // H.264 non-IDR slice, slice_type 1 (B)
)

// synth builds n access units, step ticks apart at 90 kHz, with a keyframe
// every 15 frames. mod may rewrite any unit.
func synth(codec Codec, n int, step int64, mod func(i int, au *AccessUnit)) *Media {
	m := &Media{Codec: codec, Width: 640, Height: 480, FPS: 90000 / float64(step)}
	for i := 0; i < n; i++ {
		au := AccessUnit{
			NALUs: [][]byte{pSlice}, PTS: int64(i) * step, DTS: int64(i) * step,
			RandomAccess: i%15 == 0,
		}
		if mod != nil {
			mod(i, &au)
		}
		m.AUs = append(m.AUs, au)
	}
	return m
}

func codesOf(r Report) []string {
	var c []string
	for _, is := range r.Issues {
		c = append(c, is.Code)
	}
	slices.Sort(c)
	return c
}

func TestAnalyze(t *testing.T) {
	const step = 3000 // 30 fps
	onlyFirstKey := func(i int, au *AccessUnit) { au.RandomAccess = i == 0 }
	// Decode order 0,2,1,3,5,4,...: a reorder depth of 1.
	reorder := func(i int, au *AccessUnit) {
		switch i % 3 {
		case 1:
			au.PTS = int64(i+1) * step
		case 2:
			au.PTS = int64(i-1) * step
		}
	}
	cases := []struct {
		name   string
		m      *Media
		size   int64
		status string
		want   []string
	}{
		{"clean one second clip", synth(CodecH264, 30, step, nil), 1000, StatusValid, nil},
		{"no frames", &Media{Codec: CodecH264}, 10, StatusInvalid, []string{"no_frames"}},
		{"first frame is not a keyframe", synth(CodecH264, 30, step, func(i int, au *AccessUnit) {
			au.RandomAccess = i != 0 && i%15 == 0
		}), 1000, StatusInvalid, []string{"no_start_keyframe"}},
		{"h264 B slice", synth(CodecH264, 30, step, func(i int, au *AccessUnit) {
			if i == 5 {
				au.NALUs = [][]byte{bSlice}
			}
		}), 1000, StatusWarning, []string{"b_frames"}},
		{"h264 reordering without B slices", synth(CodecH264, 30, step, reorder), 1000, StatusWarning, []string{"pts_dts_reordering"}},
		{"h265 reordering implies B-frames", synth(CodecH265, 30, step, reorder), 1000, StatusWarning, []string{"b_frames", "pts_dts_reordering"}},
		{"constant PTS offset is not reordering", synth(CodecH264, 30, step, func(i int, au *AccessUnit) { au.PTS += 6000 }), 1000, StatusValid, nil},
		{"DTS repeats", synth(CodecH264, 30, step, func(i int, au *AccessUnit) {
			if i == 10 {
				au.DTS, au.PTS = 9*step, 9*step
			}
		}), 1000, StatusWarning, []string{"dts_not_monotonic"}},
		{"timestamp jump", synth(CodecH264, 30, step, func(i int, au *AccessUnit) {
			if i >= 10 {
				au.DTS += 10 * step
				au.PTS += 10 * step
			}
		}), 1000, StatusWarning, []string{"timestamp_jump"}},
		{"variable frame rate", synth(CodecH264, 30, step, func(i int, au *AccessUnit) {
			d := int64(i) * 3000
			if i > 15 {
				d = 15*3000 + int64(i-15)*6000
			}
			au.DTS, au.PTS = d, d
		}), 1000, StatusWarning, []string{"variable_frame_rate"}},
		{"declared fps disagrees with the timestamps", func() *Media {
			m := synth(CodecH264, 30, step, nil)
			m.FPS = 25
			return m
		}(), 1000, StatusWarning, []string{"fps_mismatch"}},
		{"gop of exactly 10 s is fine", synth(CodecH264, 300, step, onlyFirstKey), 1000, StatusValid, nil},
		{"gop just over 10 s", synth(CodecH264, 301, step, onlyFirstKey), 1000, StatusWarning, []string{"long_gop"}},
		{"clip of exactly 1 s is fine", synth(CodecH264, 30, step, nil), 1000, StatusValid, nil},
		{"clip just under 1 s", synth(CodecH264, 29, step, nil), 1000, StatusWarning, []string{"short_clip"}},
		{"odd width", func() *Media {
			m := synth(CodecH264, 30, step, nil)
			m.Width = 641
			return m
		}(), 1000, StatusWarning, []string{"odd_dimensions"}},
		{"large file", synth(CodecH264, 30, step, nil), 600 << 20, StatusWarning, []string{"large_file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Analyze(c.m, c.size)
			if r.Status != c.status {
				t.Errorf("status = %q, want %q (issues %v)", r.Status, c.status, codesOf(r))
			}
			if got := codesOf(r); !slices.Equal(got, c.want) {
				t.Errorf("codes = %v, want %v", got, c.want)
			}
			if r.Issues == nil {
				t.Error("Issues is nil; it must encode as []")
			}
		})
	}
}

func TestAnalyzeFacts(t *testing.T) {
	r := Analyze(synth(CodecH264, 30, 3000, nil), 90000)
	f := r.Facts
	if f.Frames != 30 || f.Keyframes != 2 || f.MaxGOP != 15 || f.ReorderDepth != 0 {
		t.Errorf("counts = %+v", f)
	}
	if f.Width != 640 || f.Height != 480 || f.Codec != CodecH264 || f.SizeBytes != 90000 {
		t.Errorf("identity = %+v", f)
	}
	if f.DurationSeconds != 1 || f.MeasuredFPS != 30 || f.BitrateBps != 720000 {
		t.Errorf("timing = %+v", f)
	}
}

func TestAnalyzeReorderDepth(t *testing.T) {
	m := synth(CodecH264, 30, 3000, func(i int, au *AccessUnit) {
		switch i % 3 {
		case 1:
			au.PTS = int64(i+1) * 3000
		case 2:
			au.PTS = int64(i-1) * 3000
		}
	})
	if d := Analyze(m, 1).Facts.ReorderDepth; d != 1 {
		t.Errorf("ReorderDepth = %d, want 1", d)
	}
}

func TestAnalyzeBundledFixturesAreValid(t *testing.T) {
	for name, src := range map[string]Source{"h264": FixtureSource{}, "h265": FixtureH265Source{}} {
		m, err := src.Load()
		if err != nil {
			t.Fatal(err)
		}
		if r := Analyze(m, int64(len(fixtureData()))); r.Status != StatusValid {
			t.Errorf("%s fixture: status %q, issues %v", name, r.Status, r.Issues)
		}
	}
}
