package camfarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/onvif"
	"github.com/0x524a/camfarm/internal/rtsp"
	"github.com/0x524a/camfarm/internal/seed"
)

// maxEvents bounds the inspection log. A fleet may run for hours; the recorder
// keeps the most recent events and reports how many it dropped.
const maxEvents = 10000

// TB is the part of *testing.T that StartT needs.
//
// An interface rather than *testing.T so that this library does not import
// testing, which would register test flags in every consumer's binary.
// *testing.T and *testing.B both satisfy it.
type TB interface {
	Cleanup(func())
	Fatalf(format string, args ...any)
	Helper()
}

// Addrs are the fleet's bound addresses, read back after binding.
type Addrs struct {
	RTSP *net.TCPAddr
}

// Status is a camera's current state.
type Status struct {
	ID      string
	RTSPURL string
	Source  string
	Codec   string
	Width   int
	Height  int
	FPS     float64
	Readers int
}

// Stats are a camera's counters, correlated with the seed that produced them.
type Stats struct {
	// Seed is this camera's derived seed, not the fleet root.
	Seed uint64
	// FramesServed counts access units served. It is monotonic across loops of
	// the source, so it doubles as the camera's position in the run.
	FramesServed uint64
	// LoopsCompleted counts completed passes over the source media.
	LoopsCompleted uint64
	// FaultsFired counts fault decisions that have fired for this camera.
	FaultsFired uint64
	// OutboundRTPPackets counts RTP packets sent on this camera's stream,
	// read from the RTSP stream's own counters.
	OutboundRTPPackets uint64
	// OutboundBytes counts bytes sent on this camera's stream, read from the
	// RTSP stream's own counters.
	OutboundBytes uint64
	// Readers is the number of sessions currently set up against this
	// camera. Unlike the other fields, it is a live count, not a running
	// total.
	Readers int
}

// Fleet is a running set of synthetic cameras.
type Fleet struct {
	spec     Spec
	specHash string
	log      *slog.Logger
	rec      *obs.Recorder
	srv      *rtsp.Server
	onvifSrv *onvif.Server

	mu      sync.Mutex
	closed  bool
	cameras map[string]*Camera
	order   []string
	// sources caches each distinct SourceSpec's parsed media for the fleet's
	// entire lifetime, so AddCamera can share a source the same way Start
	// already shares one across cameras present from the beginning. Never
	// evicted, even after the last camera using an entry is removed: the
	// bundled fixtures are small, and eviction has no proven need yet.
	sources map[SourceSpec]*media.Media
	// nextIndex is the next stable index handed to root.Camera for seed
	// derivation. It only ever increases, even across RemoveCamera calls, so a
	// dynamically added camera's seed is a pure function of call order.
	nextIndex int
	stopWatch func()
}

// Camera is one camera in a running fleet.
type Camera struct {
	fleet  *Fleet
	id     string
	seed   seed.Seed
	source string
	media  *media.Media
}

