package camfarm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
)

// SourceKind selects where a camera's video comes from.
type SourceKind string

// Source kinds.
const (
	// SourceFixture uses the media bundled with camfarm. It is the default.
	SourceFixture SourceKind = "fixture"
	// SourceFile reads an MPEG-TS file from disk.
	SourceFile SourceKind = "file"
)

// SourceSpec describes a camera's media source.
type SourceSpec struct {
	// Kind defaults to SourceFixture.
	Kind SourceKind `json:"kind,omitempty"`
	// Path is required when Kind is SourceFile.
	Path string `json:"path,omitempty"`
}

// VideoSpec describes the video a camera should produce.
//
// Zero fields mean "whatever the source provides". A non-zero field is a request
// for that output value, which this version can satisfy only when it already
// matches the source, because it can pass a source through but not transform it.
// Any other value is refused with ErrUnsupported.
//
// Note for callers pinning this API: these fields name the *desired output*, so
// a spec that is refused today will be satisfied by rescaling or transcoding in a
// later version rather than continuing to be refused. That is a deliberate
// trade, taken for a smaller API surface than a separate output block would give;
// the refusal messages name the behaviour that will eventually replace them so
// the change is legible before it happens.
type VideoSpec struct {
	Codec  string  `json:"codec,omitempty"`
	Width  int     `json:"width,omitempty"`
	Height int     `json:"height,omitempty"`
	FPS    float64 `json:"fps,omitempty"`
}

// AuthSpec configures a camera's ONVIF HTTP Digest challenge.
//
// The zero value means open — consistent with the pattern VideoSpec already
// establishes ("zero fields mean whatever"), and consistent with RTSP itself
// having no auth today. A camera only challenges when both fields are
// non-empty.
type AuthSpec struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// FaultSpec configures one fault.
type FaultSpec struct {
	// Kind names a catalogued fault.
	Kind string `json:"kind"`
	// Rate is the probability for rate-based faults, in [0,1].
	Rate float64 `json:"rate,omitempty"`
	// ProtectKeyframes never applies frame_drop or packet_loss to a
	// random-access frame.
	ProtectKeyframes bool `json:"protectKeyframes,omitempty"`
	// MeanBurst, for packet_loss, selects burst loss when greater than 1.
	MeanBurst float64 `json:"meanBurst,omitempty"`
	// KeepEvery, for keyframe_starvation, delivers every Nth keyframe.
	KeepEvery int `json:"keepEvery,omitempty"`
	// On, for rtsp_error, names the failing method: describe, setup, play or all.
	On string `json:"on,omitempty"`
	// Status, for rtsp_error, is the RTSP status code of the reply.
	Status int `json:"status,omitempty"`
	// OnlyRequest, for rtsp_error and sdp_lie, fires on the Nth matching request only.
	OnlyRequest int `json:"onlyRequest,omitempty"`
	// AfterFrames, for teardown, is the frames delivered before teardown.
	AfterFrames int `json:"afterFrames,omitempty"`
	// Mode selects a variant of teardown (close, reset) or sdp_lie (codec,
	// sprop, resolution, contentBase).
	Mode string `json:"mode,omitempty"`
	// MaxMs, for jitter, bounds each frame's random delay in milliseconds.
	MaxMs int `json:"maxMs,omitempty"`
	// HoldFrames, for burst_delivery, is the frames held before a burst.
	HoldFrames int `json:"holdFrames,omitempty"`
	// ReorderDepth, for pts_dts_mismatch, is how far timestamps run out of order.
	ReorderDepth int `json:"reorderDepth,omitempty"`
	// DelayMs, for pts_delayed, is a constant timestamp lag in milliseconds.
	DelayMs int `json:"delayMs,omitempty"`
	// DriftPpm, for pts_delayed, is a timestamp drift in parts per million.
	DriftPpm int `json:"driftPpm,omitempty"`
}

