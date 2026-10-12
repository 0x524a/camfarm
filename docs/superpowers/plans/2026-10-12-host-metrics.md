# Host Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve host CPU, memory, GPU and camfarm-process readings at `GET /api/v1/metrics` and as `event: metrics` on `/api/v1/events`.

**Architecture:** A new `internal/hostmetrics` package with pure parsers (`/proc`, cgroup v2, `nvidia-smi` CSV), a `Sampler` that samples every second only while watched, and its own small event bus. The dashboard gains a read-only handler and one more channel in the SSE merge, exactly like stream quality. `nvidia-smi` runs through an injectable runner so tests use captured output.

**Tech Stack:** Go 1.26, `CGO_ENABLED=0`, standard library only (no new dependency).

**Spec:** `docs/superpowers/specs/2026-10-12-host-metrics-design.md`

## Global Constraints

- Module path `github.com/0x524a/camfarm`; `CGO_ENABLED=0` must still build; no new dependency in `go.mod`.
- Metrics never touch the fleet: no `Fleet.publish`, no journal entry, no clock, no RTSP client, so the replay hash cannot change.
- An unreadable section is `{"available": false, "reason": "..."}`; one failing section never fails the request or blanks another.
- `nvidia-smi` values reported as `[N/A]`, `N/A`, `Not Supported` or `[Not Supported]` are JSON `null`, never `0`.
- `nvidia-smi` timeout 2 s, output cap 1 MiB, stderr/first-line reason cap 200 characters; GPU retry after a failure at most every 15 s; sampler idles unless an event-stream client is subscribed or a `Current`/`Touch` happened in the last 10 s.
- Reason strings (exact): `nvidia-smi not found`, `nvidia-smi failed: <line>`, `timed out`, `unparseable output`, `unsupported platform`, `pending` (before the first GPU reading only).
- No employer or client names anywhere. No coverage figure or GPU-behaviour claim not produced by CI (the README says the GPU parser is verified against captured `nvidia-smi` output, not live hardware).
- Shell guard in this worktree: run plain, separate git commands (no `&&` chains, no heredocs, no `bash -c` with git). Use the Write/Edit tools for files.
- Commit only under the repo's configured identity (never add `Co-Authored-By` of another identity beyond what the repo already does; do not change git config).
- Test names and code style follow `internal/quality` (small files, injectable seams, `-race` clean).

## Review Focus

Failure modes the spec implies that deserve a test (each is pinned in the task that owns the code):

1. A `[N/A]`/`Not Supported` GPU field reported as `0` instead of `null` (Task 2).
2. A hung `nvidia-smi` stalling the CPU/memory numbers or the sampler (Task 3, slow-GPU test).
3. A broken driver (this development machine) causing a failed spawn every second instead of every 15 s (Task 3, backoff test).
4. An unwatched farm spawning processes or reading `/proc` (Task 3, idle test).
5. Counters that go backwards or stale baselines producing a negative or wildly averaged CPU % (Task 1 and Task 3: `null`, never a bad number).
6. A slow SSE client pinning the sampler or never being dropped (Task 3, slow-subscriber test).
7. Non-Linux hosts: sections report `unsupported platform`, GPU still works (Task 1).

## File Structure

- Create `internal/hostmetrics/types.go`: `Sample`, `CPU`, `Memory`, `GPU`, `Device`, `Process` and their JSON tags.
- Create `internal/hostmetrics/proc.go`: pure parsers and the `procReader` that reads `/proc` and cgroup files.
- Create `internal/hostmetrics/proc_test.go`.
- Create `internal/hostmetrics/gpu.go`: `Runner`, default exec runner, CSV parser, `queryGPU`.
- Create `internal/hostmetrics/gpu_test.go`.
- Create `internal/hostmetrics/sampler.go`: `Options`, `Sampler`, event bus, GPU cache and backoff.
- Create `internal/hostmetrics/sampler_test.go`.
- Create `internal/dashboard/metrics.go`: `getMetrics`.
- Modify `internal/dashboard/dashboard.go`: `Options.Metrics`, server field, route.
- Modify `internal/dashboard/events.go`: merge `event: metrics`.
- Create `internal/dashboard/metrics_test.go`.
- Modify `cmd/camfarm/run.go`: create and close the sampler.
- Modify `README.md`: "Host metrics" section.

---

### Task 1: Types and `/proc` readers

**Files:**
- Create: `internal/hostmetrics/types.go`
- Create: `internal/hostmetrics/proc.go`
- Test: `internal/hostmetrics/proc_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (used by Tasks 2 and 3):
  - Types `Sample{Time time.Time; CPU CPU; Memory Memory; GPU GPU; Process Process}`, `CPU`, `Memory`, `GPU{Available bool; Reason string; Devices []Device}`, `Device`, `Process` (fields in Step 3).
  - `type cpuTimes struct{ busy, total uint64 }`.
  - `type procReader struct{ proc, cgroup string }` with methods `cpu() (cpuTimes, CPU, error)`, `memory() (Memory, error)`, `process() (ticks uint64, p Process, err error)`. On error the returned section has `Available=false` and `Reason` set (via `unavailableReason(err)`); callers still receive the section.
  - `var goos = runtime.GOOS` (tests override it).
  - `func percent(delta, total uint64) float64` (one decimal) and `func round1(v float64) float64`.
  - `const clockTick = 100` (USER_HZ).

- [ ] **Step 1: Write the failing tests**

Create `internal/hostmetrics/proc_test.go`:

```go
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
		"proc/stat":         statFixture,
		"proc/meminfo":      meminfoFixture,
		"proc/loadavg":      "3.10 2.80 2.20 1/500 12345\n",
		"proc/self/stat":    "42 (camfarm) S 1 42 42 0 -1 4194304 100 0 0 0 70 30 0 0 20 0 8 0 1000 1000 500 0\n",
		"proc/self/statm":   "1000 250 0 0 0 0 0\n",
		"proc/self/fd/0":    "",
		"proc/self/fd/1":    "",
		"cg/cpu.max":        "200000 100000\n",
		"cg/memory.max":     "2147483648\n",
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/hostmetrics/ -run 'Parse|ProcReader|Unsupported|Percent' -count=1`
Expected: FAIL to compile (package and symbols do not exist yet).

- [ ] **Step 3: Write the implementation**

Create `internal/hostmetrics/types.go`:

```go
// Package hostmetrics samples host CPU, memory and GPU load plus camfarm's own
// process usage. It is independent of the fleet: it publishes nothing to the
// fleet feed, writes no journal entry and cannot change a replay hash.
package hostmetrics

import "time"

// Sample is one reading. Every section is independent: one that cannot be read
// has Available false and a Reason, and the others are unaffected.
type Sample struct {
	Time    time.Time `json:"time"`
	CPU     CPU       `json:"cpu"`
	Memory  Memory    `json:"memory"`
	GPU     GPU       `json:"gpu"`
	Process Process   `json:"process"`
}

// CPU is host-wide. UsagePercent is null until a baseline exists (the first
// sample after the sampler wakes). LimitCPUs is the cgroup quota, if any.
type CPU struct {
	Available    bool     `json:"available"`
	Reason       string   `json:"reason,omitempty"`
	UsagePercent *float64 `json:"usagePercent"`
	LogicalCPUs  int      `json:"logicalCPUs"`
	Load1        float64  `json:"load1"`
	Load5        float64  `json:"load5"`
	Load15       float64  `json:"load15"`
	LimitCPUs    *float64 `json:"limitCPUs"`
}

