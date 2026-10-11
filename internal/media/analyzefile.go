package media

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// parseTS and parseMP4 are seams so a test can make a parser panic and
// exercise AnalyzeFile's recovery.
var (
	parseTS  = parseMPEGTS
	parseMP4 = parseISOBMFF
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
	if !st.Mode().IsRegular() {
		return invalidReport(facts, "unreadable", "This is not a regular file.", "not a regular file")
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
		m, err = parseTS(f)
	default:
		m, err = parseMP4(f)
	}
	if err != nil {
		return classifyLoadError(facts, err)
	}
	rep = Analyze(m, st.Size())
	rep.Facts.Container = facts.Container
	return rep
}
