package camfarm

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/onvif"
	"github.com/0x524a/camfarm/internal/rtsp"
	"github.com/0x524a/camfarm/internal/screenshot"
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

// FaultCount is one fault kind's tally.
type FaultCount struct {
	Applied    uint64
	Suppressed uint64
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
	// FaultsFired counts fault decisions that have fired for this camera. For
	// packet_loss it counts frames that had loss, not packets; use FaultCounts
	// for packet counts.
	FaultsFired uint64
	// FaultCounts tallies each fault kind's effects: frames dropped, packets
	// or keyframes withheld (Applied) and decisions spared by
	// ProtectKeyframes (Suppressed). Never nil.
	FaultCounts map[string]FaultCount
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

	// ev is the change feed and runtime journal. Its own lock; the only lock
	// order is mu then ev.mu.
	ev events
}

// Camera is one camera in a running fleet.
type Camera struct {
	fleet  *Fleet
	id     string
	seed   seed.Seed
	source string
	media  *media.Media
	spec   CameraSpec

	mu     sync.Mutex
	faults []FaultSpec

	// removed is set by RemoveCamera. A handle kept past removal must not act
	// on a camera later re-added under the same id.
	removed atomic.Bool
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
	f.ev.init()

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
			spec:   cs,
			faults: append([]FaultSpec(nil), cs.Faults...),
		}
		f.order = append(f.order, cs.ID)

		cfg.Cameras = append(cfg.Cameras, rtsp.CameraConfig{
			ID:     cs.ID,
			Media:  m,
			Seed:   camSeed,
			Faults: toFaultSpecs(cs.Faults),
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
	return fmt.Sprintf("camfarm: seed=%#x spec=sha256:%s", f.spec.Seed, f.specHash[:12]) + f.journalSuffix()
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

	if err := f.srv.AddCamera(rtsp.CameraConfig{ID: cs.ID, Media: m, Seed: camSeed, Faults: toFaultSpecs(cs.Faults)}); err != nil {
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
		spec:   cs,
		faults: append([]FaultSpec(nil), cs.Faults...),
	}
	f.cameras[cs.ID] = cam
	f.order = append(f.order, cs.ID)

	f.publish(Event{Type: EventCameraAdded, Camera: cs.ID, FrameIndex: -1})
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
	cam, ok := f.cameras[id]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownCamera, id)
	}
	cam.removed.Store(true)
	delete(f.cameras, id)
	for i, camID := range f.order {
		if camID == id {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
	f.publish(Event{Type: EventCameraRemoved, Camera: id, FrameIndex: -1})

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
	f.closeSubscribers()
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
	fc := make(map[string]FaultCount, len(counters.ByKind))
	for k, v := range counters.ByKind {
		fc[k] = FaultCount{Applied: v.Applied, Suppressed: v.Suppressed}
	}

	return Stats{
		Seed:               uint64(c.seed),
		FramesServed:       counters.FramesServed,
		LoopsCompleted:     counters.LoopsCompleted,
		FaultsFired:        counters.FaultsFired,
		FaultCounts:        fc,
		OutboundRTPPackets: pkts,
		OutboundBytes:      bytes,
		Readers:            c.fleet.srv.Readers(c.id),
	}
}

// Screenshot returns one decoded frame from this camera's live RTSP stream.
//
// It dials the camera's own RTSP URL, so it exercises the real wire path, and
// blocks until the stream's next random access point (bounded by one loop of
// the source). It returns ErrNotReady if the fleet's RTSP server is not
// serving, and ctx's own error if ctx ends first.
func (c *Camera) Screenshot(ctx context.Context) (image.Image, error) {
	url := c.RTSPURL()
	if url == "" {
		return nil, ErrNotReady
	}
	return screenshot.Capture(ctx, url)
}

// Spec returns the camera's spec as it was added, with its currently active
// faults.
func (c *Camera) Spec() CameraSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.spec
	s.Faults = append([]FaultSpec(nil), c.faults...)
	return s
}

// Auth returns the camera's ONVIF Digest credentials, and false if it is open.
func (c *Camera) Auth() (user, pass string, ok bool) {
	a := c.spec.Auth
	return a.Username, a.Password, a.Username != "" && a.Password != ""
}

// Faults returns the camera's currently active faults.
func (c *Camera) Faults() []FaultSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]FaultSpec(nil), c.faults...)
}

// Inject adds a fault to a running camera, replacing any active fault of the
// same kind. The change takes effect at the camera's next frame boundary.
// Only implemented kinds are accepted; the rest return ErrUnsupported.
//
// Any inject or clear rebuilds all of the camera's fault engines from the
// camera seed, so every fault's random stream restarts at its first draw and
// keyframe_starvation's keyframe counter restarts too (the next keyframe counts
// as the first and is delivered), including faults the call did not touch. A replay must restart streams at each journaled inject or clear.
func (c *Camera) Inject(f FaultSpec) error {
	if err := validateFault(f); err != nil {
		return err
	}

	c.mu.Lock()
	next := make([]FaultSpec, 0, len(c.faults)+1)
	for _, ex := range c.faults {
		if ex.Kind != f.Kind {
			next = append(next, ex)
		}
	}
	next = append(next, f)
	err := c.swapFaults(next, func(i int, clock string) { c.noteFaultsApplied("inject", f, i, clock) })
	if err == nil {
		c.faults = next
	}
	c.mu.Unlock()
	return err
}

// Clear removes every fault from a camera. With no faults active this succeeds
// and does nothing: asking for the state that already holds is not an error.
func (c *Camera) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed.Load() {
		return fmt.Errorf("%w: %q", ErrUnknownCamera, c.id)
	}
	if len(c.faults) == 0 {
		return nil
	}
	err := c.swapFaults(nil, func(i int, clock string) { c.noteFaultsApplied("clear", FaultSpec{}, i, clock) })
	if err == nil {
		c.faults = nil
	}
	return err
}

// swapFaults hands a new fault set to the camera's pump. Caller holds c.mu.
func (c *Camera) swapFaults(next []FaultSpec, applied func(int, string)) error {
	if c.removed.Load() {
		return fmt.Errorf("%w: %q", ErrUnknownCamera, c.id)
	}
	if err := c.fleet.srv.SetFaults(c.id, toFaultSpecs(next), applied); err != nil {
		if !c.fleet.srv.Has(c.id) {
			return fmt.Errorf("%w: %q", ErrUnknownCamera, c.id)
		}
		return err
	}
	return nil
}

// noteFaultsApplied is called when a fault change takes effect, at a frame
// boundary (clock "frame", on the pump goroutine) or on the next request (clock
// "request", possibly synchronously from Inject). It publishes to the fleet's
// change feed and journal; it takes neither c.mu nor Fleet.mu, and Inject never
// waits on it.
func (c *Camera) noteFaultsApplied(action string, f FaultSpec, index int, clock string) {
	c.fleet.noteFault(action, c.id, f, index, clock)
}

// ONVIFUUID is the camera's ONVIF endpoint UUID. It is deterministic from the
// camera seed and is the same value the camera's ONVIF Device service returns
// from GetEndpointReference.
func (c *Camera) ONVIFUUID() string { return onvif.EndpointUUID(c.seed) }