// Memory is host-wide. LimitBytes is the cgroup limit, if any.
type Memory struct {
	Available      bool    `json:"available"`
	Reason         string  `json:"reason,omitempty"`
	TotalBytes     uint64  `json:"totalBytes"`
	UsedBytes      uint64  `json:"usedBytes"`
	AvailableBytes uint64  `json:"availableBytes"`
	SwapTotalBytes uint64  `json:"swapTotalBytes"`
	SwapUsedBytes  uint64  `json:"swapUsedBytes"`
	LimitBytes     *uint64 `json:"limitBytes"`
}

// GPU lists the devices nvidia-smi reports. Devices is never null.
type GPU struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Devices   []Device `json:"devices"`
}

// Device is one GPU. A value nvidia-smi reports as N/A or Not Supported is null.
// GPUUtilPercent is nvidia-smi's utilization.gpu, the share of time a kernel was
// running, not true SM occupancy (that needs DCGM).
type Device struct {
	Index              int      `json:"index"`
	Name               string   `json:"name"`
	UUID               string   `json:"uuid"`
	GPUUtilPercent     *float64 `json:"gpuUtilPercent"`
	MemUtilPercent     *float64 `json:"memUtilPercent"`
	MemUsedBytes       *uint64  `json:"memUsedBytes"`
	MemTotalBytes      *uint64  `json:"memTotalBytes"`
	EncoderUtilPercent *float64 `json:"encoderUtilPercent"`
	DecoderUtilPercent *float64 `json:"decoderUtilPercent"`
	TemperatureC       *float64 `json:"temperatureC"`
	PowerWatts         *float64 `json:"powerWatts"`
	PowerLimitWatts    *float64 `json:"powerLimitWatts"`
}

// Process is camfarm's own usage. CPUPercent can exceed 100 on multiple cores.
type Process struct {
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
	CPUPercent *float64 `json:"cpuPercent"`
	RSSBytes   uint64   `json:"rssBytes"`
	Goroutines int      `json:"goroutines"`
	OpenFDs    int      `json:"openFDs"`
}
```

Create `internal/hostmetrics/proc.go`:

```go
package hostmetrics

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// goos is a variable so tests can exercise the unsupported-platform path.
var goos = runtime.GOOS

// clockTick is USER_HZ, the unit of /proc/<pid>/stat CPU times. It is 100 on
// every Linux configuration camfarm targets.
const clockTick = 100

type cpuTimes struct{ busy, total uint64 }

type procReader struct{ proc, cgroup string }

func unavailableReason(err error) string {
	if goos != "linux" {
		return "unsupported platform"
	}
	return err.Error()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func percent(delta, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return round1(100 * float64(delta) / float64(total))
}

func (p procReader) read(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(p.proc, rel))
	return string(b), err
}

func (p procReader) cpu() (cpuTimes, CPU, error) {
	fail := func(err error) (cpuTimes, CPU, error) {
		return cpuTimes{}, CPU{Reason: unavailableReason(err)}, err
	}
	s, err := p.read("stat")
	if err != nil {
		return fail(err)
	}
	ct, n, err := parseStat(s)
	if err != nil {
		return fail(err)
	}
	l, err := p.read("loadavg")
	if err != nil {
		return fail(err)
	}
	l1, l5, l15, err := parseLoadavg(l)
	if err != nil {
		return fail(err)
	}
	c := CPU{Available: true, LogicalCPUs: n, Load1: l1, Load5: l5, Load15: l15}
	if b, err := os.ReadFile(filepath.Join(p.cgroup, "cpu.max")); err == nil {
		c.LimitCPUs = parseCPUMax(string(b))
	}
	return ct, c, nil
}

func (p procReader) memory() (Memory, error) {
	s, err := p.read("meminfo")
	if err != nil {
		return Memory{Reason: unavailableReason(err)}, err
	}
	m, err := parseMeminfo(s)
	if err != nil {
		return Memory{Reason: unavailableReason(err)}, err
	}
	if b, err := os.ReadFile(filepath.Join(p.cgroup, "memory.max")); err == nil {
		m.LimitBytes = parseMemMax(string(b))
	}
	return m, nil
}

func (p procReader) process() (uint64, Process, error) {
	fail := func(err error) (uint64, Process, error) {
		return 0, Process{Reason: unavailableReason(err)}, err
	}
	s, err := p.read("self/stat")
	if err != nil {
		return fail(err)
	}
	ticks, err := parseProcStat(s)
	if err != nil {
		return fail(err)
	}
	sm, err := p.read("self/statm")
	if err != nil {
		return fail(err)
	}
	f := strings.Fields(sm)
	if len(f) < 2 {
		return fail(errors.New("short statm"))
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return fail(err)
	}
	ents, err := os.ReadDir(filepath.Join(p.proc, "self", "fd"))
	if err != nil {
		return fail(err)
	}
	return ticks, Process{
		Available:  true,
		RSSBytes:   pages * uint64(os.Getpagesize()),
		Goroutines: runtime.NumGoroutine(),
		OpenFDs:    len(ents),
	}, nil
}

// parseStat reads the aggregate "cpu" line and counts the "cpuN" lines. Busy is
// total minus idle and iowait.
func parseStat(s string) (cpuTimes, int, error) {
	var t cpuTimes
	cpus, found := 0, false
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			cpus++
			continue
		}
		if len(f) < 5 {
			return t, 0, errors.New("short cpu line in /proc/stat")
		}
		var total, idle uint64
		for i := 1; i < len(f) && i <= 8; i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return t, 0, fmt.Errorf("parse /proc/stat: %w", err)
			}
			total += v
			if i == 4 || i == 5 {
				idle += v
			}
		}
		t = cpuTimes{busy: total - idle, total: total}
		found = true
	}
	if !found {
		return t, 0, errors.New("no cpu line in /proc/stat")
	}
	return t, cpus, nil
}

func parseMeminfo(s string) (Memory, error) {
	kb := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		kb[k] = n
	}
	total, okT := kb["MemTotal"]
	avail, okA := kb["MemAvailable"]
	if !okT || !okA {
		return Memory{}, errors.New("MemTotal or MemAvailable missing in /proc/meminfo")
	}
	m := Memory{Available: true, TotalBytes: total * 1024, AvailableBytes: avail * 1024}
	if avail <= total {
		m.UsedBytes = (total - avail) * 1024
	}
	m.SwapTotalBytes = kb["SwapTotal"] * 1024
	if st, sf := kb["SwapTotal"], kb["SwapFree"]; sf <= st {
		m.SwapUsedBytes = (st - sf) * 1024
	}
	return m, nil
}

func parseLoadavg(s string) (a, b, c float64, err error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, 0, errors.New("short /proc/loadavg")
	}
	var v [3]float64
	for i := range v {
		if v[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return 0, 0, 0, err
		}
	}
	return v[0], v[1], v[2], nil
}

