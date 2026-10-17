package hostmetrics

import (
	"context"
	"testing"
)

func TestParseGPUBracketedErrorsAreNull(t *testing.T) {
	in := oneGPU +
		"1, Tesla T4, GPU-bbbb-2222, 5, 6, 100, 15360, 1, 2, [Insufficient Permissions], [Unknown Error], [Unknown Error]\n"
	g, _ := queryGPU(context.Background(), runnerOut(in))
	if !g.Available || len(g.Devices) != 2 {
		t.Fatalf("%+v", g)
	}
	d := g.Devices[1]
	if d.TemperatureC != nil || d.PowerWatts != nil || d.PowerLimitWatts != nil {
		t.Fatalf("bracketed must be nil: %+v", d)
	}
	if d.GPUUtilPercent == nil || *d.GPUUtilPercent != 5 || *d.MemTotalBytes != 15360<<20 {
		t.Fatalf("other fields lost: %+v", d)
	}
	if o := g.Devices[0]; o.TemperatureC == nil || *o.TemperatureC != 54 {
		t.Fatalf("other device damaged: %+v", o)
	}
}

func TestParseGPUNonFiniteAndHugeAreNull(t *testing.T) {
	in := "0, a, b, NaN, Inf, -Inf, 99999999999999999999, 1, 2, 3, 4, 5\n"
	devs, err := parseGPU([]byte(in))
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	d := devs[0]
	if d.GPUUtilPercent != nil || d.MemUtilPercent != nil || d.MemUsedBytes != nil || d.MemTotalBytes != nil {
		t.Fatalf("want nil: %+v", d)
	}
}

func TestParseGPUCRLFAndQuotedName(t *testing.T) {
	in := "0, \"Foo, Bar 9\", GPU-x, 1, 2, 3, 4, 5, 6, 7, 8, 9\r\n"
	devs, err := parseGPU([]byte(in))
	if err != nil || len(devs) != 1 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	if devs[0].Name != "Foo, Bar 9" || devs[0].PowerLimitWatts == nil || *devs[0].PowerLimitWatts != 9 {
		t.Fatalf("%+v", devs[0])
	}
}

func TestParseLoadavgRejectsNonFinite(t *testing.T) {
	for _, in := range []string{"NaN 1 1 1/1 1\n", "1 Inf 1 1/1 1\n", "1 1 -Inf 1/1 1\n"} {
		if _, _, _, err := parseLoadavg(in); err == nil {
			t.Errorf("want error for %q", in)
		}
	}
}

func TestParseCPUMaxRejectsDegenerate(t *testing.T) {
	for _, in := range []string{"0 100000\n", "-5 100000\n", "100000 0\n", "100000 -1\n", "NaN 100000\n", "Inf 100000\n", "1e308 1e-308\n"} {
		if got := parseCPUMax(in); got != nil {
			t.Errorf("parseCPUMax(%q)=%v want nil", in, *got)
		}
	}
}