// ListenSpec describes what the fleet binds.
type ListenSpec struct {
	// Host defaults to 127.0.0.1, so a test fixture is not exposed to the
	// network by accident.
	Host string `json:"host,omitempty"`
	// RTSPPort of zero requests an ephemeral port, readable back via
	// Fleet.Addr.
	RTSPPort int `json:"rtspPort,omitempty"`
	// ONVIFPort of zero requests an ephemeral port for the ONVIF HTTP
	// listener, readable back via Camera.ONVIFEndpoint.
	ONVIFPort int `json:"onvifPort,omitempty"`
	// Discovery, when set, makes the fleet answer WS-Discovery probes. Nil
	// (the default) leaves discovery off and the spec's hash unchanged.
	// Enabling it changes Spec.Hash() and therefore the replay line, so a run
	// with discovery on does not share a replay line with the same fleet
	// without it.
	Discovery *DiscoverySpec `json:"discovery,omitempty"`
}

// Discovery modes.
const (
	// DiscoveryMulticast joins the WS-Discovery multicast group.
	DiscoveryMulticast = "multicast"
	// DiscoveryUnicast binds a unicast UDP socket, for tests and CI where
	// multicast is unavailable.
	DiscoveryUnicast = "unicast"
)

// DiscoverySpec configures the WS-Discovery responder.
type DiscoverySpec struct {
	// Mode is "multicast" or "unicast".
	Mode string `json:"mode"`
	// Interface selects the interface (name or IP) that joins the multicast
	// group. Multicast mode only.
	Interface string `json:"interface,omitempty"`
	// Addr is the UDP address to bind in unicast mode, e.g. 127.0.0.1:0.
	// Required for unicast; forbidden for multicast.
	Addr string `json:"addr,omitempty"`
	// AnnounceAddr is where Hello and Bye go. Unicast mode only; empty
	// disables announcements.
	AnnounceAddr string `json:"announceAddr,omitempty"`
}

// validateDiscovery checks the discovery block against the (defaulted) host.
func validateDiscovery(d *DiscoverySpec, host string) error {
	if d == nil {
		return nil
	}
	switch d.Mode {
	case DiscoveryMulticast:
		if d.Addr != "" {
			return fmt.Errorf("%w: listen.discovery.addr is for unicast mode; multicast joins the WS-Discovery group", ErrInvalidSpec)
		}
		if d.AnnounceAddr != "" {
			return fmt.Errorf("%w: listen.discovery.announceAddr applies to unicast mode only", ErrInvalidSpec)
		}
	case DiscoveryUnicast:
		if d.Addr == "" {
			return fmt.Errorf("%w: listen.discovery.addr is required for unicast mode", ErrInvalidSpec)
		}
		if d.Interface != "" {
			return fmt.Errorf("%w: listen.discovery.interface applies to multicast mode only", ErrInvalidSpec)
		}
		if err := validateUDPAddr("listen.discovery.addr", d.Addr, false); err != nil {
			return err
		}
		if d.AnnounceAddr != "" {
			if err := validateUDPAddr("listen.discovery.announceAddr", d.AnnounceAddr, true); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: listen.discovery.mode %q must be %q or %q", ErrInvalidSpec, d.Mode, DiscoveryMulticast, DiscoveryUnicast)
	}

	if _, _, err := net.SplitHostPort(host); err == nil {
		return fmt.Errorf("%w: listen.host %q must be a bare host without a port; ports are set by listen.rtspPort and listen.onvifPort", ErrInvalidSpec, host)
	}
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	ip := net.ParseIP(h)
	if h == "" || (ip != nil && ip.IsUnspecified()) {
		return fmt.Errorf("%w: listen.host %q is a wildcard address; discovery advertises it in the device XAddr, so clients need a reachable address", ErrInvalidSpec, host)
	}
	if d.Mode == DiscoveryMulticast && (strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())) {
		return fmt.Errorf("%w: listen.host %q is loopback; multicast discovery would advertise an address no other host can reach, set listen.host to a reachable address", ErrInvalidSpec, host)
	}
	return nil
}

