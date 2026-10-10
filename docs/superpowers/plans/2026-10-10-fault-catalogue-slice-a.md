# Fault catalogue, slice A (media path) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a server-described fault catalogue, the `packet_loss` and `keyframe_starvation` effects, per-kind fault counters, a catalogue-driven dashboard fault form, and a seed-derived ONVIF endpoint UUID per camera.

**Architecture:** `internal/fault` gains a catalogue (kinds, typed params, implemented flag) that drives validation, `GET /api/v1/faults` and the dashboard form. The pump gains two effects: `keyframe_starvation` (a withheld keyframe is never encoded, like `frame_drop`) and `packet_loss` (the frame is encoded, then individual packets are not written, leaving real sequence gaps). Per-kind counters flow `obs` → `Stats.FaultCounts` → the dashboard JSON. Each camera gets a deterministic UUID derived from its seed, served by the ONVIF `GetEndpointReference` operation and shown in the dashboard.

**Tech Stack:** Go 1.26, `gortsplib` v5, `mediacommon` v2, `pion/rtp`, vanilla JS dashboard page, Playwright e2e (`-tags e2e`).

**Spec:** `docs/superpowers/specs/2026-10-09-fault-catalogue-design.md` (sections 1, 2.1, 2.2, 3, 5, 6, 7). Slices B (control plane) and C (timing) are out of scope here.

## Global Constraints