// parseProcStat returns utime+stime in clock ticks. The command name is in
// parentheses and may itself contain spaces and ')', so it anchors on the last ')'.
func parseProcStat(s string) (uint64, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("malformed /proc/self/stat")
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state), so utime (14) is f[11] and stime (15) is f[12].
	if len(f) < 13 {
		return 0, errors.New("short /proc/self/stat")
	}
	u, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return 0, err
	}
	st, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return u + st, nil
}

// parseCPUMax reads cgroup v2 cpu.max ("max 100000" or "<quota> <period>").
func parseCPUMax(s string) *float64 {
	f := strings.Fields(s)
	if len(f) != 2 || f[0] == "max" {
		return nil
	}
	q, err1 := strconv.ParseFloat(f[0], 64)
	p, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || p <= 0 {
		return nil
	}
	v := q / p
	return &v
}

// parseMemMax reads cgroup v2 memory.max ("max" or bytes).
func parseMemMax(s string) *uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" {
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil
	}
	return &v
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/hostmetrics/ -race -count=1`
Expected: PASS. Then `go vet ./internal/hostmetrics/` and `gofmt -l internal/hostmetrics/` (no output).

- [ ] **Step 5: Commit**

```bash
git add internal/hostmetrics/types.go internal/hostmetrics/proc.go internal/hostmetrics/proc_test.go
git commit -m "hostmetrics: sample types and /proc and cgroup readers"
```

---

### Task 2: GPU via `nvidia-smi`

**Files:**
- Create: `internal/hostmetrics/gpu.go`
- Test: `internal/hostmetrics/gpu_test.go`

**Interfaces:**
- Consumes: `GPU`, `Device` from Task 1.
- Produces (used by Task 3):
  - `type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)`
  - `func execRunner(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)` (the default).
  - `func queryGPU(ctx context.Context, run Runner) (g GPU, missing bool)`: `missing` is true only for "not found", which the sampler caches for the process lifetime. `ctx` already carries the 2 s timeout.
  - `func parseGPU(out []byte) ([]Device, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/hostmetrics/gpu_test.go`:

```go
package hostmetrics

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const oneGPU = "0, NVIDIA GeForce RTX 4090, GPU-aaaa-1111, 37, 12, 1024, 24564, 5, 0, 54, 78.43, 450.00\n"

const twoGPUWithNA = oneGPU +
	"1, Tesla T4, GPU-bbbb-2222, 0, 0, 0, 15360, [N/A], [N/A], 40, [Not Supported], [Not Supported]\n"

func runnerOut(out string) Runner {
	return func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte(out), nil, nil
	}
}

func TestParseGPUOne(t *testing.T) {
	devs, err := parseGPU([]byte(oneGPU))
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	d := devs[0]
	if d.Index != 0 || d.Name != "NVIDIA GeForce RTX 4090" || d.UUID != "GPU-aaaa-1111" {
		t.Fatalf("ident %+v", d)
	}
	if d.GPUUtilPercent == nil || *d.GPUUtilPercent != 37 || *d.MemUtilPercent != 12 {
		t.Fatalf("util %+v", d)
	}
	if *d.MemUsedBytes != 1024<<20 || *d.MemTotalBytes != 24564<<20 {
		t.Fatalf("mem %d %d", *d.MemUsedBytes, *d.MemTotalBytes)
	}
	if *d.EncoderUtilPercent != 5 || *d.DecoderUtilPercent != 0 || *d.TemperatureC != 54 ||
		*d.PowerWatts != 78.43 || *d.PowerLimitWatts != 450 {
		t.Fatalf("rest %+v", d)
	}
}

func TestParseGPUNotAvailableFieldsAreNullNotZero(t *testing.T) {
	devs, err := parseGPU([]byte(twoGPUWithNA))
	if err != nil || len(devs) != 2 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	d := devs[1]
	if d.EncoderUtilPercent != nil || d.DecoderUtilPercent != nil || d.PowerWatts != nil || d.PowerLimitWatts != nil {
		t.Fatalf("N/A must be nil: %+v", d)
	}
	// A real zero stays zero.
	if d.GPUUtilPercent == nil || *d.GPUUtilPercent != 0 || d.MemUsedBytes == nil || *d.MemUsedBytes != 0 {
		t.Fatalf("zero must stay zero: %+v", d)
	}
}

func TestParseGPUGarbage(t *testing.T) {
	for _, in := range []string{
		"hello world\n",
		"0, a, b, 1, 2\n",                                     // too few columns
		"x, a, b, 1, 2, 3, 4, 5, 6, 7, 8, 9\n",               // bad index
		"0, a, b, notanumber, 2, 3, 4, 5, 6, 7, 8, 9\n",       // bad number
	} {
		if _, err := parseGPU([]byte(in)); err == nil {
			t.Errorf("want error for %q", in)
		}
	}
}

func TestQueryGPUOK(t *testing.T) {
	g, missing := queryGPU(context.Background(), runnerOut(twoGPUWithNA))
	if !g.Available || missing || len(g.Devices) != 2 || g.Reason != "" {
		t.Fatalf("%+v missing=%v", g, missing)
	}
}

func TestQueryGPUNoDevices(t *testing.T) {
	g, _ := queryGPU(context.Background(), runnerOut("\n"))
	if g.Available || g.Reason != "nvidia-smi reported no devices" || g.Devices == nil {
		t.Fatalf("%+v", g)
	}
}

func TestQueryGPUNotFound(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, nil, &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}
	}
	g, missing := queryGPU(context.Background(), run)
	if g.Available || g.Reason != "nvidia-smi not found" || !missing || g.Devices == nil {
		t.Fatalf("%+v missing=%v", g, missing)
	}
}

// This is the failure seen on the development machine: the driver and the NVML
// library disagree. nvidia-smi exits non-zero and prints the message on stdout.
func TestQueryGPUDriverMismatch(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte("Failed to initialize NVML: Driver/library version mismatch\nNVML library version: 580.178\n"),
			nil, errors.New("exit status 18")
	}
	g, missing := queryGPU(context.Background(), run)
	want := "nvidia-smi failed: Failed to initialize NVML: Driver/library version mismatch"
	if g.Available || g.Reason != want || missing {
		t.Fatalf("%+v missing=%v", g, missing)
	}
}

func TestQueryGPUFailureReasonFromStderrAndCapped(t *testing.T) {
	long := strings.Repeat("x", 500)
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("\n" + long + "\nsecond"), errors.New("exit status 1")
	}
	g, _ := queryGPU(context.Background(), run)
	if got := len([]rune(strings.TrimPrefix(g.Reason, "nvidia-smi failed: "))); got != 200 {
		t.Fatalf("reason length %d: %q", got, g.Reason)
	}
}

func TestQueryGPUTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	g, missing := queryGPU(ctx, run)
	if g.Available || g.Reason != "timed out" || missing {
		t.Fatalf("%+v missing=%v", g, missing)
	}
}

func TestQueryGPUUnparseable(t *testing.T) {
	g, _ := queryGPU(context.Background(), runnerOut("not,csv at all\n"))
	if g.Available || g.Reason != "unparseable output" {
		t.Fatalf("%+v", g)
	}
}

