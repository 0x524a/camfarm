# Media library and validator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Directories of video the farm can serve, upload through the dashboard API, and a one-time per-file validity report that blocks invalid files from becoming cameras.

**Architecture:** `internal/media` gains a pure `Analyze` plus `AnalyzeFile`. A new `internal/library` owns roots, scan, cache, upload and delete. `internal/dashboard` adds thin handlers and a `libraryId` on camera creation. `cmd/camfarm` gains flags. This plan delivers the API only; the Library tab UI is the later redesign sub-project.

**Tech Stack:** Go 1.26, `CGO_ENABLED=0`, `mediacommon` (already a dependency), stdlib `net/http`, `ffmpeg` as a test oracle (already required by `internal/media` tests).

**Spec:** `docs/superpowers/specs/2026-10-11-media-library-design.md`

## Global Constraints

- `CGO_ENABLED=0` keeps building.
- Determinism is the product: nothing here may introduce unseeded randomness into the fault or serving path.
- Real sockets and real files in tests; no mocks of the filesystem.
- No coverage figure stated anywhere that CI did not produce.
- No employer or client names anywhere (code, comments, commits, docs).
- Commit with the repo's configured git identity; never change it.
- Plain `git` commands, one per call (a shell guard in this worktree refuses compound git commands and heredocs).
- Run `go vet ./...` and `go test -race ./...` before the final task's commit.

## Review Focus

1. Upload names that try to escape or hide (`../x.mp4`, `a\b.mp4`, `.hidden.mp4`, empty, NUL): base name or 400, never a file outside the upload root. Pinned in Task 4.
2. A library file edited or deleted after the scan, then used for a camera: re-checked at creation, 422 or 404, never a panic or a stale "valid". Pinned in Task 3 (`Refresh`) and Task 6.
3. A large, slow upload: not cut off by the binary's 30 s `ReadTimeout`, and an over-limit upload leaves no partial file. Pinned in Task 4 and Task 5.
4. Corrupt, empty, truncated, or mislabelled files (a TS named `.mp4`, random bytes, a parser panic): `invalid` with a code, no panic, dashboard stays up. Pinned in Task 2.
5. Scan, upload and list running at once: no data race under `-race`. Pinned in Task 3 and Task 4.

## Spec amendments (applied in Task 1)

The code reading done while planning changed seven details. Task 1 edits the spec so spec and plan agree:

1. `unsupported_codec` is removed. The loader returns `ErrNoVideoTrack` for any non-H.264/H.265 video, so it folds into `no_video`.
2. `libraryId` is a sibling of `source` in the camera body, not inside it (`SourceSpec` is public library API).
3. Uploads go to the first writable **startup** root; roots added from the UI never receive uploads.
4. `pts_dts_reordering` means PTS out of decode order (reorder depth above 0). A constant PTS−DTS offset is not flagged.
5. `variable_frame_rate` uses the 5th–95th percentile spread of DTS steps; `fps_mismatch` compares the declared fps with `90000 / median step`.
6. `Facts.Duration` becomes `DurationSeconds float64`.
7. Camera creation from a library id re-checks the file if it changed since the scan; the upload route lifts the server `ReadTimeout`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/media/slicetype.go` (+ test) | bit reader and `isH264BSlice` |
| `internal/media/analyze.go` (+ test) | `Report`, `Facts`, `Issue`, pure `Analyze` |
| `internal/media/analyzefile.go` (+ test) | `AnalyzeFile`: sniff, load, map errors, recover panics |
| `internal/media/assemble.go` | `ErrNoAccessUnits` sentinel |
| `internal/library/library.go` (+ test) | roots, scan, cache, `Refresh`, lookups |
| `internal/library/upload.go` (+ test) | `Upload`, `Delete`, name rules, part-file cleanup |
| `internal/dashboard/library.go` (+ test) | library handlers |
| `internal/dashboard/dashboard.go` | `Options.Library`, routes, `libraryId` on add, DTO fields |
| `cmd/camfarm/run.go`, `size.go` (+ tests) | flags, `parseSize`, startup scan |
| `README.md` | usage section |

---

### Task 1: Pure validator

**Files:**
- Modify: `docs/superpowers/specs/2026-10-11-media-library-design.md`
- Create: `internal/media/slicetype.go`, `internal/media/slicetype_test.go`
- Create: `internal/media/analyze.go`, `internal/media/analyze_test.go`

**Interfaces:**
- Consumes: `Media`, `AccessUnit`, `Codec`, `Media.FrameDuration()`, the package constant `clockRate` (90000).
- Produces (later tasks rely on these exact names):
  - `type Severity string` with `SeverityWarning`, `SeverityInvalid`.
  - `const StatusValid, StatusWarning, StatusInvalid = "valid", "warning", "invalid"`.
  - `type Issue struct { Code string; Severity Severity; Message, Detail string }` (JSON: `code`, `severity`, `message`, `detail,omitempty`).
  - `type Facts struct { Container string; Codec Codec; Width, Height int; FPS, MeasuredFPS float64; DurationSeconds float64; Frames, Keyframes, MaxGOP, ReorderDepth int; BitrateBps, SizeBytes int64 }` (JSON lower camel case).
  - `type Report struct { Status string; Facts Facts; Issues []Issue }` (JSON `status`, `facts`, `issues`; `Issues` is never nil).
  - `func Analyze(m *Media, size int64) Report`.

- [ ] **Step 1: Amend the spec**

Edit `docs/superpowers/specs/2026-10-11-media-library-design.md` to apply the seven amendments listed above: delete the `unsupported_codec` row (and mention that `no_video` covers any non-H.264/H.265 video); move `libraryId` out of `source` in the API section; say UI roots never receive uploads; reword `pts_dts_reordering`, `variable_frame_rate` and `fps_mismatch`; replace `Duration time.Duration` with `DurationSeconds float64`; add the re-check-on-creation and `ReadTimeout` notes. Commit:

```bash
git add docs/superpowers/specs/2026-10-11-media-library-design.md
git commit -m "docs: align media library spec with the loader's real error set"
```

- [ ] **Step 2: Write the failing slice-type test**

Create `internal/media/slicetype_test.go`:

```go
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
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/media -run 'TestIsH264BSlice|TestUnescapeRBSP' -v`
Expected: FAIL to compile with `undefined: isH264BSlice`.

- [ ] **Step 4: Implement the parser**

Create `internal/media/slicetype.go`:

```go
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
```

- [ ] **Step 5: Run the tests and commit**

Run: `go test ./internal/media -run 'TestIsH264BSlice|TestUnescapeRBSP' -v`
Expected: PASS.

```bash
git add internal/media/slicetype.go internal/media/slicetype_test.go
git commit -m "media: parse H.264 slice_type to spot B slices"
```

- [ ] **Step 6: Write the failing Analyze tests**

Create `internal/media/analyze_test.go`:

```go
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
```

- [ ] **Step 7: Run it to verify it fails**

Run: `go test ./internal/media -run 'TestAnalyze' -v`
Expected: FAIL to compile with `undefined: Analyze`.

- [ ] **Step 8: Implement Analyze**

Create `internal/media/analyze.go`:

```go
package media

import (
	"fmt"
	"math"
	"slices"
)

// Severity ranks an Issue.
type Severity string

// Issue severities.
const (
	SeverityWarning Severity = "warning"
	SeverityInvalid Severity = "invalid"
)

// Report statuses.
const (
	StatusValid   = "valid"
	StatusWarning = "warning"
	StatusInvalid = "invalid"
)

// Thresholds. Each is pinned by a boundary row in analyze_test.go.
const (
	jumpFactor     = 4.0              // a DTS step this many times the median is a jump
	vfrSpread      = 0.10             // (p95-p5)/median above this is variable frame rate
	fpsTolerance   = 0.05             // declared vs measured fps
	longGOPTicks   = 10 * clockRate   // longest keyframe gap before a warning
	shortClipTicks = clockRate        // shorter clips loop every second
	largeFileBytes = 512 << 20        // the whole parsed file is held in memory
	reorderWindow  = 32               // frames looked back when measuring reorder depth
)

// Issue is one finding about a file.
type Issue struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Detail   string   `json:"detail,omitempty"`
}

