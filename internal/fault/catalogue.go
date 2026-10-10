package fault

import (
	"errors"
	"fmt"
	"math"
)

// ParamType is the type of a fault parameter, for validation and for the
// dashboard form generated from the catalogue.
type ParamType string

// The parameter types. An enum type arrives with the first kind that needs it.
const (
	ParamNumber  ParamType = "number"
	ParamInteger ParamType = "integer"
	ParamBoolean ParamType = "boolean"
)

// ErrBadParam reports a parameter that is missing, out of range, or does not
// apply to the kind.
var ErrBadParam = errors.New("fault: bad parameter")

// Param describes one parameter of a fault kind. Name is the JSON field name
// of the public FaultSpec.
type Param struct {
	Name     string    `json:"name"`
	Type     ParamType `json:"type"`
	Min      *float64  `json:"min,omitempty"`
	Max      *float64  `json:"max,omitempty"`
	Default  any       `json:"default,omitempty"`
	Required bool      `json:"required"`
	Help     string    `json:"help"`
}

// KindInfo describes one catalogued fault kind.
type KindInfo struct {
	Kind        Kind    `json:"kind"`
	Implemented bool    `json:"implemented"`
	Models      string  `json:"models"` // the real field bug this kind reproduces
	Clock       string  `json:"clock"`  // what its decisions are keyed on
	Params      []Param `json:"params"`
}

func f64(v float64) *float64 { return &v }

func rateParam() Param {
	return Param{Name: "rate", Type: ParamNumber, Min: f64(0), Max: f64(1), Default: 0.2,
		Help: "Probability in [0,1] that the fault fires for each unit of its clock."}
}

func protectParam() Param {
	return Param{Name: "protectKeyframes", Type: ParamBoolean, Default: false,
		Help: "Never apply the fault to a random-access (keyframe) unit. The draw is still consumed."}
}

// Catalogue returns every catalogued kind in a fixed order. It builds a fresh
// copy each call, so callers may keep or modify what they receive.
func Catalogue() []KindInfo {
	rate := rateParam()
	protect := protectParam()
	lossRate := rateParam()
	lossRate.Default = 0.05
	lossRate.Help = "Long-run fraction of packets withheld, in [0,1]."
	lossProtect := protectParam()
	lossProtect.Help = "Never withhold a packet of a random-access (keyframe) frame. Draws are still consumed."
	return []KindInfo{
		{
			Kind: KindFrameDrop, Implemented: true,
			Models: "Encoder skipping frames: a frame is never encoded, so RTP sequence numbers stay contiguous and the loss shows only as a timestamp jump.",
			Clock:  "frame counter; one draw per frame",
			Params: []Param{rate, protect},
		},
		{
			Kind: KindPacketLoss, Implemented: true,
			Models: "Lossy path: packets of an encoded frame are withheld, leaving real sequence-number gaps with honest timestamps.",
			Clock:  "packet index within the encoded frames; one draw per packet",
			Params: []Param{
				lossRate,
				{Name: "meanBurst", Type: ParamNumber, Min: f64(1), Max: f64(1000), Default: 0.0,
					Help: "Omit or 0 for independent loss. Above 1, loss comes in bursts of this mean length in packets (rate must then be below meanBurst/(1+meanBurst))."},
				lossProtect,
			},
		},
		{
			Kind: KindKeyframeStarvation, Implemented: true,
			Models: "Long-GOP camera: only every Nth keyframe is sent, so a client joining mid-stream cannot start decoding.",
			Clock:  "random-access access-unit counter; no randomness",
			Params: []Param{
				{Name: "keepEvery", Type: ParamInteger, Min: f64(2), Max: f64(1000), Default: 3.0, Required: true,
					Help: "Deliver every Nth keyframe; the first is always delivered, the rest are never encoded."},
			},
		},
		{Kind: KindTeardownMidSession, Models: "Camera that drops live sessions; exercises reconnect logic and resource leaks.", Params: []Param{}},
		{Kind: KindAcceptThenSilence, Models: "Camera that accepts the TCP connection and never answers.", Params: []Param{}},
		{Kind: KindSDPCodecLie, Models: "SDP that advertises one codec while the stream carries another.", Params: []Param{}},
		{Kind: KindSPSResolutionLie, Models: "SDP and SPS that advertise a geometry the frames do not match.", Params: []Param{}},
	}
}

// Info returns one kind's catalogue entry.
func Info(kind Kind) (KindInfo, bool) {
	for _, ki := range Catalogue() {
		if ki.Kind == kind {
			return ki, true
		}
	}
	return KindInfo{}, false
}

// paramNames fixes the order parameters are checked in, so the first error
// reported is reproducible.
var paramNames = []string{"rate", "protectKeyframes", "meanBurst", "keepEvery"}

// param reads a Spec field by its public name. A parameter counts as set when
// non-zero, which matches the omitempty behaviour of the JSON form.
func (s Spec) param(name string) (v float64, set bool) {
	switch name {
	case "rate":
		return s.Rate, s.Rate != 0
	case "protectKeyframes":
		return 1, s.ProtectKeyframes
	case "meanBurst":
		return s.MeanBurst, s.MeanBurst != 0
	case "keepEvery":
		return float64(s.KeepEvery), s.KeepEvery != 0
	}
	return 0, false
}

// Validate checks a spec against the catalogue. Unimplemented kinds are not
// parameter-checked: they are refused as unsupported before parameters matter.
func Validate(s Spec) error {
	info, ok := Info(s.Kind)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKind, s.Kind)
	}
	if !info.Implemented {
		return nil
	}
	declared := map[string]Param{}
	for _, p := range info.Params {
		declared[p.Name] = p
	}
	for _, name := range paramNames {
		v, set := s.param(name)
		p, applies := declared[name]
		switch {
		case !applies && set:
			return fmt.Errorf("%w: %q does not apply to %q", ErrBadParam, name, s.Kind)
		case !applies:
			continue
		case !set && p.Required:
			return fmt.Errorf("%w: %q requires %q", ErrBadParam, s.Kind, name)
		case !set:
			continue
		}
		if p.Type == ParamInteger && v != math.Trunc(v) {
			return fmt.Errorf("%w: %q must be a whole number, got %v", ErrBadParam, name, v)
		}
		// Written so that NaN fails both comparisons.
		if (p.Min != nil && !(v >= *p.Min)) || (p.Max != nil && !(v <= *p.Max)) {
			if name == "rate" {
				return fmt.Errorf("%w: %v for %q", ErrBadRate, v, s.Kind)
			}
			return fmt.Errorf("%w: %q = %v for %q is outside [%v, %v]", ErrBadParam, name, v, s.Kind, *p.Min, *p.Max)
		}
	}
	if s.Kind == KindPacketLoss && s.MeanBurst != 0 {
		if s.MeanBurst <= 1 {
			return fmt.Errorf("%w: meanBurst must be greater than 1 (omit it for independent loss)", ErrBadParam)
		}
		if s.Rate >= 1 {
			return fmt.Errorf("%w: burst loss needs rate below 1", ErrBadParam)
		}
		if limit := s.MeanBurst / (1 + s.MeanBurst); s.Rate > limit {
			return fmt.Errorf("%w: with meanBurst %v the highest reachable rate is %v, got %v", ErrBadParam, s.MeanBurst, limit, s.Rate)
		}
	}
	return nil
}