func TestExecRunnerMissingBinary(t *testing.T) {
	_, _, err := execRunner(context.Background(), "camfarm-no-such-binary-xyz")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/hostmetrics/ -run 'GPU|ExecRunner' -count=1`
Expected: FAIL to compile (`parseGPU`, `queryGPU`, `execRunner` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/hostmetrics/gpu.go`:

```go
package hostmetrics

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	gpuOutputCap  = 1 << 20
	reasonCap     = 200
	smiName       = "nvidia-smi"
	smiColumns    = 12
	smiWaitDelay  = time.Second
	reasonTimeout = "timed out"
)

var smiArgs = []string{
	"--query-gpu=index,name,uuid,utilization.gpu,utilization.memory,memory.used,memory.total," +
		"utilization.encoder,utilization.decoder,temperature.gpu,power.draw,power.limit",
	"--format=csv,noheader,nounits",
}

// Runner runs a command. It exists so tests supply captured nvidia-smi output
// and never need a GPU. A failing command returns its output with the error.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

// capBuf keeps at most gpuOutputCap bytes and silently discards the rest.
type capBuf struct{ b bytes.Buffer }

func (c *capBuf) Write(p []byte) (int, error) {
	if room := gpuOutputCap - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var so, se capBuf
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.WaitDelay = smiWaitDelay
	err := cmd.Run()
	return so.b.Bytes(), se.b.Bytes(), err
}

func unavailableGPU(reason string) GPU {
	return GPU{Reason: reason, Devices: []Device{}}
}

// queryGPU reads every GPU. missing is true only when nvidia-smi is not
// installed, which the sampler remembers for the life of the process.
func queryGPU(ctx context.Context, run Runner) (g GPU, missing bool) {
	out, errOut, err := run(ctx, smiName, smiArgs...)
	if err != nil {
		switch {
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
			return unavailableGPU("nvidia-smi not found"), true
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return unavailableGPU(reasonTimeout), false
		}
		return unavailableGPU("nvidia-smi failed: " + firstLine(errOut, out, []byte(err.Error()))), false
	}
	devs, err := parseGPU(out)
	if err != nil {
		return unavailableGPU("unparseable output"), false
	}
	if len(devs) == 0 {
		return unavailableGPU("nvidia-smi reported no devices"), false
	}
	return GPU{Available: true, Devices: devs}, false
}

// firstLine returns the first non-empty line of the first source that has one,
// capped at reasonCap runes. nvidia-smi prints some errors on stdout.
func firstLine(sources ...[]byte) string {
	for _, src := range sources {
		for _, line := range strings.Split(string(src), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				if r := []rune(line); len(r) > reasonCap {
					return string(r[:reasonCap])
				}
				return line
			}
		}
	}
	return ""
}

func parseGPU(out []byte) ([]Device, error) {
	r := csv.NewReader(bytes.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = smiColumns
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	devs := make([]Device, 0, len(rows))
	for _, f := range rows {
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			return nil, err
		}
		d := Device{Index: idx, Name: strings.TrimSpace(f[1]), UUID: strings.TrimSpace(f[2])}
		var bad error
		num := func(s string) *float64 {
			s = strings.TrimSpace(s)
			switch s {
			case "", "N/A", "[N/A]", "Not Supported", "[Not Supported]":
				return nil
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				bad = err
				return nil
			}
			return &v
		}
		mib := func(s string) *uint64 {
			v := num(s)
			if v == nil || *v < 0 {
				return nil
			}
			b := uint64(*v) << 20
			return &b
		}
		d.GPUUtilPercent = num(f[3])
		d.MemUtilPercent = num(f[4])
		d.MemUsedBytes = mib(f[5])
		d.MemTotalBytes = mib(f[6])
		d.EncoderUtilPercent = num(f[7])
		d.DecoderUtilPercent = num(f[8])
		d.TemperatureC = num(f[9])
		d.PowerWatts = num(f[10])
		d.PowerLimitWatts = num(f[11])
		if bad != nil {
			return nil, bad
		}
		devs = append(devs, d)
	}
	return devs, nil
}
```

Note: the `reasonTimeout` constant is also used by Task 3's timeout handling; keep the name.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/hostmetrics/ -race -count=1`
Expected: PASS (Task 1 tests too). `go vet ./internal/hostmetrics/` and `gofmt -l internal/hostmetrics/` clean.

- [ ] **Step 5: Commit**

```bash
git add internal/hostmetrics/gpu.go internal/hostmetrics/gpu_test.go
git commit -m "hostmetrics: nvidia-smi reader with captured-output tests"
```

---

### Task 3: The sampler

**Files:**
- Create: `internal/hostmetrics/sampler.go`
- Test: `internal/hostmetrics/sampler_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1 and 2.
- Produces (used by Task 4):
  - `type Options struct { ProcRoot, CgroupRoot string; Run Runner; Interval time.Duration; Ticks <-chan time.Time; Now func() time.Time; IdleAfter, GPURetry, GPUTimeout time.Duration }`. Zero values select defaults: `/proc`, `/sys/fs/cgroup`, `execRunner`, 1 s, a real ticker, `time.Now`, 10 s, 15 s, 2 s.
  - `func New(Options) *Sampler`
  - `func (s *Sampler) Current(ctx context.Context) Sample`: marks the sampler wanted; returns the latest sample if it is younger than 2 s, else samples synchronously (a cold call waits for the first GPU reading).
  - `func (s *Sampler) Touch()`, `func (s *Sampler) Latest() (Sample, bool)`, `func (s *Sampler) Subscribe(ctx context.Context) <-chan Sample`, `func (s *Sampler) Close()` (idempotent).

- [ ] **Step 1: Write the failing tests**

Create `internal/hostmetrics/sampler_test.go`:

```go
package hostmetrics

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)} }

// setProc writes a fake /proc whose cpu line has the given user and idle
// jiffies and whose process has used the given ticks.
func setProc(t *testing.T, dir string, user, idle, ticks string) {
	t.Helper()
	writeProc(t, dir, map[string]string{
		"proc/stat":       "cpu  " + user + " 0 0 " + idle + " 0 0 0 0\ncpu0 " + user + " 0 0 " + idle + " 0 0 0 0\n",
		"proc/meminfo":    meminfoFixture,
		"proc/loadavg":    "1.00 1.00 1.00 1/1 1\n",
		"proc/self/stat":  "1 (camfarm) S 1 1 1 0 -1 0 0 0 0 0 " + ticks + " 0 0 0 20 0 1 0 1 1 1 0\n",
		"proc/self/statm": "10 10 0 0 0 0 0\n",
		"proc/self/fd/0":  "",
	})
}

type harness struct {
	s     *Sampler
	clock *fakeClock
	ticks chan time.Time
	dir   string
}

func newHarness(t *testing.T, run Runner) *harness {
	t.Helper()
	dir := t.TempDir()
	setProc(t, dir, "100", "900", "0")
	clock := newClock()
	ticks := make(chan time.Time) // unbuffered: a send returns only once the loop took it
	s := New(Options{
		ProcRoot: dir + "/proc", CgroupRoot: dir + "/cg",
		Run: run, Ticks: ticks, Now: clock.Now,
	})
	t.Cleanup(s.Close)
	return &harness{s: s, clock: clock, ticks: ticks, dir: dir}
}

func (h *harness) tick(t *testing.T) {
	t.Helper()
	select {
	case h.ticks <- h.clock.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not take the tick")
	}
}