// Facts are what was measured, whether or not anything is wrong.
type Facts struct {
	Container       string  `json:"container,omitempty"`
	Codec           Codec   `json:"codec,omitempty"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	FPS             float64 `json:"fps"`
	MeasuredFPS     float64 `json:"measuredFps"`
	DurationSeconds float64 `json:"durationSeconds"`
	Frames          int     `json:"frames"`
	Keyframes       int     `json:"keyframes"`
	MaxGOP          int     `json:"maxGop"`
	ReorderDepth    int     `json:"reorderDepth"`
	BitrateBps      int64   `json:"bitrateBps"`
	SizeBytes       int64   `json:"sizeBytes"`
}

// Report is the verdict on one file.
type Report struct {
	Status string  `json:"status"`
	Facts  Facts   `json:"facts"`
	Issues []Issue `json:"issues"`
}

func (r *Report) add(code string, sev Severity, msg, detail string) {
	r.Issues = append(r.Issues, Issue{Code: code, Severity: sev, Message: msg, Detail: detail})
}

func (r *Report) finish() Report {
	if r.Issues == nil {
		r.Issues = []Issue{}
	}
	r.Status = StatusValid
	for _, is := range r.Issues {
		if is.Severity == SeverityInvalid {
			r.Status = StatusInvalid
			return *r
		}
		r.Status = StatusWarning
	}
	return *r
}

// Analyze inspects parsed media. size is the file's size in bytes. It is pure:
// it never touches the filesystem.
func Analyze(m *Media, size int64) Report {
	r := Report{Facts: Facts{
		Codec: m.Codec, Width: m.Width, Height: m.Height, FPS: m.FPS,
		SizeBytes: size, Frames: len(m.AUs),
	}}
	if len(m.AUs) == 0 {
		r.add("no_frames", SeverityInvalid, "The file holds no video frames.", "")
		return r.finish()
	}
	if !m.AUs[0].RandomAccess {
		r.add("no_start_keyframe", SeverityInvalid,
			"The first frame is not a keyframe, so a client cannot start decoding at the beginning.", "")
	}

	steps := dtsSteps(m)
	median := medianOf(steps)
	frameDur := m.FrameDuration()
	r.Facts.Keyframes, r.Facts.MaxGOP = gopStats(m)
	r.Facts.ReorderDepth = reorderDepth(m)
	span := m.AUs[len(m.AUs)-1].DTS - m.AUs[0].DTS + frameDur
	r.Facts.DurationSeconds = float64(span) / clockRate
	if median > 0 {
		r.Facts.MeasuredFPS = clockRate / float64(median)
	}
	if r.Facts.DurationSeconds > 0 {
		r.Facts.BitrateBps = int64(float64(size*8) / r.Facts.DurationSeconds)
	}

	depth := r.Facts.ReorderDepth
	switch {
	case m.Codec == CodecH264:
		if n := countBSlices(m); n > 0 {
			r.add("b_frames", SeverityWarning, "The video uses B-frames.",
				fmt.Sprintf("%d of %d frames are B slices (read from the H.264 slice headers)", n, len(m.AUs)))
		}
	case depth > 0:
		r.add("b_frames", SeverityWarning, "The video appears to use B-frames.",
			"inferred from timestamps only; H.265 slice types are not parsed")
	}
	if depth > 0 {
		r.add("pts_dts_reordering", SeverityWarning,
			"Frames are stored out of display order: presentation times do not follow decode order.",
			fmt.Sprintf("reorder depth %d", depth))
	}

	if bad := nonMonotonic(m); bad > 0 {
		r.add("dts_not_monotonic", SeverityWarning, "Decode timestamps go backwards or repeat.",
			fmt.Sprintf("%d frames", bad))
	}
	var normal []int64
	jumps := 0
	for _, s := range steps {
		if median > 0 && float64(s) > jumpFactor*float64(median) {
			jumps++
		} else {
			normal = append(normal, s)
		}
	}
	if jumps > 0 {
		r.add("timestamp_jump", SeverityWarning, "Timestamps jump forward by far more than one frame.",
			fmt.Sprintf("%d jumps over %.0f times the typical frame spacing", jumps, jumpFactor))
	}
	if len(normal) >= 3 && median > 0 {
		lo, hi := percentile(normal, 0.05), percentile(normal, 0.95)
		if spread := float64(hi-lo) / float64(median); spread > vfrSpread {
			r.add("variable_frame_rate", SeverityWarning, "The frame spacing varies, so the frame rate is not constant.",
				fmt.Sprintf("spacing runs from %d to %d ticks (typical %d)", lo, hi, median))
		}
	}
	if m.FPS > 0 && r.Facts.MeasuredFPS > 0 &&
		math.Abs(m.FPS-r.Facts.MeasuredFPS)/m.FPS > fpsTolerance {
		r.add("fps_mismatch", SeverityWarning, "The frame rate declared in the stream differs from the timestamps.",
			fmt.Sprintf("declared %.3f fps, measured %.3f fps", m.FPS, r.Facts.MeasuredFPS))
	}
	if int64(r.Facts.MaxGOP)*frameDur > longGOPTicks {
		r.add("long_gop", SeverityWarning, "Keyframes are more than 10 seconds apart.",
			fmt.Sprintf("longest gap %d frames", r.Facts.MaxGOP))
	}
	if span < shortClipTicks {
		r.add("short_clip", SeverityWarning, "The clip is shorter than one second, so it loops every second.",
			fmt.Sprintf("%.2f s", r.Facts.DurationSeconds))
	}
	if m.Width%2 != 0 || m.Height%2 != 0 {
		r.add("odd_dimensions", SeverityWarning, "The picture has an odd width or height.",
			fmt.Sprintf("%dx%d", m.Width, m.Height))
	}
	if size > largeFileBytes {
		r.add("large_file", SeverityWarning, "The file is large; the farm holds all of it in memory once a camera uses it.",
			fmt.Sprintf("%d MiB", size>>20))
	}
	return r.finish()
}

// dtsSteps returns the positive gaps between consecutive decode timestamps.
func dtsSteps(m *Media) []int64 {
	var out []int64
	for i := 1; i < len(m.AUs); i++ {
		if d := m.AUs[i].DTS - m.AUs[i-1].DTS; d > 0 {
			out = append(out, d)
		}
	}
	return out
}

func nonMonotonic(m *Media) int {
	n := 0
	for i := 1; i < len(m.AUs); i++ {
		if m.AUs[i].DTS <= m.AUs[i-1].DTS {
			n++
		}
	}
	return n
}

func medianOf(v []int64) int64 { return percentile(v, 0.5) }

func percentile(v []int64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return s[int(math.Round(p*float64(len(s)-1)))]
}

// gopStats counts keyframes and finds the longest keyframe-to-keyframe run,
// including the tail after the last keyframe (the loop restarts at a keyframe).
func gopStats(m *Media) (keyframes, maxGOP int) {
	prev := -1
	for i, au := range m.AUs {
		if !au.RandomAccess {
			continue
		}
		keyframes++
		if prev >= 0 {
			maxGOP = max(maxGOP, i-prev)
		}
		prev = i
	}
	if prev >= 0 {
		maxGOP = max(maxGOP, len(m.AUs)-prev)
	}
	return keyframes, maxGOP
}

// reorderDepth is the most frames that precede a frame in decode order but
// follow it in presentation order, looking back at most reorderWindow frames.
func reorderDepth(m *Media) int {
	depth := 0
	for i := range m.AUs {
		n := 0
		for j := max(0, i-reorderWindow); j < i; j++ {
			if m.AUs[j].PTS > m.AUs[i].PTS {
				n++
			}
		}
		depth = max(depth, n)
	}
	return depth
}

func countBSlices(m *Media) int {
	n := 0
	for _, au := range m.AUs {
		for _, nalu := range au.NALUs {
			if isH264BSlice(nalu) {
				n++
				break
			}
		}
	}
	return n
}
```

- [ ] **Step 9: Run the tests**

Run: `gofmt -l internal/media` (expected: no output; run `gofmt -w` on the new files if it lists any), then `go vet ./internal/media` and `go test ./internal/media -run 'TestAnalyze|TestIsH264BSlice' -v`
Expected: PASS. If a boundary row fails, fix the threshold or its comparison, not the row: the rows are the spec.

- [ ] **Step 10: Commit**

```bash
git add internal/media/analyze.go internal/media/analyze_test.go
git commit -m "media: add Analyze, a pure validity report over parsed media"
```

---

### Task 2: `AnalyzeFile`

**Files:**
- Modify: `internal/media/assemble.go` (sentinel)
- Create: `internal/media/analyzefile.go`, `internal/media/analyzefile_test.go`

**Interfaces:**
- Consumes: `sniff`, `containerMPEGTS`, `containerISOBMFF`, `sniffLen`, `parseMPEGTS(io.ReadSeeker)`, `parseISOBMFF(io.ReadSeeker)`, `ErrNoVideoTrack`, `ErrNoParameterSetsH264/H265`, `errNoSamples`, `Analyze`, `Report`, `Facts`, `Issue`.
- Produces: `var ErrNoAccessUnits`; `func AnalyzeFile(path string) Report` (never panics, never returns an error: every failure is an `invalid` report).

- [ ] **Step 1: Add the sentinel**

In `internal/media/assemble.go` replace `errors.New("media: source contains no access units")` with a package-level sentinel:

```go
// ErrNoAccessUnits is returned when a source parses but holds no frames.
var ErrNoAccessUnits = errors.New("media: source contains no access units")
```

and `return nil, ErrNoAccessUnits` in `assemble`. Run `go test ./internal/media` (expected PASS: the message is unchanged).

- [ ] **Step 2: Write the failing tests**

Create `internal/media/analyzefile_test.go`:

```go
package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func firstInvalid(t *testing.T, r Report) Issue {
	t.Helper()
	if r.Status != StatusInvalid {
		t.Fatalf("status = %q, want invalid (issues %v)", r.Status, r.Issues)
	}
	for _, is := range r.Issues {
		if is.Severity == SeverityInvalid {
			return is
		}
	}
	t.Fatal("status invalid but no invalid issue")
	return Issue{}
}

func TestAnalyzeFileBundledFixtures(t *testing.T) {
	for name, b := range map[string][]byte{"h264": fixture.Bytes(), "h265": fixture.BytesH265()} {
		r := AnalyzeFile(writeTemp(t, name+".ts", b))
		if r.Status != StatusValid || r.Facts.Container != string(containerMPEGTS) || r.Facts.Frames != 30 {
			t.Errorf("%s: %+v", name, r)
		}
		if r.Facts.SizeBytes != int64(len(b)) {
			t.Errorf("%s: SizeBytes = %d, want %d", name, r.Facts.SizeBytes, len(b))
		}
	}
}

func TestAnalyzeFileFailures(t *testing.T) {
	trunc := fixture.Bytes()[:188*5+17] // cut mid-packet
	cases := []struct {
		name string
		path string
		code string
	}{
		{"missing file", filepath.Join(t.TempDir(), "nope.ts"), "unreadable"},
		{"empty file", writeTemp(t, "empty.ts", nil), "unsupported_container"},
		{"random bytes", writeTemp(t, "noise.mp4", []byte("this is not video, it is a sentence of text")), "unsupported_container"},
		{"directory", t.TempDir(), "unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstInvalid(t, AnalyzeFile(c.path)); got.Code != c.code {
				t.Errorf("code = %q, want %q (%+v)", got.Code, c.code, got)
			}
		})
	}
	t.Run("truncated TS is invalid or valid-with-fewer-frames, never a panic", func(t *testing.T) {
		r := AnalyzeFile(writeTemp(t, "cut.ts", trunc))
		if r.Status == "" {
			t.Fatal("no status")
		}
	})
}

// A file whose name lies about its container still loads; the report names the
// container it really is.
func TestAnalyzeFileIgnoresExtension(t *testing.T) {
	r := AnalyzeFile(writeTemp(t, "actually-ts.mp4", fixture.Bytes()))
	if r.Status != StatusValid || r.Facts.Container != string(containerMPEGTS) {
		t.Errorf("%+v", r)
	}
}

func TestAnalyzeFileAudioOnlyMP4(t *testing.T) {
	out := filepath.Join(t.TempDir(), "audio.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac", out}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	got := firstInvalid(t, AnalyzeFile(out))
	if got.Code != "no_video" {
		t.Errorf("code = %q, want no_video (%+v)", got.Code, got)
	}
}

// Real B-frames, produced by an encoder, must be flagged. This is the check the
// synthetic tests cannot make: that isH264BSlice agrees with a real bitstream.
func TestAnalyzeFileRealBFrames(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-c:v", "libx264", "-bf", "2", "-g", "25", "-pix_fmt", "yuv420p", out}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	r := AnalyzeFile(out)
	codes := codesOf(r)
	if r.Status != StatusWarning || !contains(codes, "b_frames") || !contains(codes, "pts_dts_reordering") {
		t.Errorf("status %q codes %v facts %+v", r.Status, codes, r.Facts)
	}
	if r.Facts.Container != string(containerISOBMFF) {
		t.Errorf("container = %q", r.Facts.Container)
	}

	noB := filepath.Join(dir, "nob.mp4")
	args = []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-c:v", "libx264", "-bf", "0", "-g", "25", "-pix_fmt", "yuv420p", noB}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	if r := AnalyzeFile(noB); contains(codesOf(r), "b_frames") {
		t.Errorf("no-B encode flagged: %v", codesOf(r))
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestClassifyLoadError(t *testing.T) {
	cases := map[string]error{
		"no_video":                ErrNoVideoTrack,
		"missing_parameter_sets":  ErrNoParameterSetsH264,
		"no_frames":               ErrNoAccessUnits,
		"loader_error":            os.ErrInvalid,
	}
	for code, err := range cases {
		r := classifyLoadError(Facts{}, err)
		if got := firstInvalid(t, r); got.Code != code {
			t.Errorf("%v: code = %q, want %q", err, got.Code, code)
		}
	}
}

func TestRecoverFromParserPanic(t *testing.T) {
	r := reportFromPanic(Facts{Container: "MPEG-TS"}, "boom")
	got := firstInvalid(t, r)
	if got.Code != "loader_error" || got.Detail != "boom" || r.Facts.Container != "MPEG-TS" {
		t.Errorf("%+v", r)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/media -run 'TestAnalyzeFile|TestClassifyLoadError|TestRecoverFromParserPanic' -v`
Expected: FAIL to compile with `undefined: AnalyzeFile`.

- [ ] **Step 4: Implement**

Create `internal/media/analyzefile.go`:

```go
package media

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func invalidReport(f Facts, code, msg, detail string) Report {
	r := Report{Facts: f}
	r.add(code, SeverityInvalid, msg, detail)
	return r.finish()
}

// reportFromPanic turns a parser panic into an invalid report. A hostile or
// corrupt file must never take the dashboard down.
func reportFromPanic(f Facts, p any) Report {
	return invalidReport(f, "loader_error", "The parser crashed on this file.", fmt.Sprint(p))
}

// classifyLoadError maps the loader's errors onto report codes.
func classifyLoadError(f Facts, err error) Report {
	switch {
	case errors.Is(err, ErrNoVideoTrack), errors.Is(err, errNoSamples):
		return invalidReport(f, "no_video", "No H.264 or H.265 video was found in the file.", err.Error())
	case errors.Is(err, ErrNoParameterSetsH264), errors.Is(err, ErrNoParameterSetsH265):
		return invalidReport(f, "missing_parameter_sets",
			"The stream carries no in-band parameter sets (SPS/PPS), so a client cannot configure its decoder.", err.Error())
	case errors.Is(err, ErrNoAccessUnits):
		return invalidReport(f, "no_frames", "The file holds no video frames.", err.Error())
	default:
		return invalidReport(f, "loader_error", "The file could not be parsed.", err.Error())
	}
}

// AnalyzeFile validates one file: it detects the container from content, loads
// it with the same parser the RTSP pumps use, and inspects the result. Every
// failure is reported as an invalid Report; it never panics and never returns
// an error.
func AnalyzeFile(path string) (rep Report) {
	f, err := os.Open(path)
	if err != nil {
		return invalidReport(Facts{}, "unreadable", "The file could not be opened.", err.Error())
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return invalidReport(Facts{}, "unreadable", "The file could not be read.", err.Error())
	}
	facts := Facts{SizeBytes: st.Size()}
	if st.IsDir() {
		return invalidReport(facts, "unreadable", "This is a directory, not a file.", "")
	}

	header := make([]byte, sniffLen)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return invalidReport(facts, "unreadable", "The file could not be read.", err.Error())
	}
	kind, err := sniff(header[:n])
	if err != nil {
		return invalidReport(facts, "unsupported_container",
			"The file is not an MPEG-TS or MP4/MOV container.", err.Error())
	}
	facts.Container = string(kind)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return invalidReport(facts, "unreadable", "The file could not be read.", err.Error())
	}

	defer func() {
		if p := recover(); p != nil {
			rep = reportFromPanic(facts, p)
		}
	}()
	var m *Media
	switch kind {
	case containerMPEGTS:
		m, err = parseMPEGTS(f)
	default:
		m, err = parseISOBMFF(f)
	}
	if err != nil {
		return classifyLoadError(facts, err)
	}
	rep = Analyze(m, st.Size())
	rep.Facts.Container = facts.Container
	return rep
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/media -run 'TestAnalyzeFile|TestClassifyLoadError|TestRecoverFromParserPanic' -v`
Expected: PASS. Two results need a look, not a blind fix:
- `TestAnalyzeFileAudioOnlyMP4`: if the code is `loader_error`, print the error chain and extend `classifyLoadError` so an MP4 with no video samples maps to `no_video`; the test's expectation stays.
- `TestAnalyzeFileRealBFrames`: if `b_frames` is missing on a real B-frame file, the slice parser is wrong for real bitstreams: debug `isH264BSlice` against the file's NAL units. Do not weaken the test.

- [ ] **Step 6: Whole package, then commit**

Run: `gofmt -l internal/media` (no output), `go vet ./internal/media`, `go test -race ./internal/media`
Expected: PASS.

```bash
git add internal/media
git commit -m "media: AnalyzeFile reports every failure as an invalid report"
```

---

### Task 3: Library core: roots, scan, cache, refresh

**Files:**
- Create: `internal/library/library.go`, `internal/library/library_test.go`

**Interfaces:**
- Consumes: `media.AnalyzeFile`, `media.Report`, `media.Facts`, `media.Issue`, `media.StatusInvalid`, `media.SeverityInvalid`.
- Produces (Tasks 4–7 rely on these exact names):

```go
var ErrReadOnly, ErrTooLarge, ErrNotDeletable, ErrNotFound, ErrBadName, ErrUIPathsDisabled, ErrNotDirectory error

type Options struct {
	Roots        []string
	AllowUIPaths bool
	MaxUpload    int64                       // 0 means 2 GiB
	Workers      int                         // 0 means 2
	Timeout      time.Duration               // per-file validation limit; 0 means 30s
	Analyze      func(path string) media.Report // nil means media.AnalyzeFile; tests only
}
type RootInfo struct { Path string `json:"path"`; Writable bool `json:"writable"`; UI bool `json:"ui"` }
type Entry struct {
	ID string `json:"id"`; Root string `json:"root"`; Path string `json:"path"`; Name string `json:"name"`
	Size int64 `json:"size"`; Modified time.Time `json:"modified"`; Report media.Report `json:"report"`
}
func New(opt Options) (*Library, error)
func (l *Library) Roots() []RootInfo
func (l *Library) UploadEnabled() bool
func (l *Library) MaxUpload() int64
func (l *Library) Scanning() bool
func (l *Library) List() []Entry                       // sorted by Name then Path
func (l *Library) Resolve(id string) (Entry, bool)
func (l *Library) ByPath(path string) (Entry, bool)
func (l *Library) Scan(ctx context.Context, root string, force bool) error // root "" means all
func (l *Library) AddRoot(path string) error
func (l *Library) Refresh(ctx context.Context, id string) (Entry, error)   // re-stats; re-analyzes if changed; ErrNotFound if gone
```

Task 4 adds `Upload` and `Delete` in `upload.go` and uses the unexported helpers `l.analyze(ctx, path) (media.Report, bool)`, `l.uploadRoot() (string, bool)`, `idFor(path) string`, `videoExt`.

- [ ] **Step 1: Write the failing tests**

Create `internal/library/library_test.go`:

```go
package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newLib(t *testing.T, opt Options) *Library {
	t.Helper()
	l, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func scanAll(t *testing.T, l *Library) {
	t.Helper()
	if err := l.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
}

func names(l *Library) []string {
	var out []string
	for _, e := range l.List() {
		out = append(out, e.Name)
	}
	return out
}

func TestScanListsVideoFilesWithRealReports(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "good.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "bad.mp4"), []byte("not video at all, just some text"))
	write(t, filepath.Join(dir, "notes.txt"), []byte("ignored"))
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)

	got := l.List()
	if len(got) != 2 || got[0].Name != "bad.mp4" || got[1].Name != "good.ts" {
		t.Fatalf("List = %v", names(l))
	}
	if got[0].Report.Status != media.StatusInvalid || got[1].Report.Status != media.StatusValid {
		t.Errorf("statuses = %q, %q", got[0].Report.Status, got[1].Report.Status)
	}
	if got[1].Size != int64(len(fixture.Bytes())) || got[1].Path != filepath.Join(l.Roots()[0].Path, "good.ts") {
		t.Errorf("entry = %+v", got[1])
	}
	if e, ok := l.Resolve(got[1].ID); !ok || e.Path != got[1].Path {
		t.Errorf("Resolve = %+v, %v", e, ok)
	}
	if e, ok := l.ByPath(got[1].Path); !ok || e.ID != got[1].ID {
		t.Errorf("ByPath = %+v, %v", e, ok)
	}
}

func TestScanDepthLimit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "x", "b.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "x", "y", "c.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "x", "y", "z", "d.ts"), fixture.Bytes()) // depth 4: too deep
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	want := []string{"a.ts", filepath.Join("x", "b.ts"), filepath.Join("x", "y", "c.ts")}
	if got := names(l); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestScanSkipsSymlinksAndHiddenFiles(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "secret.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "real.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, ".hidden.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, ".upload-1.part"), []byte("partial"))
	if err := os.Symlink(filepath.Join(outside, "secret.ts"), filepath.Join(dir, "link.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	if got := names(l); len(got) != 1 || got[0] != "real.ts" {
		t.Errorf("names = %v, want only real.ts", got)
	}
}

func TestScanCacheAndForce(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.ts"), filepath.Join(dir, "b.ts")
	write(t, a, fixture.Bytes())
	write(t, b, fixture.Bytes())
	var calls atomic.Int32
	l := newLib(t, Options{Roots: []string{dir}, Analyze: func(p string) media.Report {
		calls.Add(1)
		return media.AnalyzeFile(p)
	}})
	scanAll(t, l)
	if calls.Load() != 2 {
		t.Fatalf("first scan analysed %d files, want 2", calls.Load())
	}
	scanAll(t, l)
	if calls.Load() != 2 {
		t.Errorf("unchanged rescan analysed again (%d calls)", calls.Load())
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(a, future, future); err != nil {
		t.Fatal(err)
	}
	scanAll(t, l)
	if calls.Load() != 3 {
		t.Errorf("after touching one file: %d calls, want 3", calls.Load())
	}
	if err := l.Scan(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Errorf("forced scan: %d calls, want 5", calls.Load())
	}
}

func TestScanDropsVanishedFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.ts")
	write(t, p, fixture.Bytes())
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	scanAll(t, l)
	if len(l.List()) != 0 {
		t.Errorf("List = %v after the file was removed", names(l))
	}
}

func TestScanUnknownRootAndMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	l := newLib(t, Options{Roots: []string{dir}})
	if err := l.Scan(context.Background(), "/not/a/root", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown root: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(context.Background(), "", false); err == nil {
		t.Error("scanning a root that was deleted: want an error")
	}
}

func TestAnalyzeTimeoutIsAnInvalidEntry(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "slow.ts"), fixture.Bytes())
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	l := newLib(t, Options{Roots: []string{dir}, Timeout: 50 * time.Millisecond,
		Analyze: func(string) media.Report { <-release; return media.Report{} }})
	scanAll(t, l)
	es := l.List()
	if len(es) != 1 || es[0].Report.Status != media.StatusInvalid || es[0].Report.Issues[0].Code != "loader_error" {
		t.Errorf("List = %+v", es)
	}
}

func TestNewRejectsBadRoots(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	write(t, f, []byte("x"))
	for name, root := range map[string]string{"missing": filepath.Join(t.TempDir(), "nope"), "a file": f} {
		if _, err := New(Options{Roots: []string{root}}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestRootsReportWritabilityAndUploadTarget(t *testing.T) {
	rw := t.TempDir()
	l := newLib(t, Options{Roots: []string{rw}})
	r := l.Roots()
	if len(r) != 1 || !r[0].Writable || r[0].UI || !l.UploadEnabled() {
		t.Errorf("roots = %+v, upload %v", r, l.UploadEnabled())
	}
	if os.Geteuid() != 0 {
		ro := t.TempDir()
		if err := os.Chmod(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
		l2 := newLib(t, Options{Roots: []string{ro}})
		if l2.Roots()[0].Writable || l2.UploadEnabled() {
			t.Errorf("read-only root reported writable: %+v", l2.Roots())
		}
	}
	if l3 := newLib(t, Options{}); l3.UploadEnabled() || len(l3.Roots()) != 0 {
		t.Error("a library with no roots must not allow uploads")
	}
}

func TestAddRootIsGated(t *testing.T) {
	dir := t.TempDir()
	if err := newLib(t, Options{}).AddRoot(dir); !errors.Is(err, ErrUIPathsDisabled) {
		t.Errorf("AddRoot without the flag = %v", err)
	}
	l := newLib(t, Options{AllowUIPaths: true})
	if err := l.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	if err := l.AddRoot(dir); err != nil || len(l.Roots()) != 1 {
		t.Errorf("adding the same root twice: err %v, roots %+v", err, l.Roots())
	}
	if r := l.Roots()[0]; !r.UI || r.Writable || l.UploadEnabled() {
		t.Errorf("a UI root must never be an upload target: %+v", r)
	}
	if err := l.AddRoot(filepath.Join(dir, "nope")); err == nil {
		t.Error("nonexistent path: want an error")
	}
	f := filepath.Join(dir, "f")
	write(t, f, []byte("x"))
	if err := l.AddRoot(f); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("file path = %v", err)
	}
}

func TestRefresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.ts")
	write(t, p, fixture.Bytes())
	var calls atomic.Int32
	l := newLib(t, Options{Roots: []string{dir}, Analyze: func(p string) media.Report {
		calls.Add(1)
		return media.AnalyzeFile(p)
	}})
	scanAll(t, l)
	id := l.List()[0].ID

	if _, err := l.Refresh(context.Background(), id); err != nil || calls.Load() != 1 {
		t.Fatalf("unchanged: err %v, calls %d (want 1: no re-analysis)", err, calls.Load())
	}
	write(t, p, []byte("overwritten with text"))
	e, err := l.Refresh(context.Background(), id)
	if err != nil || calls.Load() != 2 || e.Report.Status != media.StatusInvalid {
		t.Fatalf("changed: err %v, calls %d, status %q", err, calls.Load(), e.Report.Status)
	}
	if got, _ := l.Resolve(id); got.Report.Status != media.StatusInvalid {
		t.Error("Refresh did not update the cached entry")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refresh(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted file: %v", err)
	}
	if _, ok := l.Resolve(id); ok {
		t.Error("a vanished file stayed in the library")
	}
	if _, err := l.Refresh(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestConcurrentScanAndListIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.ts", "b.ts", "c.ts"} {
		write(t, filepath.Join(dir, n), fixture.Bytes())
	}
	l := newLib(t, Options{Roots: []string{dir}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = l.Scan(context.Background(), "", i%2 == 0) }()
		go func() { defer wg.Done(); _ = l.List(); _ = l.Scanning(); _ = l.Roots() }()
	}
	wg.Wait()
	if len(l.List()) != 3 {
		t.Errorf("List = %v", names(l))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/library -v`
Expected: FAIL to compile with `undefined: New`.

- [ ] **Step 3: Implement**

Create `internal/library/library.go`:

```go
// Package library is the set of video files the farm can serve: directories
// ("roots") that are scanned, an optional upload target, and a validity report
// per file. Validation runs once per file, on scan or upload, never continuously.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0x524a/camfarm/internal/media"
)

// Errors the dashboard maps to HTTP statuses.
var (
	ErrReadOnly        = errors.New("library: no writable media directory is configured")
	ErrTooLarge        = errors.New("library: upload exceeds the size limit")
	ErrNotDeletable    = errors.New("library: only files in the upload directory can be deleted")
	ErrNotFound        = errors.New("library: unknown file")
	ErrBadName         = errors.New("library: invalid file name")
	ErrUIPathsDisabled = errors.New("library: adding directories from the UI is disabled (start with --allow-ui-paths)")
	ErrNotDirectory    = errors.New("library: not a directory")
)

const (
	defaultMaxUpload = 2 << 30
	defaultWorkers   = 2
	defaultTimeout   = 30 * time.Second
	// maxDepth is how many path components below a root a file may sit.
	maxDepth = 3
)

// videoExt lists the extensions that are scanned. The container is still
// decided from content, so a listed extension on an unsupported container is
// reported as invalid, not skipped.
var videoExt = map[string]bool{
	".ts": true, ".mp4": true, ".m4v": true, ".mov": true, ".mkv": true,
	".avi": true, ".h264": true, ".h265": true, ".mpg": true,
}

// Options configures a Library.
type Options struct {
	Roots        []string
	AllowUIPaths bool
	MaxUpload    int64
	Workers      int
	Timeout      time.Duration
	// Analyze validates one file; nil means media.AnalyzeFile. Tests only.
	Analyze func(path string) media.Report
}

// RootInfo describes one configured directory.
type RootInfo struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	UI       bool   `json:"ui"`
}

// Entry is one file and its report.
type Entry struct {
	ID       string       `json:"id"`
	Root     string       `json:"root"`
	Path     string       `json:"path"`
	Name     string       `json:"name"`
	Size     int64        `json:"size"`
	Modified time.Time    `json:"modified"`
	Report   media.Report `json:"report"`
}

// Library holds the roots and the cached reports.
type Library struct {
	opt Options
	sem chan struct{}

	scanning atomic.Int32

	mu      sync.Mutex
	roots   []RootInfo
	entries map[string]Entry
}

// New builds a Library over the startup roots. A root that does not exist or
// is not a directory is an error. Leftover upload part files are removed.
func New(opt Options) (*Library, error) {
	if opt.MaxUpload <= 0 {
		opt.MaxUpload = defaultMaxUpload
	}
	if opt.Workers <= 0 {
		opt.Workers = defaultWorkers
	}
	if opt.Timeout <= 0 {
		opt.Timeout = defaultTimeout
	}
	l := &Library{opt: opt, sem: make(chan struct{}, opt.Workers), entries: map[string]Entry{}}
	for _, p := range opt.Roots {
		if err := l.addRoot(p, false); err != nil {
			return nil, fmt.Errorf("media directory %q: %w", p, err)
		}
	}
	if up, ok := l.uploadRoot(); ok {
		cleanPartFiles(up)
	}
	return l, nil
}

func (l *Library) addRoot(p string, ui bool) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	st, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return ErrNotDirectory
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.roots {
		if r.Path == real {
			return nil
		}
	}
	l.roots = append(l.roots, RootInfo{Path: real, Writable: !ui && probeWritable(real), UI: ui})
	return nil
}

func probeWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".camfarm-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// AddRoot adds a directory chosen in the UI. It never becomes an upload target.
func (l *Library) AddRoot(path string) error {
	if !l.opt.AllowUIPaths {
		return ErrUIPathsDisabled
	}
	return l.addRoot(path, true)
}

// Roots returns the configured directories in the order they were added.
func (l *Library) Roots() []RootInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.roots)
}

// uploadRoot is the first writable startup root.
func (l *Library) uploadRoot() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.roots {
		if r.Writable {
			return r.Path, true
		}
	}
	return "", false
}

// UploadEnabled reports whether Upload has somewhere to write.
func (l *Library) UploadEnabled() bool { _, ok := l.uploadRoot(); return ok }

// MaxUpload is the largest accepted upload in bytes.
func (l *Library) MaxUpload() int64 { return l.opt.MaxUpload }

// Scanning reports whether a scan is running.
func (l *Library) Scanning() bool { return l.scanning.Load() > 0 }

// List returns every entry, sorted by name then path.
func (l *Library) List() []Entry {
	l.mu.Lock()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b Entry) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// Resolve finds an entry by id.
func (l *Library) Resolve(id string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[id]
	return e, ok
}

// ByPath finds an entry by absolute path.
func (l *Library) ByPath(path string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.Path == path {
			return e, true
		}
	}
	return Entry{}, false
}

func idFor(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:8])
}

func (l *Library) store(e Entry) {
	l.mu.Lock()
	l.entries[e.ID] = e
	l.mu.Unlock()
}

// analyze validates one file with a time limit. ok is false only when ctx ended
// first; a timeout is an ordinary invalid report. The abandoned goroutine
// finishes on its own: AnalyzeFile cannot be interrupted.
func (l *Library) analyze(ctx context.Context, path string) (media.Report, bool) {
	fn := l.opt.Analyze
	if fn == nil {
		fn = media.AnalyzeFile
	}
	done := make(chan media.Report, 1)
	go func() { done <- fn(path) }()
	timer := time.NewTimer(l.opt.Timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r, true
	case <-timer.C:
		r := media.Report{Status: media.StatusInvalid, Issues: []media.Issue{{
			Code: "loader_error", Severity: media.SeverityInvalid,
			Message: "Checking the file took too long and was stopped.",
			Detail:  "limit " + l.opt.Timeout.String(),
		}}}
		return r, true
	case <-ctx.Done():
		return media.Report{}, false
	}
}

// Scan walks one root (or all, when root is empty) and validates new or changed
// files. Unchanged files, judged by size and mtime, keep their cached report
// unless force is set. Files that disappeared are dropped.
func (l *Library) Scan(ctx context.Context, root string, force bool) error {
	roots := l.Roots()
	if root != "" {
		i := slices.IndexFunc(roots, func(r RootInfo) bool { return r.Path == root })
		if i < 0 {
			return fmt.Errorf("%w: %q is not a configured directory", ErrNotFound, root)
		}
		roots = roots[i : i+1]
	}
	l.scanning.Add(1)
	defer l.scanning.Add(-1)
	for _, r := range roots {
		if err := l.scanRoot(ctx, r.Path, force); err != nil {
			return err
		}
	}
	return nil
}

type candidate struct {
	path, name string
	size       int64
	mod        time.Time
}

func (l *Library) scanRoot(ctx context.Context, root string, force bool) error {
	var cands []candidate
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // an unreadable subdirectory is skipped, not fatal
		}
		if p == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow links: they could point outside the root
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator))+1 >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !videoExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		cands = append(cands, candidate{p, rel, info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("library: scanning %q: %w", root, err)
	}

	seen := make(map[string]bool, len(cands))
	var wg sync.WaitGroup
	for _, c := range cands {
		id := idFor(c.path)
		seen[id] = true
		if old, ok := l.Resolve(id); ok && !force && old.Size == c.size && old.Modified.Equal(c.mod) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case l.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-l.sem }()
			rep, ok := l.analyze(ctx, c.path)
			if !ok {
				return
			}
			l.store(Entry{ID: id, Root: root, Path: c.path, Name: c.name, Size: c.size, Modified: c.mod, Report: rep})
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	l.mu.Lock()
	for id, e := range l.entries {
		if e.Root == root && !seen[id] {
			delete(l.entries, id)
		}
	}
	l.mu.Unlock()
	return nil
}

// Refresh re-checks one file just before it is used. If the file changed since
// the scan it is re-validated; if it is gone it is dropped and ErrNotFound is
// returned. This keeps a stale "valid" from becoming a camera.
func (l *Library) Refresh(ctx context.Context, id string) (Entry, error) {
	e, ok := l.Resolve(id)
	if !ok {
		return Entry{}, ErrNotFound
	}
	st, err := os.Stat(e.Path)
	if err != nil || !st.Mode().IsRegular() {
		l.mu.Lock()
		delete(l.entries, id)
		l.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: %q is gone", ErrNotFound, e.Name)
	}
	if st.Size() == e.Size && st.ModTime().Equal(e.Modified) {
		return e, nil
	}
	rep, ok := l.analyze(ctx, e.Path)
	if !ok {
		return Entry{}, ctx.Err()
	}
	e.Size, e.Modified, e.Report = st.Size(), st.ModTime(), rep
	l.store(e)
	return e, nil
}

// cleanPartFiles removes upload temp files left by a crash.
func cleanPartFiles(dir string) {
	parts, _ := filepath.Glob(filepath.Join(dir, ".upload-*.part"))
	for _, p := range parts {
		_ = os.Remove(p)
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `gofmt -l internal/library` (no output), `go vet ./internal/library`, `go test -race ./internal/library -v`
Expected: PASS. `TestScanUnknownRootAndMissingDirectory` exercises `Scan`'s error return when the root vanished; if `WalkDir` reports the missing root through the callback, the `p == root` branch returns it.

- [ ] **Step 5: Commit**

```bash
git add internal/library
git commit -m "library: roots, depth-limited scan, size+mtime cache and refresh"
```

---

### Task 4: Upload and delete

**Files:**
- Create: `internal/library/upload.go`, `internal/library/upload_test.go`

**Interfaces:**
- Consumes (Task 3): `Library`, `Options`, `Entry`, `videoExt`, `idFor`, `l.analyze`, `l.uploadRoot`, `l.store`, `cleanPartFiles`, the error values.
- Produces:

```go
func (l *Library) Upload(ctx context.Context, name string, r io.Reader) (Entry, error)
func (l *Library) Delete(id string) error
func cleanName(name string) (string, error)
```

Rules (from the spec): stream to a temp part file in the upload root; abort with `ErrTooLarge` past `MaxUpload` and remove the part file; reduce the name to its base, reject empty, `.`/`..`, leading `.`, control characters and names over 200 bytes with `ErrBadName`; require a listed video extension; never overwrite (a numeric suffix is added); validate; keep an invalid upload so the user can see why. `Delete` only works inside the upload root.

- [ ] **Step 1: Write the failing tests**

Create `internal/library/upload_test.go`:

```go
package library

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

func upLib(t *testing.T, max int64) (*Library, string) {
	t.Helper()
	dir := t.TempDir()
	l := newLib(t, Options{Roots: []string{dir}, MaxUpload: max})
	return l, l.Roots()[0].Path
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestUploadStoresValidatesAndListsTheFile(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if e.Report.Status != media.StatusValid || e.Name != "clip.ts" || e.Path != filepath.Join(root, "clip.ts") {
		t.Errorf("entry = %+v", e)
	}
	if got, ok := l.Resolve(e.ID); !ok || got.Path != e.Path {
		t.Error("uploaded file is not in the library")
	}
	if names := dirEntries(t, root); len(names) != 1 || names[0] != "clip.ts" {
		t.Errorf("directory = %v (a part file must not be left behind)", names)
	}
}

func TestUploadKeepsAnInvalidFileWithItsReason(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "junk.mp4", strings.NewReader("definitely not a video file"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Report.Status != media.StatusInvalid || e.Report.Issues[0].Code != "unsupported_container" {
		t.Errorf("report = %+v", e.Report)
	}
	if _, err := os.Stat(filepath.Join(root, "junk.mp4")); err != nil {
		t.Errorf("invalid upload was not kept: %v", err)
	}
}

func TestUploadNeverOverwrites(t *testing.T) {
	l, root := upLib(t, 0)
	for i := 0; i < 3; i++ {
		if _, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"clip-1.ts", "clip-2.ts", "clip.ts"}
	if got := dirEntries(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directory = %v, want %v", got, want)
	}
}

func TestConcurrentUploadsOfOneNameKeepEveryFile(t *testing.T) {
	l, root := upLib(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Upload(context.Background(), "same.ts", bytes.NewReader(fixture.Bytes())); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := dirEntries(t, root); len(got) != 6 {
		t.Errorf("directory = %v, want 6 distinct files", got)
	}
	if len(l.List()) != 6 {
		t.Errorf("List has %d entries, want 6", len(l.List()))
	}
}

func TestUploadOverTheLimitLeavesNothingBehind(t *testing.T) {
	l, root := upLib(t, 1000)
	_, err := l.Upload(context.Background(), "big.ts", bytes.NewReader(make([]byte, 5000)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := dirEntries(t, root); len(got) != 0 {
		t.Errorf("directory = %v, want empty", got)
	}
	// Exactly at the limit is accepted.
	if _, err := l.Upload(context.Background(), "edge.ts", bytes.NewReader(make([]byte, 1000))); err != nil {
		t.Errorf("upload of exactly the limit: %v", err)
	}
}

func TestUploadReaderFailureLeavesNothingBehind(t *testing.T) {
	l, root := upLib(t, 0)
	boom := errors.New("connection reset")
	r := io.MultiReader(bytes.NewReader(fixture.Bytes()[:400]), errReader{boom})
	if _, err := l.Upload(context.Background(), "x.ts", r); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := dirEntries(t, root); len(got) != 0 {
		t.Errorf("directory = %v, want empty", got)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestUploadNames(t *testing.T) {
	cases := []struct {
		name    string
		want    string // file created in the root; "" means rejected
	}{
		{"clip.ts", "clip.ts"},
		{"CLIP.MP4", "CLIP.MP4"},
		{"../escape.ts", "escape.ts"},
		{`C:\videos\win.ts`, "win.ts"},
		{"a/b/c.ts", "c.ts"},
		{"", ""},
		{".", ""},
		{"..", ""},
		{".hidden.ts", ""},
		{"no-extension", ""},
		{"notes.txt", ""},
		{"bad\x00name.ts", ""},
		{"tab\tname.ts", ""},
		{strings.Repeat("a", 201) + ".ts", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, root := upLib(t, 0)
			outside := filepath.Join(filepath.Dir(root), "escape.ts")
			_, err := l.Upload(context.Background(), c.name, bytes.NewReader(fixture.Bytes()))
			if c.want == "" {
				if !errors.Is(err, ErrBadName) {
					t.Errorf("err = %v, want ErrBadName", err)
				}
				if got := dirEntries(t, root); len(got) != 0 {
					t.Errorf("a rejected name left %v behind", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := dirEntries(t, root); len(got) != 1 || got[0] != c.want {
				t.Errorf("directory = %v, want [%s]", got, c.want)
			}
			if _, err := os.Stat(outside); err == nil {
				t.Error("a file was written outside the upload root")
			}
		})
	}
}

func TestUploadReadOnlyLibrary(t *testing.T) {
	l := newLib(t, Options{})
	if _, err := l.Upload(context.Background(), "a.ts", bytes.NewReader(fixture.Bytes())); !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
	if err := l.Delete("anything"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete on an empty library = %v", err)
	}
}

func TestNewRemovesLeftoverPartFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".upload-123.part"), []byte("partial"))
	write(t, filepath.Join(dir, "keep.ts"), fixture.Bytes())
	newLib(t, Options{Roots: []string{dir}})
	if got := dirEntries(t, dir); len(got) != 1 || got[0] != "keep.ts" {
		t.Errorf("directory = %v", got)
	}
}

func TestDelete(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "gone.ts", bytes.NewReader(fixture.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Path); !os.IsNotExist(err) {
		t.Errorf("file still on disk: %v", err)
	}
	if _, ok := l.Resolve(e.ID); ok {
		t.Error("entry still listed")
	}
	if got := dirEntries(t, root); len(got) != 0 {
		t.Errorf("directory = %v", got)
	}
	if err := l.Delete(e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

func TestDeleteRefusesFilesOutsideTheUploadRoot(t *testing.T) {
	up, other := t.TempDir(), t.TempDir()
	write(t, filepath.Join(other, "keep.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{up, other}})
	scanAll(t, l)
	var id string
	for _, e := range l.List() {
		id = e.ID
	}
	if err := l.Delete(id); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("err = %v, want ErrNotDeletable", err)
	}
	if _, err := os.Stat(filepath.Join(other, "keep.ts")); err != nil {
		t.Errorf("a file outside the upload root was removed: %v", err)
	}
}

func TestUploadScanAndListRaceFree(t *testing.T) {
	l, _ := upLib(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = l.Upload(context.Background(), "r.ts", bytes.NewReader(fixture.Bytes())) }()
		go func() { defer wg.Done(); _ = l.Scan(context.Background(), "", false) }()
		go func() { defer wg.Done(); _ = l.List() }()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/library -run 'Upload|Delete|PartFiles' -v`
Expected: FAIL to compile with `l.Upload undefined`.

- [ ] **Step 3: Implement**

Create `internal/library/upload.go`:

```go
package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const maxNameLen = 200

// cleanName reduces an uploaded file name to a safe base name.
func cleanName(name string) (string, error) {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == "." || name == "..":
		return "", fmt.Errorf("%w: the name is empty", ErrBadName)
	case strings.HasPrefix(name, "."):
		return "", fmt.Errorf("%w: names starting with a dot are hidden from the library", ErrBadName)
	case len(name) > maxNameLen:
		return "", fmt.Errorf("%w: longer than %d bytes", ErrBadName, maxNameLen)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", fmt.Errorf("%w: contains a control character", ErrBadName)
	case !videoExt[strings.ToLower(filepath.Ext(name))]:
		return "", fmt.Errorf("%w: the extension must be a video type such as .ts or .mp4", ErrBadName)
	}
	return name, nil
}

// linkUnique gives the part file its final name without overwriting anything.
// os.Link fails if the target exists, so two uploads of one name cannot clobber
// each other: the loser takes the next numeric suffix.
func linkUnique(part, dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		final := filepath.Join(dir, candidate)
		err := os.Link(part, final)
		if err == nil {
			return final, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("library: no free name for %q", name)
}

// Upload stores r as a new library file and validates it. An invalid file is
// kept, so the report explaining why stays visible; the caller may delete it.
func (l *Library) Upload(ctx context.Context, name string, r io.Reader) (Entry, error) {
	dir, ok := l.uploadRoot()
	if !ok {
		return Entry{}, ErrReadOnly
	}
	base, err := cleanName(name)
	if err != nil {
		return Entry{}, err
	}

	part, err := os.CreateTemp(dir, ".upload-*.part")
	if err != nil {
		return Entry{}, fmt.Errorf("library: creating upload file: %w", err)
	}
	partPath := part.Name()
	defer func() { _ = os.Remove(partPath) }() // after the link, this removes only the temp name

	n, err := io.Copy(part, io.LimitReader(r, l.opt.MaxUpload+1))
	if cerr := part.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Entry{}, fmt.Errorf("library: receiving upload: %w", err)
	}
	if n > l.opt.MaxUpload {
		return Entry{}, fmt.Errorf("%w (%d bytes)", ErrTooLarge, l.opt.MaxUpload)
	}

	final, err := linkUnique(partPath, dir, base)
	if err != nil {
		return Entry{}, fmt.Errorf("library: placing upload: %w", err)
	}
	st, err := os.Stat(final)
	if err != nil {
		return Entry{}, err
	}
	rep, ok := l.analyze(ctx, final)
	if !ok {
		_ = os.Remove(final)
		return Entry{}, ctx.Err()
	}
	e := Entry{
		ID: idFor(final), Root: dir, Path: final, Name: filepath.Base(final),
		Size: st.Size(), Modified: st.ModTime(), Report: rep,
	}
	l.store(e)
	return e, nil
}

// Delete removes a file from disk and from the library. Only files in the
// upload root can be deleted. The caller refuses first if a camera uses it.
func (l *Library) Delete(id string) error {
	e, ok := l.Resolve(id)
	if !ok {
		return ErrNotFound
	}
	up, ok := l.uploadRoot()
	if !ok || e.Root != up {
		return ErrNotDeletable
	}
	if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("library: deleting %q: %w", e.Name, err)
	}
	l.mu.Lock()
	delete(l.entries, id)
	l.mu.Unlock()
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `gofmt -l internal/library` (no output), `go vet ./internal/library`, `go test -race ./internal/library -v`
Expected: PASS, including every row of `TestUploadNames`. If `TestUploadReaderFailureLeavesNothingBehind` fails to remove the part file, the `defer` is not running on the early error return: check the order of `defer` and the returns.

- [ ] **Step 5: Commit**

```bash
git add internal/library/upload.go internal/library/upload_test.go
git commit -m "library: upload with size cap and safe names, delete inside the upload root"
```

---

### Task 5: Dashboard library endpoints

**Files:**
- Modify: `internal/library/library.go` (one accessor), `internal/dashboard/dashboard.go` (field, routes)
- Create: `internal/dashboard/library.go`, `internal/dashboard/library_test.go`

**Interfaces:**
- Consumes (Tasks 3–4): the whole `library.Library` API, `media.Report`.
- Produces:
  - `func (l *Library) AllowUIPaths() bool` in `internal/library`.
  - `Options.Library *library.Library` in `internal/dashboard` (nil disables every library route with a 404 JSON error).
  - Routes: `GET /api/v1/library`, `POST /api/v1/library/scan`, `POST /api/v1/library/upload`, `DELETE /api/v1/library/files/{id}`, `POST /api/v1/library/roots`.
  - `type libraryDTO`, `type fileDTO` (a `library.Entry` plus `inUseBy []string`), `func (s *server) inUse() map[string][]string` (library path to camera ids), used again by Task 6.

- [ ] **Step 1: Write the failing tests**

Create `internal/dashboard/library_test.go`:

```go
package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// libServer serves a dashboard over a library whose first root is a fresh
// temp directory.
func libServer(t *testing.T, opt library.Options) (*httptest.Server, *library.Library, string) {
	t.Helper()
	opt.Roots = append([]string{t.TempDir()}, opt.Roots...)
	lib, err := library.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := newServer(t, Options{Library: lib})
	return ts, lib, lib.Roots()[0].Path
}

func multipartBody(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func postFile(t *testing.T, base, filename string, data []byte) *http.Response {
	t.Helper()
	body, ct := multipartBody(t, filename, data)
	resp, err := http.Post(base+"/api/v1/library/upload", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeLibrary(t *testing.T, resp *http.Response) libraryDTO {
	t.Helper()
	var v libraryDTO
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestLibraryRoutesAre404WhenDisabled(t *testing.T) {
	ts, _ := newServer(t, Options{})
	for _, c := range [][2]string{
		{"GET", "/api/v1/library"}, {"POST", "/api/v1/library/scan"},
		{"POST", "/api/v1/library/roots"}, {"DELETE", "/api/v1/library/files/x"},
	} {
		resp := do(t, c[0], ts.URL+c[1], "")
		if resp.StatusCode != 404 {
			t.Errorf("%s %s = %d, want 404", c[0], c[1], resp.StatusCode)
		}
	}
}

func TestGetLibraryDescribesRootsAndFiles(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{AllowUIPaths: true, MaxUpload: 1234})
	v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", ""))
	if len(v.Roots) != 1 || !v.UploadEnabled || !v.AllowUIPaths || v.MaxUpload != 1234 || v.Files == nil || len(v.Files) != 0 {
		t.Errorf("library = %+v", v)
	}
}

func TestScanListsFilesWithReports(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	_ = os.WriteFile(filepath.Join(root, "good.ts"), fixture.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(root, "bad.mp4"), []byte("plain text, not video"), 0o644)

	resp := do(t, "POST", ts.URL+"/api/v1/library/scan", "")
	if resp.StatusCode != 200 {
		t.Fatalf("scan = %d", resp.StatusCode)
	}
	v := decodeLibrary(t, resp)
	if len(v.Files) != 2 || v.Files[0].Name != "bad.mp4" || v.Files[0].Report.Status != "invalid" ||
		v.Files[1].Name != "good.ts" || v.Files[1].Report.Status != "valid" {
		t.Fatalf("files = %+v", v.Files)
	}
	if v.Files[0].InUseBy == nil {
		t.Error("inUseBy must encode as [], not null")
	}

	if r := do(t, "POST", ts.URL+"/api/v1/library/scan", `{"force":true}`); r.StatusCode != 200 {
		t.Errorf("forced scan = %d", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/api/v1/library/scan", `{"root":"/not/a/root"}`); r.StatusCode != 404 {
		t.Errorf("unknown root = %d, want 404", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/api/v1/library/scan", `{"nope":1}`); r.StatusCode != 400 {
		t.Errorf("unknown field = %d, want 400", r.StatusCode)
	}
}

func TestUploadEndpoint(t *testing.T) {
	ts, _, root := libServer(t, library.Options{MaxUpload: 100000})

	resp := postFile(t, ts.URL, "clip.ts", fixture.Bytes())
	if resp.StatusCode != 201 {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	var f fileDTO
	_ = json.NewDecoder(resp.Body).Decode(&f)
	if f.Name != "clip.ts" || f.Report.Status != "valid" {
		t.Errorf("file = %+v", f)
	}

	resp = postFile(t, ts.URL, "junk.mp4", []byte("not a video"))
	_ = json.NewDecoder(resp.Body).Decode(&f)
	if resp.StatusCode != 201 || f.Report.Status != "invalid" {
		t.Errorf("invalid upload: %d %+v (an invalid file is kept, with its report)", resp.StatusCode, f.Report)
	}

	if r := postFile(t, ts.URL, "../x.txt", fixture.Bytes()); r.StatusCode != 400 {
		t.Errorf("bad name = %d, want 400", r.StatusCode)
	}

	before, _ := os.ReadDir(root)
	if r := postFile(t, ts.URL, "big.ts", make([]byte, 200000)); r.StatusCode != 413 {
		t.Errorf("over the limit = %d, want 413", r.StatusCode)
	}
	after, _ := os.ReadDir(root)
	if len(after) != len(before) {
		t.Errorf("an over-limit upload left files behind: %d -> %d", len(before), len(after))
	}

	// Not multipart, and multipart without a file part.
	if r := do(t, "POST", ts.URL+"/api/v1/library/upload", `{}`); r.StatusCode != 400 {
		t.Errorf("json body = %d, want 400", r.StatusCode)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	r, err := http.Post(ts.URL+"/api/v1/library/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("no file part = %d, want 400", r.StatusCode)
	}
}

func TestUploadForbiddenWithoutAWritableRoot(t *testing.T) {
	lib, err := library.New(library.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := newServer(t, Options{Library: lib})
	if r := postFile(t, ts.URL, "a.ts", fixture.Bytes()); r.StatusCode != 403 {
		t.Errorf("upload with no writable root = %d, want 403", r.StatusCode)
	}
}

// The binary sets ReadTimeout to bound slow request bodies; a large upload over
// a slow link must not be cut off by it.
func TestSlowUploadSurvivesTheServerReadTimeout(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "c"}}})
	srv := httptest.NewUnstartedServer(Handler(f, Options{Library: lib}))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormFile("file", "slow.ts")
		data := fixture.Bytes()
		_, _ = fw.Write(data[:len(data)/2])
		time.Sleep(900 * time.Millisecond) // three times the ReadTimeout
		_, _ = fw.Write(data[len(data)/2:])
		_ = mw.Close()
		_ = pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/library/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow upload = %d %s", resp.StatusCode, b)
	}
}

func TestDeleteEndpoint(t *testing.T) {
	ts, lib, _ := libServer(t, library.Options{})
	var f fileDTO
	_ = json.NewDecoder(postFile(t, ts.URL, "gone.ts", fixture.Bytes()).Body).Decode(&f)

	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 204 {
		t.Fatalf("delete = %d", r.StatusCode)
	}
	if _, ok := lib.Resolve(f.ID); ok {
		t.Error("entry survived the delete")
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 404 {
		t.Errorf("second delete = %d, want 404", r.StatusCode)
	}
}

func TestDeleteRefusesFilesOutsideTheUploadRoot(t *testing.T) {
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, "keep.ts"), fixture.Bytes(), 0o644)
	ts, lib, _ := libServer(t, library.Options{Roots: []string{other}})
	do(t, "POST", ts.URL+"/api/v1/library/scan", "")
	var id string
	for _, e := range lib.List() {
		id = e.ID
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+id, ""); r.StatusCode != 403 {
		t.Errorf("delete outside the upload root = %d, want 403", r.StatusCode)
	}
}

func TestAddRootEndpoint(t *testing.T) {
	extra := t.TempDir()
	_ = os.WriteFile(filepath.Join(extra, "e.ts"), fixture.Bytes(), 0o644)

	off, _, _ := libServer(t, library.Options{})
	if r := do(t, "POST", off.URL+"/api/v1/library/roots", `{"path":"`+extra+`"}`); r.StatusCode != 403 {
		t.Errorf("without --allow-ui-paths = %d, want 403", r.StatusCode)
	}

	on, _, _ := libServer(t, library.Options{AllowUIPaths: true})
	resp := do(t, "POST", on.URL+"/api/v1/library/roots", `{"path":"`+extra+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("add root = %d", resp.StatusCode)
	}
	v := decodeLibrary(t, resp)
	if len(v.Roots) != 2 || !v.Roots[1].UI || len(v.Files) != 1 || v.Files[0].Name != "e.ts" {
		t.Errorf("library = %+v", v)
	}
	if r := do(t, "POST", on.URL+"/api/v1/library/roots", `{"path":"`+filepath.Join(extra, "nope")+`"}`); r.StatusCode != 400 {
		t.Errorf("nonexistent directory = %d, want 400", r.StatusCode)
	}
	if r := do(t, "POST", on.URL+"/api/v1/library/roots", `{"path":"`+filepath.Join(extra, "e.ts")+`"}`); r.StatusCode != 400 {
		t.Errorf("a file = %d, want 400", r.StatusCode)
	}
}

func TestLibraryErrorsNeverEchoUnrelatedPaths(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{AllowUIPaths: true})
	resp := do(t, "POST", ts.URL+"/api/v1/library/roots", `{"path":"/etc/shadow"}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || strings.Contains(string(b), "root:") {
		t.Errorf("status %d body %s", resp.StatusCode, b)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/dashboard -run 'Library|Upload|AddRoot|Scan|DeleteEndpoint' -v`
Expected: FAIL to compile with `unknown field Library in struct literal of type Options`.

- [ ] **Step 3: Add the library accessor**

In `internal/library/library.go`, next to `MaxUpload`:

```go
// AllowUIPaths reports whether AddRoot is enabled.
func (l *Library) AllowUIPaths() bool { return l.opt.AllowUIPaths }
```

- [ ] **Step 4: Wire the dashboard**

In `internal/dashboard/dashboard.go`:

1. Import `"github.com/0x524a/camfarm/internal/library"`.
2. Add to `Options`:

```go
	// Library, when non-nil, enables the media library endpoints.
	Library *library.Library
```

3. Add `lib *library.Library` to `server`, set it in `Handler` (`s := &server{fleet: f, snap: newSnapshotter(opt), lib: opt.Library}`), and register before the `GET /` catch-all:

```go
	mux.HandleFunc("GET /api/v1/library", s.getLibrary)
	mux.HandleFunc("POST /api/v1/library/scan", s.scanLibrary)
	mux.HandleFunc("POST /api/v1/library/upload", s.uploadFile)
	mux.HandleFunc("DELETE /api/v1/library/files/{id}", s.deleteFile)
	mux.HandleFunc("POST /api/v1/library/roots", s.addRoot)
```

Create `internal/dashboard/library.go`:

```go
package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0x524a/camfarm/internal/library"
)

// multipartSlack is how much framing a multipart body may add beyond the file.
const multipartSlack = 1 << 20

type fileDTO struct {
	library.Entry
	InUseBy []string `json:"inUseBy"`
}

type libraryDTO struct {
	Roots         []library.RootInfo `json:"roots"`
	UploadEnabled bool               `json:"uploadEnabled"`
	AllowUIPaths  bool               `json:"allowUIPaths"`
	MaxUpload     int64              `json:"maxUpload"`
	Scanning      bool               `json:"scanning"`
	Files         []fileDTO          `json:"files"`
}

// needLibrary replies 404 and returns false when the library is not enabled.
func (s *server) needLibrary(w http.ResponseWriter) bool {
	if s.lib == nil {
		writeError(w, http.StatusNotFound, "the media library is not enabled")
		return false
	}
	return true
}

// inUse maps a library file path to the cameras serving it.
func (s *server) inUse() map[string][]string {
	out := map[string][]string{}
	for _, st := range s.fleet.List() {
		if p, ok := strings.CutPrefix(st.Source, "file:"); ok {
			out[p] = append(out[p], st.ID)
		}
	}
	return out
}

func (s *server) fileView(e library.Entry, inUse map[string][]string) fileDTO {
	users := inUse[e.Path]
	if users == nil {
		users = []string{}
	}
	return fileDTO{Entry: e, InUseBy: users}
}

func (s *server) libraryView() libraryDTO {
	inUse := s.inUse()
	entries := s.lib.List()
	files := make([]fileDTO, 0, len(entries))
	for _, e := range entries {
		files = append(files, s.fileView(e, inUse))
	}
	return libraryDTO{
		Roots: s.lib.Roots(), UploadEnabled: s.lib.UploadEnabled(), AllowUIPaths: s.lib.AllowUIPaths(),
		MaxUpload: s.lib.MaxUpload(), Scanning: s.lib.Scanning(), Files: files,
	}
}

// writeLibraryErr maps a library error to an HTTP status.
func writeLibraryErr(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig), errors.Is(err, library.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, library.ErrBadName), errors.Is(err, library.ErrNotDirectory):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrReadOnly), errors.Is(err, library.ErrUIPathsDisabled), errors.Is(err, library.ErrNotDeletable):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, library.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeErr(w, err)
	}
}

func (s *server) getLibrary(w http.ResponseWriter, _ *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.libraryView())
}

func (s *server) scanLibrary(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	var req struct {
		Force bool   `json:"force"`
		Root  string `json:"root"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	if err := s.lib.Scan(r.Context(), req.Root, req.Force); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, err)
			return
		}
		writeLibraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.libraryView())
}

func (s *server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	// The binary's ReadTimeout bounds slow request bodies; a large upload over a
	// slow link must not be cut off by it. The size cap below still bounds it.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, s.lib.MaxUpload()+multipartSlack)

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data with a file part: "+err.Error())
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, `the upload has no "file" part`)
			return
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
				return
			}
			writeError(w, http.StatusBadRequest, "malformed upload: "+err.Error())
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			continue
		}
		e, err := s.lib.Upload(r.Context(), part.FileName(), part)
		if err != nil {
			writeLibraryErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.fileView(e, s.inUse()))
		return
	}
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	id := r.PathValue("id")
	e, ok := s.lib.Resolve(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown library file")
		return
	}
	if users := s.inUse()[e.Path]; len(users) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "a camera is serving this file; remove the camera first",
			"cameras": users,
		})
		return
	}
	if err := s.lib.Delete(id); err != nil {
		writeLibraryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) addRoot(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.lib.AddRoot(req.Path); err != nil {
		if errors.Is(err, library.ErrUIPathsDisabled) {
			writeLibraryErr(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, "cannot use that directory: "+err.Error())
		return
	}
	if err := s.lib.Scan(r.Context(), "", false); err != nil {
		writeLibraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.libraryView())
}
```

- [ ] **Step 5: Run the tests**

Run: `gofmt -l internal` (no output), `go vet ./...`, `go test -race ./internal/dashboard ./internal/library`
Expected: PASS. If `TestSlowUploadSurvivesTheServerReadTimeout` fails with a connection reset, `SetReadDeadline(time.Time{})` is not taking effect: confirm it runs before the first body read and that the `ResponseWriter` is the server's, not a wrapper.

- [ ] **Step 6: Commit**

```bash
git add internal/library/library.go internal/dashboard
git commit -m "dashboard: library endpoints for listing, scanning, uploading and deleting"
```

---

### Task 6: Cameras from library files

**Files:**
- Modify: `internal/dashboard/dashboard.go` (`addCamera`, `cameraDTO`, `dto`), `internal/dashboard/library.go`
- Create: `internal/dashboard/library_camera_test.go`

**Interfaces:**
- Consumes (Task 5): `s.lib`, `s.needLibrary`, `s.inUse`, `writeLibraryErr`; (Task 3) `Library.Refresh`, `Library.ByPath`; `media.StatusInvalid`, `media.SeverityWarning`.
- Produces: `POST /api/v1/cameras` accepts a top-level `libraryId` (a sibling of `source`); `cameraDTO` gains `libraryId` and `warnings`.

Behaviour: `libraryId` cannot be combined with `source.path` or a non-file `source.kind` (400). The file is re-checked first (`Refresh`): unknown or vanished is 404; invalid is 422 with `{"error", "report"}`; valid or warning proceeds with `source = {file, <entry path>}`. The typed `source.path` form is unchanged.

- [ ] **Step 1: Write the failing tests**

Create `internal/dashboard/library_camera_test.go`:

```go
package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// stage writes a file into the library root and scans, returning its id.
func stage(t *testing.T, ts string, root, name string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	v := decodeLibrary(t, do(t, "POST", ts+"/api/v1/library/scan", ""))
	for _, f := range v.Files {
		if f.Name == name {
			return f.ID
		}
	}
	t.Fatalf("%s not in the library: %+v", name, v.Files)
	return ""
}

func addFromLibrary(t *testing.T, ts, camID, libID string) *http.Response {
	t.Helper()
	return do(t, "POST", ts+"/api/v1/cameras", `{"id":"`+camID+`","libraryId":"`+libID+`"}`)
}

func TestCameraFromLibraryFileServesIt(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())

	resp := addFromLibrary(t, ts.URL, "from-lib", id)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("add = %d %s", resp.StatusCode, b)
	}
	var cam cameraDTO
	_ = json.NewDecoder(resp.Body).Decode(&cam)
	if cam.LibraryID != id || cam.Source != "file:"+filepath.Join(root, "clip.ts") || len(cam.Warnings) != 0 {
		t.Errorf("camera = %+v", cam)
	}

	// The proof the file is really served: a snapshot is captured over RTSP.
	snap := do(t, "GET", ts.URL+"/api/v1/cameras/from-lib/snapshot.jpg", "")
	if snap.StatusCode != 200 || snap.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("snapshot = %d %s", snap.StatusCode, snap.Header.Get("Content-Type"))
	}

	v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", ""))
	if len(v.Files[0].InUseBy) != 1 || v.Files[0].InUseBy[0] != "from-lib" {
		t.Errorf("inUseBy = %v", v.Files[0].InUseBy)
	}
	del := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+id, "")
	if del.StatusCode != 409 {
		t.Errorf("deleting a file in use = %d, want 409", del.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "clip.ts")); err != nil {
		t.Errorf("a file in use was removed: %v", err)
	}
}