// Start brings up a fleet.
//
// Cancelling ctx closes the fleet.
func Start(ctx context.Context, spec Spec) (*Fleet, error) {
	valid, err := spec.validate()
	if err != nil {
		return nil, err
	}

	log := valid.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	f := &Fleet{
		spec:     valid,
		specHash: spec.Hash(),
		log:      log,
		rec:      obs.New(maxEvents),
		cameras:  make(map[string]*Camera, len(valid.Cameras)),
		sources:  make(map[SourceSpec]*media.Media),
	}

	// Parse each distinct source once and share it. This is the scale lever:
	// M cameras on one source cost one copy of the media plus M encoder states.
	root := seed.Seed(valid.Seed)

	cfg := rtsp.Config{
		Host: valid.Listen.Host,
		Port: valid.Listen.RTSPPort,
		Log:  log,
		Obs:  f.rec,
	}

	for i, cs := range valid.Cameras {
		m, ok := f.sources[cs.Source]
		if !ok {
			src, err := newSource(cs.Source)
			if err != nil {
				return nil, err
			}
			m, err = src.Load()
			if err != nil {
				return nil, fmt.Errorf("camfarm: camera %q: %w", cs.ID, err)
			}
			f.sources[cs.Source] = m
		}
		if err := checkAdvertised(cs, m); err != nil {
			return nil, err
		}

		// The index is the camera's position in the validated spec, which is a
		// slice. It must never come from map iteration, which Go randomises.
		camSeed := root.Camera(i)

		f.cameras[cs.ID] = &Camera{
			fleet:  f,
			id:     cs.ID,
			seed:   camSeed,
			source: sourceDescription(cs.Source),
			media:  m,
		}
		f.order = append(f.order, cs.ID)

		// Faults is deliberately left unset: spec.validate() above rejects any
		// camera with a non-empty Faults before this loop runs, so no camera
		// reaching here has any.
		cfg.Cameras = append(cfg.Cameras, rtsp.CameraConfig{
			ID:    cs.ID,
			Media: m,
			Seed:  camSeed,
		})
	}

	srv, err := rtsp.New(cfg)
	if err != nil {
		return nil, err
	}
	if err := srv.Start(); err != nil {
		return nil, err
	}
	f.srv = srv

	onvifCfg := onvif.Config{
		Host: valid.Listen.Host,
		Port: valid.Listen.ONVIFPort,
		Log:  log,
	}
	for _, cs := range valid.Cameras {
		cam := f.cameras[cs.ID]
		onvifCfg.Cameras = append(onvifCfg.Cameras, onvif.CameraConfig{
			ID:       cs.ID,
			Media:    cam.media,
			RTSPURL:  srv.URL(cs.ID),
			Username: cs.Auth.Username,
			Password: cs.Auth.Password,
			Seed:     cam.seed,
		})
	}

	onvifSrv, err := onvif.New(onvifCfg)
	if err != nil {
		srv.Close()
		return nil, err
	}
	if err := onvifSrv.Start(); err != nil {
		srv.Close()
		return nil, err
	}
	f.onvifSrv = onvifSrv

	if ctx != nil && ctx.Done() != nil {
		watchCtx, cancel := context.WithCancel(ctx)
		f.stopWatch = cancel
		go func() {
			<-watchCtx.Done()
			_ = f.Close()
		}()
	}

	f.nextIndex = len(valid.Cameras)

	log.Info("fleet started",
		"cameras", len(f.order),
		"rtsp", srv.Addr().String(),
		"seed", fmt.Sprintf("%#x", valid.Seed),
		"spec", f.specHash[:12])

	return f, nil
}