func recv(t *testing.T, ch <-chan Sample) Sample {
	t.Helper()
	select {
	case s, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no sample")
		return Sample{}
	}
}

func okRunner(string) Runner { return runnerOut(oneGPU) }

func TestSamplerBaselineThenRealPercentages(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())

	h.tick(t)
	first := recv(t, sub)
	if first.CPU.UsagePercent != nil || first.Process.CPUPercent != nil {
		t.Fatalf("first sample has no baseline, want null: %+v %+v", first.CPU.UsagePercent, first.Process.CPUPercent)
	}
	if !first.CPU.Available || !first.Memory.Available || !first.Process.Available {
		t.Fatalf("first sample: %+v", first)
	}

	// busy 100 -> 200, total 1000 -> 2000: 10.0 %. Process 0 -> 50 ticks in 1 s: 50.0 %.
	setProc(t, h.dir, "200", "1800", "50")
	h.clock.Advance(time.Second)
	h.tick(t)
	second := recv(t, sub)
	if second.CPU.UsagePercent == nil || *second.CPU.UsagePercent != 10.0 {
		t.Fatalf("cpu %v", second.CPU.UsagePercent)
	}
	if second.Process.CPUPercent == nil || *second.Process.CPUPercent != 50.0 {
		t.Fatalf("proc %v", second.Process.CPUPercent)
	}
}

func TestSamplerCountersGoingBackwardsGiveNullNotNegative(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	h.tick(t)
	recv(t, sub)
	setProc(t, h.dir, "10", "90", "0") // counters reset (container restart)
	h.clock.Advance(time.Second)
	h.tick(t)
	if s := recv(t, sub); s.CPU.UsagePercent != nil {
		t.Fatalf("want null, got %v", *s.CPU.UsagePercent)
	}
}

func TestSamplerStaleBaselineIsNotAveraged(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	h.tick(t)
	recv(t, sub)
	setProc(t, h.dir, "200", "1800", "50")
	h.clock.Advance(time.Minute) // far longer than three intervals
	h.tick(t)
	if s := recv(t, sub); s.CPU.UsagePercent != nil || s.Process.CPUPercent != nil {
		t.Fatal("a stale baseline must give null")
	}
}

func TestSamplerIdlesWhenUnwatched(t *testing.T) {
	var calls atomic.Int32
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		calls.Add(1)
		return []byte(oneGPU), nil, nil
	}
	h := newHarness(t, run)
	h.tick(t)
	h.tick(t) // barrier: the first tick is fully processed once the loop takes this one
	if _, ok := h.s.Latest(); ok || calls.Load() != 0 {
		t.Fatalf("unwatched sampler must not sample: latest=%v calls=%d", ok, calls.Load())
	}
	h.s.Touch()
	h.tick(t)
	h.tick(t)
	if _, ok := h.s.Latest(); !ok {
		t.Fatal("touched sampler must sample")
	}
	// Ten seconds without a touch and it goes quiet again.
	h.clock.Advance(11 * time.Second)
	before, _ := h.s.Latest()
	h.tick(t)
	h.tick(t)
	if after, _ := h.s.Latest(); !after.Time.Equal(before.Time) {
		t.Fatal("sampler kept sampling after the idle window")
	}
}

