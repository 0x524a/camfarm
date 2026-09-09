package camfarm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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

// FaultSpec configures one fault.
type FaultSpec struct {
	// Kind names a catalogued fault.
	Kind string `json:"kind"`
	// Rate is the per-frame probability for rate-based faults, in [0,1].
	Rate float64 `json:"rate,omitempty"`
}

// ListenSpec describes what the fleet binds.
type ListenSpec struct {
	// Host defaults to 127.0.0.1, so a test fixture is not exposed to the
	// network by accident.
	Host string `json:"host,omitempty"`
	// RTSPPort of zero requests an ephemeral port, readable back via
	// Fleet.Addr.
	RTSPPort int `json:"rtspPort,omitempty"`
}

// CameraSpec describes one camera.
type CameraSpec struct {
	// ID is operator-chosen and appears in the camera's RTSP path.
	ID     string      `json:"id"`
	Source SourceSpec  `json:"source,omitempty"`
	Video  VideoSpec   `json:"video,omitempty"`
	Faults []FaultSpec `json:"faults,omitempty"`
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

	seen := make(map[string]bool, len(s.Cameras))
	cameras := make([]CameraSpec, len(s.Cameras))
	copy(cameras, s.Cameras)

	for i := range cameras {
		c := &cameras[i]
		if c.ID == "" {
			return s, fmt.Errorf("camfarm: camera %d has an empty ID", i)
		}
		if strings.ContainsAny(c.ID, "/?# ") {
			return s, fmt.Errorf("camfarm: camera ID %q contains a character reserved in a URL path", c.ID)
		}
		if seen[c.ID] {
			return s, fmt.Errorf("camfarm: duplicate camera ID %q", c.ID)
		}
		seen[c.ID] = true

		if c.Source.Kind == "" {
			c.Source.Kind = SourceFixture
		}
		switch c.Source.Kind {
		case SourceFixture:
			if c.Source.Path != "" {
				return s, fmt.Errorf("camfarm: camera %q uses the bundled fixture but also sets a path", c.ID)
			}
		case SourceFile:
			if c.Source.Path == "" {
				return s, fmt.Errorf("camfarm: camera %q has source kind %q but no path", c.ID, SourceFile)
			}
		default:
			return s, fmt.Errorf("camfarm: camera %q has unknown source kind %q", c.ID, c.Source.Kind)
		}

		// Only the string is checked here; comparing it against the loaded
		// source's actual codec needs the source, so checkAdvertised in
		// camfarm.go does that.
		if c.Video.Codec != "" {
			switch media.Codec(c.Video.Codec) {
			case media.CodecH264, media.CodecH265:
			default:
				return s, fmt.Errorf("%w: camera %q requests codec %q; this version implements %q and %q",
					ErrUnsupported, c.ID, c.Video.Codec, media.CodecH264, media.CodecH265)
			}
		}

		for _, f := range c.Faults {
			if !fault.Known(fault.Kind(f.Kind)) {
				return s, fmt.Errorf("camfarm: camera %q requests unknown fault kind %q", c.ID, f.Kind)
			}
			if f.Rate < 0 || f.Rate > 1 {
				return s, fmt.Errorf("camfarm: camera %q fault %q has rate %v outside [0,1]", c.ID, f.Kind, f.Rate)
			}
			// Catalogued, validated, and refused. Accepting it silently would
			// make a test that asked for a fault pass while nothing misbehaved,
			// which is worse than refusing.
			return s, fmt.Errorf("%w: fault %q is catalogued but its effect is not implemented in this version",
				ErrUnsupported, f.Kind)
		}
	}

	s.Cameras = cameras
	return s, nil
}
