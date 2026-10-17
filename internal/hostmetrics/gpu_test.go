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
		"0, a, b, 1, 2\n",                               // too few columns
		"x, a, b, 1, 2, 3, 4, 5, 6, 7, 8, 9\n",          // bad index
		"0, a, b, notanumber, 2, 3, 4, 5, 6, 7, 8, 9\n", // bad number
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
