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
	frames := sseFramesAt(t, env.ts.URL+"/api/v1/events?metrics=1")

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
	frames := sseFramesAt(t, env.ts.URL+"/api/v1/events?metrics=1")
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

func TestMetricsEventIsOptIn(t *testing.T) {
	env := metricsServer(t, procFixture(t), gpuOK)
	frames := sseFrames(t, env.ts.URL) // no metrics=1

	select {
	case env.ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("sampler loop did not take the tick")
	}
	select {
	case env.ticks <- time.Now(): // barrier: the first tick is fully processed
	case <-time.After(5 * time.Second):
		t.Fatal("sampler loop did not take the second tick")
	}
	if _, ok := env.s.Latest(); ok {
		t.Fatal("the sampler must stay idle when no client asked for metrics")
	}
	select {
	case f := <-frames:
		t.Fatalf("unexpected frame without metrics=1: %v", f)
	default:
	}
}