// validateUDPAddr checks a "host:port" field. The port must be numeric and in
// 0-65535; the host may be empty (bind all interfaces) unless needHost is set.
func validateUDPAddr(field, addr string, needHost bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%w: %s %q must be host:port: %v", ErrInvalidSpec, field, addr, err)
	}
	if needHost && host == "" {
		return fmt.Errorf("%w: %s %q needs a host", ErrInvalidSpec, field, addr)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%w: %s %q has a non-numeric or out-of-range port %q (want 0-65535)", ErrInvalidSpec, field, addr, port)
	}
	return nil
}

// CameraSpec describes one camera.
type CameraSpec struct {
	// ID is operator-chosen and appears in the camera's RTSP path.
	ID     string      `json:"id"`
	Source SourceSpec  `json:"source,omitempty"`
	Video  VideoSpec   `json:"video,omitempty"`
	Faults []FaultSpec `json:"faults,omitempty"`
	Auth   AuthSpec    `json:"auth,omitempty"`
}

// Spec describes a fleet.
type Spec struct {
	// Seed roots every random decision the fleet makes.
	Seed    uint64       `json:"seed"`
	Cameras []CameraSpec `json:"cameras"`
	Listen  ListenSpec   `json:"listen,omitempty"`

	// Log receives fleet events. Nil means silent, which is the default: a
	// consumer may run camfarm inside a process whose stdout carries a protocol
	// stream, so nothing is ever written there.
	Log *slog.Logger `json:"-"`
}