func TestSamplerGPUBackoff(t *testing.T) {
	var calls atomic.Int32
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		calls.Add(1)
		return []byte("Failed to initialize NVML: Driver/library version mismatch\n"), nil, errors.New("exit status 18")
	}
	h := newHarness(t, run)
	sub := h.s.Subscribe(context.Background())

	// Tick until the first (asynchronous) GPU reading lands.
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("GPU reading never arrived")
		}
		h.clock.Advance(time.Second)
		h.tick(t)
		if s := recv(t, sub); s.GPU.Reason != "pending" {
			if s.GPU.Available || s.GPU.Reason != "nvidia-smi failed: Failed to initialize NVML: Driver/library version mismatch" {
				t.Fatalf("gpu %+v", s.GPU)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	n0 := calls.Load()
	if n0 != 1 {
		t.Fatalf("want exactly one spawn so far, got %d", n0)
	}

	for i := 0; i < 5; i++ { // five seconds of ticks: still inside the 15 s retry window
		h.clock.Advance(time.Second)
		h.tick(t)
		if s := recv(t, sub); s.GPU.Available || s.GPU.Reason == "" {
			t.Fatalf("last reason must repeat: %+v", s.GPU)
		}
	}
	if calls.Load() != n0 {
		t.Fatalf("respawned inside the retry window: %d", calls.Load())
	}

	h.clock.Advance(15 * time.Second)
	h.tick(t)
	recv(t, sub)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() != n0+1 {
		if time.Now().After(deadline) {
			t.Fatalf("no retry after the window: %d", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errNotFound = &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}

func TestSamplerNotFoundIsCachedForever(t *testing.T) {
	var n atomic.Int32
	notFound := func(context.Context, string, ...string) ([]byte, []byte, error) {
		n.Add(1)
		return nil, nil, errNotFound
	}
	h := newHarness(t, notFound)
	sub := h.s.Subscribe(context.Background())
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("GPU reading never arrived")
		}
		h.clock.Advance(time.Second)
		h.tick(t)
		if s := recv(t, sub); s.GPU.Reason == "nvidia-smi not found" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.clock.Advance(time.Hour)
	h.tick(t)
	recv(t, sub)
	h.tick(t)
	recv(t, sub)
	if n.Load() != 1 {
		t.Fatalf("a missing binary must be tried once, got %d", n.Load())
	}
}

func TestSamplerSlowGPUDoesNotStallTheRest(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, []byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil, ctx.Err()
	}
	h := newHarness(t, run)
	sub := h.s.Subscribe(context.Background())
	h.tick(t)
	s := recv(t, sub) // would hang for 2 s if the GPU call were inline
	if !s.CPU.Available || !s.Memory.Available || !s.Process.Available {
		t.Fatalf("%+v", s)
	}
	if s.GPU.Available || s.GPU.Reason != "pending" {
		t.Fatalf("gpu %+v", s.GPU)
	}
	h.clock.Advance(time.Second)
	h.tick(t) // the loop is not blocked behind the hung call
	recv(t, sub)
}

func TestCurrentColdThenCached(t *testing.T) {
	h := newHarness(t, runnerOut(twoGPUWithNA))
	ctx := context.Background()
	first := h.s.Current(ctx)
	if !first.GPU.Available || len(first.GPU.Devices) != 2 {
		t.Fatalf("a cold Current must wait for the first GPU reading: %+v", first.GPU)
	}
	if first.CPU.UsagePercent != nil {
		t.Fatal("cold sample has no baseline")
	}
	h.clock.Advance(time.Second)
	if again := h.s.Current(ctx); !again.Time.Equal(first.Time) {
		t.Fatal("a sample younger than 2 s must be reused")
	}
	h.clock.Advance(5 * time.Second)
	if fresh := h.s.Current(ctx); fresh.Time.Equal(first.Time) {
		t.Fatal("an older sample must be replaced")
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	for i := 0; i < eventBuffer+2; i++ { // never read
		h.clock.Advance(time.Second)
		h.tick(t)
	}
	h.tick(t) // barrier
	n := 0
	for range sub { // a dropped subscription is closed, so this ends
		n++
		if n > eventBuffer {
			t.Fatal("subscriber was not dropped")
		}
	}
}

func TestCloseEndsSubscribersAndIsIdempotent(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	h.s.Close()
	h.s.Close()
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("want closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the subscription")
	}
	if _, ok := <-h.s.Subscribe(context.Background()); ok {
		t.Fatal("Subscribe after Close must return a closed channel")
	}
}

func TestSubscribeEndsWithContext(t *testing.T) {
	h := newHarness(t, okRunner(""))
	ctx, cancel := context.WithCancel(context.Background())
	sub := h.s.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("want closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the subscription")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/hostmetrics/ -run 'Sampler|Current|Subscriber|Close|Subscribe' -count=1`
Expected: FAIL to compile (`New`, `Options`, `Sampler`, `eventBuffer` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/hostmetrics/sampler.go`:

```go
package hostmetrics

import (
	"context"
	"sync"
	"time"
)

const (
	defaultInterval   = time.Second
	defaultIdleAfter  = 10 * time.Second
	defaultGPURetry   = 15 * time.Second
	defaultGPUTimeout = 2 * time.Second
	defaultProcRoot   = "/proc"
	defaultCgroupRoot = "/sys/fs/cgroup"
	freshFor          = 2 * time.Second
	eventBuffer       = 8
	reasonPending     = "pending"
)

// Options configures a Sampler. Zero values select the defaults named below.
type Options struct {
	ProcRoot   string // default /proc
	CgroupRoot string // default /sys/fs/cgroup (cgroup v2)
	Run        Runner // default: exec nvidia-smi
	Interval   time.Duration
	// Ticks replaces the internal ticker. Tests use it with an unbuffered
	// channel so a completed send means the previous tick is fully processed.
	Ticks      <-chan time.Time
	Now        func() time.Time
	IdleAfter  time.Duration // default 10 s
	GPURetry   time.Duration // default 15 s
	GPUTimeout time.Duration // default 2 s
}

// base is the previous reading of one counter, used to turn it into a rate.
type base[T any] struct {
	ok bool
	at time.Time
	v  T
}

// prev returns the previous value if it is recent enough to average over.
func (b *base[T]) prev(now time.Time, maxAge time.Duration) (T, bool) {
	var zero T
	if !b.ok || !now.After(b.at) || now.Sub(b.at) > maxAge {
		return zero, false
	}
	return b.v, true
}

func (b *base[T]) set(now time.Time, v T) { b.ok, b.at, b.v = true, now, v }
func (b *base[T]) reset()                 { b.ok = false }

type gpuState struct {
	mu       sync.Mutex
	sec      GPU
	have     bool
	at       time.Time
	inflight bool
	missing  bool
	done     chan struct{}
}

type sub struct {
	ch   chan Sample
	gone chan struct{} // closed when the subscription is dropped
}

// Sampler samples while it is watched: an event-stream client is subscribed,
// or Current or Touch was called within IdleAfter. Otherwise it does nothing.
type Sampler struct {
	rd         procReader
	run        Runner
	now        func() time.Time
	interval   time.Duration
	idleAfter  time.Duration
	gpuRetry   time.Duration
	gpuTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu          sync.Mutex // latest, wantedUntil, subs, closed
	latest      *Sample
	wantedUntil time.Time
	subs        map[*sub]struct{}
	closed      bool

	cmu       sync.Mutex // the counter baselines
	cpuBase   base[cpuTimes]
	procBase  base[uint64]

	g gpuState
}

// New starts a Sampler. Close it when done.
func New(opt Options) *Sampler {
	s := &Sampler{
		rd:         procReader{proc: opt.ProcRoot, cgroup: opt.CgroupRoot},
		run:        opt.Run,
		now:        opt.Now,
		interval:   opt.Interval,
		idleAfter:  opt.IdleAfter,
		gpuRetry:   opt.GPURetry,
		gpuTimeout: opt.GPUTimeout,
		subs:       map[*sub]struct{}{},
	}
	if s.rd.proc == "" {
		s.rd.proc = defaultProcRoot
	}
	if s.rd.cgroup == "" {
		s.rd.cgroup = defaultCgroupRoot
	}
	if s.run == nil {
		s.run = execRunner
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.interval <= 0 {
		s.interval = defaultInterval
	}
	if s.idleAfter <= 0 {
		s.idleAfter = defaultIdleAfter
	}
	if s.gpuRetry <= 0 {
		s.gpuRetry = defaultGPURetry
	}
	if s.gpuTimeout <= 0 {
		s.gpuTimeout = defaultGPUTimeout
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())

	ticks := opt.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(s.interval)
		ticks = ticker.C
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticks:
				if s.watched() {
					s.publish(s.collect(s.ctx, false))
				}
			}
		}
	}()
	return s
}

func (s *Sampler) watched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs) > 0 || s.now().Before(s.wantedUntil)
}

// Touch marks the sampler wanted for IdleAfter.
func (s *Sampler) Touch() {
	s.mu.Lock()
	s.wantedUntil = s.now().Add(s.idleAfter)
	s.mu.Unlock()
}

// Latest returns the most recent sample, if any.
func (s *Sampler) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return Sample{}, false
	}
	return *s.latest, true
}

// Current marks the sampler wanted and returns a sample no older than 2 s,
// sampling synchronously if it has to. A cold call waits for the first GPU
// reading (at most the GPU timeout).
func (s *Sampler) Current(ctx context.Context) Sample {
	s.Touch()
	if l, ok := s.Latest(); ok && s.now().Sub(l.Time) < freshFor {
		return l
	}
	smp := s.collect(ctx, true)
	s.publish(smp)
	return smp
}

func (s *Sampler) collect(ctx context.Context, waitGPU bool) Sample {
	now := s.now()
	smp := Sample{Time: now.UTC()}
	maxAge := 3 * s.interval

	s.cmu.Lock()
	ct, cpu, cerr := s.rd.cpu()
	if cerr == nil {
		if p, ok := s.cpuBase.prev(now, maxAge); ok && ct.total > p.total && ct.busy >= p.busy {
			v := percent(ct.busy-p.busy, ct.total-p.total)
			cpu.UsagePercent = &v
		}
		s.cpuBase.set(now, ct)
	} else {
		s.cpuBase.reset()
	}
	smp.CPU = cpu

	smp.Memory, _ = s.rd.memory()

	ticks, proc, perr := s.rd.process()
	if perr == nil {
		if p, ok := s.procBase.prev(now, maxAge); ok && ticks >= p {
			secs := now.Sub(s.procBase.at).Seconds()
			v := round1(float64(ticks-p) / clockTick / secs * 100)
			proc.CPUPercent = &v
		}
		s.procBase.set(now, ticks)
	} else {
		s.procBase.reset()
	}
	smp.Process = proc
	s.cmu.Unlock()

	// The GPU step runs outside cmu so a slow nvidia-smi never holds up the
	// counters of another caller.
	smp.GPU = s.gpu(ctx, now, waitGPU)

	s.mu.Lock()
	s.latest = &smp
	s.mu.Unlock()
	return smp
}

// gpu returns the cached GPU reading, starting a refresh when one is due:
// every sample while the last reading worked, and after a failure no more
// often than GPURetry. A missing nvidia-smi is never retried. With wait set, a
// call that has no reading yet waits for the one in flight.
func (s *Sampler) gpu(ctx context.Context, now time.Time, wait bool) GPU {
	g := &s.g
	g.mu.Lock()
	if !g.inflight && !g.missing && (!g.have || g.sec.Available || now.Sub(g.at) >= s.gpuRetry) {
		s.mu.Lock()
		if !s.closed {
			s.wg.Add(1)
			g.inflight = true
			g.done = make(chan struct{})
			go s.runGPU(g.done)
		}
		s.mu.Unlock()
	}
	done, have := g.done, g.have
	g.mu.Unlock()

	if wait && !have && done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.have {
		return unavailableGPU(reasonPending)
	}
	return g.sec
}

func (s *Sampler) runGPU(done chan struct{}) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, s.gpuTimeout)
	defer cancel()
	sec, missing := queryGPU(ctx, s.run)
	g := &s.g
	g.mu.Lock()
	g.sec, g.have, g.at, g.inflight, g.missing = sec, true, s.now(), false, missing
	g.mu.Unlock()
	close(done)
}

// Subscribe returns a channel of samples that ends when ctx does, when the
// subscriber falls behind, or when the sampler closes. While at least one
// subscription is open the sampler samples every tick.
func (s *Sampler) Subscribe(ctx context.Context) <-chan Sample {
	c := &sub{ch: make(chan Sample, eventBuffer), gone: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(c.ch)
		return c.ch
	}
	s.subs[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.dropLocked(c)
			s.mu.Unlock()
		case <-c.gone:
		}
	}()
	return c.ch
}

// publish never blocks: a full subscriber is dropped and reconnects.
func (s *Sampler) publish(smp Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.subs {
		select {
		case c.ch <- smp:
		default:
			s.dropLocked(c)
		}
	}
}

func (s *Sampler) dropLocked(c *sub) {
	if _, ok := s.subs[c]; !ok {
		return
	}
	delete(s.subs, c)
	close(c.ch)
	close(c.gone)
}

// Close stops the sampler, ends every subscription and waits for its
// goroutines. It is safe to call twice.
func (s *Sampler) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		for c := range s.subs {
			s.dropLocked(c)
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}
```

Fix gofmt alignment of the struct fields (`cmu`/`cpuBase`/`procBase`) with `gofmt -w` after pasting.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/hostmetrics/ -race -count=5`
Expected: PASS, no race reports, no flake across 5 runs. Then `go vet ./internal/hostmetrics/` and `gofmt -l internal/hostmetrics/` clean.

If `TestSamplerIdlesWhenUnwatched` reports `calls` > 0 right after Touch it is a harness issue, not a bug: only the "unwatched" assertion reads `calls`.

- [ ] **Step 5: Commit**

```bash
git add internal/hostmetrics/sampler.go internal/hostmetrics/sampler_test.go
git commit -m "hostmetrics: watched sampler with GPU backoff and event bus"
```

---

### Task 4: Dashboard endpoint, event stream, binary wiring, README

**Files:**
- Create: `internal/dashboard/metrics.go`
- Modify: `internal/dashboard/dashboard.go` (Options near line 37, server struct near line 50, constructor near line 62, routes near line 72)
- Modify: `internal/dashboard/events.go` (the `qch` block and select)
- Test: `internal/dashboard/metrics_test.go`
- Modify: `cmd/camfarm/run.go` (near line 185 and the `dashboard.Options` at line 204)
- Modify: `README.md` (new section after "Stream quality test")

**Interfaces:**
- Consumes: `hostmetrics.New`, `Options`, `Sampler.Current/Subscribe/Close`, `Sample` from Task 3.
- Produces: `dashboard.Options.Metrics *hostmetrics.Sampler` (nil → 404); `GET /api/v1/metrics`; `event: metrics` on `/api/v1/events` carrying the JSON `Sample`.

- [ ] **Step 1: Write the failing tests**

Helpers that already exist in the `dashboard` package tests and are reused here: `do(t, method, url, body)` and `newServer(t, Options)` (`dashboard_test.go`), `sseFrames(t, baseURL)` and `nextFrame(t, ch, event)` (`quality_events_test.go`).

Create `internal/dashboard/metrics_test.go`:

```go
package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/hostmetrics"
)

const gpuRow = "0, NVIDIA GeForce RTX 4090, GPU-aaaa-1111, 37, 12, 1024, 24564, 5, 0, 54, 78.43, 450.00\n"

// procFixture writes a fake /proc that hostmetrics can read.
func procFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"proc/stat":       "cpu  100 0 0 900 0 0 0 0\ncpu0 100 0 0 900 0 0 0 0\n",
		"proc/meminfo":    "MemTotal: 65000000 kB\nMemAvailable: 40000000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"proc/loadavg":    "1.00 1.00 1.00 1/1 1\n",
		"proc/self/stat":  "1 (camfarm) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 1 1 1 0\n",
		"proc/self/statm": "10 10 0 0 0 0 0\n",
		"proc/self/fd/0":  "",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type metricsEnv struct {
	ts    *httptest.Server
	fleet *camfarm.Fleet
	s     *hostmetrics.Sampler
	ticks chan time.Time
}

func metricsServer(t *testing.T, root string, run hostmetrics.Runner) metricsEnv {
	t.Helper()
	fleet := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ticks := make(chan time.Time)
	s := hostmetrics.New(hostmetrics.Options{
		ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg"),
		Run: run, Ticks: ticks,
	})
	t.Cleanup(s.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Metrics: s}))
	t.Cleanup(ts.Close)
	return metricsEnv{ts: ts, fleet: fleet, s: s, ticks: ticks}
}