// StartT brings up a fleet for a test: ephemeral ports, silent by default, and
// closed automatically when the test finishes.
func StartT(t TB, spec Spec) *Fleet {
	t.Helper()
	f, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("camfarm: StartT: %v", err)
		return nil
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// newSource builds a media source from a spec.
func newSource(s SourceSpec) (media.Source, error) {
	switch s.Kind {
	case SourceFixture:
		return media.FixtureSource{}, nil
	case SourceFile:
		return &media.FileSource{Path: s.Path}, nil
	default:
		return nil, fmt.Errorf("camfarm: unknown source kind %q", s.Kind)
	}
}

func sourceDescription(s SourceSpec) string {
	src, err := newSource(s)
	if err != nil {
		return string(s.Kind)
	}
	return src.Describe()
}

// checkAdvertised refuses a VideoSpec asking for output the source cannot supply
// as-is.
//
// VideoSpec names the desired output (see its own documentation). This version
// passes a source through and cannot transform it, so a request equal to the
// source's own value is honoured and any other value is refused. Each refusal
// names the behaviour that will eventually satisfy it, because that behaviour
// arriving is a silent semantic change for a caller whose spec is refused today.
func checkAdvertised(cs CameraSpec, m *media.Media) error {
	if cs.Video.Codec != "" && media.Codec(cs.Video.Codec) != m.Codec {
		return fmt.Errorf("%w: camera %q requests codec %q but its source is %q; transcoding between codecs is not implemented in this version",
			ErrUnsupported, cs.ID, cs.Video.Codec, m.Codec)
	}
	if cs.Video.Width != 0 && cs.Video.Width != m.Width {
		return fmt.Errorf("%w: camera %q requests width %d but its source is %d wide; rescaling is not implemented in this version",
			ErrUnsupported, cs.ID, cs.Video.Width, m.Width)
	}
	if cs.Video.Height != 0 && cs.Video.Height != m.Height {
		return fmt.Errorf("%w: camera %q requests height %d but its source is %d high; rescaling is not implemented in this version",
			ErrUnsupported, cs.ID, cs.Video.Height, m.Height)
	}
	if cs.Video.FPS != 0 && cs.Video.FPS != m.FPS {
		return fmt.Errorf("%w: camera %q requests %v fps but its source is %v; frame-rate conversion is not implemented in this version",
			ErrUnsupported, cs.ID, cs.Video.FPS, m.FPS)
	}
	return nil
}

// Seed returns the fleet's root seed.
func (f *Fleet) Seed() uint64 { return f.spec.Seed }

// SpecHash returns the digest of the spec this fleet was built from.
func (f *Fleet) SpecHash() string { return f.specHash }

// ReplayLine returns the one line a failing test should print to make its
// failure reproducible.
func (f *Fleet) ReplayLine() string {
	return fmt.Sprintf("camfarm: seed=%#x spec=sha256:%s", f.spec.Seed, f.specHash[:12])
}

// Addr returns the fleet's bound addresses.
func (f *Fleet) Addr() Addrs {
	return Addrs{RTSP: f.srv.Addr()}
}

// AddCamera starts a new camera on a running fleet. It applies the same
// validation Start does, so a spec that would be refused at Start is refused
// here too.
//
// Its seed is derived from a monotonically increasing index that never
// resets, even across RemoveCamera calls, so a dynamically added camera's
// seed is a pure function of call order -- reproducible if that order is
// logged, per CLAUDE.md's "Determinism is the product."
func (f *Fleet) AddCamera(spec CameraSpec) (*Camera, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	seen := make(map[string]bool, len(f.cameras))
	for id := range f.cameras {
		seen[id] = true
	}
	cs, err := validateCameraSpec(spec, seen)
	if err != nil {
		return nil, err
	}

	m, ok := f.sources[cs.Source]
	if !ok {
		src, err := newSource(cs.Source)
		if err != nil {
			return nil, err
		}
		m, err = src.Load()
		if err != nil {
			return nil, fmt.Errorf("camfarm: camera %q: %w", cs.ID, err)
		}
		f.sources[cs.Source] = m
	}
	if err := checkAdvertised(cs, m); err != nil {
		return nil, err
	}

	root := seed.Seed(f.spec.Seed)
	camSeed := root.Camera(f.nextIndex)
	f.nextIndex++

	if err := f.srv.AddCamera(rtsp.CameraConfig{ID: cs.ID, Media: m, Seed: camSeed}); err != nil {
		return nil, err
	}

	if f.onvifSrv != nil {
		if err := f.onvifSrv.AddCamera(onvif.CameraConfig{
			ID:       cs.ID,
			Media:    m,
			RTSPURL:  f.srv.URL(cs.ID),
			Username: cs.Auth.Username,
			Password: cs.Auth.Password,
			Seed:     camSeed,
		}); err != nil {
			if rmErr := f.srv.RemoveCamera(cs.ID); rmErr != nil {
				f.log.Warn("rollback: could not remove camera from rtsp after onvif AddCamera failed",
					"camera", cs.ID, "err", rmErr)
			}
			return nil, err
		}
	}

	cam := &Camera{
		fleet:  f,
		id:     cs.ID,
		seed:   camSeed,
		source: sourceDescription(cs.Source),
		media:  m,
	}
	f.cameras[cs.ID] = cam
	f.order = append(f.order, cs.ID)

	f.log.Info("camera added", "camera", cs.ID, "seed", fmt.Sprintf("%#x", camSeed))
	return cam, nil
}

// RemoveCamera stops and removes a camera from a running fleet.
//
// It is best effort once id is known to exist: there is nothing a caller
// could sensibly do to retry a partial removal, so both the RTSP and ONVIF
// sides are always attempted regardless of whether the other failed.
func (f *Fleet) RemoveCamera(id string) error {
	f.mu.Lock()
	if _, ok := f.cameras[id]; !ok {
		f.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownCamera, id)
	}
	delete(f.cameras, id)
	for i, camID := range f.order {
		if camID == id {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	f.mu.Unlock()

	var errs []error
	if err := f.srv.RemoveCamera(id); err != nil {
		errs = append(errs, err)
	}
	if f.onvifSrv != nil {
		if err := f.onvifSrv.RemoveCamera(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	f.log.Info("camera removed", "camera", id)
	return nil
}

// Camera returns one camera.
func (f *Fleet) Camera(id string) (*Camera, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cam, ok := f.cameras[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCamera, id)
	}
	return cam, nil
}

// List returns every camera's status, in spec order.
func (f *Fleet) List() []Status {
	f.mu.Lock()
	order := make([]string, len(f.order))
	copy(order, f.order)
	cams := make([]*Camera, 0, len(order))
	for _, id := range order {
		cams = append(cams, f.cameras[id])
	}
	f.mu.Unlock()

	out := make([]Status, 0, len(cams))
	for _, cam := range cams {
		out = append(out, Status{
			ID:      cam.id,
			RTSPURL: f.srv.URL(cam.id),
			Source:  cam.source,
			Codec:   string(cam.media.Codec),
			Width:   cam.media.Width,
			Height:  cam.media.Height,
			FPS:     cam.media.FPS,
			Readers: f.srv.Readers(cam.id),
		})
	}
	return out
}

// Close stops the fleet. It is safe to call more than once.
func (f *Fleet) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	stop := f.stopWatch
	f.mu.Unlock()

	if stop != nil {
		stop()
	}
	f.srv.Close()
	if f.onvifSrv != nil {
		f.onvifSrv.Close()
	}
	f.log.Info("fleet closed")
	return nil
}

// ID returns the camera's operator-chosen identifier.
func (c *Camera) ID() string { return c.id }

// RTSPURL returns the camera's stream URL.
func (c *Camera) RTSPURL() string { return c.fleet.srv.URL(c.id) }

// ONVIFEndpoint returns the camera's ONVIF base URL, e.g.
// "http://127.0.0.1:PORT/onvif/front-door". Append "/device" or "/media" for
// the two services this version serves.
func (c *Camera) ONVIFEndpoint() string { return c.fleet.onvifSrv.Endpoint(c.id) }

// Stats returns the camera's counters.
func (c *Camera) Stats() Stats {
	counters := c.fleet.rec.Counters(c.id)
	pkts, bytes := c.fleet.srv.StreamStats(c.id)

	return Stats{
		Seed:               uint64(c.seed),
		FramesServed:       counters.FramesServed,
		LoopsCompleted:     counters.LoopsCompleted,
		FaultsFired:        counters.FaultsFired,
		OutboundRTPPackets: pkts,
		OutboundBytes:      bytes,
		Readers:            c.fleet.srv.Readers(c.id),
	}
}

// Inject adds a fault to a running camera.
//
// Not implemented in this version: the fault catalogue's effects land in the
// next slice. The refusal names the kind so a caller learns what was asked for
// rather than seeing a silent no-op.
func (c *Camera) Inject(f FaultSpec) error {
	return fmt.Errorf("%w: injecting fault %q into a running camera is not implemented in this version",
		ErrUnsupported, f.Kind)
}

// Clear removes every fault from a camera. With no faults configured this
// succeeds and does nothing: asking for the state that already holds is not an
// error.
func (c *Camera) Clear() error { return nil }