// Hash returns a stable digest of the fleet's identity.
//
// The logger is excluded: it changes what a run prints, not which fleet it is.
// A replay reports this alongside the seed so that reproducing a failure
// verifies it is the same fleet rather than assuming it.
func (s Spec) Hash() string {
	// Marshal a copy with Log already excluded by its json tag.
	b, err := json.Marshal(s)
	if err != nil {
		// Spec contains no unmarshalable types, so this is unreachable; hashing
		// the error keeps the function total rather than panicking in a caller's
		// test output.
		b = []byte("camfarm: unhashable spec: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// validate checks the spec and fills defaults on a copy.
func (s Spec) validate() (Spec, error) {
	if len(s.Cameras) == 0 {
		return s, fmt.Errorf("camfarm: spec has no cameras")
	}
	if s.Listen.Host == "" {
		s.Listen.Host = "127.0.0.1"
	}
	if err := validateDiscovery(s.Listen.Discovery, s.Listen.Host); err != nil {
		return s, err
	}

	seen := make(map[string]bool, len(s.Cameras))
	cameras := make([]CameraSpec, len(s.Cameras))
	copy(cameras, s.Cameras)

	for i := range cameras {
		c, err := validateCameraSpec(cameras[i], seen)
		if err != nil {
			return s, err
		}
		cameras[i] = c
	}

	s.Cameras = cameras
	return s, nil
}

// validateCameraSpec checks one camera's spec and fills defaults on a copy,
// marking its ID as seen. Shared by Spec.validate (which builds seen fresh
// for a whole spec) and Fleet.AddCamera (which seeds it from the fleet's
// current camera IDs), so the two paths cannot enforce different rules for
// what a valid CameraSpec is.
func validateCameraSpec(c CameraSpec, seen map[string]bool) (CameraSpec, error) {
	if c.ID == "" {
		return c, fmt.Errorf("%w: camera has an empty ID", ErrInvalidSpec)
	}
	if strings.ContainsAny(c.ID, "/?# ") {
		return c, fmt.Errorf("%w: camera ID %q contains a character reserved in a URL path", ErrInvalidSpec, c.ID)
	}
	if seen[c.ID] {
		return c, fmt.Errorf("%w: %q", ErrDuplicateCamera, c.ID)
	}
	seen[c.ID] = true

	if c.Source.Kind == "" {
		c.Source.Kind = SourceFixture
	}
	switch c.Source.Kind {
	case SourceFixture:
		if c.Source.Path != "" {
			return c, fmt.Errorf("%w: camera %q uses the bundled fixture but also sets a path", ErrInvalidSpec, c.ID)
		}
	case SourceFile:
		if c.Source.Path == "" {
			return c, fmt.Errorf("%w: camera %q has source kind %q but no path", ErrInvalidSpec, c.ID, SourceFile)
		}
	default:
		return c, fmt.Errorf("%w: camera %q has unknown source kind %q", ErrInvalidSpec, c.ID, c.Source.Kind)
	}

	// Only the string is checked here; comparing it against the loaded
	// source's actual codec needs the source, so checkAdvertised in
	// camfarm.go does that.
	if c.Video.Codec != "" {
		switch media.Codec(c.Video.Codec) {
		case media.CodecH264, media.CodecH265:
		default:
			return c, fmt.Errorf("%w: camera %q requests codec %q; this version implements %q and %q",
				ErrUnsupported, c.ID, c.Video.Codec, media.CodecH264, media.CodecH265)
		}
	}

	kinds := map[string]bool{}
	for _, f := range c.Faults {
		if err := validateFault(f); err != nil {
			return c, fmt.Errorf("camera %q: %w", c.ID, err)
		}
		if kinds[f.Kind] {
			return c, fmt.Errorf("%w: camera %q repeats fault kind %q; inject replaces per kind, so a spec may name each once", ErrInvalidSpec, c.ID, f.Kind)
		}
		kinds[f.Kind] = true
	}

	if (c.Auth.Username == "") != (c.Auth.Password == "") {
		return c, fmt.Errorf("%w: camera %q sets one of auth username/password but not both", ErrInvalidSpec, c.ID)
	}

	return c, nil
}

// toFaultSpecs converts the public fault specs to the engine's.
func toFaultSpecs(in []FaultSpec) []fault.Spec {
	out := make([]fault.Spec, 0, len(in))
	for _, f := range in {
		out = append(out, toFaultSpec(f))
	}
	return out
}

func toFaultSpec(f FaultSpec) fault.Spec {
	return fault.Spec{Kind: fault.Kind(f.Kind), Rate: f.Rate, ProtectKeyframes: f.ProtectKeyframes,
		MeanBurst: f.MeanBurst, KeepEvery: f.KeepEvery,
		On: f.On, Status: f.Status, OnlyRequest: f.OnlyRequest, AfterFrames: f.AfterFrames, Mode: f.Mode,
		MaxMs: f.MaxMs, HoldFrames: f.HoldFrames, ReorderDepth: f.ReorderDepth, DelayMs: f.DelayMs, DriftPpm: f.DriftPpm}
}

// validateFault is the one place a fault is checked, shared by spec
// validation, AddCamera and Camera.Inject so they cannot disagree.
func validateFault(f FaultSpec) error {
	k := fault.Kind(f.Kind)
	if !fault.Known(k) {
		return fmt.Errorf("%w: unknown fault kind %q", ErrInvalidSpec, f.Kind)
	}
	// Written so that NaN is refused too.
	if !(f.Rate >= 0 && f.Rate <= 1) {
		return fmt.Errorf("%w: fault %q has rate %v outside [0,1]", ErrInvalidSpec, f.Kind, f.Rate)
	}
	if !fault.Implemented(k) {
		// Catalogued, validated, and refused. Accepting it silently would
		// make a test that asked for a fault pass while nothing misbehaved.
		return fmt.Errorf("%w: fault %q is catalogued but its effect is not implemented in this version", ErrUnsupported, f.Kind)
	}
	if err := fault.Validate(toFaultSpec(f)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	return nil
}
