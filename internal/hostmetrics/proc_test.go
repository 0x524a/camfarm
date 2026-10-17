package hostmetrics

import (
	"os"
	"path/filepath"
	"testing"
)

const statFixture = `cpu  100 10 50 800 40 0 0 0 0 0
cpu0 50 5 25 400 20 0 0 0 0 0
cpu1 50 5 25 400 20 0 0 0 0 0
intr 12345
`

const meminfoFixture = `MemTotal:       65000000 kB
MemFree:         1000000 kB
MemAvailable:   40000000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
`

func TestParseStat(t *testing.T) {
	ct, cpus, err := parseStat(statFixture)
	if err != nil {
		t.Fatal(err)
	}
	// total = 100+10+50+800+40 = 1000; idle = 800+40 = 840; busy = 160.
	if ct.total != 1000 || ct.busy != 160 || cpus != 2 {
		t.Fatalf("got %+v cpus=%d", ct, cpus)
	}
	if _, _, err := parseStat("intr 1\n"); err == nil {
		t.Fatal("want error for a file with no aggregate cpu line")
	}
	if _, _, err := parseStat("cpu  1 2\n"); err == nil {
		t.Fatal("want error for a short cpu line")
	}
}

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo(meminfoFixture)
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalBytes != 65000000*1024 || m.AvailableBytes != 40000000*1024 ||
		m.UsedBytes != 25000000*1024 || m.SwapTotalBytes != 2000000*1024 || m.SwapUsedBytes != 500000*1024 {
		t.Fatalf("got %+v", m)
	}
	if _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Fatal("want error when MemAvailable is missing")
	}
}

func TestParseLoadavg(t *testing.T) {
	a, b, c, err := parseLoadavg("3.10 2.80 2.20 1/500 12345\n")
	if err != nil || a != 3.10 || b != 2.80 || c != 2.20 {
		t.Fatalf("got %v %v %v %v", a, b, c, err)
	}
	if _, _, _, err := parseLoadavg("x\n"); err == nil {
		t.Fatal("want error")
	}
}

func TestParseProcStat(t *testing.T) {
	// comm contains spaces and a ')' to prove the parser anchors on the last ')'.
	line := "42 (cam (farm)) S 1 42 42 0 -1 4194304 100 0 0 0 70 30 0 0 20 0 8 0 1000 1000 500 18446744073709551615\n"
	ticks, err := parseProcStat(line)
	if err != nil || ticks != 100 {
		t.Fatalf("ticks=%d err=%v", ticks, err)
	}
	if _, err := parseProcStat("garbage"); err == nil {
		t.Fatal("want error")
	}
}

func TestParseCgroupLimits(t *testing.T) {
	cases := []struct {
		in   string
		want *float64
	}{
		{"max 100000\n", nil},
		{"200000 100000\n", f64(2)},
		{"50000 100000\n", f64(0.5)},
		{"", nil},
		{"junk\n", nil},
	}
	for _, c := range cases {
		got := parseCPUMax(c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("parseCPUMax(%q)=%v want %v", c.in, got, c.want)
		}
	}
	if parseMemMax("max\n") != nil || parseMemMax("") != nil {
		t.Fatal("max/empty must be nil")
	}
	if v := parseMemMax("1073741824\n"); v == nil || *v != 1073741824 {
		t.Fatalf("got %v", v)
	}
}

func f64(v float64) *float64 { return &v }

// writeProc builds a fake /proc and cgroup tree under dir.
func writeProc(t *testing.T, dir string, files map[string]string) procReader {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return procReader{proc: filepath.Join(dir, "proc"), cgroup: filepath.Join(dir, "cg")}
}

func TestProcReader(t *testing.T) {
	dir := t.TempDir()
	rd := writeProc(t, dir, map[string]string{
		"proc/stat":       statFixture,
		"proc/meminfo":    meminfoFixture,
		"proc/loadavg":    "3.10 2.80 2.20 1/500 12345\n",
		"proc/self/stat":  "42 (camfarm) S 1 42 42 0 -1 4194304 100 0 0 0 70 30 0 0 20 0 8 0 1000 1000 500 0\n",
		"proc/self/statm": "1000 250 0 0 0 0 0\n",
		"proc/self/fd/0":  "",
		"proc/self/fd/1":  "",
		"cg/cpu.max":      "200000 100000\n",
		"cg/memory.max":   "2147483648\n",
	})
	ct, cpu, err := rd.cpu()
	if err != nil || !cpu.Available || cpu.LogicalCPUs != 2 || cpu.Load1 != 3.10 || ct.total != 1000 {
		t.Fatalf("cpu=%+v ct=%+v err=%v", cpu, ct, err)
	}
	if cpu.LimitCPUs == nil || *cpu.LimitCPUs != 2 {
		t.Fatalf("limitCPUs=%v", cpu.LimitCPUs)
	}
	if cpu.UsagePercent != nil {
		t.Fatal("reader must not invent a usage percent")
	}
	mem, err := rd.memory()
	if err != nil || !mem.Available || mem.LimitBytes == nil || *mem.LimitBytes != 2147483648 {
		t.Fatalf("mem=%+v err=%v", mem, err)
	}
	ticks, p, err := rd.process()
	if err != nil || !p.Available || ticks != 100 || p.RSSBytes != 250*uint64(os.Getpagesize()) || p.OpenFDs != 2 || p.Goroutines < 1 {
		t.Fatalf("ticks=%d p=%+v err=%v", ticks, p, err)
	}
}

func TestProcReaderMissingFilesAreUnavailableNotFatal(t *testing.T) {
	rd := procReader{proc: t.TempDir(), cgroup: t.TempDir()}
	_, cpu, err := rd.cpu()
	if err == nil || cpu.Available || cpu.Reason == "" {
		t.Fatalf("cpu=%+v err=%v", cpu, err)
	}
	mem, err := rd.memory()
	if err == nil || mem.Available || mem.Reason == "" {
		t.Fatalf("mem=%+v err=%v", mem, err)
	}
	_, p, err := rd.process()
	if err == nil || p.Available || p.Reason == "" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	old := goos
	goos = "plan9"
	t.Cleanup(func() { goos = old })
	rd := procReader{proc: t.TempDir(), cgroup: t.TempDir()}
	if _, cpu, _ := rd.cpu(); cpu.Reason != "unsupported platform" {
		t.Fatalf("reason=%q", cpu.Reason)
	}
	if mem, _ := rd.memory(); mem.Reason != "unsupported platform" {
		t.Fatalf("reason=%q", mem.Reason)
	}
}

func TestPercent(t *testing.T) {
	if got := percent(100, 1000); got != 10.0 {
		t.Fatalf("got %v", got)
	}
	if got := percent(1, 3); got != 33.3 {
		t.Fatalf("got %v", got)
	}
	if got := percent(5, 0); got != 0 {
		t.Fatalf("got %v", got)
	}
}
