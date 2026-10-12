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
