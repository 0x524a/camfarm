package dashboard

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"sync"
	"time"

	"github.com/0x524a/camfarm"
)

const (
	defaultSnapshotTimeout = 10 * time.Second
	defaultMaxSnapshots    = 4
	jpegQuality            = 90
)

type flight struct {
	done chan struct{}
	jpeg []byte
	err  error
}

// snapshotter serves one JPEG per camera at a time: concurrent requests for the
// same camera share one capture, the capture is bounded by a timeout detached
// from any single requester, and captures across cameras are capped.
type snapshotter struct {
	timeout time.Duration
	sem     chan struct{}
	capture func(context.Context, *camfarm.Camera) (image.Image, error)

	mu      sync.Mutex
	flights map[string]*flight
}

func newSnapshotter(opt Options) *snapshotter {
	s := &snapshotter{
		timeout: opt.SnapshotTimeout,
		capture: opt.capture,
		flights: make(map[string]*flight),
	}
	if s.timeout <= 0 {
		s.timeout = defaultSnapshotTimeout
	}
	n := opt.MaxConcurrentSnapshots
	if n <= 0 {
		n = defaultMaxSnapshots
	}
	s.sem = make(chan struct{}, n)
	if s.capture == nil {
		s.capture = func(ctx context.Context, c *camfarm.Camera) (image.Image, error) { return c.Screenshot(ctx) }
	}
	return s
}

// get returns a JPEG of the camera, sharing an in-flight capture if one exists.
func (s *snapshotter) get(ctx context.Context, cam *camfarm.Camera) ([]byte, error) {
	s.mu.Lock()
	fl, joined := s.flights[cam.ID()]
	if !joined {
		fl = &flight{done: make(chan struct{})}
		s.flights[cam.ID()] = fl
		go s.run(cam, fl)
	}
	s.mu.Unlock()

	select {
	case <-fl.done:
		return fl.jpeg, fl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *snapshotter) run(cam *camfarm.Camera, fl *flight) {
	defer func() {
		s.mu.Lock()
		delete(s.flights, cam.ID())
		s.mu.Unlock()
		close(fl.done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		fl.err = ctx.Err()
		return
	}

	img, err := s.capture(ctx, cam)
	if err != nil {
		fl.err = err
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		fl.err = err
		return
	}
	fl.jpeg = buf.Bytes()
}
