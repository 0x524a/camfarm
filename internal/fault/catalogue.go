package fault

import (
	"errors"
	"fmt"
	"math"
)

// ParamType is the type of a fault parameter, for validation and for the
// dashboard form generated from the catalogue.
type ParamType string

// The parameter types. An enum parameter holds one string from Param.Options.
const (
	ParamNumber  ParamType = "number"
	ParamInteger ParamType = "integer"
	ParamBoolean ParamType = "boolean"
	ParamEnum    ParamType = "enum"
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
	Options  []string  `json:"options,omitempty"` // allowed values of an enum parameter
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

// onceRate is a rate parameter that defaults to 1: the fault fires every time
// unless told otherwise.
func onceRate(help string) Param {
	p := rateParam()
	p.Default = 1.0
	p.Help = help
	return p
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
				{Name: "meanBurst", Type: ParamNumber, Min: f64(1), Max: f64(1000),
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
		{
			Kind: KindRTSPError, Implemented: true, Models: "Flaky camera that answers 5xx or 454 to some DESCRIBE, SETUP or PLAY requests.",
			Clock: "per-method request counter; one draw per matching request",
			Params: []Param{
				{Name: "on", Type: ParamEnum, Options: []string{"describe", "setup", "play", "all"}, Default: "describe", Required: true,
					Help: "Which RTSP method the fault answers with an error; all covers every method."},
				{Name: "status", Type: ParamInteger, Min: f64(400), Max: f64(599), Default: 503.0,
					Help: "RTSP status code of the error reply."},
				rate,
				{Name: "onlyRequest", Type: ParamInteger, Min: f64(1), Max: f64(1000000), Default: 0.0,
					Help: "Fail only the Nth matching request (ignores rate); with on all, requests of every method share one count. 0 or omitted means unset."},
			},
		},
		{
			Kind: KindTeardown, Implemented: true, Models: "Camera that drops live sessions; TCP reset when mode is reset.",
			Clock: "frames since the reader joined; one draw per PLAY",
			Params: []Param{
				{Name: "afterFrames", Type: ParamInteger, Min: f64(1), Max: f64(100000), Default: 30.0, Required: true,
					Help: "Frames delivered to a reader before its session is torn down."},
				{Name: "mode", Type: ParamEnum, Options: []string{"close", "reset"}, Default: "close",
					Help: "close ends the session cleanly; reset aborts the TCP connection."},
				onceRate("Probability in [0,1] that a PLAY is torn down."),
			},
		},
		{
			Kind: KindSDPLie, Implemented: true, Models: "SDP that misdescribes the stream: wrong codec, wrong parameter sets, wrong geometry or wrong Content-Base.",
			Clock: "per-camera DESCRIBE counter; one draw per DESCRIBE",
			Params: []Param{
				{Name: "mode", Type: ParamEnum, Options: []string{"codec", "sprop", "resolution", "contentBase"}, Default: "codec", Required: true,
					Help: "Which part of the SDP lies. codec changes only the a=rtpmap codec name (the honest a=fmtp stays); sprop and resolution edit the parameter sets; contentBase swaps the Content-Base host."},
				onceRate("Probability in [0,1] that a DESCRIBE gets the lying SDP."),
				{Name: "onlyRequest", Type: ParamInteger, Min: f64(1), Max: f64(1000000), Default: 0.0,
					Help: "Lie only on the Nth DESCRIBE (ignores rate). 0 or omitted means unset."},
			},
		},
		{Kind: KindAcceptThenSilence, Models: "Camera that accepts the TCP connection and never answers.", Params: []Param{}},
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
var paramNames = []string{"rate", "protectKeyframes", "meanBurst", "keepEvery", "on", "status", "onlyRequest", "afterFrames", "mode"}

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
	case "status":
		return float64(s.Status), s.Status != 0
	case "onlyRequest":
		return float64(s.OnlyRequest), s.OnlyRequest != 0
	case "afterFrames":
		return float64(s.AfterFrames), s.AfterFrames != 0
	}
	return 0, false
}

// enumParam reads an enum-valued Spec field by its public name. isEnum reports
// whether name is an enum field at all; set whether it is non-empty.
func (s Spec) enumParam(name string) (v string, set, isEnum bool) {
	switch name {
	case "on":
		return s.On, s.On != "", true
	case "mode":
		return s.Mode, s.Mode != "", true
	}
	return "", false, false
}

// checkEnum reports whether v is one of p's options.
func checkEnum(p Param, v string) error {
	for _, o := range p.Options {
		if o == v {
			return nil
		}
	}
	return fmt.Errorf("%w: %q must be one of %v, got %q", ErrBadParam, p.Name, p.Options, v)
}

// boundsText formats only the bounds a parameter declares.
func boundsText(p Param) string {
	switch {
	case p.Min != nil && p.Max != nil:
		return fmt.Sprintf("[%v, %v]", *p.Min, *p.Max)
	case p.Min != nil:
		return fmt.Sprintf("[%v, no upper bound)", *p.Min)
	case p.Max != nil:
		return fmt.Sprintf("(no lower bound, %v]", *p.Max)
	}
	return "its range"
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
	return checkParams(info, s)
}

// checkParams validates s's parameters against info, whether or not the kind
// is implemented. It is separate from Validate so the checks, including enum
// membership, can be exercised on a KindInfo the tests construct.
func checkParams(info KindInfo, s Spec) error {
	declared := map[string]Param{}
	for _, p := range info.Params {
		declared[p.Name] = p
	}
	for _, name := range paramNames {
		p, applies := declared[name]
		if ev, eset, isEnum := s.enumParam(name); isEnum {
			switch {
			case !applies && eset:
				return fmt.Errorf("%w: %q does not apply to %q", ErrBadParam, name, s.Kind)
			case !applies:
			case !eset && p.Required:
				return fmt.Errorf("%w: %q requires %q", ErrBadParam, s.Kind, name)
			case eset:
				if err := checkEnum(p, ev); err != nil {
					return err
				}
			}
			continue
		}
		v, set := s.param(name)
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
			return fmt.Errorf("%w: %q = %v for %q is outside %s", ErrBadParam, name, v, s.Kind, boundsText(p))
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