func gpuOK(context.Context, string, ...string) ([]byte, []byte, error) {
	return []byte(gpuRow), nil, nil
}

func decodeSample(t *testing.T, r io.Reader) hostmetrics.Sample {
	t.Helper()
	var s hostmetrics.Sample
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	return s
}

func TestMetricsRouteIs404WhenDisabled(t *testing.T) {
	ts, _ := newServer(t, Options{})
	if r := do(t, "GET", ts.URL+"/api/v1/metrics", ""); r.StatusCode != 404 {
		t.Fatalf("got %d, want 404", r.StatusCode)
	}
}

func TestMetricsGet(t *testing.T) {
	env := metricsServer(t, procFixture(t), gpuOK)
	resp := do(t, "GET", env.ts.URL+"/api/v1/metrics", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	first := decodeSample(t, resp.Body)
	if !first.CPU.Available || !first.Memory.Available || !first.Process.Available {
		t.Fatalf("%+v", first)
	}
	if !first.GPU.Available || len(first.GPU.Devices) != 1 || first.GPU.Devices[0].Name != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("gpu %+v", first.GPU)
	}
	again := decodeSample(t, do(t, "GET", env.ts.URL+"/api/v1/metrics", "").Body)
	if !again.Time.Equal(first.Time) {
		t.Fatal("a second GET inside two seconds must reuse the sample")
	}
}

