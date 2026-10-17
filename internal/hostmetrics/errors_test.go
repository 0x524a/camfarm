package hostmetrics

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCapBufKeepsOnlyTheCap(t *testing.T) {
	var c capBuf
	chunk := make([]byte, gpuOutputCap/2+10)
	for range 3 {
		if n, err := c.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("Write must report the whole chunk consumed: n=%d err=%v", n, err)
		}
	}
	if c.b.Len() != gpuOutputCap {
		t.Fatalf("kept %d bytes, want %d", c.b.Len(), gpuOutputCap)
	}
}

func TestExecRunnerCapsOutputAndReturnsStderrOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, errOut, err := execRunner(ctx, "sh", "-c", "head -c 3000000 /dev/zero; echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if len(out) != gpuOutputCap {
		t.Fatalf("stdout kept %d bytes, want the %d cap", len(out), gpuOutputCap)
	}
	if !strings.Contains(string(errOut), "boom") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestExecRunnerSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	out, _, err := execRunner(context.Background(), "sh", "-c", "echo hello")
	if err != nil || strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestFirstLineSkipsEmptySourcesAndBlankLines(t *testing.T) {
	if got := firstLine(nil, []byte("\n  \n"), []byte("\n second \nthird")); got != "second" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine(nil, []byte("")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestBaseResetForgetsThePreviousValue(t *testing.T) {
	var b base[uint64]
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	b.set(now, 7)
	if v, ok := b.prev(now.Add(time.Second), 3*time.Second); !ok || v != 7 {
		t.Fatalf("v=%d ok=%v", v, ok)
	}
	b.reset()
	if _, ok := b.prev(now.Add(time.Second), 3*time.Second); ok {
		t.Fatal("a reset baseline must not be usable")
	}
}

// validProc is a complete fake /proc; each case below breaks one file.
func validProc() map[string]string {
	return map[string]string{
		"proc/stat":       statFixture,
		"proc/meminfo":    meminfoFixture,
		"proc/loadavg":    "1.00 1.00 1.00 1/1 1\n",
		"proc/self/stat":  "1 (camfarm) S 1 1 1 0 -1 0 0 0 0 0 5 5 0 0 20 0 1 0 1 1 1 0\n",
		"proc/self/statm": "10 10 0 0 0 0 0\n",
		"proc/self/fd/0":  "",
	}
}

func TestProcReaderEachBrokenFileMakesOnlyItsSectionUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		file    string // "" deletes the file's body to the given value
		body    string
		section string // cpu | memory | process
	}{
		{"loadavg garbage", "proc/loadavg", "x\n", "cpu"},
		{"loadavg non-numeric", "proc/loadavg", "a b c\n", "cpu"},
		{"stat garbage", "proc/stat", "nonsense\n", "cpu"},
		{"meminfo garbage", "proc/meminfo", "nonsense\n", "memory"},
		{"self/stat garbage", "proc/self/stat", "garbage", "process"},
		{"self/stat short", "proc/self/stat", "1 (x) S 1\n", "process"},
		{"self/stat non-numeric utime", "proc/self/stat", "1 (x) S 1 1 1 0 -1 0 0 0 0 0 a b 0\n", "process"},
		{"statm short", "proc/self/statm", "1\n", "process"},
		{"statm non-numeric", "proc/self/statm", "1 x\n", "process"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := validProc()
			files[c.file] = c.body
			rd := writeProc(t, t.TempDir(), files)
			_, cpu, cerr := rd.cpu()
			mem, merr := rd.memory()
			_, proc, perr := rd.process()
			broken := map[string]bool{
				"cpu":     cerr != nil && !cpu.Available && cpu.Reason != "",
				"memory":  merr != nil && !mem.Available && mem.Reason != "",
				"process": perr != nil && !proc.Available && proc.Reason != "",
			}
			for section, isBroken := range broken {
				if want := section == c.section; isBroken != want {
					t.Errorf("%s section broken=%v, want %v", section, isBroken, want)
				}
			}
		})
	}
}

func TestProcReaderMissingFdDirectory(t *testing.T) {
	files := validProc()
	delete(files, "proc/self/fd/0")
	rd := writeProc(t, t.TempDir(), files)
	if _, p, err := rd.process(); err == nil || p.Available || p.Reason == "" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestParseLoadavgNonNumeric(t *testing.T) {
	if _, _, _, err := parseLoadavg("a b c\n"); err == nil {
		t.Fatal("want error")
	}
}

func TestParseProcStatNonNumeric(t *testing.T) {
	if _, err := parseProcStat("1 (x) S 1 1 1 0 -1 0 0 0 0 0 a 5 0\n"); err == nil {
		t.Fatal("want error for a non-numeric utime")
	}
	if _, err := parseProcStat("1 (x) S 1 1 1 0 -1 0 0 0 0 0 5 b 0\n"); err == nil {
		t.Fatal("want error for a non-numeric stime")
	}
	if _, err := parseProcStat("1 (x) S 1\n"); err == nil {
		t.Fatal("want error for too few fields")
	}
}

func TestParseMemMaxGarbageIsNil(t *testing.T) {
	if parseMemMax("lots\n") != nil {
		t.Fatal("garbage must be nil")
	}
}

func TestParseMeminfoClampsInconsistentCounters(t *testing.T) {
	m, err := parseMeminfo("MemTotal: 10 kB\nMemAvailable: 20 kB\nSwapTotal: 5 kB\nSwapFree: 9 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.UsedBytes != 0 || m.SwapUsedBytes != 0 {
		t.Fatalf("inconsistent counters must clamp to zero used: %+v", m)
	}
}

func TestParseStatNonNumericField(t *testing.T) {
	if _, _, err := parseStat("cpu  1 2 x 4 5\n"); err == nil {
		t.Fatal("want error")
	}
}