- `CGO_ENABLED=0` must keep building and testing (`go build ./...`, `go vet ./...`).
- Determinism is the product: every decision is a pure function of (camera seed, kind, clock value, params). No wall clock, goroutine order or map order in a decision path. Each kind draws from its own stream, `seed.Stream(kind)`.
- Draws are unconditional: the draw count depends only on the clock (frames or packets), never on an earlier outcome of the same kind.
- Real sockets in wire tests; no mocks of the RTSP wire. Fixed seeds. `go test -race ./...` must pass.
- Quality bar: every new behaviour maps to a real field bug (stated in the catalogue's `Models` text). README must not claim anything the code or a test does not back. No coverage figure not produced by CI.
- The README's "Scope / what this does not do" section and the "not ONVIF conformant" notice stay.
- Display name is **CamFarm**; module path, packages and binary stay lowercase `camfarm`. No employer or client names anywhere.
- gofmt-clean, `golangci-lint run` clean (errcheck on `fmt.Fprint*` must be assigned to `_, _`).
- Never use bare `git stash`. Commit after each task.

## Rulings (decided while writing this plan)

1. **Starved keyframes are not encoded.** The spec says a starved keyframe is "withheld". This plan implements that as encoder-side, like `frame_drop`: the access unit is never encoded, RTP sequence numbers stay contiguous and the loss shows as a timestamp jump. That models a long-GOP camera that simply does not emit keyframes. Cost if wrong: switch to encode-then-skip in Task 4 (one block).
2. **`packet_loss` draws only for frames that are encoded.** A frame removed by `frame_drop` or `keyframe_starvation` produces no packets, so it consumes no loss draws. Spec 1.1 says a sequence must not shift because of a neighbour's decision; for this one pair it does (the loss clock is "packets produced"). Stated as a limit in the README (Task 9). Cost if wrong: precompute packet counts per access unit; deferred.
3. **The catalogue lists every catalogued kind, including unimplemented ones, with `implemented:false` and no params.** Slice B and C add params as they implement kinds. The `enum` param type from the spec is deferred to slice B, the first slice that needs it.
4. **Existing kind names stay** (`teardown_mid_session`, `sdp_codec_lie`, `sps_resolution_lie`, `accept_then_silence`). Slice B renames/merges them to the spec's `teardown` and `sdp_lie`.
5. **Only `meanBurst` and `keepEvery` are added to `FaultSpec` now.** The other new fields in spec section 3 arrive with the slices that use them.
6. **`Validate` skips parameter checks for unimplemented kinds.** They are refused as `ErrUnsupported` before parameters matter, so old specs keep their error class.
7. **"Parameter is set" means non-zero** (matches `omitempty`). `rate: 0` is accepted for rate-based kinds (a no-op) and is not "set".
8. **The ONVIF UUID is a seed-derived, version-4-shaped UUID served bare** in `GetEndpointReferenceResponse/GUID` and shown bare in the UI and API. `onvif-go` has no server-side device UUID and no `GetEndpointReference` handler; its client parses `GUID` and a real camera's report says "Endpoint reference UUID returned", but the exact value format (bare or `urn:uuid:` prefixed) was not found in a capture. If a real capture shows the prefix, change `Camera.ONVIFUUID` and the handler in one place (`EndpointUUID`). WS-Discovery, which will use `urn:uuid:<same uuid>`, is still not built.

## File Structure

- Create `internal/fault/catalogue.go`: `ParamType`, `Param`, `KindInfo`, `Catalogue()`, `Info()`, `Validate()`, errors.
- Create `internal/fault/catalogue_test.go`.
- Modify `internal/fault/fault.go`: new kind and `Spec` fields, `Implemented` via catalogue, engine state and `DecidePackets`, starvation rewrite.
- Modify `internal/fault/fault_test.go`; create `internal/fault/loss_test.go` for packet-loss and starvation engine tests.
- Modify `internal/obs/obs.go`, `internal/obs/obs_test.go`: per-kind tallies.
- Modify `internal/rtsp/pump.go`, `internal/rtsp/pump_test.go`: the two effects.
- Create `internal/rtsp/faults_wire_test.go`: real-client tests.
- Modify `spec.go`, `camfarm.go`, `camfarm_test.go`: public fields, shared validation, `Stats.FaultCounts`, `Camera.ONVIFUUID`.
- Modify `internal/onvif/server.go`, `internal/onvif/dispatch.go` (native handler), `internal/onvif/server_test.go`; create `internal/onvif/uuid.go`, `internal/onvif/uuid_test.go`.
- Modify `internal/dashboard/dashboard.go`, `internal/dashboard/dashboard_test.go`, `internal/dashboard/web/index.html`, `internal/dashboard/e2e_test.go`.
- Create `readme_test.go` (root package); modify `README.md`.

---

### Task 1: Fault catalogue, new kind and validation

**Files:**
- Create: `internal/fault/catalogue.go`
- Create: `internal/fault/catalogue_test.go`
- Modify: `internal/fault/fault.go` (constants, `Spec`, `knownKinds`, `Implemented`, `New`)
- Modify: `internal/fault/fault_test.go` (`TestImplemented`, `TestKnown`, `TestAddingAKindDoesNotShiftAnother`)

**Interfaces:**
- Consumes: existing `fault.Kind`, `seed.Seed`.
- Produces (later tasks rely on these exact names):
  - `const KindPacketLoss Kind = "packet_loss"`
  - `Spec` gains `MeanBurst float64` and `KeepEvery int`.
  - `type ParamType string` with `ParamNumber`, `ParamInteger`, `ParamBoolean`.
  - `type Param struct { Name string; Type ParamType; Min, Max *float64; Default any; Required bool; Help string }` with JSON tags `name,type,min,max,default,required,help` (lowerCamel, `omitempty` on `min,max,default`).
  - `type KindInfo struct { Kind Kind; Implemented bool; Models string; Clock string; Params []Param }` with JSON tags `kind,implemented,models,clock,params`.
  - `func Catalogue() []KindInfo` (fresh deep copy each call, fixed order), `func Info(Kind) (KindInfo, bool)`, `func Validate(Spec) error`.
  - `var ErrBadParam`. `ErrBadRate` is kept and is what an out-of-range `rate` returns.

- [ ] **Step 1: Write the failing tests**

Create `internal/fault/catalogue_test.go`:

```go
package fault

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestCatalogueMatchesKnownKinds(t *testing.T) {
	seen := map[Kind]bool{}
	for _, ki := range Catalogue() {
		if seen[ki.Kind] {
			t.Fatalf("duplicate catalogue entry %q", ki.Kind)
		}
		seen[ki.Kind] = true
		if !Known(ki.Kind) {
			t.Errorf("%q is in the catalogue but not Known", ki.Kind)
		}
		if strings.TrimSpace(ki.Models) == "" {
			t.Errorf("%q has no Models text: every kind must name the real bug it models", ki.Kind)
		}
	}
	for k := range knownKinds {
		if !seen[k] {
			t.Errorf("%q is Known but missing from the catalogue", k)
		}
	}
}

func TestCatalogueOrderAndImplementedFlags(t *testing.T) {
	var kinds []Kind
	impl := map[Kind]bool{}
	for _, ki := range Catalogue() {
		kinds = append(kinds, ki.Kind)
		impl[ki.Kind] = ki.Implemented
		if Implemented(ki.Kind) != ki.Implemented {
			t.Errorf("Implemented(%q) disagrees with the catalogue", ki.Kind)
		}
	}
	want := []Kind{KindFrameDrop, KindPacketLoss, KindKeyframeStarvation,
		KindTeardownMidSession, KindAcceptThenSilence, KindSDPCodecLie, KindSPSResolutionLie}
	if len(kinds) != len(want) {
		t.Fatalf("catalogue kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("catalogue order = %v, want %v", kinds, want)
		}
	}
	for _, k := range []Kind{KindFrameDrop, KindPacketLoss, KindKeyframeStarvation} {
		if !impl[k] {
			t.Errorf("%q should be implemented", k)
		}
	}
	for _, k := range []Kind{KindTeardownMidSession, KindAcceptThenSilence, KindSDPCodecLie, KindSPSResolutionLie} {
		if impl[k] {
			t.Errorf("%q should not be implemented yet", k)
		}
	}
}

func TestCatalogueReturnsACopy(t *testing.T) {
	c := Catalogue()
	c[0].Models = "mutated"
	c[0].Params[0].Name = "mutated"
	again := Catalogue()
	if again[0].Models == "mutated" || again[0].Params[0].Name == "mutated" {
		t.Fatal("mutating a returned catalogue changed the next one")
	}
}

func TestCatalogueJSONShape(t *testing.T) {
	b, err := json.Marshal(Catalogue())
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	fd := got[0]
	if fd["kind"] != "frame_drop" || fd["implemented"] != true {
		t.Fatalf("first entry = %v", fd)
	}
	params := fd["params"].([]any)
	names := []string{}
	for _, p := range params {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "rate,protectKeyframes" {
		t.Fatalf("frame_drop params = %v, want rate,protectKeyframes", names)
	}
	// Unimplemented kinds are listed with an empty (never null) params array.
	last := got[len(got)-1]
	if last["implemented"] != false {
		t.Fatalf("last entry should be unimplemented: %v", last)
	}
	if p, ok := last["params"].([]any); !ok || len(p) != 0 {
		t.Fatalf("unimplemented kind params = %v, want []", last["params"])
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want error // nil means valid
	}{
		{"frame_drop ok", Spec{Kind: KindFrameDrop, Rate: 0.5}, nil},
		{"frame_drop rate zero is a no-op", Spec{Kind: KindFrameDrop}, nil},
		{"frame_drop rate high", Spec{Kind: KindFrameDrop, Rate: 1.5}, ErrBadRate},
		{"frame_drop rate negative", Spec{Kind: KindFrameDrop, Rate: -0.1}, ErrBadRate},
		{"frame_drop rate NaN", Spec{Kind: KindFrameDrop, Rate: math.NaN()}, ErrBadRate},
		{"frame_drop keepEvery does not apply", Spec{Kind: KindFrameDrop, Rate: 0.1, KeepEvery: 3}, ErrBadParam},
		{"frame_drop meanBurst does not apply", Spec{Kind: KindFrameDrop, Rate: 0.1, MeanBurst: 2}, ErrBadParam},
		{"packet_loss independent", Spec{Kind: KindPacketLoss, Rate: 0.1}, nil},
		{"packet_loss rate one independent", Spec{Kind: KindPacketLoss, Rate: 1}, nil},
		{"packet_loss burst ok", Spec{Kind: KindPacketLoss, Rate: 0.2, MeanBurst: 4}, nil},
		{"packet_loss burst at the limit", Spec{Kind: KindPacketLoss, Rate: 0.8, MeanBurst: 4}, nil},
		{"packet_loss burst above the limit", Spec{Kind: KindPacketLoss, Rate: 0.9, MeanBurst: 4}, ErrBadParam},
		{"packet_loss burst needs rate below one", Spec{Kind: KindPacketLoss, Rate: 1, MeanBurst: 4}, ErrBadParam},
		{"packet_loss meanBurst must exceed one", Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 1}, ErrBadParam},
		{"packet_loss meanBurst below one", Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 0.5}, ErrBadParam},
		{"packet_loss keepEvery does not apply", Spec{Kind: KindPacketLoss, Rate: 0.1, KeepEvery: 2}, ErrBadParam},
		{"keyframe_starvation ok", Spec{Kind: KindKeyframeStarvation, KeepEvery: 3}, nil},
		{"keyframe_starvation keepEvery missing", Spec{Kind: KindKeyframeStarvation}, ErrBadParam},
		{"keyframe_starvation keepEvery one", Spec{Kind: KindKeyframeStarvation, KeepEvery: 1}, ErrBadParam},
		{"keyframe_starvation keepEvery too large", Spec{Kind: KindKeyframeStarvation, KeepEvery: 1001}, ErrBadParam},
		{"keyframe_starvation rate does not apply", Spec{Kind: KindKeyframeStarvation, KeepEvery: 3, Rate: 0.5}, ErrBadParam},
		{"keyframe_starvation protectKeyframes does not apply", Spec{Kind: KindKeyframeStarvation, KeepEvery: 3, ProtectKeyframes: true}, ErrBadParam},
		{"unknown kind", Spec{Kind: "nope"}, ErrUnknownKind},
		{"unimplemented kinds skip param checks", Spec{Kind: KindSDPCodecLie, Rate: 0.5, KeepEvery: 7}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.spec)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/fault/ 2>&1 | head -20`
Expected: build failure (`undefined: Catalogue`, `KindPacketLoss`, `Spec.KeepEvery`, ...).

- [ ] **Step 3: Implement**

In `internal/fault/fault.go`:

1. Add the constant after `KindKeyframeStarvation`:

```go
	// KindPacketLoss withholds individual RTP packets of an encoded frame,
	// leaving real sequence-number gaps with honest timestamps. Reproduces
	// lossy links and receivers that mishandle gaps.
	KindPacketLoss Kind = "packet_loss"
```

2. Add `KindPacketLoss: true,` to `knownKinds`.
3. Replace `Implemented` with:

```go
// Implemented reports whether a kind's effect exists in this version. Only
// implemented kinds are accepted by the public API; the rest stay refused.
func Implemented(kind Kind) bool {
	info, ok := Info(kind)
	return ok && info.Implemented
}
```

4. Extend `Spec`:

```go
	// MeanBurst, for packet_loss, selects burst loss when greater than 1: the
	// mean length, in packets, of a run of consecutive losses. Zero means
	// independent loss.
	MeanBurst float64
	// KeepEvery, for keyframe_starvation, delivers only every Nth
	// random-access access unit (the first is always delivered).
	KeepEvery int
```

5. In `New`, replace the two checks with:

```go
		if !knownKinds[sp.Kind] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, sp.Kind)
		}
		if sp.Rate < 0 || sp.Rate > 1 {
			return nil, fmt.Errorf("%w: %v for %q", ErrBadRate, sp.Rate, sp.Kind)
		}
		if err := Validate(sp); err != nil {
			return nil, err
		}
```

(The rate check stays for unimplemented kinds, as before; `Validate` is a no-op for them.)

Create `internal/fault/catalogue.go`:

```go
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

var (
	rateParam = Param{Name: "rate", Type: ParamNumber, Min: f64(0), Max: f64(1), Default: 0.2,
		Help: "Probability in [0,1] that the fault fires for each unit of its clock."}
	protectParam = Param{Name: "protectKeyframes", Type: ParamBoolean, Default: false,
		Help: "Never apply the fault to a random-access (keyframe) unit. The draw is still consumed."}
)

// Catalogue returns every catalogued kind in a fixed order. It builds a fresh
// copy each call, so callers may keep or modify what they receive.
func Catalogue() []KindInfo {
	rate := rateParam
	protect := protectParam
	lossRate := rateParam
	lossRate.Default = 0.05
	lossRate.Help = "Long-run fraction of packets withheld, in [0,1]."
	lossProtect := protectParam
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
```

Update `internal/fault/fault_test.go`:
- `TestImplemented`: implemented set is `KindFrameDrop, KindPacketLoss, KindKeyframeStarvation`; the not-implemented loop is `KindTeardownMidSession, KindAcceptThenSilence, KindSDPCodecLie, KindSPSResolutionLie`.
- `TestKnown`: add `KindPacketLoss` to the list.
- `TestAddingAKindDoesNotShiftAnother`: the extended set becomes `{Kind: KindFrameDrop, Rate: 0.4}, {Kind: KindKeyframeStarvation, KeepEvery: 3}, {Kind: KindPacketLoss, Rate: 0.4}`.

- [ ] **Step 4: Run the tests**

Run: `gofmt -l . ; go vet ./internal/fault/ && go test -race -count=1 ./internal/fault/`
Expected: PASS. (`DecideFrame` still has the old starvation rate branch; Task 2 replaces it. `TestAddingAKindDoesNotShiftAnother` passes either way.)

- [ ] **Step 5: Commit**

```bash
git add internal/fault
git commit -m "fault: add the catalogue, packet_loss kind and spec validation"
```

---

### Task 2: Engine decisions for keyframe starvation and packet loss

**Files:**
- Modify: `internal/fault/fault.go` (`entry`, `New`, `DecideFrame`, new `DecidePackets`)
- Create: `internal/fault/loss_test.go`

**Interfaces:**
- Consumes: Task 1 (`Spec.KeepEvery`, `Spec.MeanBurst`, `KindPacketLoss`).
- Produces:
  - `type PacketDecision struct { Index int; Suppressed bool }` (`Index` is the packet's position within the frame's packet list).
  - `func (e *Engine) DecidePackets(frameIndex, count int, randomAccess bool) []PacketDecision`: call once per **encoded** frame, after `DecideFrame`. Returns the packets a `packet_loss` fault fires on; `Suppressed` means spared by `ProtectKeyframes`.
  - `DecideFrame` now returns a `KindKeyframeStarvation` decision for each random-access unit that is not kept. It must be called exactly once per frame (the engine counts random-access units).

- [ ] **Step 1: Write the failing tests**

Create `internal/fault/loss_test.go`:

```go
package fault

import (
	"reflect"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

func TestKeyframeStarvationDeliversEveryNthRandomAccessUnit(t *testing.T) {
	e, err := New(seed.Seed(1), []Spec{{Kind: KindKeyframeStarvation, KeepEvery: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var withheld []int
	for i := 0; i < 60; i++ {
		ra := i%5 == 0 // random-access units: frames 0,5,10,...,55 -> k = 0..11
		for _, d := range e.DecideFrame(i, ra) {
			if d.Kind != KindKeyframeStarvation || d.Suppressed {
				t.Fatalf("frame %d: unexpected decision %+v", i, d)
			}
			withheld = append(withheld, i)
		}
	}
	// k mod 3 != 0 is withheld: k = 1,2,4,5,7,8,10,11 -> frames 5,10,20,25,35,40,50,55.
	want := []int{5, 10, 20, 25, 35, 40, 50, 55}
	if !reflect.DeepEqual(withheld, want) {
		t.Fatalf("withheld frames = %v, want %v", withheld, want)
	}
}

func TestKeyframeStarvationHasNoRandomness(t *testing.T) {
	run := func(s seed.Seed) []int {
		e, _ := New(s, []Spec{{Kind: KindKeyframeStarvation, KeepEvery: 4}})
		var out []int
		for i := 0; i < 100; i++ {
			if len(e.DecideFrame(i, i%7 == 0)) > 0 {
				out = append(out, i)
			}
		}
		return out
	}
	if !reflect.DeepEqual(run(1), run(999)) {
		t.Fatal("keyframe_starvation depended on the seed")
	}
}

func lossIndices(t *testing.T, s seed.Seed, spec Spec, frames, perFrame int, ra func(int) bool) [][]PacketDecision {
	t.Helper()
	e, err := New(s, []Spec{spec})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out [][]PacketDecision
	for i := 0; i < frames; i++ {
		out = append(out, e.DecidePackets(i, perFrame, ra(i)))
	}
	return out
}

func never(int) bool { return false }

func TestPacketLossRateBounds(t *testing.T) {
	for _, d := range lossIndices(t, 1, Spec{Kind: KindPacketLoss, Rate: 0}, 50, 4, never) {
		if len(d) != 0 {
			t.Fatalf("rate 0 withheld %v", d)
		}
	}
	for i, d := range lossIndices(t, 1, Spec{Kind: KindPacketLoss, Rate: 1}, 50, 4, never) {
		if len(d) != 4 {
			t.Fatalf("frame %d: rate 1 withheld %d of 4 packets", i, len(d))
		}
	}
}

func TestPacketLossIsSeedDeterministic(t *testing.T) {
	spec := Spec{Kind: KindPacketLoss, Rate: 0.3}
	a := lossIndices(t, 42, spec, 200, 3, never)
	b := lossIndices(t, 42, spec, 200, 3, never)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different loss")
	}
	if reflect.DeepEqual(a, lossIndices(t, 43, spec, 200, 3, never)) {
		t.Fatal("different seeds produced identical loss")
	}
}

func TestPacketLossIndependentRateConverges(t *testing.T) {
	const frames, per = 20000, 5
	lost := 0
	for _, d := range lossIndices(t, 7, Spec{Kind: KindPacketLoss, Rate: 0.2}, frames, per, never) {
		lost += len(d)
	}
	got := float64(lost) / float64(frames*per)
	if got < 0.19 || got > 0.21 {
		t.Fatalf("measured loss %.4f, want 0.20 +/- 0.01", got)
	}
}

// Burst mode must hit both the configured long-run rate and the configured
// mean run length. The seed is fixed, so this is an exact, repeatable
// measurement; the tolerances are about 7 standard deviations wide.
func TestPacketLossBurstModeMatchesRateAndMeanBurst(t *testing.T) {
	const frames, per = 100000, 4
	spec := Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 4}
	lost, runs, run := 0, 0, 0
	for _, d := range lossIndices(t, 11, spec, frames, per, never) {
		hit := map[int]bool{}
		for _, p := range d {
			hit[p.Index] = true
		}
		for j := 0; j < per; j++ { // walk packets in order to find consecutive runs
			if hit[j] {
				lost++
				run++
				continue
			}
			if run > 0 {
				runs++
				run = 0
			}
		}
	}
	if run > 0 {
		runs++
	}
	total := frames * per
	if rate := float64(lost) / float64(total); rate < 0.09 || rate > 0.11 {
		t.Fatalf("measured loss %.4f, want 0.10 +/- 0.01", rate)
	}
	if mean := float64(lost) / float64(runs); mean < 3.75 || mean > 4.25 {
		t.Fatalf("measured mean burst %.3f, want 4 +/- 0.25", mean)
	}
}

func TestPacketLossDrawsDoNotDependOnProtectKeyframes(t *testing.T) {
	ra := func(i int) bool { return i%4 == 0 }
	plain := lossIndices(t, 5, Spec{Kind: KindPacketLoss, Rate: 0.4}, 200, 3, ra)
	protected := lossIndices(t, 5, Spec{Kind: KindPacketLoss, Rate: 0.4, ProtectKeyframes: true}, 200, 3, ra)
	for i := range plain {
		if len(plain[i]) != len(protected[i]) {
			t.Fatalf("frame %d: protecting keyframes changed which packets were drawn", i)
		}
		for j := range plain[i] {
			if plain[i][j].Index != protected[i][j].Index {
				t.Fatalf("frame %d: packet index moved", i)
			}
			if got, want := protected[i][j].Suppressed, ra(i); got != want {
				t.Fatalf("frame %d: Suppressed = %v, want %v", i, got, want)
			}
			if plain[i][j].Suppressed {
				t.Fatalf("frame %d: unprotected loss was marked suppressed", i)
			}
		}
	}
}

func TestPacketLossDoesNotShiftFrameDrop(t *testing.T) {
	drops := func(specs []Spec, withPackets bool) []int {
		e, err := New(seed.Seed(99), specs)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for i := 0; i < 300; i++ {
			for _, d := range e.DecideFrame(i, false) {
				if d.Kind == KindFrameDrop {
					out = append(out, i)
				}
			}
			if withPackets {
				e.DecidePackets(i, 3, false)
			}
		}
		return out
	}
	base := drops([]Spec{{Kind: KindFrameDrop, Rate: 0.4}}, false)
	both := drops([]Spec{{Kind: KindFrameDrop, Rate: 0.4}, {Kind: KindPacketLoss, Rate: 0.4}}, true)
	if !reflect.DeepEqual(base, both) {
		t.Fatal("adding packet_loss moved frame_drop decisions")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/fault/ 2>&1 | head`
Expected: build failure on `DecidePackets` / `PacketDecision`.

- [ ] **Step 3: Implement**

In `internal/fault/fault.go`:

1. Extend `entry`:

```go
type entry struct {
	spec Spec
	rnd  *rand.Rand

	// seenRandomAccess counts random-access units seen by keyframe_starvation.
	seenRandomAccess int
	// bad is packet_loss burst state; p and q are its transition
	// probabilities (good->bad and bad->bad).
	bad  bool
	p, q float64
}
```

2. In `New`, after the validation, build the entry with burst parameters:

```go
		en := entry{spec: sp, rnd: s.Stream(string(sp.Kind)).Rand()}
		if sp.Kind == KindPacketLoss && sp.MeanBurst > 1 {
			// Two-state loss model: long-run loss equals Rate and the mean
			// run of losses equals MeanBurst. Validate guarantees p <= 1.
			en.q = 1 - 1/sp.MeanBurst
			en.p = sp.Rate * (1 - en.q) / (1 - sp.Rate)
		}
		e.entries = append(e.entries, en)
```

(replacing the existing `e.entries = append(e.entries, entry{...})`).

3. Replace the `switch` in `DecideFrame`:

```go
		switch en.spec.Kind {
		case KindFrameDrop:
			// Draw unconditionally so the sequence does not depend on earlier
			// outcomes.
			if en.rnd.Float64() >= en.spec.Rate {
				continue
			}
			d := Decision{Kind: en.spec.Kind, FrameIndex: frameIndex}
			if en.spec.ProtectKeyframes && randomAccess {
				d.Suppressed = true
			}
			out = append(out, d)
		case KindKeyframeStarvation:
			// No randomness: the k-th random-access unit is delivered when
			// k mod KeepEvery == 0, so the first one always is.
			if !randomAccess {
				continue
			}
			k := en.seenRandomAccess
			en.seenRandomAccess++
			if k%en.spec.KeepEvery != 0 {
				out = append(out, Decision{Kind: en.spec.Kind, FrameIndex: frameIndex})
			}
		default:
			// Packet loss is decided per packet (DecidePackets); session,
			// transport and control-plane faults are not frame-scoped.
		}
```

and update the `DecideFrame` doc comment to say it must be called exactly once per frame.

4. Add below `DecideFrame`:

```go
// PacketDecision is one packet a packet_loss fault fires on.
type PacketDecision struct {
	// Index is the packet's position in the frame's packet list.
	Index int
	// Suppressed reports that the loss fired but must not be applied (the
	// packet belongs to a random-access frame and ProtectKeyframes is set).
	Suppressed bool
}

// DecidePackets returns the packets of one encoded frame that a packet_loss
// fault fires on. Call it once per encoded frame, after DecideFrame: a frame
// that was never encoded has no packets and consumes no draws.
//
// Exactly one draw is consumed per packet, unconditionally, so the sequence
// depends only on the packet count so far, never on earlier outcomes.
func (e *Engine) DecidePackets(frameIndex, count int, randomAccess bool) []PacketDecision {
	var out []PacketDecision
	for i := range e.entries {
		en := &e.entries[i]
		if en.spec.Kind != KindPacketLoss {
			continue
		}
		for j := 0; j < count; j++ {
			u := en.rnd.Float64()
			var lost bool
			if en.spec.MeanBurst > 1 {
				if en.bad {
					en.bad = u < en.q
				} else {
					en.bad = u < en.p
				}
				lost = en.bad
			} else {
				lost = u < en.spec.Rate
			}
			if lost {
				out = append(out, PacketDecision{Index: j, Suppressed: en.spec.ProtectKeyframes && randomAccess})
			}
		}
	}
	return out
}
```

(`frameIndex` is accepted for symmetry with `DecideFrame` and future logging; if the linter reports it unused, name it `_`.)

- [ ] **Step 4: Run the tests**

Run: `gofmt -l . ; go vet ./internal/fault/ && go test -race -count=1 ./internal/fault/`
Expected: PASS. If the burst convergence test fails, the engine is wrong: debug the transition logic, do not widen tolerances.

- [ ] **Step 5: Commit**

```bash
git add internal/fault
git commit -m "fault: keyframe starvation and packet loss decisions"
```

---

### Task 3: Per-kind fault tallies in obs

**Files:**
- Modify: `internal/obs/obs.go`, `internal/obs/obs_test.go`

**Interfaces:**
- Produces:
  - `type KindCount struct{ Applied, Suppressed uint64 }`
  - `Counters` gains `ByKind map[string]KindCount` (never nil in a returned snapshot).
  - `func (r *Recorder) FaultTally(cameraID, kind string, applied, suppressed uint64)`. It adds to the tally only; it neither increments `FaultsFired` nor logs an event (the pump still calls `Fault` for that).
  - `Counters(id)` returns a snapshot whose `ByKind` is an independent copy.

- [ ] **Step 1: Write the failing tests** (append to `internal/obs/obs_test.go`)

```go
func TestFaultTallyCountsPerKindPerCamera(t *testing.T) {
	r := New(10)
	r.FaultTally("a", "frame_drop", 2, 1)
	r.FaultTally("a", "frame_drop", 3, 0)
	r.FaultTally("a", "packet_loss", 7, 4)
	r.FaultTally("b", "frame_drop", 1, 0)

	got := r.Counters("a").ByKind
	if got["frame_drop"] != (KindCount{Applied: 5, Suppressed: 1}) {
		t.Errorf("a/frame_drop = %+v", got["frame_drop"])
	}
	if got["packet_loss"] != (KindCount{Applied: 7, Suppressed: 4}) {
		t.Errorf("a/packet_loss = %+v", got["packet_loss"])
	}
	if r.Counters("b").ByKind["frame_drop"] != (KindCount{Applied: 1}) {
		t.Errorf("b/frame_drop = %+v", r.Counters("b").ByKind["frame_drop"])
	}
	if r.Counters("a").FaultsFired != 0 {
		t.Error("FaultTally must not change FaultsFired")
	}
}

func TestCountersByKindIsNeverNilAndIsACopy(t *testing.T) {
	r := New(10)
	if r.Counters("nobody").ByKind == nil {
		t.Fatal("ByKind is nil for an unknown camera")
	}
	r.FaultTally("a", "frame_drop", 1, 0)
	snap := r.Counters("a")
	snap.ByKind["frame_drop"] = KindCount{Applied: 999}
	if r.Counters("a").ByKind["frame_drop"].Applied != 1 {
		t.Fatal("mutating a snapshot changed the recorder")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/obs/ 2>&1 | head`
Expected: build failure (`undefined: KindCount`).

- [ ] **Step 3: Implement** in `internal/obs/obs.go`

```go
// KindCount tallies one fault kind's effects on one camera. Applied counts
// frames dropped, packets withheld or keyframes withheld; Suppressed counts
// decisions spared by ProtectKeyframes.
type KindCount struct {
	Applied    uint64
	Suppressed uint64
}
```

Add `ByKind map[string]KindCount` to `Counters`. Add:

```go
// FaultTally adds to a camera's per-kind effect counts. It is separate from
// Fault so that one event can account for many packets.
func (r *Recorder) FaultTally(cameraID, kind string, applied, suppressed uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.counterFor(cameraID)
	if c.ByKind == nil {
		c.ByKind = make(map[string]KindCount)
	}
	k := c.ByKind[kind]
	k.Applied += applied
	k.Suppressed += suppressed
	c.ByKind[kind] = k
}
```

Change `Counters` (the method) to deep-copy:

```go
func (r *Recorder) Counters(cameraID string) Counters {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Counters{ByKind: map[string]KindCount{}}
	if c, ok := r.counters[cameraID]; ok {
		out = *c
		out.ByKind = make(map[string]KindCount, len(c.ByKind))
		for k, v := range c.ByKind {
			out.ByKind[k] = v
		}
	}
	return out
}
```

`Counters` now contains a map, so it is no longer comparable with `==`. If any existing test or code compares two `Counters` values, compare the fields instead.

- [ ] **Step 4: Run**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/obs/ ./internal/rtsp/ .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/obs
git commit -m "obs: per-kind fault tallies"
```

---

### Task 4: Pump effects for keyframe starvation and packet loss

**Files:**
- Modify: `internal/rtsp/pump.go` (constants, `Step`)
- Modify: `internal/rtsp/pump_test.go`

**Interfaces:**
- Consumes: Task 2 (`DecideFrame` starvation decisions, `DecidePackets`), Task 3 (`obs.Recorder.FaultTally`).
- Produces: pump behaviour only. Observation contract that Tasks 5 to 8 rely on:
  - `frame_drop` applied: one `obs.Fault` event and `FaultTally(id,"frame_drop",1,0)`; suppressed: `FaultTally(id,"frame_drop",0,1)`.
  - `keyframe_starvation`: not encoded; one `obs.Fault` event with detail `detailKeyframeWithheld`; `FaultTally(id,"keyframe_starvation",1,0)`.
  - `packet_loss`: frame encoded; if any packet was withheld or spared, one `obs.Fault` event per frame with a detail naming the counts; `FaultTally(id,"packet_loss",applied,suppressed)`. `FramesServed` still counts the frame.

- [ ] **Step 1: Write the failing tests** (append to `internal/rtsp/pump_test.go`)

Also change the existing `TestUnimplementedFaultIsRecordedButNotApplied`: use `fault.KindTeardownMidSession` with `Rate: 1` instead of `KindKeyframeStarvation` (an unimplemented kind is still recorded and not applied).

```go
// perFrame runs a fault-free pump and records, for each of the first n access
// units, how many packets it produced and whether it is random access. Loss
// tests compare against it.
func perFrame(t *testing.T, n int) (counts []int, random []bool) {
	t.Helper()
	m := testMedia(t)
	p, w := newTestPump(t, seed.Seed(1), nil, obs.New(10))
	for i := 0; i < n; i++ {
		before := len(w.pkts)
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		counts = append(counts, len(w.pkts)-before)
		random = append(random, m.AUs[i%len(m.AUs)].RandomAccess)
	}
	return counts, random
}

func TestKeyframeStarvationWithholdsAllButEveryNthKeyframe(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(3), []fault.Spec{{Kind: fault.KindKeyframeStarvation, KeepEvery: 2}}, rec)

	steps := len(m.AUs) * 3
	wantWithheld := map[int]bool{}
	k := 0
	for i := 0; i < steps; i++ {
		if m.AUs[i%len(m.AUs)].RandomAccess {
			if k%2 != 0 {
				wantWithheld[i] = true
			}
			k++
		}
	}
	if len(wantWithheld) == 0 {
		t.Fatal("fixture has too few keyframes for this test")
	}
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	gotWithheld := map[int]bool{}
	for _, ev := range rec.Events() {
		if ev.Kind != string(fault.KindKeyframeStarvation) || ev.Detail != detailKeyframeWithheld {
			t.Fatalf("unexpected event %+v", ev)
		}
		gotWithheld[ev.FrameIndex] = true
	}
	if len(gotWithheld) != len(wantWithheld) {
		t.Fatalf("withheld frames = %v, want %v", gotWithheld, wantWithheld)
	}
	for i := range wantWithheld {
		if !gotWithheld[i] {
			t.Fatalf("frame %d should have been withheld; got %v", i, gotWithheld)
		}
	}
	c := rec.Counters("cam")
	if got := c.FramesServed; got != uint64(steps-len(wantWithheld)) {
		t.Errorf("FramesServed = %d, want %d", got, steps-len(wantWithheld))
	}
	if got := c.ByKind["keyframe_starvation"].Applied; got != uint64(len(wantWithheld)) {
		t.Errorf("tally = %d, want %d", got, len(wantWithheld))
	}
	// Withheld frames are never encoded, so sequence numbers stay contiguous.
	for i := 1; i < len(w.pkts); i++ {
		if w.pkts[i].SequenceNumber != w.pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d", i)
		}
	}
}