func TestCameraFromWarningFileCarriesItsWarnings(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	out := filepath.Join(root, "b.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-c:v", "libx264", "-bf", "2", "-g", "25", "-pix_fmt", "yuv420p", out}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	v := decodeLibrary(t, do(t, "POST", ts.URL+"/api/v1/library/scan", ""))
	if len(v.Files) != 1 || v.Files[0].Report.Status != media.StatusWarning {
		t.Fatalf("files = %+v", v.Files)
	}
	resp := addFromLibrary(t, ts.URL, "bframes", v.Files[0].ID)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("a warning file must be usable: %d %s", resp.StatusCode, b)
	}
	var cam cameraDTO
	_ = json.NewDecoder(resp.Body).Decode(&cam)
	codes := map[string]bool{}
	for _, w := range cam.Warnings {
		codes[w.Code] = true
	}
	if !codes["b_frames"] {
		t.Errorf("warnings = %+v, want b_frames", cam.Warnings)
	}
}

func TestInvalidLibraryFileCannotBecomeACamera(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "junk.mp4", []byte("this is just text"))
	resp := addFromLibrary(t, ts.URL, "nope", id)
	if resp.StatusCode != 422 {
		t.Fatalf("add = %d, want 422", resp.StatusCode)
	}
	var body struct {
		Error  string       `json:"error"`
		Report media.Report `json:"report"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error == "" || body.Report.Status != media.StatusInvalid || len(body.Report.Issues) == 0 {
		t.Errorf("body = %+v", body)
	}
	if r := do(t, "GET", ts.URL+"/api/v1/cameras/nope", ""); r.StatusCode != 404 {
		t.Errorf("a refused camera exists anyway: %d", r.StatusCode)
	}
}

// A file can change between the scan and the click. The report the user saw may
// be stale, so the server re-checks before it builds the camera.
func TestStaleLibraryFileIsRecheckedAtCreation(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	if err := os.WriteFile(filepath.Join(root, "clip.ts"), []byte("replaced by text after the scan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := addFromLibrary(t, ts.URL, "stale", id); r.StatusCode != 422 {
		t.Errorf("edited-after-scan file = %d, want 422", r.StatusCode)
	}

	id2 := stage(t, ts.URL, root, "other.ts", fixture.Bytes())
	if err := os.Remove(filepath.Join(root, "other.ts")); err != nil {
		t.Fatal(err)
	}
	if r := addFromLibrary(t, ts.URL, "gone", id2); r.StatusCode != 404 {
		t.Errorf("deleted-after-scan file = %d, want 404", r.StatusCode)
	}
}

func TestLibraryIDRequestShapes(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown id", `{"id":"a","libraryId":"deadbeef"}`, 404},
		{"with a typed path", `{"id":"b","libraryId":"` + id + `","source":{"kind":"file","path":"/x.ts"}}`, 400},
		{"with the fixture kind", `{"id":"c","libraryId":"` + id + `","source":{"kind":"fixture"}}`, 400},
		{"with an explicit file kind", `{"id":"d","libraryId":"` + id + `","source":{"kind":"file"}}`, 201},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := do(t, "POST", ts.URL+"/api/v1/cameras", c.body); r.StatusCode != c.want {
				b, _ := io.ReadAll(r.Body)
				t.Errorf("status = %d, want %d: %s", r.StatusCode, c.want, b)
			}
		})
	}
}

func TestLibraryIDWithoutALibrary(t *testing.T) {
	ts, _ := newServer(t, Options{})
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"a","libraryId":"x"}`); r.StatusCode != 404 {
		t.Errorf("status = %d, want 404", r.StatusCode)
	}
}

