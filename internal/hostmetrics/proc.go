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
		if math.IsNaN(v[i]) || math.IsInf(v[i], 0) {
			return 0, 0, 0, errors.New("non-finite value in /proc/loadavg")
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
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return nil
	}
	v := q / p
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
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
