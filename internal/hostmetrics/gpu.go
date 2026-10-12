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