func TestTypedPathStillWorksAndIsLinkedToTheLibrary(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	p := filepath.Join(root, "clip.ts")
	resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"typed","source":{"kind":"file","path":"`+p+`"}}`)
	if resp.StatusCode != 201 {
		t.Fatalf("typed path = %d", resp.StatusCode)
	}
	var cam cameraDTO
	_ = json.NewDecoder(resp.Body).Decode(&cam)
	if cam.LibraryID != id {
		t.Errorf("libraryId = %q, want %q: a camera on a library path should link to its entry", cam.LibraryID, id)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/dashboard -run 'Library|Stale|TypedPath|Invalid' -v`
Expected: FAIL to compile with `cam.LibraryID undefined`.

- [ ] **Step 3: Implement**

In `internal/dashboard/library.go` add the `media` import (`"github.com/0x524a/camfarm/internal/media"`) and the `camfarm` import (`"github.com/0x524a/camfarm"`), then append:

```go
// addCameraReq is the body of POST /cameras: a CameraSpec plus an optional
// libraryId. libraryId is a sibling of source, not part of it, because
// SourceSpec is public library API and the library is a dashboard concern.
type addCameraReq struct {
	camfarm.CameraSpec
	LibraryID string `json:"libraryId,omitempty"`
}

// specFromLibrary resolves req.LibraryID to a file source. It replies and
// returns false when the file is unusable. The file is re-checked first, so a
// report that went stale between the scan and the click is never trusted.
func (s *server) specFromLibrary(w http.ResponseWriter, r *http.Request, req addCameraReq) (camfarm.CameraSpec, bool) {
	spec := req.CameraSpec
	if !s.needLibrary(w) {
		return spec, false
	}
	if spec.Source.Path != "" || (spec.Source.Kind != "" && spec.Source.Kind != camfarm.SourceFile) {
		writeError(w, http.StatusBadRequest, "libraryId cannot be combined with source.path or a source kind other than file")
		return spec, false
	}
	e, err := s.lib.Refresh(r.Context(), req.LibraryID)
	if err != nil {
		writeLibraryErr(w, err)
		return spec, false
	}
	if e.Report.Status == media.StatusInvalid {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "this file is invalid and cannot be used as a camera",
			"report": e.Report,
		})
		return spec, false
	}
	spec.Source = camfarm.SourceSpec{Kind: camfarm.SourceFile, Path: e.Path}
	return spec, true
}

// libraryInfo links a camera's source to its library entry, if it has one, and
// returns that entry's warnings so the camera card can show them.
func (s *server) libraryInfo(source string) (string, []media.Issue) {
	if s.lib == nil {
		return "", nil
	}
	p, ok := strings.CutPrefix(source, "file:")
	if !ok {
		return "", nil
	}
	e, ok := s.lib.ByPath(p)
	if !ok {
		return "", nil
	}
	var warns []media.Issue
	for _, is := range e.Report.Issues {
		if is.Severity == media.SeverityWarning {
			warns = append(warns, is)
		}
	}
	return e.ID, warns
}
```