func TestPacketLossLeavesSequenceGapsAndHonestTimestamps(t *testing.T) {
	const steps = 60
	counts, _ := perFrame(t, steps)
	total := 0
	for _, c := range counts {
		total += c
	}

	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(0x1055), []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 0.3}}, rec)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	tally := rec.Counters("cam").ByKind["packet_loss"]
	if tally.Applied == 0 || int(tally.Applied) == total {
		t.Fatalf("withheld %d of %d packets; need a mix", tally.Applied, total)
	}
	if got := uint64(len(w.pkts)) + tally.Applied; got != uint64(total) {
		t.Fatalf("written %d + withheld %d != produced %d", len(w.pkts), tally.Applied, total)
	}
	gaps := 0
	for i := 1; i < len(w.pkts); i++ {
		d := w.pkts[i].SequenceNumber - w.pkts[i-1].SequenceNumber
		if d == 0 {
			t.Fatalf("duplicate sequence number at packet %d", i)
		}
		if d > 1 {
			gaps++
		}
		if w.pkts[i].Timestamp < w.pkts[i-1].Timestamp {
			t.Fatalf("timestamp went backwards at packet %d", i)
		}
	}
	if gaps == 0 {
		t.Fatal("no sequence gaps despite withheld packets")
	}
	// The frame still counts as served: it was encoded and offered.
	if got := rec.Counters("cam").FramesServed; got != steps {
		t.Errorf("FramesServed = %d, want %d", got, steps)
	}
}

