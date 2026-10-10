package screenshot

import (
	"context"
	"errors"
	"image"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/rtsp"
	"github.com/0x524a/camfarm/internal/seed"
)

// startRTSP serves src as camera "cam" over a real RTSP server and returns the
// camera's URL.
func startRTSP(t *testing.T, src media.Source) string {
	t.Helper()
	m, err := src.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s, err := rtsp.New(rtsp.Config{
		Host: "127.0.0.1", Obs: obs.New(1000),
		Cameras: []rtsp.CameraConfig{{ID: "cam", Media: m, Seed: seed.Seed(7)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s.URL("cam")
}

func TestCaptureH264(t *testing.T) {
	url := startRTSP(t, media.FixtureSource{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	img, err := Capture(ctx, url)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if b := img.Bounds(); b != image.Rect(0, 0, 320, 240) {
		t.Errorf("bounds = %v, want 320x240", b)
	}
}

func TestCaptureCancelled(t *testing.T) {
	url := startRTSP(t, media.FixtureSource{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, url); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCaptureDeadAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Capture(ctx, "rtsp://127.0.0.1:1/nope"); err == nil {
		t.Fatal("want an error dialing a closed port")
	}
}