func TestMetricsSectionFailureIsStill200(t *testing.T) {
	failing := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	env := metricsServer(t, t.TempDir(), failing) // empty root: no /proc files at all
	resp := do(t, "GET", env.ts.URL+"/api/v1/metrics", "")
	if resp.StatusCode != 200 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var s hostmetrics.Sample
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.CPU.Available || s.CPU.Reason == "" || s.GPU.Available || s.GPU.Reason == "" {
		t.Fatalf("%+v", s)
	}
	var generic map[string]map[string]any
	_ = json.Unmarshal(raw, &generic)
	if devs, ok := generic["gpu"]["devices"].([]any); !ok || len(devs) != 0 {
		t.Fatalf("gpu.devices must be [] not null: %v", generic["gpu"]["devices"])
	}
}

func TestMetricsEventArrivesOnTheEventStreamAndTouchesNothingElse(t *testing.T) {
	env := metricsServer(t, procFixture(t), gpuOK)
	journalBefore := len(env.fleet.Journal())
	frames := sseFrames(t, env.ts.URL)

	select {
	case env.ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not take the tick")
	}
	var s hostmetrics.Sample
	if err := json.Unmarshal([]byte(nextFrame(t, frames, "metrics")), &s); err != nil {
		t.Fatal(err)
	}
	if !s.CPU.Available || !s.Process.Available {
		t.Fatalf("%+v", s)
	}
	if got := len(env.fleet.Journal()); got != journalBefore {
		t.Fatalf("metrics must not add journal entries: %d -> %d", journalBefore, got)
	}
}

func TestMetricsStreamEndsWhenTheSamplerCloses(t *testing.T) {
	env := metricsServer(t, procFixture(t), gpuOK)
	frames := sseFrames(t, env.ts.URL)
	env.s.Close()
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("unexpected frame")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream did not end when the sampler closed")
	}
}
```

Create `internal/dashboard/metrics_real_test.go`:

```go
package dashboard

import (
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/hostmetrics"
)

// TestMetricsRealHost reads this machine's own /proc. It asserts only what is
// true on every Linux host. The GPU section may be available or carry a reason
// (a driver mismatch, no nvidia-smi); both are valid.
func TestMetricsRealHost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("CPU, memory and process readings are Linux only")
	}
	fleet := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	s := hostmetrics.New(hostmetrics.Options{})
	t.Cleanup(s.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Metrics: s}))
	t.Cleanup(ts.Close)

	got := decodeSample(t, do(t, "GET", ts.URL+"/api/v1/metrics", "").Body)
	if !got.CPU.Available || got.CPU.LogicalCPUs < 1 {
		t.Fatalf("cpu %+v", got.CPU)
	}
	if !got.Memory.Available || got.Memory.TotalBytes == 0 {
		t.Fatalf("memory %+v", got.Memory)
	}
	if !got.Process.Available || got.Process.Goroutines < 1 || got.Process.RSSBytes == 0 {
		t.Fatalf("process %+v", got.Process)
	}
	if !got.GPU.Available && got.GPU.Reason == "" {
		t.Fatalf("an unavailable GPU section needs a reason: %+v", got.GPU)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dashboard/ -run Metrics -count=1`
Expected: FAIL to compile (`Options.Metrics` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/dashboard/metrics.go`:

```go
package dashboard

import "net/http"

// getMetrics returns a host metrics sample no older than two seconds and marks
// the sampler wanted, so the event stream keeps flowing for the next ten.
// Read-only, so it needs no same-origin guard.
func (s *server) getMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		writeError(w, http.StatusNotFound, "host metrics are not enabled")
		return
	}
	writeJSON(w, http.StatusOK, s.metrics.Current(r.Context()))
}
```

Edit `internal/dashboard/dashboard.go`:
- import `"github.com/0x524a/camfarm/internal/hostmetrics"`.
- In `Options`, after `Quality`: `// Metrics, when non-nil, enables GET /api/v1/metrics and the "metrics" event. The caller closes it.` / `Metrics *hostmetrics.Sampler`.
- In `server`: `metrics *hostmetrics.Sampler`.
- In the constructor literal: `metrics: opt.Metrics`.
- Route, next to the quality routes: `mux.HandleFunc("GET /api/v1/metrics", s.getMetrics)`.

Edit `internal/dashboard/events.go`: after the `qch` block add

```go
	// mch is nil (and so never ready) when host metrics are disabled. Samples
	// come from the sampler's own bus and never touch the fleet's feed or journal.
	var mch <-chan hostmetrics.Sample
	if s.metrics != nil {
		mch = s.metrics.Subscribe(r.Context())
	}
```

import `hostmetrics`, update the doc comment to say "merged with `quality` and `metrics` events", and add this case to the `select`, after the `qch` case:

```go
		case ms, ok := <-mch:
			if !ok {
				return // dropped or closed: the client reconnects and re-lists
			}
			b, err := json.Marshal(ms)
			if err != nil {
				continue
			}
			if !send("event: metrics\ndata: %s\n\n", b) {
				return
			}
```

Edit `cmd/camfarm/run.go`: import `hostmetrics`; after `defer qr.Close()` add

```go
	// Samples only while someone is watching, so it costs nothing otherwise.
	hm := hostmetrics.New(hostmetrics.Options{})
	defer hm.Close()
```

and change the handler to `dashboard.Options{Library: lib, Quality: qr, Metrics: hm}`. Then run `grep -n "quality" cmd/camfarm/*_test.go` to find how the binary's tests exercise quality wiring and add the equivalent for `GET /api/v1/metrics` (200 and a JSON `time` field), using the same helper.

Edit `README.md`: add, after the "Stream quality test" section, a "Host metrics" section with: the endpoint and event; the JSON shape (the spec's example, with real field names); sections are independent and an unreadable one carries a `reason`; `null` means not reported (never 0); `gpuUtilPercent` is nvidia-smi's `utilization.gpu` (kernel-busy time, not SM occupancy); Linux only for CPU/memory/process (other OSes report `unsupported platform`), GPU needs `nvidia-smi` on the PATH; the sampler runs only while an event-stream client is connected or the endpoint was fetched in the last 10 s; the endpoint exposes host details and the dashboard has no authentication; metrics are not part of the replay hash; the GPU parser is verified against captured `nvidia-smi` output, not live hardware. Add one bullet to the README Status "Works" list in the same style as the stream quality bullet. State no coverage number.

- [ ] **Step 4: Run to verify pass**

Run: `go vet ./...`
Run: `gofmt -l .` (no output)
Run: `CGO_ENABLED=0 go build ./...`
Run: `go test -race ./internal/hostmetrics/ ./internal/dashboard/ ./cmd/... -count=1`
Expected: all PASS. Then run the real binary once by hand: start it with the repo's smoke command (see the README Quick start), `curl -s localhost:<port>/api/v1/metrics`, confirm `process.available` is true and the GPU section shows the real reason on this machine; stop it with SIGINT and confirm a prompt exit.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/metrics.go internal/dashboard/metrics_test.go internal/dashboard/metrics_real_test.go internal/dashboard/dashboard.go internal/dashboard/events.go cmd/camfarm/run.go README.md
git commit -m "dashboard: host metrics endpoint and event; wire the sampler into the binary"
```

(Add any `cmd/camfarm/*_test.go` file you changed to the `git add`.)