func TestPacketLossIsSeedDeterministicThroughThePump(t *testing.T) {
	run := func() []uint16 {
		p, w := newTestPump(t, seed.Seed(77), []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 0.4}}, obs.New(1000))
		for i := 0; i < 90; i++ {
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		var seqs []uint16
		for _, pkt := range w.pkts {
			seqs = append(seqs, pkt.SequenceNumber)
		}
		return seqs
	}
	a, b := run(), run()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("runs differ in length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("packet %d: sequence %d vs %d", i, a[i], b[i])
		}
	}
}

func TestPacketLossProtectKeyframesSparesRandomAccessFrames(t *testing.T) {
	const steps = 60
	counts, random := perFrame(t, steps)
	wantWritten, wantSpared := 0, 0
	for i, c := range counts {
		if random[i] {
			wantWritten += c
			wantSpared += c
		}
	}
	if wantSpared == 0 {
		t.Fatal("fixture produced no keyframe packets")
	}

	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(4), []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 1, ProtectKeyframes: true}}, rec)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if len(w.pkts) != wantWritten {
		t.Fatalf("wrote %d packets, want exactly the keyframe packets (%d)", len(w.pkts), wantWritten)
	}
	tally := rec.Counters("cam").ByKind["packet_loss"]
	if tally.Suppressed != uint64(wantSpared) {
		t.Errorf("suppressed = %d, want %d", tally.Suppressed, wantSpared)
	}
}

func TestFrameDropTallyCountsAppliedAndSuppressed(t *testing.T) {
	rec := obs.New(1000)
	p, _ := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1, ProtectKeyframes: true}}, rec)
	steps := len(testMedia(t).AUs)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	tally := rec.Counters("cam").ByKind["frame_drop"]
	if tally.Applied == 0 || tally.Suppressed == 0 || tally.Applied+tally.Suppressed != uint64(steps) {
		t.Fatalf("tally = %+v over %d frames; want applied+suppressed == frames, both non-zero", tally, steps)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/rtsp/ -run 'Starvation|PacketLoss|Tally' 2>&1 | head`
Expected: build failure on `detailKeyframeWithheld`.

- [ ] **Step 3: Implement** in `internal/rtsp/pump.go`

Add next to the other `detail*` constants:

```go
const (
	detailKeyframeWithheld = "keyframe withheld (not encoded)"
	detailPacketLossFmt    = "withheld %d of %d packets"
	detailPacketSparedFmt  = "withheld %d of %d packets; spared %d keyframe packets"
)
```

Replace the body of `Step` from the `drop := false` line through the end of the `if !drop { ... }` block with:

```go
	drop := false
	for _, d := range p.faults.DecideFrame(p.frameCounter, au.RandomAccess) {
		kind := string(d.Kind)
		switch {
		case d.Suppressed:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailSuppressed)
			p.obs.FaultTally(p.id, kind, 0, 1)
		case d.Kind == fault.KindFrameDrop:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailFrameDropApplied)
			p.obs.FaultTally(p.id, kind, 1, 0)
			drop = true
		case d.Kind == fault.KindKeyframeStarvation:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailKeyframeWithheld)
			p.obs.FaultTally(p.id, kind, 1, 0)
			drop = true
		default:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailDecidedNotApplied)
		}
	}

	if !drop {
		pkts, err := p.enc.Encode(au.NALUs)
		if err != nil {
			return fmt.Errorf("rtsp: encoding access unit %d of %q: %w", p.idx, p.id, err)
		}

		// packet_loss: the frame is encoded (so the packetizer assigns
		// sequence numbers) and individual packets are then not written,
		// which is what leaves a real gap.
		skip := make([]bool, len(pkts))
		withheld, spared := 0, 0
		for _, d := range p.faults.DecidePackets(p.frameCounter, len(pkts), au.RandomAccess) {
			if d.Suppressed {
				spared++
			} else if !skip[d.Index] {
				skip[d.Index] = true
				withheld++
			}
		}
		if withheld > 0 || spared > 0 {
			detail := fmt.Sprintf(detailPacketLossFmt, withheld, len(pkts))
			if spared > 0 {
				detail = fmt.Sprintf(detailPacketSparedFmt, withheld, len(pkts), spared)
			}
			p.obs.Fault(p.id, p.frameCounter, string(fault.KindPacketLoss), detail)
			p.obs.FaultTally(p.id, string(fault.KindPacketLoss), uint64(withheld), uint64(spared))
		}

		ts := uint32(p.tsOffset + au.DTS)
		for i, pkt := range pkts {
			if skip[i] {
				continue
			}
			pkt.Timestamp = ts
			if err := p.w.WritePacketRTP(p.medi, pkt); err != nil {
				return fmt.Errorf("rtsp: writing RTP for %q: %w", p.id, err)
			}
		}
		p.obs.Frame(p.id)
	}