In `internal/dashboard/dashboard.go`:

1. Add to `cameraDTO` (after `Source`): 

```go
	LibraryID string        `json:"libraryId,omitempty"`
	Warnings  []media.Issue `json:"warnings,omitempty"`
```

and import `"github.com/0x524a/camfarm/internal/media"`.

2. In `dto`, build the value into `d`, then fill the two fields and return it. Replace `return cameraDTO{ ... }` with `d := cameraDTO{ ... }` (same literal), followed by:

```go
	d.LibraryID, d.Warnings = s.libraryInfo(st.Source)
	return d
```

3. Replace the start of `addCamera`:

```go
func (s *server) addCamera(w http.ResponseWriter, r *http.Request) {
	var req addCameraReq
	if !decodeJSON(w, r, &req) {
		return
	}
	spec := req.CameraSpec
	if req.LibraryID != "" {
		var ok bool
		if spec, ok = s.specFromLibrary(w, r, req); !ok {
			return
		}
	}
	cam, err := s.fleet.AddCamera(spec)
```

(the rest of the function is unchanged).

- [ ] **Step 4: Run the tests**

Run: `gofmt -l internal` (no output), `go vet ./...`, `go test -race ./internal/dashboard`
Expected: PASS, including the whole existing dashboard suite (the `DisallowUnknownFields` decode must still reject unknown top-level fields).

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard
git commit -m "dashboard: create cameras from library files; block invalid ones, show warnings"
```

---

### Task 7: Binary flags, startup scan and README

**Files:**
- Create: `cmd/camfarm/size.go`, `cmd/camfarm/size_test.go`
- Modify: `cmd/camfarm/run.go`, `cmd/camfarm/run_test.go`, `README.md`

**Interfaces:**
- Consumes: `library.New`, `library.Options`, `Library.Scan`, `dashboard.Options.Library`.
- Produces: flags `--media-dir` (repeatable), `--allow-ui-paths`, `--max-upload` (default `2GiB`); `func parseSize(s string) (int64, error)`.

- [ ] **Step 1: Write the failing size tests**

Create `cmd/camfarm/size_test.go`:

```go
package main

