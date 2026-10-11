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

// Thresholds. Each is pinned by a boundary row in TestAnalyze, except
// reorderWindow, which TestAnalyzeReorderDepthWindow pins.
const (
	jumpFactor     = 4.0            // a DTS step this many times the median is a jump
	vfrSpread      = 0.10           // (p95-p5)/median above this is variable frame rate
	fpsTolerance   = 0.05           // declared vs measured fps
	longGOPTicks   = 10 * clockRate // longest keyframe gap before a warning
	shortClipTicks = clockRate      // shorter clips loop every second
	largeFileBytes = 512 << 20      // the whole parsed file is held in memory
	reorderWindow  = 32             // frames looked back when measuring reorder depth
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
// it never touches the filesystem. It leaves Facts.Container empty; AnalyzeFile
// fills it.
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