```

- [ ] **Step 4: Run**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/rtsp/ ./internal/fault/ ./internal/obs/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rtsp
git commit -m "rtsp: apply keyframe_starvation and packet_loss in the pump"
```

---

### Task 5: Public API, shared validation and `Stats.FaultCounts`

**Files:**
- Modify: `spec.go` (`FaultSpec`, per-camera fault loop, `toFaultSpecs`, new `validateFault`)
- Modify: `camfarm.go` (`Stats`, `Stats()`, `Inject`)
- Modify: `camfarm_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 4.
- Produces:
  - `FaultSpec` gains `MeanBurst float64 \`json:"meanBurst,omitempty"\`` and `KeepEvery int \`json:"keepEvery,omitempty"\``.
  - `func validateFault(f FaultSpec) error` (root package, unexported), returning `ErrInvalidSpec` for unknown kinds and for any parameter error, and `ErrUnsupported` for a known but unimplemented kind. Order: unknown → rate out of `[0,1]` (invalid) → unimplemented (unsupported) → `fault.Validate` (invalid).
  - `type FaultCount struct{ Applied, Suppressed uint64 }`; `Stats` gains `FaultCounts map[string]FaultCount` (never nil).

- [ ] **Step 1: Update existing tests and write the new ones**

In `camfarm_test.go`, the four places that use `keyframe_starvation` as the "unimplemented" example (the `Faults` literal near line 117 with its `strings.Contains(err.Error(), ...)` check near 124, the `AddCamera` literal near 390, and the `Inject` near 579) switch to `accept_then_silence` (keep the `Rate` values). Then append:

```go
func TestInjectPacketLossAndKeyframeStarvationOnARunningCamera(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")

	if err := cam.Inject(FaultSpec{Kind: "packet_loss", Rate: 0.3, MeanBurst: 3}); err != nil {
		t.Fatalf("Inject packet_loss: %v", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "keyframe_starvation", KeepEvery: 2}); err != nil {
		t.Fatalf("Inject keyframe_starvation: %v", err)
	}
	if got := cam.Faults(); len(got) != 2 {
		t.Fatalf("Faults() = %+v, want both kinds active", got)
	}
}

func TestInjectRejectsBadParametersAsInvalidSpec(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	for name, spec := range map[string]FaultSpec{
		"starvation without keepEvery":   {Kind: "keyframe_starvation"},
		"starvation with rate":           {Kind: "keyframe_starvation", KeepEvery: 3, Rate: 0.5},
		"frame_drop with keepEvery":      {Kind: "frame_drop", Rate: 0.1, KeepEvery: 3},
		"burst above the reachable rate": {Kind: "packet_loss", Rate: 0.9, MeanBurst: 4},
		"rate above one":                 {Kind: "packet_loss", Rate: 1.5},
	} {
		if err := cam.Inject(spec); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: Inject = %v, want ErrInvalidSpec", name, err)
		}
	}
	if len(cam.Faults()) != 0 {
		t.Fatal("a refused injection changed the active faults")
	}
}

func TestSpecAcceptsPacketLossAndRefusesBadParameters(t *testing.T) {
	s := minimalSpec()
	s.Cameras[0].Faults = []FaultSpec{{Kind: "packet_loss", Rate: 0.1}}
	f, err := Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start with packet_loss: %v", err)
	}
	f.Close()

	s.Cameras[0].Faults = []FaultSpec{{Kind: "keyframe_starvation"}}
	if _, err := Start(context.Background(), s); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Start with starvation lacking keepEvery = %v, want ErrInvalidSpec", err)
	}
}

func TestStatsFaultCountsReportPerKindEffects(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	if cam.Stats().FaultCounts == nil {
		t.Fatal("FaultCounts is nil on a camera with no faults")
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "drops to be tallied", func() bool {
		return cam.Stats().FaultCounts["frame_drop"].Applied >= 3
	})
}
```

(`minimalSpec`, `waitUntil`, `errors`, `context` already exist/are imported in `camfarm_test.go`; add imports only if missing.)

- [ ] **Step 2: Run to verify failure**

Run: `go test . 2>&1 | head`
Expected: build failure (`unknown field MeanBurst`, `FaultCounts`).

- [ ] **Step 3: Implement**

`spec.go`:

```go
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
}
```

Replace the fault loop in the per-camera validation with:

```go
	for _, f := range c.Faults {
		if err := validateFault(f); err != nil {
			return c, fmt.Errorf("camera %q: %w", c.ID, err)
		}
	}
```

Add:

```go
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

func toFaultSpec(f FaultSpec) fault.Spec {
	return fault.Spec{Kind: fault.Kind(f.Kind), Rate: f.Rate, ProtectKeyframes: f.ProtectKeyframes,
		MeanBurst: f.MeanBurst, KeepEvery: f.KeepEvery}
}
```

and make `toFaultSpecs` call `toFaultSpec` per element.

`camfarm.go`: in `Inject`, replace the three `if` checks before `c.mu.Lock()` with:

```go
	if err := validateFault(f); err != nil {
		return err
	}
```

Add to `Stats` (after `FaultsFired`) and define the type:

```go
	// FaultCounts tallies each fault kind's effects: frames dropped, packets
	// or keyframes withheld (Applied) and decisions spared by
	// ProtectKeyframes (Suppressed). Never nil.
	FaultCounts map[string]FaultCount
```

```go
// FaultCount is one fault kind's tally.
type FaultCount struct {
	Applied    uint64
	Suppressed uint64
}
```

In `Camera.Stats()` build it from `counters.ByKind`:

```go
	fc := make(map[string]FaultCount, len(counters.ByKind))
	for k, v := range counters.ByKind {
		fc[k] = FaultCount{Applied: v.Applied, Suppressed: v.Suppressed}
	}
```

and set `FaultCounts: fc` in the returned `Stats`.

- [ ] **Step 4: Run**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 . ./internal/...`
Expected: PASS. (Dashboard tests that post `keyframe_starvation` as unimplemented fail until Task 7; if so run `go test . ./internal/fault ./internal/rtsp ./internal/obs ./internal/onvif` here.)

- [ ] **Step 5: Commit**

```bash
git add spec.go camfarm.go camfarm_test.go
git commit -m "camfarm: packet_loss and keyframe_starvation in the public API, shared fault validation, per-kind stats"
```

---

### Task 6: Wire tests with a real RTSP client

**Files:**
- Create: `internal/rtsp/faults_wire_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 4, `CameraConfig.Faults`, existing `newTestClient`, `testMedia`.
- Produces: tests only.

- [ ] **Step 1: Write the tests**

Create `internal/rtsp/faults_wire_test.go`:

```go
package rtsp

import (
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

func awaitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startFaultServer(t *testing.T, faults []fault.Spec) *Server {
	t.Helper()
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(1000), Cameras: []CameraConfig{
		{ID: "cam-00", Media: testMedia(t), Seed: seed.Seed(21), Faults: faults},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// A withheld packet is a real sequence gap on the wire. Over TCP-interleaved
// transport the client sees the gap but no stall, because TCP delivers
// everything it is given; over UDP it would be a genuine loss. This test only
// claims what both transports share: gaps, in order, with nothing duplicated.
func TestPacketLossGapsAreVisibleToARealClient(t *testing.T) {
	s := startFaultServer(t, []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 0.3}})
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	var mu sync.Mutex
	var seqs []uint16
	c.OnPacketRTPAny(func(_ *description.Media, _ format.Format, p *rtp.Packet) {
		mu.Lock()
		seqs = append(seqs, p.SequenceNumber)
		mu.Unlock()
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
	awaitCond(t, "150 packets", func() bool { mu.Lock(); defer mu.Unlock(); return len(seqs) >= 150 })

	mu.Lock()
	defer mu.Unlock()
	gaps := 0
	for i := 1; i < len(seqs); i++ {
		d := seqs[i] - seqs[i-1] // uint16 arithmetic: correct across wraparound
		if d == 0 || d > 0x8000 {
			t.Fatalf("not strictly increasing at %d: %d then %d", i, seqs[i-1], seqs[i])
		}
		if d > 1 {
			gaps++
		}
	}
	if gaps == 0 {
		t.Fatal("no sequence gaps seen by the client despite rate 0.3")
	}
}

// countKeyframes joins the camera as a late client, plays for d, and returns
// how many access units and how many random-access units it reassembled.
func countKeyframes(t *testing.T, s *Server, d time.Duration) (units, keyframes int) {
	t.Helper()
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	var h264Format *format.H264
	medi := desc.FindFormat(&h264Format)
	if medi == nil {
		t.Fatal("no H264 format in the description")
	}
	dec, err := h264Format.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder: %v", err)
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	var mu sync.Mutex
	c.OnPacketRTP(medi, h264Format, func(pkt *rtp.Packet) {
		au, err := dec.Decode(pkt)
		if err != nil || au == nil {
			return
		}
		mu.Lock()
		units++
		if h264.IsRandomAccess(au) {
			keyframes++
		}
		mu.Unlock()
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
	time.Sleep(d)
	mu.Lock()
	defer mu.Unlock()
	return units, keyframes
}

func TestKeyframeStarvationLeavesALateJoinerWithoutAKeyframe(t *testing.T) {
	// Control: without the fault a late joiner gets a keyframe within a few
	// seconds. Without this the starved assertion could pass vacuously (a
	// detector that never fires).
	clean := startFaultServer(t, nil)
	if units, keys := countKeyframes(t, clean, 4*time.Second); units == 0 || keys == 0 {
		t.Fatalf("control: %d units, %d keyframes; want both non-zero", units, keys)
	}

	// keepEvery 1000 means only the very first keyframe is ever delivered,
	// and the camera has long since sent it by the time a client joins.
	starved := startFaultServer(t, []fault.Spec{{Kind: fault.KindKeyframeStarvation, KeepEvery: 1000}})
	time.Sleep(500 * time.Millisecond) // let the first (delivered) keyframe pass before the client joins
	units, keys := countKeyframes(t, starved, 4*time.Second)
	if units == 0 {
		t.Fatal("starved camera delivered no access units at all; P frames should still flow")
	}
	if keys != 0 {
		t.Fatalf("late joiner received %d keyframes from a starved camera, want 0", keys)
	}
}
```