import "testing"

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
	for _, in := range []string{"", "0", "-1", "abc", "2XB", "1e3", "1.2.3", "GiB", "99999999999GiB"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/camfarm -run TestParseSize -v`
Expected: FAIL to compile with `undefined: parseSize`.

- [ ] **Step 3: Implement parseSize**

Create `cmd/camfarm/size.go`:

```go
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
```

Run: `go test ./cmd/camfarm -run TestParseSize -v` (expected PASS).

- [ ] **Step 4: Write the failing binary tests**

In `cmd/camfarm/run_test.go` add two rows to the `TestRunRejectsBadInput` table:

```go
		{"missing media dir", []string{"--media-dir", "/no/such/dir", "--http-addr", addr}, "--media-dir"},
		{"bad max-upload", []string{"--max-upload", "lots", "--http-addr", addr}, "invalid --max-upload"},
```

and append new tests (add `"io"`, `"mime/multipart"` and `github.com/0x524a/camfarm/internal/media/fixture` to the imports):

```go
func getLibraryFiles(t *testing.T, url string) []map[string]any {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/library")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.Files
}

func TestRunScansMediaDirsInTheBackground(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(a, "one.ts"), fixture.Bytes(), 0o600)
	_ = os.WriteFile(filepath.Join(b, "two.ts"), fixture.Bytes(), 0o600)
	url, stop, _ := start(t, "--cameras", "1", "--http-addr", "127.0.0.1:0", "--media-dir", a, "--media-dir", b)

	deadline := time.Now().Add(10 * time.Second)
	var files []map[string]any
	for time.Now().Before(deadline) {
		if files = getLibraryFiles(t, url); len(files) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(files) != 2 {
		t.Fatalf("library files = %v, want both directories scanned", files)
	}
	if err := stop(); err != nil {
		t.Errorf("run = %v", err)
	}
}

func TestRunAcceptsAnUploadIntoTheFirstMediaDir(t *testing.T) {
	dir := t.TempDir()
	url, _, _ := start(t, "--cameras", "1", "--http-addr", "127.0.0.1:0", "--media-dir", dir, "--max-upload", "1MiB")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "up.ts")
	_, _ = fw.Write(fixture.Bytes())
	_ = mw.Close()
	resp, err := http.Post(url+"/api/v1/library/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d %s", resp.StatusCode, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "up.ts")); err != nil {
		t.Errorf("uploaded file missing from the media dir: %v", err)
	}
}