If the pinned `gortsplib`/`mediacommon` API differs from the above in a small way (for example `FindFormat` on `*description.Session`, `CreateDecoder` returning only a decoder, or `Setup` taking different arguments), adapt the call to compile against the pinned version; the assertions do not change. `go doc github.com/bluenviron/gortsplib/v5 Client.Setup` shows the signature.

- [ ] **Step 2: Run**

Run: `go vet ./internal/rtsp/ && go test -race -count=3 -run 'PacketLossGaps|KeyframeStarvationLeaves' ./internal/rtsp/`
Expected: PASS on all three runs.

- [ ] **Step 3: Commit**

```bash
git add internal/rtsp/faults_wire_test.go
git commit -m "rtsp: wire tests for packet_loss gaps and keyframe starvation"
```

---

### Task 7: Catalogue endpoint and per-kind counts in the dashboard API

**Files:**
- Modify: `internal/dashboard/dashboard.go`
- Modify: `internal/dashboard/dashboard_test.go`

**Interfaces:**
- Consumes: Task 1 (`fault.Catalogue`), Task 5 (`Stats.FaultCounts`).
- Produces: `GET /api/v1/faults` returning `fault.Catalogue()` as JSON; the camera JSON's `stats` object gains `FaultCounts` (map of kind to `{Applied, Suppressed}`), following the existing PascalCase convention of that object.

- [ ] **Step 1: Write the failing tests** (append to `dashboard_test.go`; reuse the existing `do`/handler helpers used by neighbouring tests)

First change the existing assertion near line 258 that POSTs `{"kind":"keyframe_starvation","rate":1}` to POST `{"kind":"accept_then_silence","rate":1}` (still expecting the unimplemented-kind status the test asserts).

```go
func TestFaultCatalogueEndpoint(t *testing.T) {
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, Options{}))
	defer ts.Close()

	resp := do(t, "GET", ts.URL+"/api/v1/faults", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var cat []struct {
		Kind        string `json:"kind"`
		Implemented bool   `json:"implemented"`
		Models      string `json:"models"`
		Params      []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"params"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	byKind := map[string]int{}
	for i, k := range cat {
		byKind[k.Kind] = i
		if k.Models == "" {
			t.Errorf("%s has no models text", k.Kind)
		}
	}
	for _, want := range []string{"frame_drop", "packet_loss", "keyframe_starvation", "accept_then_silence"} {
		if _, ok := byKind[want]; !ok {
			t.Fatalf("catalogue lacks %q: %+v", want, cat)
		}
	}
	pl := cat[byKind["packet_loss"]]
	if !pl.Implemented {
		t.Error("packet_loss should be implemented")
	}
	names := map[string]bool{}
	for _, p := range pl.Params {
		names[p.Name] = true
	}
	for _, n := range []string{"rate", "meanBurst", "protectKeyframes"} {
		if !names[n] {
			t.Errorf("packet_loss lacks param %q", n)
		}
	}
	if cat[byKind["accept_then_silence"]].Implemented {
		t.Error("accept_then_silence should not be implemented")
	}
}

func TestCameraJSONCarriesPerKindFaultCounts(t *testing.T) {
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, Options{}))
	defer ts.Close()
	if resp := do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"frame_drop","rate":1}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("inject status = %d", resp.StatusCode)
	}
	waitFor(t, "frame_drop applied count", func() bool {
		resp := do(t, "GET", ts.URL+"/api/v1/cameras", "")
		defer resp.Body.Close()
		var cams []struct {
			Stats struct {
				FaultCounts map[string]struct{ Applied, Suppressed uint64 }
			} `json:"stats"`
		}
		if json.NewDecoder(resp.Body).Decode(&cams) != nil || len(cams) != 1 {
			return false
		}
		return cams[0].Stats.FaultCounts["frame_drop"].Applied >= 2
	})
}
```

(`waitFor` is defined in `sse_deadline_test.go`; `do` is the existing request helper in `dashboard_test.go`. Add `encoding/json` / `net/http` imports only if missing.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dashboard/ -run 'FaultCatalogue|PerKind' 2>&1 | head`
Expected: FAIL (404 for `/api/v1/faults`; no `FaultCounts`).

- [ ] **Step 3: Implement** in `internal/dashboard/dashboard.go`

Import `github.com/0x524a/camfarm/internal/fault`. Register next to the other routes:

```go
	mux.HandleFunc("GET /api/v1/faults", s.getFaultCatalogue)
```

Add:

```go
// getFaultCatalogue serves the fault catalogue that drives validation and the
// page's fault form, so the page cannot drift from the server.
func (s *server) getFaultCatalogue(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, fault.Catalogue())
}
```

In the camera JSON's stats struct (the one with `FaultsFired uint64`, around line 88) add `FaultCounts map[string]camfarm.FaultCount`, and set it from `stats.FaultCounts` where `FaultsFired: stats.FaultsFired` is copied.

- [ ] **Step 4: Run**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/dashboard/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard
git commit -m "dashboard: GET /api/v1/faults and per-kind fault counts"
```

---

### Task 8: Catalogue-driven fault form and e2e tests

**Files:**
- Modify: `internal/dashboard/web/index.html`
- Modify: `internal/dashboard/e2e_test.go`

**Interfaces:**
- Consumes: Task 7 endpoints.
- Produces (DOM hooks the e2e tests use): `.fault-kind` (select; unimplemented kinds are `disabled` options labelled "(not implemented)"), `.fault-params` (container regenerated when the kind changes), inputs `.fault-param[data-param=<name>]` with the extra classes `.fault-rate` on `rate` and `.fault-protect` on `protectKeyframes`, `.fault-hint` (models text plus the selected kind's param help), `.btn-fault`, `.btn-clear-fault`, `.fault-active`, and `#notice` for non-fatal page notices. All text goes in with `textContent`.

- [ ] **Step 1: Update and add the e2e tests** (`internal/dashboard/e2e_test.go`, behind the existing `e2e` build tag)

Replace `TestE2EUnimplementedFaultKindsAreDisabled`'s kind list with `teardown_mid_session` and `accept_then_silence` (read the test first and keep its mechanics). Append:

```go
func TestE2EFaultFormFollowsTheCatalogue(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	row := s.row("front-door")
	kind := row.Locator(".fault-kind")

	if _, err := kind.SelectOption(playwright.SelectOptionValues{Values: &[]string{"keyframe_starvation"}}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(row.Locator(`.fault-param[data-param="keepEvery"]`)).ToBeVisible(); err != nil {
		t.Fatalf("keepEvery input missing for keyframe_starvation: %v", err)
	}
	if err := expect().Locator(row.Locator(`.fault-param[data-param="rate"]`)).ToHaveCount(0); err != nil {
		t.Fatalf("rate input should not exist for keyframe_starvation: %v", err)
	}
	if err := expect().Locator(row.Locator(".fault-hint")).ToContainText("keyframe"); err != nil {
		t.Fatalf("hint text missing: %v", err)
	}

	if _, err := kind.SelectOption(playwright.SelectOptionValues{Values: &[]string{"packet_loss"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"rate", "meanBurst", "protectKeyframes"} {
		if err := expect().Locator(row.Locator(`.fault-param[data-param="` + p + `"]`)).ToBeVisible(); err != nil {
			t.Fatalf("%s input missing for packet_loss: %v", p, err)
		}
	}
	s.requireClean(t)
}

func TestE2EApplyAndClearPacketLossAndKeyframeStarvation(t *testing.T) {
	url, f := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	row := s.row("front-door")
	kind := row.Locator(".fault-kind")
	cam, _ := f.Camera("front-door")

	if _, err := kind.SelectOption(playwright.SelectOptionValues{Values: &[]string{"packet_loss"}}); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(`.fault-param[data-param="rate"]`).Fill("0.4"); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(`.fault-param[data-param="meanBurst"]`).Fill("3"); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(".btn-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(row.Locator(".fault-active")).ToContainText("packet_loss"); err != nil {
		t.Fatalf("packet_loss not shown as active: %v", err)
	}
	got := cam.Faults()
	if len(got) != 1 || got[0].Kind != "packet_loss" || got[0].MeanBurst != 3 || got[0].Rate != 0.4 {
		t.Fatalf("camera faults = %+v", got)
	}

	if _, err := kind.SelectOption(playwright.SelectOptionValues{Values: &[]string{"keyframe_starvation"}}); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(`.fault-param[data-param="keepEvery"]`).Fill("2"); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(".btn-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(row.Locator(".fault-active")).ToContainText("keyframe_starvation"); err != nil {
		t.Fatalf("keyframe_starvation not shown as active: %v", err)
	}

	if err := row.Locator(".btn-clear-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(row.Locator(".fault-active")).ToContainText("none"); err != nil {
		t.Fatalf("faults not cleared: %v", err)
	}
	s.requireClean(t)
}
```

Imports: use whatever `e2e_test.go` already imports for `playwright`; adapt `SelectOption` argument shape to the pinned playwright-go version if needed (the mechanics of the existing frame_drop test show the project's pattern).

- [ ] **Step 2: Run to verify failure**

Run: `PLAYWRIGHT_DRIVER_PATH=<driver dir> go test -tags e2e -count=1 -run 'FaultForm|PacketLoss' ./internal/dashboard/ 2>&1 | tail`
(The driver is assembled as in the `e2e` job of `.github/workflows/ci.yml`; `playwright-go`'s own download is dead.)
Expected: FAIL (no `.fault-param` elements).

- [ ] **Step 3: Implement** in `internal/dashboard/web/index.html`

1. Markup: add `<p id="notice" class="sub" role="status"></p>` directly under the `#error` element, and CSS `.fault-hint { color: var(--muted, inherit); font-size: 12px; }` (match the file's existing CSS variable names; use whichever muted-colour variable it already defines).
2. State and loading, near the other `var` declarations:

```js
  var catalogue = [];       // [{kind, implemented, models, clock, params:[...]}]
  var noticeBox = document.getElementById("notice");
  function showNotice(msg) { noticeBox.textContent = msg || ""; }
  var catalogueReady = request("GET", "/faults")
    .then(function (r) { return r.json(); })
    .then(function (c) { catalogue = c; })
    .catch(function (e) { showNotice("Fault catalogue unavailable: " + e.message); });
```

3. Make `refresh` wait for the catalogue: rename the existing `refresh` function body to `doRefresh` and add

```js
  function refresh() { return catalogueReady.then(doRefresh); }
```

(keep the `seq/applied` ordering logic inside `doRefresh` unchanged).

4. Replace the hard-coded `rate`, `protect` and `kind` construction in `createRow` with:

```js
    var kind = el("select", { class: "fault-kind", "aria-label": "Fault kind for " + c.id });
    catalogue.forEach(function (k) {
      kind.appendChild(el("option", {
        value: k.kind,
        text: k.implemented ? k.kind : k.kind + " (not implemented)",
        disabled: !k.implemented
      }));
    });
    var paramBox = el("div", { class: "fault-params row" });
    var hint = el("div", { class: "fault-hint" });

    function paramInput(k, p) {
      var input;
      if (p.type === "boolean") {
        input = el("input", { type: "checkbox", checked: p.default === true });
      } else if (p.type === "number" || p.type === "integer") {
        input = el("input", { type: "number", value: p["default"] === undefined ? "" : String(p["default"]) });
        if (p.min !== undefined) { input.setAttribute("min", String(p.min)); }
        if (p.max !== undefined) { input.setAttribute("max", String(p.max)); }
        input.setAttribute("step", p.type === "integer" ? "1" : "any");
      } else {
        showNotice("Skipped parameter " + p.name + " of " + k.kind + ": unsupported type " + p.type);
        return null;
      }
      var cls = "fault-param";
      if (p.name === "rate") { cls += " fault-rate"; }
      if (p.name === "protectKeyframes") { cls += " fault-protect"; }
      input.className = cls;
      input.setAttribute("data-param", p.name);
      input.setAttribute("title", p.help || "");
      input.setAttribute("aria-label", p.name + " for " + c.id);
      return input;
    }

    function renderParams() {
      var k = catalogue.filter(function (x) { return x.kind === kind.value; })[0];
      paramBox.textContent = "";
      hint.textContent = "";
      if (!k) { return; }
      hint.textContent = k.models;
      (k.params || []).forEach(function (p) {
        var input = paramInput(k, p);
        if (!input) { return; }
        paramBox.appendChild(el("label", {}, [p.name + (p.required ? " *" : ""), input]));
      });
    }
    // Default to the first implemented kind.
    var firstImpl = catalogue.filter(function (x) { return x.implemented; })[0];
    if (firstImpl) { kind.value = firstImpl.kind; }
    kind.addEventListener("change", renderParams);
    renderParams();
```

5. Replace the Apply handler body so it collects whatever inputs exist:

```js
    guard(applyBtn, function () {
      var body = { kind: kind.value };
      paramBox.querySelectorAll(".fault-param").forEach(function (input) {
        var name = input.getAttribute("data-param");
        if (input.type === "checkbox") {
          if (input.checked) { body[name] = true; }
        } else if (input.value !== "") {
          body[name] = parseFloat(input.value);
        }
      });
      return request("POST", base + "/faults", body).then(refresh);
    });
```

6. In the row's Faults cell replace the old controls row with `[kind, paramBox, hint]` (keep `active` above them), keeping the surrounding structure.
7. Replace `faultText` so it shows parameters and counts:

```js
  function faultText(c) {
    if (!c.faults.length) { return "none"; }
    var counts = (c.stats && c.stats.FaultCounts) || {};
    return c.faults.map(function (f) {
      var parts = [f.kind];
      ["rate", "meanBurst", "keepEvery"].forEach(function (k) { if (f[k]) { parts.push(k + "=" + f[k]); } });
      if (f.protectKeyframes) { parts.push("keyframes protected"); }
      var n = counts[f.kind];
      if (n) { parts.push("applied " + n.Applied + (n.Suppressed ? ", spared " + n.Suppressed : "")); }
      return parts.join(" ");
    }).join(", ");
  }
```

- [ ] **Step 4: Run all dashboard tests**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/dashboard/ && PLAYWRIGHT_DRIVER_PATH=<driver dir> go test -tags e2e -count=1 ./internal/dashboard/`
Expected: PASS, including the existing `TestE2EFrameDropControlInjectsAndClears` (the `.fault-rate`/`.fault-protect` hooks are preserved), `TestE2EBothColourSchemesRender`, `TestE2ENarrowViewportDoesNotScrollTheWholePage` and the console-clean `requireClean` checks.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard
git commit -m "dashboard: catalogue-driven fault form"
```

---

### Task 9: README table, catalogue/README agreement test, docs

**Files:**
- Create: `readme_test.go` (package `camfarm`, root)
- Modify: `README.md`, `CLAUDE.md` (only the sentence noted below)

**Interfaces:**
- Consumes: `fault.Catalogue()`.
- Produces: a test that fails when the README's fault table and the catalogue disagree.

- [ ] **Step 1: Write the failing test**

Create `readme_test.go`:

```go
package camfarm

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
)

const (
	faultTableStart = "<!-- fault-table:start -->"
	faultTableEnd   = "<!-- fault-table:end -->"
)

// faultTable renders the catalogue as the README table. The README must
// contain it verbatim between the markers, so the documentation cannot drift
// from what the code reports.
func faultTable() string {
	var b strings.Builder
	b.WriteString("| Kind | Implemented | Models |\n|---|---|---|\n")
	for _, ki := range fault.Catalogue() {
		impl := "no"
		if ki.Implemented {
			impl = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", ki.Kind, impl, ki.Models)
	}
	return b.String()
}

func TestREADMEFaultTableMatchesTheCatalogue(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	i, j := strings.Index(s, faultTableStart), strings.Index(s, faultTableEnd)
	if i < 0 || j < i {
		t.Fatalf("README.md lacks the %s ... %s markers", faultTableStart, faultTableEnd)
	}
	got := strings.TrimSpace(s[i+len(faultTableStart) : j])
	want := strings.TrimSpace(faultTable())
	if got != want {
		t.Fatalf("README fault table is out of date. Replace the text between the markers with:\n\n%s\n", want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run TestREADMEFaultTable .`
Expected: FAIL naming the missing markers.

- [ ] **Step 3: Update `README.md`**

1. Replace the two bullets "**Works:** one fault, `frame_drop`..." and "**Not built yet:** ..." with a single bullet block:

```markdown
- **Works:** three fault effects, each tied to a real field bug (table below): `frame_drop`,
  `packet_loss` and `keyframe_starvation`. A fault comes from a spec or is injected into and
  cleared from a running camera (`Camera.Inject`, the dashboard, `POST /api/v1/cameras/{id}/faults`).
  Runtime injections and clears are journaled, and the fleet replay line gets a
  ` journal=<n>:<12hex>` suffix hashing that journal. Per-kind effect counts are in
  `Stats.FaultCounts` and the dashboard. `GET /api/v1/faults` returns the catalogue that
  validates specs and generates the dashboard's fault form.
  Honest limits: which frame an interactive click lands on is recorded but cannot be derived
  from the seed, because it depends on when the click arrived; `packet_loss` draws only for
  frames that were actually encoded, so combining it with `frame_drop` or `keyframe_starvation`
  makes its sequence depend on theirs; a withheld packet is a genuine loss over UDP but only a
  sequence gap, with no stall, over TCP-interleaved transport (TCP delivers everything it is
  given); loss applies to every reader of the camera, not per reader.
- **Not built yet:** device discovery (WS-Discovery), PTZ, Imaging, and the *effect* of the
  other catalogued faults. A spec that names an unimplemented fault is **refused**
  (`ErrUnsupported`), not silently ignored.
```

2. Add a section after "Status" titled `## Fault catalogue` containing one sentence ("Generated from the catalogue in `internal/fault`; a test fails if this table drifts.") followed by the markers and the table:

```markdown
<!-- fault-table:start -->
| Kind | Implemented | Models |
|---|---|---|
| `frame_drop` | yes | Encoder skipping frames: a frame is never encoded, so RTP sequence numbers stay contiguous and the loss shows only as a timestamp jump. |
| `packet_loss` | yes | Lossy path: packets of an encoded frame are withheld, leaving real sequence-number gaps with honest timestamps. |
| `keyframe_starvation` | yes | Long-GOP camera: only every Nth keyframe is sent, so a client joining mid-stream cannot start decoding. |
| `teardown_mid_session` | no | Camera that drops live sessions; exercises reconnect logic and resource leaks. |
| `accept_then_silence` | no | Camera that accepts the TCP connection and never answers. |
| `sdp_codec_lie` | no | SDP that advertises one codec while the stream carries another. |
| `sps_resolution_lie` | no | SDP and SPS that advertise a geometry the frames do not match. |
<!-- fault-table:end -->
```

3. In the Scope section, the "No network-path impairment" bullet must be reworded so it does not contradict `packet_loss`: say that bandwidth ceilings, reordering and real network-path effects still need `tc`/`netem`, and that `packet_loss` is a *sender-side simulation* of loss (the camera chooses not to write packets), not a network impairment.

In `CLAUDE.md`, "Open architectural decisions" item 2 (the fault-injection catalogue) gets one appended sentence: "Slice A (`frame_drop`, `packet_loss`, `keyframe_starvation`, the catalogue) has shipped; see the README fault table for the current state."

- [ ] **Step 4: Run**

Run: `go test -count=1 -run TestREADMEFaultTable . && gofmt -l .`
Expected: PASS. If the table text differs by a character from `faultTable()`, paste the test's printed expectation into the README.

- [ ] **Step 5: Commit**

```bash
git add readme_test.go README.md CLAUDE.md
git commit -m "docs: README fault table generated from the catalogue, with a drift test"
```

---

### Task 10: ONVIF endpoint UUID per camera

**Context:** `onvif-go`'s server has no device UUID and no `GetEndpointReference` handler (its client implements the call, and its WS-Discovery client parses `EndpointReference/Address`), so CamFarm derives and serves the value natively around `onvif-go`, per the project's resolved "depend" decision. See Ruling 8.

**Files:**
- Create: `internal/onvif/uuid.go`, `internal/onvif/uuid_test.go`
- Modify: `internal/onvif/server.go` (`camera` struct, `buildCamera`, `handle`)
- Modify: `camfarm.go` (`Camera.ONVIFUUID`), `camfarm_test.go`
- Modify: `internal/dashboard/dashboard.go`, `internal/dashboard/dashboard_test.go`, `internal/dashboard/web/index.html`, `internal/dashboard/e2e_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `seed.Seed.Stream(label).Rand()` (a `*math/rand/v2.Rand`), `onvif.CameraConfig.Seed`.
- Produces:
  - `func EndpointUUID(s seed.Seed) string` in `internal/onvif`: 8-4-4-4-12 lowercase hex, RFC 4122 version 4 and variant bits set, deterministic from the seed.
  - SOAP operation `GetEndpointReference` on each camera's device service, answering `<tds:GetEndpointReferenceResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><tds:GUID>…</tds:GUID></tds:GetEndpointReferenceResponse>`, behind the same Digest auth as every other operation.
  - `func (c *Camera) ONVIFUUID() string` (root).
  - Camera JSON field `onvifUuid`.

- [ ] **Step 1: Write the failing tests**

`internal/onvif/uuid_test.go`:

```go
package onvif

import (
	"regexp"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestEndpointUUIDIsAVersion4UUID(t *testing.T) {
	for _, s := range []seed.Seed{0, 1, 0xdeadbeef, ^seed.Seed(0)} {
		if u := EndpointUUID(s); !uuidV4.MatchString(u) {
			t.Errorf("EndpointUUID(%#x) = %q, not a v4 UUID", uint64(s), u)
		}
	}
}

func TestEndpointUUIDIsDeterministicAndPerSeed(t *testing.T) {
	if EndpointUUID(42) != EndpointUUID(42) {
		t.Fatal("same seed gave different UUIDs")
	}
	seen := map[string]seed.Seed{}
	for i := 0; i < 1000; i++ {
		u := EndpointUUID(seed.Seed(1).Camera(i))
		if prev, dup := seen[u]; dup {
			t.Fatalf("cameras %#x and %#x share UUID %s", uint64(prev), uint64(seed.Seed(1).Camera(i)), u)
		}
		seen[u] = seed.Seed(1).Camera(i)
	}
}
```

Add an HTTP-level test to `internal/onvif/server_test.go` (it reuses that file's `startTestServer` and `soapRequest` helpers; `bytes` and `io` are already imported there):

```go
func TestGetEndpointReferenceReturnsTheCameraUUID(t *testing.T) {
	srv := startTestServer(t,
		CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)},
		CameraConfig{ID: "b", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/b", Seed: seed.Seed(2)},
	)
	guid := func(id string, s seed.Seed) string {
		t.Helper()
		resp := soapRequest(t, srv.Endpoint(id)+"/device", "GetEndpointReference")
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status = %d, want 200", id, resp.StatusCode)
		}
		out, _ := io.ReadAll(resp.Body)
		want := EndpointUUID(s)
		if !bytes.Contains(out, []byte("GetEndpointReferenceResponse")) || !bytes.Contains(out, []byte(">"+want+"<")) {
			t.Fatalf("%s: body = %q, want a GetEndpointReferenceResponse carrying GUID %s", id, out, want)
		}
		return want
	}
	if guid("a", seed.Seed(1)) == guid("b", seed.Seed(2)) {
		t.Fatal("two cameras answered with the same GUID")
	}
}
```

Write the body of that test fully against those helpers (the helpers are in this package's existing tests; read them first). In the root package add:

```go
func TestCameraONVIFUUIDIsStableAndDistinct(t *testing.T) {
	f := StartT(t, Spec{Seed: 7, Cameras: []CameraSpec{{ID: "a"}, {ID: "b"}}})
	a, _ := f.Camera("a")
	b, _ := f.Camera("b")
	if a.ONVIFUUID() == "" || a.ONVIFUUID() == b.ONVIFUUID() {
		t.Fatalf("UUIDs: a=%q b=%q", a.ONVIFUUID(), b.ONVIFUUID())
	}
	g := StartT(t, Spec{Seed: 7, Cameras: []CameraSpec{{ID: "a"}, {ID: "b"}}})
	a2, _ := g.Camera("a")
	if a.ONVIFUUID() != a2.ONVIFUUID() {
		t.Fatal("same seed and spec gave a different UUID")
	}
}
```

Dashboard unit test: the `GET /api/v1/cameras` JSON has `onvifUuid` equal to `cam.ONVIFUUID()`. e2e: the row shows a `[data-testid="cell-uuid"]` element whose text equals `cam.ONVIFUUID()`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/onvif/ . ./internal/dashboard/ 2>&1 | head`
Expected: build failures (`EndpointUUID`, `ONVIFUUID` undefined).

- [ ] **Step 3: Implement**

`internal/onvif/uuid.go`:

```go
package onvif

import (
	"encoding/binary"
	"fmt"

	"github.com/0x524a/camfarm/internal/seed"
)

// EndpointUUID derives a camera's ONVIF endpoint-reference UUID from its seed.
//
// A real device has a stable UUID that identifies it on the network; here it
// must also be reproducible, so it comes from the seed rather than from a
// random source. The result is shaped like an RFC 4122 version 4 UUID.
func EndpointUUID(s seed.Seed) string {
	r := s.Stream("onvif-uuid").Rand()
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], r.Uint64())
	binary.BigEndian.PutUint64(b[8:16], r.Uint64())
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
```

`internal/onvif/server.go`: add `uuid string` to the `camera` struct and set `uuid: EndpointUUID(cc.Seed)` where `buildCamera` constructs the `camera`. In `handle`, replace the `h, ok := handlers[actionName]` block with:

```go
	var resp any
	if actionName == "GetEndpointReference" {
		// onvif-go has no such handler: answered natively from the camera's
		// seed-derived UUID.
		resp = &getEndpointReferenceResponse{Xmlns: deviceNS, GUID: cam.uuid}
	} else {
		h, ok := handlers[actionName]
		if !ok {
			writeFault(w, "Client", "Action not supported")
			return
		}
		var err error
		resp, err = h(cam.srv, payload)
		if err != nil {
			writeFault(w, "Server", err.Error())
			return
		}
	}
```

(keep the following `rewriteXAddr` / `rewriteSnapshotURI` / `writeResponse` lines). Add to `dispatch.go`:

```go
const deviceNS = "http://www.onvif.org/ver10/device/wsdl"

// getEndpointReferenceResponse is the Device service's answer to
// GetEndpointReference. Only GUID is populated, which is all onvif-go's own
// client reads.
type getEndpointReferenceResponse struct {
	XMLName xml.Name `xml:"tds:GetEndpointReferenceResponse"`
	Xmlns   string   `xml:"xmlns:tds,attr"`
	GUID    string   `xml:"tds:GUID"`
}
```

(import `encoding/xml`). Check `writeResponse` (envelope.go) marshals arbitrary `any` the same way for `onvif-go` response structs; follow whatever namespace-prefix convention those structs use, and match it if it differs from `tds:`.

`camfarm.go`: `func (c *Camera) ONVIFUUID() string { return onvif.EndpointUUID(c.seed) }` with a doc comment ("deterministic from the camera seed; the same value the camera's ONVIF Device service returns from GetEndpointReference"). Confirm that the `seed` the root passes to `onvif.CameraConfig.Seed` is `c.seed`; if it passes a different derivation, use that same value.

Dashboard: add `OnvifUUID string \`json:"onvifUuid"\`` to the camera JSON struct and set it from `cam.ONVIFUUID()`. In `index.html`, after the ONVIF `urlLine`, add `urlLine("UUID", c.onvifUuid, "cell-uuid")` (same row layout, with a Copy button).

README: in the ONVIF bullet add: "each camera has a seed-derived endpoint UUID returned by `GetEndpointReference` and shown in the dashboard; WS-Discovery, which would announce it, is not built." Do not claim it matches any real device's format beyond being a version 4 UUID.

- [ ] **Step 4: Run**

Run: `gofmt -l . ; go vet ./... && golangci-lint run && go test -race -count=1 ./... && PLAYWRIGHT_DRIVER_PATH=<driver dir> go test -tags e2e -count=1 ./internal/dashboard/`
Expected: PASS everywhere.

- [ ] **Step 5: Commit**

```bash
git add -A internal/onvif camfarm.go camfarm_test.go internal/dashboard README.md
git commit -m "onvif: seed-derived endpoint UUID per camera, served by GetEndpointReference and shown in the dashboard"
```

---

## Final verification (controller, after Task 10)

- `gofmt -l .` prints nothing; `go vet ./...`; `golangci-lint run` (0 issues); `CGO_ENABLED=0 go build ./...`; `go test -race -count=1 ./...`; the e2e suite.
- Re-run the wire tests with `-count=10` to check for flakes.
- Confirm the README fault table test passes and that no README sentence claims more than the tests prove (in particular: loss is shared by all readers; TCP shows gaps without a stall; the UUID is not announced by WS-Discovery).
- Whole-branch review, then open one PR.

## Self-review notes

- Spec coverage: schema + `GET /api/v1/faults` (T1, T7); generic UI (T8); `packet_loss` independent and burst, `protectKeyframes`, shared across readers, transport honesty (T1, T2, T4, T6, T9); `keyframe_starvation` (T1, T2, T4, T6); `FaultCounts` (T3, T5, T7, T8); README table + drift test (T9); parameter-that-does-not-apply refused (T1, T5). Not in slice A by design: control-plane engine, `Clock` on journal entries, `rtsp_error`/`teardown`/`sdp_lie` (slice B), timing kinds (slice C).
- Names used across tasks: `KindPacketLoss`, `Spec.MeanBurst/KeepEvery`, `Catalogue/Info/Validate/ErrBadParam`, `PacketDecision`, `DecidePackets`, `KindCount`, `FaultTally`, `Counters.ByKind`, `detailKeyframeWithheld`, `validateFault`, `FaultCount`, `Stats.FaultCounts`, `EndpointUUID`, `Camera.ONVIFUUID`, `onvifUuid`.
- Known soft spots, deliberately flagged rather than hidden: the pinned `gortsplib` call shapes in Task 6; the playwright-go option shapes in Task 8; the `writeResponse` namespace convention in Task 10; Ruling 8's UUID format.