func TestRunWithoutMediaDirHasAnEmptyLibrary(t *testing.T) {
	url, _, _ := start(t, "--cameras", "1", "--http-addr", "127.0.0.1:0")
	if files := getLibraryFiles(t, url); len(files) != 0 {
		t.Errorf("files = %v", files)
	}
	resp, err := http.Post(url+"/api/v1/library/roots", "application/json", strings.NewReader(`{"path":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("adding a root without --allow-ui-paths = %d, want 403", resp.StatusCode)
	}
}

func TestHelpMentionsTheMediaLibrary(t *testing.T) {
	var out, errb bytes.Buffer
	_ = run(context.Background(), []string{"--help"}, &out, &errb)
	got := errb.String() + out.String()
	for _, want := range []string{"--media-dir", "--allow-ui-paths", "--max-upload"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage text missing %q", want)
		}
	}
}
```

Run: `go test ./cmd/camfarm -run 'Media|Upload|RejectsBadInput' -v`
Expected: FAIL (flag provided but not defined).

- [ ] **Step 5: Implement the flags**

In `cmd/camfarm/run.go`:

1. Import `"github.com/0x524a/camfarm/internal/library"` and `"strings"` if not already imported.
2. Add the flag type near `exitError`:

```go
// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
```

3. Next to the other flag definitions:

```go
	var mediaDirs stringList
	fs.Var(&mediaDirs, "media-dir", "directory of video files for the media library; repeatable.\n"+
		"the first writable one receives dashboard uploads. Files are validated once, at startup, on rescan and on upload")
	allowUIPaths := fs.Bool("allow-ui-paths", false, "let the dashboard add more media directories by typed path (any directory this process can read)")
	maxUpload := fs.String("max-upload", "2GiB", "largest accepted dashboard upload, e.g. 500MB or 2GiB")
```

4. After the discovery handling and before `net.Listen`, build the library:

```go
	maxBytes, err := parseSize(*maxUpload)
	if err != nil {
		return usageErr("invalid --max-upload %q: %v", *maxUpload, err)
	}
	lib, err := library.New(library.Options{Roots: mediaDirs, AllowUIPaths: *allowUIPaths, MaxUpload: maxBytes})
	if err != nil {
		return usageErr("--media-dir: %v", err)
	}
```

5. Pass it to the dashboard: `dashboard.Handler(fleet, dashboard.Options{Library: lib})`.

6. Scan in the background once the dashboard is listening, and wait for it on the way out so it cannot write to `stderr` after `run` returns. Place this right after the `dashboard: http://...` line is printed:

```go
	scanCtx, scanCancel := context.WithCancel(ctx)
	scanDone := make(chan struct{})
	defer func() { scanCancel(); <-scanDone }()
	go func() {
		defer close(scanDone)
		if err := lib.Scan(scanCtx, "", false); err != nil && scanCtx.Err() == nil {
			_, _ = fmt.Fprintf(stderr, "warning: scanning the media library: %v\n", err)
		}
	}()
```

7. Extend the `usage` constant's SECURITY paragraph with: `--media-dir makes every video file under that directory listable and servable, and uploads write to the first writable one; --allow-ui-paths lets anyone who can reach the dashboard point it at any readable directory.`

- [ ] **Step 6: Run the binary tests**

Run: `gofmt -l cmd` (no output), `go vet ./...`, `go test -race ./cmd/camfarm`
Expected: PASS, including `TestHelpStatesTheSecurityPosture` (the new wording must not remove any word that test requires).

- [ ] **Step 7: Document it in the README**

In `README.md` add one bullet to the Status "Works" list and a new section before `## Use as a library`:

```markdown
- **Works:** a media library — point the binary at directories of your own video
  with `--media-dir` (repeatable) and the dashboard API lists, validates and
  serves them. Each file is checked once, on startup, on rescan and on upload;
  the report says whether it is `valid`, `warning` (for example B-frames,
  reordered timestamps, a variable frame rate) or `invalid` and why. Invalid
  files cannot become cameras; files with warnings can.
```

````markdown
## Bring your own video

```sh
camfarm --media-dir /srv/clips --media-dir ./more-clips
```

In Docker, a volume is just a `--media-dir` pointing at the mount:

```sh
docker run -v /srv/clips:/media -p 8080:8080 camfarm --http-addr 0.0.0.0:8080 --media-dir /media
```

- Files with a video extension (`.ts .mp4 .m4v .mov .mkv .avi .h264 .h265 .mpg`)
  are listed, up to three directories deep. Symlinks and dot-files are skipped.
  The container is judged from content, so a `.mkv` is reported as an
  unsupported container rather than ignored. Only MPEG-TS and MP4/MOV carrying
  H.264 or H.265 can be served.
- Dashboard uploads go to the first writable `--media-dir`, capped by
  `--max-upload` (default 2GiB). An upload that fails validation is kept so its
  report stays visible; delete it with `DELETE /api/v1/library/files/{id}`.
- `--allow-ui-paths` lets the dashboard add directories by typed path. It is off
  by default because the dashboard has no authentication and the option lets
  anyone who can reach it point the process at any directory it can read.
- A camera takes a library file with `{"id": "cam", "libraryId": "<id from GET
  /api/v1/library>"}`. The file is re-checked first, since it may have changed
  since the scan.

What the validator can and cannot see: it reports what camfarm's own parser
loads. B-frames are read from H.264 slice headers; for H.265 they are inferred
from timestamp reordering, because H.265 slice types are not parsed. It does not
repair or transcode anything.
````

Run `go test ./...`: the README drift test must still pass.

- [ ] **Step 8: Commit**

```bash
git add cmd/camfarm README.md
git commit -m "camfarm: --media-dir, --allow-ui-paths and --max-upload; scan at startup"
```

---

### Task 8: Whole-branch verification

**Files:** none new. Fix whatever this finds in the file that owns it.

- [ ] **Step 1: Static checks and the full suite**

Run, one per call:
- `gofmt -l .` (expected: no output)
- `go vet ./...`
- `CGO_ENABLED=0 go build ./...`
- `go test -race ./...`

Expected: all clean and PASS. A failure in a package this plan did not touch is a regression from `addCameraReq` or the `Options` change: fix it, do not skip it.

- [ ] **Step 2: Smoke-test the real binary**

```bash
mkdir -p /tmp/camfarm-lib
CGO_ENABLED=0 go build -o /tmp/camfarm-lib-bin ./cmd/camfarm
/tmp/camfarm-lib-bin --cameras 1 --seed 1 --http-addr 127.0.0.1:8099 --media-dir /tmp/camfarm-lib
```

In a second terminal (or after backgrounding the first), with any small MPEG-TS or MP4 file at `./clip.ts`:

```bash
curl -s -F file=@clip.ts http://127.0.0.1:8099/api/v1/library/upload
curl -s http://127.0.0.1:8099/api/v1/library
curl -s -X POST -H 'Content-Type: application/json' -d '{"id":"mine","libraryId":"<id from the upload response>"}' http://127.0.0.1:8099/api/v1/cameras
curl -s -o /tmp/mine.jpg -w '%{http_code}\n' http://127.0.0.1:8099/api/v1/cameras/mine/snapshot.jpg
```

Expected: a 201 upload response with `report.status`, a library listing that includes the file with `inUseBy: ["mine"]` after the third call, `201` for the camera, and `200` for the snapshot. Upload a text file renamed `x.mp4` and confirm `report.status` is `invalid` with `unsupported_container`, and that adding it as a camera returns `422`. Stop the binary and remove `/tmp/camfarm-lib` and `/tmp/camfarm-lib-bin`.

- [ ] **Step 3: Check the spec is honoured**

Re-read `docs/superpowers/specs/2026-10-11-media-library-design.md` against the diff (`git diff main...HEAD --stat`). Every endpoint, flag, issue code and test bullet in the spec should map to code or a test; list any gap and close it. Confirm nothing in the diff names an employer or client and that the README states no coverage number.

- [ ] **Step 4: Commit any fixes**

```bash
git add -A
git status --short
git commit -m "library: fixes from whole-branch verification"
```

Skip the commit if there is nothing to commit. Do not push or open a PR without being asked.

---

## Self-review (writing-plans checklist)

**Spec coverage.** Validator codes and thresholds: Task 1 (every code except `loader_error`/`unreadable`/`unsupported_container`/`no_video`/`missing_parameter_sets`/`no_frames`, which are Task 2). Roots, scan limits, cache, force, timeout: Task 3. Upload, names, no-overwrite, part cleanup, delete: Task 4. All five endpoints: Task 5. `libraryId`, 422, DTO fields: Task 6. Flags, background scan, README: Task 7. Testing section of the spec: spread across the same tasks; the end-to-end bullet is the Task 6 snapshot test plus the Task 8 smoke test.

**Placeholder scan.** No TBD/TODO; every code step has code.

**Type consistency.** `media.Report/Facts/Issue`, `Status*`, `Severity*` (Task 1) are the names used in Tasks 2–7. `library.Entry`, `Options`, `RootInfo`, `Library` methods (Task 3) match Tasks 4–7. `fileDTO`/`libraryDTO`/`inUse` (Task 5) match Task 6. `Options.Analyze` signature `func(string) media.Report` matches its uses in the tests.

**Review Focus.** Item 1 → `TestUploadNames`. Item 2 → `TestRefresh`, `TestStaleLibraryFileIsRecheckedAtCreation`. Item 3 → `TestUploadOverTheLimitLeavesNothingBehind`, `TestSlowUploadSurvivesTheServerReadTimeout`. Item 4 → `TestAnalyzeFileFailures`, `TestRecoverFromParserPanic`, `TestAnalyzeFileIgnoresExtension`. Item 5 → `TestConcurrentScanAndListIsRaceFree`, `TestConcurrentUploadsOfOneNameKeepEveryFile`, `TestUploadScanAndListRaceFree`.

**Known soft spots to watch during execution.** (a) The audio-only MP4 error chain may not wrap `errNoSamples`; Task 2 Step 5 says how to resolve it. (b) `TestAnalyzeFileRealBFrames` is the only test tying `isH264BSlice` to a real encoder; if libx264 output changes, fix the parser, not the test. (c) `AnalyzeFile` cannot be interrupted, so a timed-out check leaves its goroutine running until it returns; that is bounded by file size and is documented in `analyze`.

