# RTSP media slice — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `camfarm.Start(spec)` serves N deterministic synthetic RTSP cameras from a bundled fixture, with the seed and fault-injection seams built in and every fault deliberately inert.

**Architecture:** One process, one `gortsplib/v5` server, N `ServerStream`s dispatched by `ctx.Path`, media parsed once into an immutable slice of access units shared by every camera. Each camera owns a pump whose unit of work is `Step()` — one access unit — so fault decisions key on a frame counter rather than elapsed time. Seeds derive from `(rootSeed, cameraIndex, concernLabel)`.

**Tech Stack:** Go ≥ 1.25, `gortsplib/v5 v5.6.5`, `mediacommon/v2 v2.9.4`, `math/rand/v2`, `log/slog`, `embed`. No cgo. ffmpeg/ffprobe used as an external test oracle and to generate the fixture, never linked.

**Spec:** `docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md`

## Global Constraints

- Module path stays `github.com/0x524a/camfarm`; the name is still a placeholder and this slice does not settle it (spec §11.2).
- `CGO_ENABLED=0` for the artifact and its tests; the race detector runs in a separate `CGO_ENABLED=1` job (spec §10.0).
- Go directive `go 1.25.0` in `go.mod` stays as-is. `gortsplib/v5`'s own `go.mod` declares `go 1.26.0`, so the toolchain must be ≥ 1.26 to build it; the local toolchain is `go1.26.1`.
- **No ONVIF in this slice.** No ONVIF-related identifier, README sentence, or dependency. That means no `ONVIF®` trademark obligation is triggered yet (spec §11.1) — and no claim may hint at one.
- **No badge in the README.** There is no git remote, therefore no workflow has ever run, therefore no badge is backed by a passing run (spec §12).
- No coverage number stated anywhere that CI did not produce.
- No skipped tests. `t.Skip` must not appear. Missing tooling is installed in CI, not skipped around (spec §10.7).
- Every fault decision is a pure function of `(seed, cameraIndex, frameCounter)`. Never wall-clock, never goroutine order, never map iteration order (spec §8).
- No employer or client named anywhere (spec §12).
- All changes confined to this repository. Sibling repos are read-only prior art.

## What this slice deliberately does not build

Naming these keeps the plan honest about the gap to spec §9:

- **No ONVIF control plane, no WS-Discovery, no PTZ.** Therefore `Camera.ONVIFEndpoint()`, `Credentials()`, `ProfileToken()`, `PresetToken()` are **not** implemented. They are not stubbed to return `""` either — a stub that returns a plausible-looking empty value is exactly the "advertised-but-dead endpoint" fault this project exists to reproduce on purpose (spec §1.1). They arrive with the ONVIF slice.
- **No fault effects.** Fault *decisions* are implemented, seeded and tested; no decision changes a byte on the wire. `CameraSpec.Faults` non-empty is rejected with `ErrUnsupported`, so `FaultsFired` is zero by construction rather than by luck (spec §7.7).
- **No `Replay(ctx, seed, specPath)`.** Spec §9 signatures it against a spec *file*, and this slice builds no file format (that is decision 8, a later slice). What ships instead is `Spec.Hash()` and `Fleet.ReplayLine()`, which produce the exact reporting line spec §8.2 requires. Deviation recorded in Task 9.
- **No upstream-RTSP pull source.** `media.Source` is the interface spec §5 asks for; only the file/fixture implementation lands here.
- **No control API, no UI, no binary.** Library first (spec §6).

## Corrections to the design spec, established by running code

These were measured in a scratch module against `gortsplib/v5 v5.6.5`, not reasoned about. Task 9 back-ports them into the spec.

1. **`Server.NetListener()` exists** (`server.go:366-369`), returning the live `net.Listener`. Port-0 read-back is a one-liner: `s.NetListener().Addr().(*net.TCPAddr).Port`. The spec treats this as an open problem inherited from upstream issue #63; it is not one. Note it panics if called before a successful `Start()`.
2. **Spec §8.3 carve-out 1 is right in its conclusion and wrong in its reason.** `rtph264.Encoder.SSRC *uint32` and `InitialSequenceNumber *uint16` are both exported and settable. Measured end-to-end: the **initial sequence number is preserved** to the client, so it *is* reproducible. The **SSRC is not**, because `ServerStream` overwrites it unconditionally at `server_stream_format.go:103` (`pkt.SSRC = ssf.localSSRC`) with a per-format value generated during `Initialize()`. So: assert sequence numbers in determinism tests, never SSRC. There is also **no `InitialTimestamp` field** — per-packet `pkt.Timestamp` assignment is the only timestamp lever.
3. **`Server.AuthRealm` does not exist.** The realm is the hardcoded package constant `serverAuthRealm = "ipcam"` (`server.go:19`). Spec §7.3 offers `Server.AuthMethods` for the Basic-vs-Digest fault — that field is real and exported, defaulting to `{Basic, DigestMD5}` — but the wrong-realm fault is reachable *only* via the `OnResponse` rewrite path the spec lists as the alternative. Not a blocker for this slice; it removes an option from the phase-2 auth work.

Two further facts that shape this slice:

- **`ServerStream` exposes no reader count.** `Stats()` returns `OutboundBytes`, `OutboundRTPPackets`, `OutboundRTCPPackets` and per-media stats, and nothing else. Connected-client counts must be tracked by us in `OnSetup`/`OnSessionClose`. This is why spec §9's `Stats()` cannot simply forward.
- **`mediacommon/v2` has no Annex-B stream reader.** `h264.AnnexB.Unmarshal` takes one complete in-memory access unit and returns slices that **alias the input buffer**. `mpegts.Reader.OnDataH264` does the Annex-B split, strips the access-unit delimiter, and hands over PTS/DTS. Hence: the fixture is MPEG-TS, and the parser must **copy** every NALU it retains.

## File structure

| Path | Responsibility |
|---|---|
| `doc.go` | Package doc. Modify: drop "no behaviour is implemented yet". |
| `spec.go` | `Spec`, `CameraSpec`, `VideoSpec`, `SourceSpec`, `ListenSpec`, `FaultSpec`; validation; stable index assignment; `Hash()`. |
| `errors.go` | `ErrUnknownCamera`, `ErrUnsupported`, `ErrNotReady`, `ErrFaultActive`. |
| `camfarm.go` | `Start`, `StartT`, `Fleet`, `Camera`, `Status`, `Stats`, `Addrs`, `ReplayLine`. |
| `internal/seed/seed.go` | `Seed`, splitmix64, `Camera(index)`, `Stream(label)`, `Rand()`. |
| `internal/clock/clock.go` | `Clock`, `Ticker`, `Real`, `Virtual`. |
| `internal/media/media.go` | `Codec`, `AccessUnit`, `Media`, `Source` interface. |
| `internal/media/mpegts.go` | MPEG-TS → `*Media`, copying NALUs; SPS-derived geometry. |
| `internal/media/file.go` | `FileSource`, `FixtureSource`. |
| `internal/media/fixture/fixture.ts` | Committed 320×240 fixture, embedded. |
| `internal/media/fixture/fixture.go` | `//go:embed fixture.ts`. |
| `internal/obs/obs.go` | `Recorder`: bounded event log, per-camera counters. |
| `internal/fault/fault.go` | `Kind`, `Spec`, `Decision`, `Engine` with per-kind seeded streams. |
| `internal/rtsp/server.go` | `gortsplib` server, path dispatch, N streams, reader tracking, `OnResponse` seam. |
| `internal/rtsp/pump.go` | `Pump.Step()` — one access unit; loop seam; deterministic encoder. |
| `scripts/gen-fixture.sh` | Fixture provenance, committed and runnable. |
| `.github/workflows/ci.yml` | Modify: install ffmpeg in both jobs. |
| `README.md` | Create: honest, RTSP-only, no badge. |

`internal/media/fixture/` rather than `testdata/` because `go:embed` must reach the file from a normal package directory and `testdata` is excluded from package builds.

---

### Task 1: Seeded random derivation

**Files:**
- Create: `internal/seed/seed.go`
- Test: `internal/seed/seed_test.go`
- Modify: `.gitignore` (add `.serena/`)

**Interfaces:**
- Consumes: nothing.
- Produces: `type Seed uint64`; `func (Seed) Camera(index int) Seed`; `func (Seed) Stream(label string) Seed`; `func (Seed) Rand() *rand.Rand`.

- [ ] **Step 1: Write the failing test**

`internal/seed/seed_test.go`:

```go
package seed

import "testing"

func TestCameraIsDeterministicAndDistinct(t *testing.T) {
	root := Seed(0x3f2a9c81)

	a := root.Camera(0)
	b := root.Camera(0)
	if a != b {
		t.Fatalf("Camera(0) not deterministic: %#x vs %#x", a, b)
	}

	seen := map[Seed]int{}
	for i := 0; i < 1000; i++ {
		s := root.Camera(i)
		if prev, dup := seen[s]; dup {
			t.Fatalf("camera %d collides with %d at %#x", i, prev, s)
		}
		seen[s] = i
	}

	if root.Camera(0) == Seed(0) {
		t.Fatal("derived seed is zero")
	}
	if root.Camera(0) == root {
		t.Fatal("derived seed equals root")
	}
}

// A different root must move every camera, or one fleet's faults leak into another.
func TestDifferentRootsDiverge(t *testing.T) {
	for i := 0; i < 100; i++ {
		if Seed(1).Camera(i) == Seed(2).Camera(i) {
			t.Fatalf("roots 1 and 2 agree at camera %d", i)
		}
	}
}

// Spec section 8: streams split per concern so that adding a fault type does
// not shift another concern's sequence.
func TestStreamLabelsAreIndependent(t *testing.T) {
	cam := Seed(0x3f2a9c81).Camera(7)

	media1 := cam.Stream("media")
	media2 := cam.Stream("media")
	if media1 != media2 {
		t.Fatalf("Stream(media) not deterministic: %#x vs %#x", media1, media2)
	}
	if cam.Stream("media") == cam.Stream("fault") {
		t.Fatal("media and fault streams are identical")
	}

	// The point of the property: introducing a new label must leave the old
	// ones byte-identical.
	before := cam.Stream("fault")
	_ = cam.Stream("a-brand-new-concern")
	if cam.Stream("fault") != before {
		t.Fatal("deriving a new label perturbed an existing one")
	}
}

func TestRandIsReproducible(t *testing.T) {
	s := Seed(0x3f2a9c81).Camera(3).Stream("fault")

	first := make([]uint64, 8)
	r := s.Rand()
	for i := range first {
		first[i] = r.Uint64()
	}

	r2 := s.Rand()
	for i := range first {
		if got := r2.Uint64(); got != first[i] {
			t.Fatalf("draw %d differs: %#x vs %#x", i, got, first[i])
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/seed/ -run . -v`
Expected: FAIL — `internal/seed/seed.go` does not exist, so the package does not build (`undefined: Seed`).

- [ ] **Step 3: Write minimal implementation**

`internal/seed/seed.go`:

```go
// Package seed derives the deterministic random streams camfarm runs on.
//
// Every fault decision must be a pure function of the root seed, the camera's
// stable index, and an event counter -- never of wall-clock time, goroutine
// scheduling order, or map iteration order. This package supplies the first two
// of those three inputs.
package seed

import (
	"hash/fnv"
	"math/rand/v2"
)

// Seed is a 64-bit seed for one concern of one camera.
type Seed uint64

// splitmix64 is the mixing function from Vigna's SplittableRandom. It is used
// rather than hashing because it is a bijection on uint64: distinct inputs give
// distinct outputs, so per-camera seeds cannot silently collide.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Camera derives the seed for the camera at the given stable index.
//
// The index must come from the camera's position in the parsed spec, never from
// iteration over a map: Go randomises map order, which would make a fleet's
// seeds differ between runs of the same spec.
func (s Seed) Camera(index int) Seed {
	return Seed(splitmix64(uint64(s) + splitmix64(uint64(index)+1)))
}

// Stream derives a substream for one concern, such as "media" or "fault".
//
// Labels rather than ordinals: a new concern added later hashes to its own
// value and leaves every existing substream byte-identical, so introducing a
// fault type cannot shift an unrelated sequence.
func (s Seed) Stream(label string) Seed {
	h := fnv.New64a()
	_, _ = h.Write([]byte(label))
	return Seed(splitmix64(uint64(s) + splitmix64(h.Sum64())))
}

// Rand returns a generator seeded from s. Two calls on equal Seeds yield
// identical sequences.
func (s Seed) Rand() *rand.Rand {
	return rand.New(rand.NewPCG(uint64(s), splitmix64(uint64(s))))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/seed/ -v`
Expected: PASS, all four tests.

- [ ] **Step 5: Ignore the Serena tooling directory**

Append to `.gitignore`:

```
# Local Serena tooling state, not part of the project.
.serena/
```

- [ ] **Step 6: Commit**

```bash
git add internal/seed .gitignore
git commit -m "Add seeded random derivation

Per-camera and per-concern seeds derive from a root seed through splitmix64,
a bijection, so distinct cameras cannot collide. Concerns are keyed by string
label rather than ordinal so that adding one later leaves existing substreams
unchanged."
```

---

### Task 2: Clock

**Files:**
- Create: `internal/clock/clock.go`
- Test: `internal/clock/clock_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Ticker interface { C() <-chan time.Time; Stop() }`; `type Clock interface { Now() time.Time; NewTicker(time.Duration) Ticker }`; `type Real struct{}`; `type Virtual struct{...}` with `func NewVirtual(time.Time) *Virtual` and `func (*Virtual) Advance(time.Duration)`.

Design note for the implementer: this clock governs *pacing only*. Determinism does not depend on it, because the pump's unit of work is `Step()` and fault decisions key on a frame counter (spec §8.1). Tests that assert determinism call `Step()` directly and need no clock at all. `Virtual` exists so the driver loop itself can be tested without real sleeping.

- [ ] **Step 1: Write the failing test**

`internal/clock/clock_test.go`:

```go
package clock

import (
	"testing"
	"time"
)

func TestRealAdvances(t *testing.T) {
	var c Clock = Real{}
	t0 := c.Now()
	tk := c.NewTicker(5 * time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("real ticker never fired")
	}
	if !c.Now().After(t0) {
		t.Fatal("real clock did not advance")
	}
}

func TestVirtualOnlyMovesWhenAdvanced(t *testing.T) {
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	v := NewVirtual(start)
	var c Clock = v

	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}

	tk := c.NewTicker(time.Second)
	defer tk.Stop()

	select {
	case <-tk.C():
		t.Fatal("virtual ticker fired without Advance")
	default:
	}

	v.Advance(time.Second)
	if !c.Now().Equal(start.Add(time.Second)) {
		t.Fatalf("Now() = %v after Advance", c.Now())
	}
	select {
	case at := <-tk.C():
		if !at.Equal(start.Add(time.Second)) {
			t.Fatalf("tick carried %v, want %v", at, start.Add(time.Second))
		}
	default:
		t.Fatal("virtual ticker did not fire after Advance")
	}
}

func TestVirtualAdvanceCoversMultiplePeriods(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(100 * time.Millisecond)
	defer tk.Stop()

	// A single large Advance must not lose ticks silently beyond the one-slot
	// buffer that time.Ticker also uses; it must deliver at least one and leave
	// the clock at the right instant.
	v.Advance(350 * time.Millisecond)
	select {
	case <-tk.C():
	default:
		t.Fatal("no tick after a multi-period Advance")
	}
	if got := v.Now().Sub(time.Unix(0, 0).UTC()); got != 350*time.Millisecond {
		t.Fatalf("clock at %v, want 350ms", got)
	}
}

func TestVirtualStopSilencesTicker(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(time.Second)
	tk.Stop()
	v.Advance(10 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker still fired")
	default:
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/clock/ -v`
Expected: FAIL — package does not build (`undefined: Clock`, `undefined: Real`, `undefined: NewVirtual`).

- [ ] **Step 3: Write minimal implementation**

`internal/clock/clock.go`:

```go
// Package clock supplies the time source the media pump is paced by.
//
// It governs pacing only. Determinism does not rest on it: the pump's unit of
// work is one access unit, and fault decisions key on a frame counter, so a
// deterministic test drives the pump directly and never consults a clock.
// Virtual exists so the driver loop can be exercised without real sleeping.
package clock

import (
	"sync"
	"time"
)

// Ticker is the subset of time.Ticker the pump driver needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock is a source of time.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// Real is the wall clock.
type Real struct{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// NewTicker returns a ticker backed by time.Ticker.
func (Real) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// Virtual is a clock that only moves when Advance is called.
type Virtual struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*virtualTicker
}

// NewVirtual returns a Virtual clock reading start.
func NewVirtual(start time.Time) *Virtual { return &Virtual{now: start} }

// Now returns the current virtual time.
func (v *Virtual) Now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.now
}

// NewTicker returns a ticker that fires during Advance.
func (v *Virtual) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive ticker interval")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	tk := &virtualTicker{
		ch:     make(chan time.Time, 1),
		period: d,
		next:   v.now.Add(d),
	}
	v.tickers = append(v.tickers, tk)
	return tk
}

// Advance moves the clock forward and delivers every tick that falls due.
//
// Delivery is non-blocking into a one-slot buffer, matching time.Ticker: a
// receiver that is not keeping up loses ticks rather than stalling the clock.
func (v *Virtual) Advance(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()

	target := v.now.Add(d)
	for _, tk := range v.tickers {
		for !tk.stopped && !tk.next.After(target) {
			select {
			case tk.ch <- tk.next:
			default:
			}
			tk.next = tk.next.Add(tk.period)
		}
	}
	v.now = target
}

type virtualTicker struct {
	ch      chan time.Time
	period  time.Duration
	next    time.Time
	stopped bool
}

func (t *virtualTicker) C() <-chan time.Time { return t.ch }
func (t *virtualTicker) Stop()               { t.stopped = true }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/clock/ -v`
Expected: PASS, all four tests.

Note for the implementer: `virtualTicker.Stop` writes `stopped` without holding `v.mu`, and `Advance` reads it while holding `v.mu`. Under `-race` with a concurrent `Stop` this is a data race. If the race detector flags it, give `Virtual` a `stop(tk)` method that takes `v.mu` and have `virtualTicker` hold a back-pointer to its `*Virtual`; do not paper over it with an atomic bool, because `next` is shared too.

- [ ] **Step 5: Commit**

```bash
git add internal/clock
git commit -m "Add clock abstraction with real and virtual sources

The clock paces the media pump. It deliberately carries no determinism
guarantee: the pump steps one access unit at a time and fault decisions key on
a frame counter, so determinism tests drive the pump directly. Virtual exists
to test the driver loop without sleeping."
```

---

### Task 3: Bundled fixture and media sources

Spec §5 requires one committed fixture, because §2.6 established there is no media anywhere on the account to inherit and camfarm's own tests must not need media sourced before they run.

**Fixture provenance — this resolves spec open item 3.** The fixture is a synthetic test pattern (`testsrc2`), generated by a committed script, encoded from nothing but that pattern. It contains no third-party footage, so it is redistributable under this repository's MIT licence with no further attribution. The generating command is committed so provenance is a runnable fact rather than a claim. Note honestly in the script that regeneration is *not* byte-reproducible across ffmpeg versions: the committed artifact is authoritative and the script documents where it came from.

**Files:**
- Create: `scripts/gen-fixture.sh`
- Create: `internal/media/fixture/fixture.ts` (generated, committed, ~40 KB)
- Create: `internal/media/fixture/fixture.go`
- Create: `internal/media/media.go`
- Create: `internal/media/mpegts.go`
- Create: `internal/media/file.go`
- Test: `internal/media/media_test.go`
- Modify: `go.mod` (add `mediacommon/v2`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Codec string`, `const CodecH264 Codec = "H264"`
  - `type AccessUnit struct { NALUs [][]byte; PTS, DTS int64; RandomAccess bool }`
  - `type Media struct { Codec Codec; SPS, PPS []byte; Width, Height int; FPS float64; AUs []AccessUnit }`
  - `func (m *Media) FrameDuration() int64` — 90 kHz units per access unit
  - `type Source interface { Load() (*Media, error); Deterministic() bool; Describe() string }`
  - `type FileSource struct { Path string }`, `type FixtureSource struct{}`
  - `fixture.Bytes() []byte`

- [ ] **Step 1: Generate and commit the fixture**

Create `scripts/gen-fixture.sh`:

```bash
#!/usr/bin/env bash
# Regenerates the bundled media fixture.
#
# Provenance: the fixture is a synthetic test pattern generated by this script.
# It contains no third-party footage, so it is redistributable under this
# repository's MIT licence.
#
# This script documents where the committed fixture came from. It is NOT a build
# step and it is NOT byte-reproducible: a different ffmpeg or libx264 build will
# encode the same pattern to different bytes. The committed fixture is
# authoritative. If you regenerate it and the properties asserted in
# internal/media/media_test.go change, update those constants deliberately and
# say why in the commit message.
#
# Generated with: ffmpeg 6.1.1
set -euo pipefail

out="$(dirname "$0")/../internal/media/fixture/fixture.ts"

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc2=size=320x240:rate=15" \
  -t 2 \
  -c:v libx264 -preset veryslow -crf 30 \
  -pix_fmt yuv420p \
  -g 15 -profile:v baseline -bf 0 \
  "$out"

echo "wrote $out ($(stat -c%s "$out") bytes)"
```

Then:

```bash
chmod +x scripts/gen-fixture.sh
mkdir -p internal/media/fixture
./scripts/gen-fixture.sh
```

Expected: writes roughly 40,000 bytes. The exact size will differ on a different ffmpeg build; that is fine and expected.

- [ ] **Step 2: Write the failing test**

`internal/media/media_test.go`:

```go
package media

import (
	"os"
	"path/filepath"
	"testing"
)

// These constants describe the committed fixture. They are measured, not
// chosen. If scripts/gen-fixture.sh is re-run on a different ffmpeg build and
// these change, update them deliberately.
const (
	fixtureWidth      = 320
	fixtureHeight     = 240
	fixtureFPS        = 15.0
	fixtureAUs        = 30
	fixtureProfileIdc = 66 // baseline
)

// fixtureKeyframes are the access-unit indices that can be randomly accessed.
// -g 15 at 15 fps over 2 seconds puts an IDR at 0 and 15.
var fixtureKeyframes = []int{0, 15}

func TestFixtureSourceProperties(t *testing.T) {
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	if m.Codec != CodecH264 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH264)
	}
	if m.Width != fixtureWidth || m.Height != fixtureHeight {
		t.Errorf("geometry = %dx%d, want %dx%d", m.Width, m.Height, fixtureWidth, fixtureHeight)
	}
	if m.FPS != fixtureFPS {
		t.Errorf("fps = %v, want %v", m.FPS, fixtureFPS)
	}
	if len(m.AUs) != fixtureAUs {
		t.Errorf("access units = %d, want %d", len(m.AUs), fixtureAUs)
	}
	if len(m.SPS) == 0 {
		t.Error("SPS is empty")
	}
	if len(m.PPS) == 0 {
		t.Error("PPS is empty")
	}

	var keyframes []int
	for i, au := range m.AUs {
		if au.RandomAccess {
			keyframes = append(keyframes, i)
		}
	}
	if len(keyframes) != len(fixtureKeyframes) {
		t.Fatalf("keyframes = %v, want %v", keyframes, fixtureKeyframes)
	}
	for i := range keyframes {
		if keyframes[i] != fixtureKeyframes[i] {
			t.Fatalf("keyframes = %v, want %v", keyframes, fixtureKeyframes)
		}
	}

	// A client that joins mid-stream needs an IDR to decode anything, so the
	// very first access unit must be one.
	if !m.AUs[0].RandomAccess {
		t.Error("first access unit is not a keyframe")
	}

	// Every AU must carry at least one non-empty NALU, or the RTP encoder panics.
	for i, au := range m.AUs {
		if len(au.NALUs) == 0 {
			t.Fatalf("access unit %d has no NALUs", i)
		}
		for j, n := range au.NALUs {
			if len(n) == 0 {
				t.Fatalf("access unit %d NALU %d is empty", i, j)
			}
		}
	}
}

// The parser must copy: mediacommon's Annex-B unmarshal returns slices that
// alias the reader's buffer, and the parsed media is shared immutably across
// every camera in the fleet.
func TestFixtureNALUsAreCopiedNotAliased(t *testing.T) {
	m1, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m2, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Mutating one load must not disturb another.
	m1.AUs[0].NALUs[0][0] ^= 0xFF
	if m2.AUs[0].NALUs[0][0] == m1.AUs[0].NALUs[0][0] {
		t.Fatal("two loads share backing memory")
	}
}

func TestFixtureTimestampsAreMonotonic(t *testing.T) {
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for i := 1; i < len(m.AUs); i++ {
		if m.AUs[i].DTS <= m.AUs[i-1].DTS {
			t.Fatalf("DTS not increasing at %d: %d then %d", i, m.AUs[i-1].DTS, m.AUs[i].DTS)
		}
	}
	if m.AUs[0].DTS != 0 {
		t.Errorf("first DTS = %d, want 0 (decoder rebases to the first timestamp)", m.AUs[0].DTS)
	}
}

func TestFrameDurationMatchesFPS(t *testing.T) {
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// 90 kHz / 15 fps = 6000.
	if got := m.FrameDuration(); got != 6000 {
		t.Errorf("FrameDuration() = %d, want 6000", got)
	}
}

func TestFileSourceMatchesFixtureSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy.ts")
	if err := os.WriteFile(path, fixtureBytesForTest(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fromFile, err := (&FileSource{Path: path}).Load()
	if err != nil {
		t.Fatalf("file load: %v", err)
	}
	fromEmbed, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("fixture load: %v", err)
	}
	if len(fromFile.AUs) != len(fromEmbed.AUs) {
		t.Fatalf("file gave %d AUs, embed gave %d", len(fromFile.AUs), len(fromEmbed.AUs))
	}
	if fromFile.Width != fromEmbed.Width || fromFile.Height != fromEmbed.Height {
		t.Fatal("geometry differs between file and embedded source")
	}
}

func TestFileSourceMissingFile(t *testing.T) {
	_, err := (&FileSource{Path: filepath.Join(t.TempDir(), "nope.ts")}).Load()
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestSourcesReportDeterminism(t *testing.T) {
	if !(FixtureSource{}).Deterministic() {
		t.Error("fixture source must be deterministic")
	}
	if !(&FileSource{Path: "x.ts"}).Deterministic() {
		t.Error("file source must be deterministic")
	}
	if (FixtureSource{}).Describe() == "" {
		t.Error("Describe() must not be empty")
	}
}
```

Add this helper in the same file, so the test can reach the embedded bytes without exporting them from the fixture package twice:

```go
func fixtureBytesForTest(t *testing.T) []byte {
	t.Helper()
	b := fixtureData()
	if len(b) == 0 {
		t.Fatal("embedded fixture is empty")
	}
	return b
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/media/ -v`
Expected: FAIL — package does not build (`undefined: FixtureSource`, `undefined: CodecH264`, `undefined: fixtureData`).

- [ ] **Step 4: Write the embedded fixture package**

`internal/media/fixture/fixture.go`:

```go
// Package fixture holds the bundled default media.
//
// Provenance: a synthetic test pattern generated by scripts/gen-fixture.sh. It
// contains no third-party footage and is redistributable under this
// repository's MIT licence. It is a minimal default for self-tests and for
// consumers with no media of their own; it is not representative of real camera
// output.
package fixture

import _ "embed"

//go:embed fixture.ts
var data []byte

// Bytes returns the bundled MPEG-TS fixture.
func Bytes() []byte { return data }
```

- [ ] **Step 5: Write the media types and parser**

`internal/media/media.go`:

```go
// Package media turns a source of encoded video into the immutable slice of
// access units the RTSP pumps serve.
//
// Media is parsed once and shared by every camera that uses the same source:
// that sharing is the scale lever, so a *Media must be treated as read-only
// once Load returns.
package media

// Codec identifies the video codec of a source.
type Codec string

// CodecH264 is H.264.
const CodecH264 Codec = "H264"

// AccessUnit is one coded picture, as a list of NAL units without start codes.
type AccessUnit struct {
	// NALUs are owned by this AccessUnit and must not be mutated.
	NALUs [][]byte
	// PTS and DTS are in 90 kHz units, rebased so the first access unit is 0.
	PTS int64
	DTS int64
	// RandomAccess reports whether a decoder can start here.
	RandomAccess bool
}

// Media is parsed, immutable source video.
type Media struct {
	Codec  Codec
	SPS    []byte
	PPS    []byte
	Width  int
	Height int
	// FPS is as declared by the bitstream. Zero when the stream carries no
	// timing information.
	FPS float64
	AUs []AccessUnit
}

// FrameDuration returns the nominal spacing between access units in 90 kHz
// units. It is derived from FPS when the bitstream declares timing, and from
// the mean DTS delta otherwise.
func (m *Media) FrameDuration() int64 {
	const clockRate = 90000
	if m.FPS > 0 {
		return int64(clockRate / m.FPS)
	}
	if len(m.AUs) < 2 {
		return clockRate / 30
	}
	span := m.AUs[len(m.AUs)-1].DTS - m.AUs[0].DTS
	return span / int64(len(m.AUs)-1)
}

// Source is somewhere media comes from.
type Source interface {
	// Load parses the source into immutable media.
	Load() (*Media, error)
	// Deterministic reports whether replaying this source yields identical
	// bytes. A live upstream feed does not, which places it outside camfarm's
	// reproducibility guarantee.
	Deterministic() bool
	// Describe returns a short human-readable identification, for logs and
	// inspection output.
	Describe() string
}
```

`internal/media/mpegts.go`:

```go
package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
)

// ErrNoH264Track is returned when a source carries no H.264 video.
var ErrNoH264Track = errors.New("media: no H264 track in source")

// parseMPEGTS reads an MPEG-TS stream into immutable media.
func parseMPEGTS(r io.Reader) (*Media, error) {
	mr := &mpegts.Reader{R: r}
	if err := mr.Initialize(); err != nil {
		return nil, fmt.Errorf("media: reading MPEG-TS: %w", err)
	}

	var track *mpegts.Track
	for _, t := range mr.Tracks() {
		if _, ok := t.Codec.(*mpegts.CodecH264); ok {
			track = t
			break
		}
	}
	if track == nil {
		return nil, ErrNoH264Track
	}

	m := &Media{Codec: CodecH264}

	// Separate decoders for PTS and DTS. One decoder fed both series would
	// interleave two sequences through a single 33-bit wraparound detector; that
	// happens to work when there are no B-frames but is wrong in general.
	var ptsDec, dtsDec mpegts.TimeDecoder
	ptsDec.Initialize()
	dtsDec.Initialize()

	var decodeErr error
	mr.OnDecodeError(func(err error) {
		if decodeErr == nil {
			decodeErr = err
		}
	})

	mr.OnDataH264(track, func(pts, dts int64, au [][]byte) error {
		// Copy every NALU. mediacommon's Annex-B unmarshal returns slices that
		// alias the reader's buffer, which is reused; the parsed media outlives
		// it and is shared across cameras.
		nalus := make([][]byte, 0, len(au))
		for _, n := range au {
			if len(n) == 0 {
				continue
			}
			c := make([]byte, len(n))
			copy(c, n)
			switch h264.NALUType(c[0] & 0x1F) {
			case h264.NALUTypeSPS:
				m.SPS = c
			case h264.NALUTypePPS:
				m.PPS = c
			}
			nalus = append(nalus, c)
		}
		if len(nalus) == 0 {
			return nil
		}
		m.AUs = append(m.AUs, AccessUnit{
			NALUs:        nalus,
			PTS:          ptsDec.Decode(pts),
			DTS:          dtsDec.Decode(dts),
			RandomAccess: h264.IsRandomAccess(nalus),
		})
		return nil
	})

	for {
		if err := mr.Read(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("media: reading MPEG-TS: %w", err)
		}
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("media: decoding MPEG-TS: %w", decodeErr)
	}
	if len(m.AUs) == 0 {
		return nil, errors.New("media: source contains no access units")
	}
	if len(m.SPS) == 0 || len(m.PPS) == 0 {
		return nil, errors.New("media: source carries no in-band SPS/PPS")
	}

	var sps h264.SPS
	if err := sps.Unmarshal(m.SPS); err != nil {
		return nil, fmt.Errorf("media: parsing SPS: %w", err)
	}
	m.Width = sps.Width()
	m.Height = sps.Height()
	m.FPS = sps.FPS()

	return m, nil
}
```

`internal/media/file.go`:

```go
package media

import (
	"bytes"
	"fmt"
	"os"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

// FileSource reads media from an MPEG-TS file on disk.
type FileSource struct {
	Path string
}

// Load parses the file.
func (s *FileSource) Load() (*Media, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, fmt.Errorf("media: opening source: %w", err)
	}
	defer f.Close()
	return parseMPEGTS(f)
}

// Deterministic reports true: a file replays identically.
func (s *FileSource) Deterministic() bool { return true }

// Describe identifies the source.
func (s *FileSource) Describe() string { return "file:" + s.Path }

// FixtureSource reads the media bundled with camfarm.
type FixtureSource struct{}

// Load parses the bundled fixture.
func (FixtureSource) Load() (*Media, error) { return parseMPEGTS(bytes.NewReader(fixture.Bytes())) }

// Deterministic reports true.
func (FixtureSource) Deterministic() bool { return true }

// Describe identifies the source.
func (FixtureSource) Describe() string { return "fixture:bundled" }

// fixtureData exposes the embedded bytes to this package's tests.
func fixtureData() []byte { return fixture.Bytes() }
```

- [ ] **Step 6: Add the dependency and run the tests**

```bash
go get github.com/bluenviron/mediacommon/v2@v2.9.4
go test -race ./internal/media/ -v
```

Expected: PASS. If the keyframe or access-unit assertions fail, the local ffmpeg produced a different encode than the one these constants were measured against — read the actual values from the failure, confirm they are sane (an IDR at index 0, keyframes every 15 frames), and update the constants in one commit that says so.

- [ ] **Step 7: Commit**

```bash
git add scripts/gen-fixture.sh internal/media go.mod go.sum
git commit -m "Add media sources and the bundled fixture

Media is parsed once into an immutable slice of access units, which is the
sharing lever the scale model rests on. NALUs are copied out of the reader,
because mediacommon's Annex-B unmarshal aliases a buffer that is reused.

The fixture is a synthetic test pattern generated by a committed script, so it
carries no third-party content and its provenance is a runnable fact. The
script notes that regeneration is not byte-reproducible; the committed artifact
is authoritative."
```

---

### Task 4: Inspection recorder

Spec §2.9 records that a real consumer reached for a leaked `*server.Server` pointer purely to assert state. This package is the first-class replacement.

**Files:**
- Create: `internal/obs/obs.go`
- Test: `internal/obs/obs_test.go`

**Interfaces:**
- Consumes: nothing. Deliberately imports no camfarm package — `internal/fault` will import this, so taking a `fault.Decision` here would be an import cycle. Fault events arrive as primitives.
- Produces:
  - `type Event struct { Seq uint64; CameraID string; FrameIndex int; Kind, Detail string }`
  - `type Counters struct { FramesServed, LoopsCompleted, FaultsFired uint64 }`
  - `type Recorder struct{...}`, `func New(maxEvents int) *Recorder`
  - `func (*Recorder) Frame(cameraID string)`, `Loop(cameraID string)`, `Fault(cameraID string, frameIndex int, kind, detail string)`
  - `func (*Recorder) Counters(cameraID string) Counters`, `Events() []Event`, `Dropped() uint64`

- [ ] **Step 1: Write the failing test**

`internal/obs/obs_test.go`:

```go
package obs

import (
	"sync"
	"testing"
)

func TestCountersPerCamera(t *testing.T) {
	r := New(100)

	r.Frame("a")
	r.Frame("a")
	r.Frame("b")
	r.Loop("a")
	r.Fault("b", 7, "frame_drop", "decided")

	if got := r.Counters("a"); got.FramesServed != 2 || got.LoopsCompleted != 1 || got.FaultsFired != 0 {
		t.Errorf("camera a: %+v", got)
	}
	if got := r.Counters("b"); got.FramesServed != 1 || got.LoopsCompleted != 0 || got.FaultsFired != 1 {
		t.Errorf("camera b: %+v", got)
	}
	if got := r.Counters("unknown"); got != (Counters{}) {
		t.Errorf("unknown camera: %+v, want zero", got)
	}
}

// Spec section 8.2: a fault firing records enough to reproduce it.
func TestFaultEventsCarryFrameIndexAndKind(t *testing.T) {
	r := New(100)
	r.Fault("front-door", 42, "keyframe_starvation", "decided; effect not implemented")

	ev := r.Events()
	if len(ev) != 1 {
		t.Fatalf("events = %d, want 1", len(ev))
	}
	if ev[0].CameraID != "front-door" || ev[0].FrameIndex != 42 || ev[0].Kind != "keyframe_starvation" {
		t.Fatalf("event = %+v", ev[0])
	}
	if ev[0].Seq != 1 {
		t.Errorf("Seq = %d, want 1", ev[0].Seq)
	}
}

// Only faults are logged as events. Frames are far too many to log and are
// counted instead.
func TestFramesAreCountedNotLogged(t *testing.T) {
	r := New(100)
	for i := 0; i < 50; i++ {
		r.Frame("a")
	}
	if n := len(r.Events()); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
	if got := r.Counters("a").FramesServed; got != 50 {
		t.Fatalf("FramesServed = %d, want 50", got)
	}
}

// A fleet may run for hours. The log is bounded, and says so rather than
// silently forgetting.
func TestEventLogIsBoundedAndReportsDrops(t *testing.T) {
	r := New(4)
	for i := 0; i < 10; i++ {
		r.Fault("a", i, "frame_drop", "")
	}
	ev := r.Events()
	if len(ev) != 4 {
		t.Fatalf("events = %d, want 4", len(ev))
	}
	// The most recent must be kept: a failing test cares about what just
	// happened.
	if ev[len(ev)-1].FrameIndex != 9 {
		t.Errorf("last event frame = %d, want 9", ev[len(ev)-1].FrameIndex)
	}
	if got := r.Dropped(); got != 6 {
		t.Errorf("Dropped() = %d, want 6", got)
	}
}

func TestEventsReturnsACopy(t *testing.T) {
	r := New(10)
	r.Fault("a", 1, "k", "")
	ev := r.Events()
	ev[0].CameraID = "mutated"
	if r.Events()[0].CameraID != "a" {
		t.Fatal("Events() exposed internal storage")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Frame("a")
				r.Fault("a", j, "frame_drop", "")
				_ = r.Counters("a")
				_ = r.Events()
			}
		}()
	}
	wg.Wait()
	if got := r.Counters("a").FramesServed; got != 800 {
		t.Fatalf("FramesServed = %d, want 800", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/obs/ -v`
Expected: FAIL — package does not build (`undefined: New`).

- [ ] **Step 3: Write minimal implementation**

`internal/obs/obs.go`:

```go
// Package obs records what a fleet did, so a test can assert it and a failing
// test can report enough to reproduce it.
//
// It imports no other camfarm package on purpose: the fault engine records into
// it, so accepting a fault type here would be an import cycle. Fault events
// arrive as primitives.
package obs

import "sync"

// Event is one recorded fault decision.
type Event struct {
	// Seq numbers events from 1, across all cameras, in the order recorded.
	Seq        uint64
	CameraID   string
	FrameIndex int
	Kind       string
	Detail     string
}

// Counters are per-camera totals.
type Counters struct {
	FramesServed   uint64
	LoopsCompleted uint64
	FaultsFired    uint64
}

// Recorder collects counters and a bounded event log. It is safe for concurrent
// use.
type Recorder struct {
	mu        sync.Mutex
	maxEvents int
	seq       uint64
	dropped   uint64
	events    []Event
	counters  map[string]*Counters
}

// New returns a Recorder keeping at most maxEvents events. A non-positive
// maxEvents means keep none.
func New(maxEvents int) *Recorder {
	return &Recorder{
		maxEvents: maxEvents,
		counters:  make(map[string]*Counters),
	}
}

// counterFor returns the counters for id, creating them if needed. Caller holds
// r.mu.
func (r *Recorder) counterFor(id string) *Counters {
	c, ok := r.counters[id]
	if !ok {
		c = &Counters{}
		r.counters[id] = c
	}
	return c
}

// Frame records one access unit served. Frames are counted, never logged: there
// are far too many of them to keep individually.
func (r *Recorder) Frame(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counterFor(cameraID).FramesServed++
}

// Loop records one completed pass over the source media.
func (r *Recorder) Loop(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counterFor(cameraID).LoopsCompleted++
}

// Fault records a fault decision that fired.
func (r *Recorder) Fault(cameraID string, frameIndex int, kind, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counterFor(cameraID).FaultsFired++
	r.seq++

	if r.maxEvents <= 0 {
		r.dropped++
		return
	}
	ev := Event{
		Seq:        r.seq,
		CameraID:   cameraID,
		FrameIndex: frameIndex,
		Kind:       kind,
		Detail:     detail,
	}
	if len(r.events) == r.maxEvents {
		// Keep the most recent: a failing test cares about what just happened.
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = ev
		r.dropped++
		return
	}
	r.events = append(r.events, ev)
}

// Counters returns a snapshot for one camera. An unknown camera reads as zero.
func (r *Recorder) Counters(cameraID string) Counters {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[cameraID]; ok {
		return *c
	}
	return Counters{}
}

// Events returns a copy of the retained event log, oldest first.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Dropped returns how many events were discarded because the log was full.
func (r *Recorder) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/obs/ -v`
Expected: PASS, all six tests.

- [ ] **Step 5: Commit**

```bash
git add internal/obs
git commit -m "Add inspection recorder

Per-camera counters plus a bounded fault-event log, replacing the leaked server
pointer a consumer of the upstream virtual camera had to reach for. Frames are
counted rather than logged; the log keeps the most recent events and reports how
many it dropped, so a long-running fleet degrades visibly instead of silently."
```

---

### Task 5: Fault engine — seeded decisions, no effects

This is the seam spec §7.7 requires in v1: the decision machinery real and tested, no decision reaching the wire. Building it now is not optional polish — a seam added after the fact would not be reachable from code paths written without it (spec §4).

**Files:**
- Create: `internal/fault/fault.go`
- Test: `internal/fault/fault_test.go`

**Interfaces:**
- Consumes: `seed.Seed` (`Stream(label)`, `Rand()`).
- Produces:
  - `type Kind string` with `KindFrameDrop`, `KindKeyframeStarvation`, `KindTeardownMidSession`, `KindAcceptThenSilence`, `KindSDPCodecLie`, `KindSPSResolutionLie`
  - `type Spec struct { Kind Kind; Rate float64 }`
  - `type Decision struct { Kind Kind; FrameIndex int }`
  - `func New(s seed.Seed, specs []Spec) (*Engine, error)`
  - `func (*Engine) DecideFrame(frameIndex int) []Decision`
  - `func (*Engine) Enabled() bool`
  - `var ErrUnknownKind`, `var ErrBadRate`

Design constraints the implementer must honour:

- **One generator per kind**, seeded from `s.Stream(string(kind))`. Adding a kind must not shift another kind's draw sequence (spec §8).
- **Iterate `specs` in slice order, never a map.** Map iteration order is randomised and would make the decision sequence differ run to run.
- `DecideFrame` returns only decisions that fired, so an empty return is the normal case and callers do not branch on `Fire` flags.

- [ ] **Step 1: Write the failing test**

`internal/fault/fault_test.go`:

```go
package fault

import (
	"errors"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

func TestNoSpecsDecidesNothing(t *testing.T) {
	e, err := New(seed.Seed(1), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Enabled() {
		t.Error("Enabled() true with no specs")
	}
	for i := 0; i < 1000; i++ {
		if d := e.DecideFrame(i); len(d) != 0 {
			t.Fatalf("frame %d decided %v with no specs configured", i, d)
		}
	}
}

// The core guarantee: same seed, same spec, same sequence of decisions.
func TestSameSeedSameDecisionSequence(t *testing.T) {
	specs := []Spec{{Kind: KindFrameDrop, Rate: 0.3}}

	run := func() []Decision {
		e, err := New(seed.Seed(0x3f2a9c81).Camera(4), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var out []Decision
		for i := 0; i < 500; i++ {
			out = append(out, e.DecideFrame(i)...)
		}
		return out
	}

	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("rate 0.3 over 500 frames fired nothing; the generator is not being drawn from")
	}
	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("decision %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestDifferentCamerasDecideDifferently(t *testing.T) {
	specs := []Spec{{Kind: KindFrameDrop, Rate: 0.5}}
	root := seed.Seed(0x3f2a9c81)

	collect := func(idx int) []Decision {
		e, err := New(root.Camera(idx), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var out []Decision
		for i := 0; i < 200; i++ {
			out = append(out, e.DecideFrame(i)...)
		}
		return out
	}

	a, b := collect(0), collect(1)
	if len(a) == len(b) {
		same := true
		for i := range a {
			if a[i] != b[i] {
				same = false
				break
			}
		}
		if same {
			t.Fatal("two cameras produced an identical decision sequence")
		}
	}
}

// Spec section 8: adding a fault kind must not perturb an existing kind's
// sequence. This is why each kind draws from its own generator.
func TestAddingAKindDoesNotShiftAnother(t *testing.T) {
	base := []Spec{{Kind: KindFrameDrop, Rate: 0.4}}
	extended := []Spec{
		{Kind: KindFrameDrop, Rate: 0.4},
		{Kind: KindKeyframeStarvation, Rate: 0.4},
	}

	dropsFrom := func(specs []Spec) []int {
		e, err := New(seed.Seed(99), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var frames []int
		for i := 0; i < 300; i++ {
			for _, d := range e.DecideFrame(i) {
				if d.Kind == KindFrameDrop {
					frames = append(frames, d.FrameIndex)
				}
			}
		}
		return frames
	}

	before, after := dropsFrom(base), dropsFrom(extended)
	if len(before) != len(after) {
		t.Fatalf("frame-drop count changed when another kind was added: %d vs %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("frame-drop decision %d moved from frame %d to %d", i, before[i], after[i])
		}
	}
}

func TestRateBoundaries(t *testing.T) {
	e, err := New(seed.Seed(7), []Spec{{Kind: KindFrameDrop, Rate: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 50; i++ {
		if len(e.DecideFrame(i)) != 1 {
			t.Fatalf("rate 1 did not fire at frame %d", i)
		}
	}

	e0, err := New(seed.Seed(7), []Spec{{Kind: KindFrameDrop, Rate: 0}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 50; i++ {
		if len(e0.DecideFrame(i)) != 0 {
			t.Fatalf("rate 0 fired at frame %d", i)
		}
	}
}

func TestDecisionCarriesFrameIndex(t *testing.T) {
	e, err := New(seed.Seed(7), []Spec{{Kind: KindFrameDrop, Rate: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := e.DecideFrame(123)
	if len(d) != 1 {
		t.Fatalf("decisions = %d, want 1", len(d))
	}
	if d[0].FrameIndex != 123 {
		t.Errorf("FrameIndex = %d, want 123", d[0].FrameIndex)
	}
	if d[0].Kind != KindFrameDrop {
		t.Errorf("Kind = %q", d[0].Kind)
	}
}

func TestRejectsUnknownKindAndBadRate(t *testing.T) {
	if _, err := New(seed.Seed(1), []Spec{{Kind: "not-a-fault", Rate: 0.5}}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown kind: err = %v, want ErrUnknownKind", err)
	}
	for _, rate := range []float64{-0.1, 1.1} {
		if _, err := New(seed.Seed(1), []Spec{{Kind: KindFrameDrop, Rate: rate}}); !errors.Is(err, ErrBadRate) {
			t.Errorf("rate %v: err = %v, want ErrBadRate", rate, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/fault/ -v`
Expected: FAIL — package does not build (`undefined: New`, `undefined: KindFrameDrop`).

- [ ] **Step 3: Write minimal implementation**

`internal/fault/fault.go`:

```go
// Package fault decides, deterministically, when a camera should misbehave.
//
// It decides only. In this version no decision changes a byte on the wire: the
// seam exists so that the code paths carrying media and control responses are
// written with an interception point from the start, because a seam added later
// would not be reachable from paths built without one.
//
// Every decision is a pure function of the camera's seed and the frame counter.
// Nothing here consults wall-clock time, goroutine order, or map iteration
// order.
package fault

import (
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/0x524a/camfarm/internal/seed"
)

// Kind identifies a fault. Each names a class of real field bug; see the
// architecture design, section 7.
type Kind string

// The catalogued faults. Only the decision half of each exists in this version.
const (
	// KindFrameDrop drops access units at a rate. Reproduces decoders and
	// trackers that assume contiguous frames.
	KindFrameDrop Kind = "frame_drop"
	// KindKeyframeStarvation withholds IDRs, so a client joining mid-stream
	// never gets one. The "works on camera restart, breaks on reconnect" bug.
	KindKeyframeStarvation Kind = "keyframe_starvation"
	// KindTeardownMidSession closes a live session. Reproduces reconnect logic
	// and resource leaks.
	KindTeardownMidSession Kind = "teardown_mid_session"
	// KindAcceptThenSilence accepts the TCP connection and never answers.
	KindAcceptThenSilence Kind = "accept_then_silence"
	// KindSDPCodecLie advertises one codec and sends another.
	KindSDPCodecLie Kind = "sdp_codec_lie"
	// KindSPSResolutionLie advertises a geometry the frames do not match.
	KindSPSResolutionLie Kind = "sps_resolution_lie"
)

// Errors reported by New.
var (
	ErrUnknownKind = errors.New("fault: unknown kind")
	ErrBadRate     = errors.New("fault: rate must be in [0,1]")
)

// knownKinds is the catalogue. Membership here means the kind can be named and
// decided, not that its effect is implemented.
var knownKinds = map[Kind]bool{
	KindFrameDrop:          true,
	KindKeyframeStarvation: true,
	KindTeardownMidSession: true,
	KindAcceptThenSilence:  true,
	KindSDPCodecLie:        true,
	KindSPSResolutionLie:   true,
}

// Known reports whether kind is catalogued.
func Known(kind Kind) bool { return knownKinds[kind] }

// Spec configures one fault.
type Spec struct {
	Kind Kind
	// Rate is the per-frame probability for rate-based kinds, in [0,1].
	Rate float64
}

// Decision is a fault that fired.
type Decision struct {
	Kind       Kind
	FrameIndex int
}

// Engine makes fault decisions for one camera.
//
// It is not safe for concurrent use. Each camera owns one, driven by that
// camera's pump, so nothing is shared and no lock is needed.
type Engine struct {
	entries []entry
}

type entry struct {
	spec Spec
	rnd  *rand.Rand
}

// New returns an Engine for one camera. Each kind draws from its own generator,
// seeded by kind name, so introducing a kind leaves every other kind's sequence
// byte-identical.
func New(s seed.Seed, specs []Spec) (*Engine, error) {
	e := &Engine{}
	for _, sp := range specs {
		if !knownKinds[sp.Kind] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, sp.Kind)
		}
		if sp.Rate < 0 || sp.Rate > 1 {
			return nil, fmt.Errorf("%w: %v for %q", ErrBadRate, sp.Rate, sp.Kind)
		}
		e.entries = append(e.entries, entry{
			spec: sp,
			rnd:  s.Stream(string(sp.Kind)).Rand(),
		})
	}
	return e, nil
}

// Enabled reports whether any fault is configured.
func (e *Engine) Enabled() bool { return len(e.entries) > 0 }

// DecideFrame returns the faults that fire for the given frame counter.
//
// The counter is monotonic across loops of the source media, so a decision is
// tied to a position in the run rather than to a position in the file.
//
// Entries are walked in configured order, never map order, so the draw sequence
// is reproducible.
func (e *Engine) DecideFrame(frameIndex int) []Decision {
	var out []Decision
	for i := range e.entries {
		en := &e.entries[i]
		switch en.spec.Kind {
		case KindFrameDrop, KindKeyframeStarvation:
			// Draw unconditionally so the sequence does not depend on earlier
			// outcomes.
			if en.rnd.Float64() < en.spec.Rate {
				out = append(out, Decision{Kind: en.spec.Kind, FrameIndex: frameIndex})
			}
		default:
			// Session, transport and control-plane faults are not frame-scoped.
			// They are catalogued and validated, and decided elsewhere when
			// their effects land.
		}
	}
	return out
}
```

Note on `TestRateBoundaries`: `rand.Float64` returns a value in `[0,1)`, so `< 1` is always true and `< 0` never is. Both boundary assertions therefore hold exactly.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/fault/ -v`
Expected: PASS, all seven tests.

- [ ] **Step 5: Commit**

```bash
git add internal/fault
git commit -m "Add seeded fault-decision engine

Decisions only: nothing here changes a byte on the wire yet. The seam is built
now because one added later would not be reachable from code paths written
without it.

Each kind draws from its own generator, keyed by kind name, so adding a fault
type leaves every existing type's sequence byte-identical. Entries are walked in
configured order rather than map order, because Go randomises the latter."
```

---

### Task 6: RTSP server — N cameras, one listener

Spec §10.2: one process, one RTSP server with N `ServerStream`s dispatched in `OnDescribe` by `ctx.Path`. This task lands the control plane with no media flowing; Task 7 adds the pump.

**Files:**
- Create: `internal/rtsp/server.go`
- Test: `internal/rtsp/server_test.go`
- Modify: `go.mod` (add `gortsplib/v5`)

**Interfaces:**
- Consumes: `media.Media`, `seed.Seed`, `fault.Spec`, `obs.Recorder`, `clock.Clock`.
- Produces:
  - `type CameraConfig struct { ID string; Media *media.Media; Seed seed.Seed; Faults []fault.Spec }`
  - `type Config struct { Host string; Port int; Log *slog.Logger; Clock clock.Clock; Obs *obs.Recorder; Cameras []CameraConfig }`
  - `func New(cfg Config) (*Server, error)`
  - `func (*Server) Start() error`, `Close()`, `Addr() *net.TCPAddr`
  - `func (*Server) Has(id string) bool`, `Path(id string) string`, `URL(id string) string`
  - `func (*Server) Readers(id string) int`
  - `func (*Server) StreamStats(id string) (packets, bytes uint64)`
  - `var ErrNotStarted`

Two traps the implementer must respect (spec §10.4):

- `ServerStream.Initialize()` requires an already-started server, but the handler must be live before the first connection and `Start()` launches the accept loop immediately. Hold the write lock across the whole sequence — start, then initialize every stream, then unlock. Read handlers take the read lock, so early connections block rather than racing. `rtspeek` assigns its handler *after* `Start()` in all three of its server tests; that is the pattern to avoid, not to copy.
- `Server.NetListener()` panics if `Start()` has not succeeded. `Addr()` must return nil rather than panicking when called too early.

- [ ] **Step 1: Write the failing test**

`internal/rtsp/server_test.go`:

```go
package rtsp

import (
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

func testMedia(t *testing.T) *media.Media {
	t.Helper()
	m, err := (media.FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return m
}

// startServer brings up n cameras named cam-00..cam-(n-1) and returns it.
func startServer(t *testing.T, n int) *Server {
	t.Helper()
	m := testMedia(t)
	root := seed.Seed(0x3f2a9c81)

	cfg := Config{Host: "127.0.0.1", Obs: obs.New(1000)}
	for i := 0; i < n; i++ {
		cfg.Cameras = append(cfg.Cameras, CameraConfig{
			ID:    cameraID(i),
			Media: m,
			Seed:  root.Camera(i),
		})
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func cameraID(i int) string {
	return "cam-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func TestAddrIsNilBeforeStart(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(10), Cameras: []CameraConfig{
		{ID: "a", Media: testMedia(t), Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Addr() != nil {
		t.Fatal("Addr() non-nil before Start()")
	}
	if _, _, err := s.streamStatsErr("a"); err == nil {
		t.Fatal("expected ErrNotStarted before Start()")
	}
}

// Port 0 must be resolved and readable back, so a test never has to guess a
// free port.
func TestEphemeralPortIsReadBack(t *testing.T) {
	s := startServer(t, 1)
	addr := s.Addr()
	if addr == nil {
		t.Fatal("Addr() is nil after Start()")
	}
	if addr.Port == 0 {
		t.Fatal("port was not resolved")
	}
	if !strings.HasPrefix(s.URL("cam-00"), "rtsp://127.0.0.1:") {
		t.Errorf("URL = %q", s.URL("cam-00"))
	}
	if !strings.HasSuffix(s.URL("cam-00"), "/cam-00") {
		t.Errorf("URL = %q", s.URL("cam-00"))
	}
}

func TestDescribeAdvertisesTheSourceParameterSets(t *testing.T) {
	s := startServer(t, 1)
	want := testMedia(t)

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()

	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if len(desc.Medias) != 1 {
		t.Fatalf("medias = %d, want 1", len(desc.Medias))
	}

	var forma *format.H264
	if desc.FindFormat(&forma) == nil {
		t.Fatal("no H264 format in the SDP")
	}
	if string(forma.SPS) != string(want.SPS) {
		t.Error("advertised SPS differs from the source SPS")
	}
	if string(forma.PPS) != string(want.PPS) {
		t.Error("advertised PPS differs from the source PPS")
	}
	if forma.PacketizationMode != 1 {
		t.Errorf("packetization mode = %d, want 1", forma.PacketizationMode)
	}
}

func TestUnknownCameraIsNotFound(t *testing.T) {
	s := startServer(t, 1)

	u, err := base.ParseURL(strings.Replace(s.URL("cam-00"), "/cam-00", "/nope", 1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()

	if _, _, err := c.Describe(u); err == nil {
		t.Fatal("DESCRIBE of an unknown camera succeeded")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404", err)
	}
}

// Every camera must be independently addressable behind the one listener.
func TestFleetIsIndependentlyAddressable(t *testing.T) {
	const n = 25
	s := startServer(t, n)

	for i := 0; i < n; i++ {
		id := cameraID(i)
		u, err := base.ParseURL(s.URL(id))
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		if err := c.Start(); err != nil {
			t.Fatalf("client start %s: %v", id, err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			c.Close()
			t.Fatalf("DESCRIBE %s: %v", id, err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			c.Close()
			t.Fatalf("SETUP %s: %v", id, err)
		}
		if _, err := c.Play(nil); err != nil {
			c.Close()
			t.Fatalf("PLAY %s: %v", id, err)
		}
		c.Close()
	}
}

// gortsplib exposes no reader count, so the server tracks it. Spec section 2.9:
// a consumer needs to assert connected clients without a leaked pointer.
func TestReaderCountRisesAndFalls(t *testing.T) {
	s := startServer(t, 1)

	if got := s.Readers("cam-00"); got != 0 {
		t.Fatalf("readers = %d before any client, want 0", got)
	}

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}

	if got := waitForReaders(s, "cam-00", 1); got != 1 {
		t.Fatalf("readers = %d with one client, want 1", got)
	}

	c.Close()

	if got := waitForReaders(s, "cam-00", 0); got != 0 {
		t.Fatalf("readers = %d after close, want 0", got)
	}
}

// waitForReaders polls briefly: session teardown is asynchronous on the server.
func waitForReaders(s *Server, id string, want int) int {
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.Readers(id)
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRejectsBadConfig(t *testing.T) {
	m := testMedia(t)
	cases := map[string]Config{
		"no cameras":   {Host: "127.0.0.1", Obs: obs.New(1)},
		"empty id":     {Host: "127.0.0.1", Obs: obs.New(1), Cameras: []CameraConfig{{ID: "", Media: m}}},
		"nil media":    {Host: "127.0.0.1", Obs: obs.New(1), Cameras: []CameraConfig{{ID: "a"}}},
		"duplicate id": {Host: "127.0.0.1", Obs: obs.New(1), Cameras: []CameraConfig{{ID: "a", Media: m}, {ID: "a", Media: m}}},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}
```

Add this small accessor to the server so `TestAddrIsNilBeforeStart` can assert the not-started error without exporting a second stats method:

```go
// streamStatsErr is StreamStats with the error surfaced, for tests.
func (s *Server) streamStatsErr(id string) (packets, bytes uint64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.srv == nil {
		return 0, 0, ErrNotStarted
	}
	cam, ok := s.cams[id]
	if !ok {
		return 0, 0, ErrNotStarted
	}
	st := cam.stream.Stats()
	return st.OutboundRTPPackets, st.OutboundBytes, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/rtsp/ -v`
Expected: FAIL — package does not build (`undefined: New`, `undefined: Config`).

- [ ] **Step 3: Write minimal implementation**

`internal/rtsp/server.go`:

```go
// Package rtsp serves the fleet's synthetic streams.
//
// One gortsplib server holds N ServerStreams and dispatches by request path, so
// a fleet costs one listener and one accept loop regardless of camera count. The
// media behind those streams is parsed once and shared; per-camera state is only
// an encoder and a position in the loop.
package rtsp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// ErrNotStarted is returned when the server has not been started yet.
var ErrNotStarted = errors.New("rtsp: server not started")

// CameraConfig describes one camera.
type CameraConfig struct {
	ID     string
	Media  *media.Media
	Seed   seed.Seed
	Faults []fault.Spec
}

// Config configures the server.
type Config struct {
	// Host to bind. Defaults to 127.0.0.1: a test fixture should not be
	// reachable from the network by accident.
	Host string
	// Port to bind. Zero requests an ephemeral port, readable back via Addr.
	Port int
	// Log receives server events. Nil means silent. Never stdout: a consumer
	// runs camfarm inside a process whose stdout is a protocol stream.
	Log *slog.Logger
	// Clock paces the pumps. Nil means the real clock.
	Clock clock.Clock
	// Obs records what happened. Required.
	Obs *obs.Recorder

	Cameras []CameraConfig
}

type camera struct {
	id     string
	cfg    CameraConfig
	stream *gortsplib.ServerStream
	medi   *description.Media
	forma  *format.H264
}

// Server serves a fleet of synthetic RTSP cameras.
type Server struct {
	cfg Config
	log *slog.Logger
	ck  clock.Clock

	mu      sync.RWMutex
	srv     *gortsplib.Server
	cams    map[string]*camera
	order   []string
	readers map[*gortsplib.ServerSession]string
}

// New validates cfg and returns an unstarted server.
func New(cfg Config) (*Server, error) {
	if cfg.Obs == nil {
		return nil, errors.New("rtsp: Config.Obs is required")
	}
	if len(cfg.Cameras) == 0 {
		return nil, errors.New("rtsp: no cameras configured")
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}

	s := &Server{
		cfg:     cfg,
		log:     cfg.Log,
		ck:      cfg.Clock,
		cams:    make(map[string]*camera, len(cfg.Cameras)),
		readers: make(map[*gortsplib.ServerSession]string),
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.ck == nil {
		s.ck = clock.Real{}
	}

	for _, cc := range cfg.Cameras {
		if cc.ID == "" {
			return nil, errors.New("rtsp: camera with an empty ID")
		}
		if strings.ContainsAny(cc.ID, "/?#") {
			return nil, fmt.Errorf("rtsp: camera ID %q contains a character reserved in a URL path", cc.ID)
		}
		if cc.Media == nil {
			return nil, fmt.Errorf("rtsp: camera %q has no media", cc.ID)
		}
		if len(cc.Media.SPS) == 0 || len(cc.Media.PPS) == 0 {
			return nil, fmt.Errorf("rtsp: camera %q media carries no SPS/PPS", cc.ID)
		}
		if _, dup := s.cams[cc.ID]; dup {
			return nil, fmt.Errorf("rtsp: duplicate camera ID %q", cc.ID)
		}
		s.cams[cc.ID] = &camera{id: cc.ID, cfg: cc}
		s.order = append(s.order, cc.ID)
	}
	return s, nil
}

// Start binds the listener and initializes every stream.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv != nil {
		return errors.New("rtsp: already started")
	}

	srv := &gortsplib.Server{
		Handler:     s,
		RTSPAddress: net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)),
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("rtsp: starting server: %w", err)
	}

	// ServerStream.Initialize needs a started server, and the handler must
	// already be live because Start launched the accept loop. Holding the write
	// lock for the whole sequence is what makes that safe: read handlers block
	// until every stream exists.
	for _, id := range s.order {
		cam := s.cams[id]
		cam.forma = &format.H264{
			PayloadTyp:        96,
			SPS:               cam.cfg.Media.SPS,
			PPS:               cam.cfg.Media.PPS,
			PacketizationMode: 1,
		}
		cam.medi = &description.Media{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{cam.forma},
		}
		cam.stream = &gortsplib.ServerStream{
			Server: srv,
			Desc:   &description.Session{Medias: []*description.Media{cam.medi}},
		}
		if err := cam.stream.Initialize(); err != nil {
			for _, done := range s.order {
				if c := s.cams[done]; c.stream != nil && c.id != id {
					c.stream.Close()
				}
			}
			srv.Close()
			return fmt.Errorf("rtsp: initializing stream for %q: %w", id, err)
		}
	}

	s.srv = srv
	s.log.Info("rtsp server started", "addr", srv.NetListener().Addr().String(), "cameras", len(s.order))
	return nil
}

// Close stops the server and every stream.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return
	}
	for _, id := range s.order {
		if cam := s.cams[id]; cam.stream != nil {
			cam.stream.Close()
			cam.stream = nil
		}
	}
	s.srv.Close()
	s.srv = nil
}

// Addr returns the bound address, or nil before a successful Start.
func (s *Server) Addr() *net.TCPAddr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.srv == nil {
		return nil
	}
	// NetListener panics before Start succeeds, which is why this is guarded.
	addr, ok := s.srv.NetListener().Addr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return addr
}

// Has reports whether a camera exists.
func (s *Server) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.cams[id]
	return ok
}

// Path returns the request path a camera answers on.
func (s *Server) Path(id string) string { return "/" + id }

// URL returns the full RTSP URL of a camera, or "" before Start.
func (s *Server) URL(id string) string {
	addr := s.Addr()
	if addr == nil {
		return ""
	}
	return "rtsp://" + addr.String() + s.Path(id)
}

// Readers returns the number of sessions currently set up against a camera.
//
// gortsplib exposes no reader count of its own, so this is tracked here.
func (s *Server) Readers(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, camID := range s.readers {
		if camID == id {
			n++
		}
	}
	return n
}

// StreamStats returns outbound RTP packet and byte counts for a camera.
func (s *Server) StreamStats(id string) (packets, bytes uint64) {
	p, b, _ := s.streamStatsErr(id)
	return p, b
}

// lookup resolves a request path to a camera.
func (s *Server) lookup(path string) *camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cams[strings.Trim(path, "/")]
}

// --- gortsplib handlers ---
//
// gortsplib's ServerHandler is `any`; a server implements whichever of the
// optional interfaces it needs.

// OnConnClose logs connection teardown.
func (s *Server) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	s.log.Debug("connection closed", "err", ctx.Error)
}

// OnSessionClose drops the session's reader registration.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.readers, ctx.Session)
}

// OnDescribe answers DESCRIBE by path.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	cam := s.lookup(ctx.Path)
	if cam == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, cam.stream, nil
}

// OnSetup answers SETUP by path and registers the session as a reader.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	cam := s.lookup(ctx.Path)
	if cam == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.mu.Lock()
	s.readers[ctx.Session] = cam.id
	s.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, cam.stream, nil
}

// OnPlay answers PLAY.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	if cam := s.lookup(ctx.Path); cam == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnResponse is the header-level fault seam.
//
// gortsplib sets Content-Base, Content-Type, CSeq, RTP-Info and the SDP body
// after a handler returns, so a handler cannot lie about them. This hook
// receives the same live response immediately before it is marshalled, which is
// the only place those headers can be rewritten. No fault rewrites anything in
// this version; the hook exists so the phase-2 faults have somewhere to live.
func (s *Server) OnResponse(_ *gortsplib.ServerConn, _ *base.Response) {}
```

- [ ] **Step 4: Add the dependency and run the tests**

```bash
go get github.com/bluenviron/gortsplib/v5@v5.6.5
go test -race ./internal/rtsp/ -v
```

Expected: PASS. Note the client will log `switching to TCP because server requested it` — the server configures no UDP ports, so it forces TCP interleaved. That is expected and is the gortsplib *client* logging, not this server.

- [ ] **Step 5: Commit**

```bash
git add internal/rtsp go.mod go.sum
git commit -m "Add RTSP server with per-path camera dispatch

One listener holds N streams and dispatches on request path, so a fleet costs
one accept loop. Setup holds the write lock across start-then-initialize,
because ServerStream.Initialize needs a started server while the handler must
already be live.

Reader counts are tracked here because gortsplib exposes none, and a consumer
needs to assert connected clients without being handed an internal pointer.
OnResponse is wired as a no-op: it is the only point at which the headers
gortsplib writes after a handler returns can be rewritten, so phase-2 faults
need it to exist now."
```

---

### Task 7: Media pump

The pump's unit of work is one access unit, exposed as `Step()`. That is the decision that makes determinism testable without a virtual clock: fault decisions key on a frame counter, so a test calls `Step()` in a loop and asserts an exact sequence, while production calls it from a clock-paced goroutine (spec §8.1).

The pump writes through a narrow interface rather than straight to `*gortsplib.ServerStream`, so its tests are hermetic and can assert exact sequence numbers with no socket involved.

**Files:**
- Create: `internal/rtsp/pump.go`
- Test: `internal/rtsp/pump_test.go`
- Modify: `internal/rtsp/server.go` (create pumps in `Start`, drive them, stop them in `Close`)
- Modify: `internal/rtsp/server_test.go` (add the media-flow test below)

**Interfaces:**
- Consumes: `media.Media`, `seed.Seed`, `fault.Engine`, `obs.Recorder`, `clock.Clock`, `description.Media`.
- Produces:
  - `type packetWriter interface { WritePacketRTP(*description.Media, *rtp.Packet) error }`
  - `type PumpConfig struct { CameraID string; Media *media.Media; Medi *description.Media; Writer packetWriter; Seed seed.Seed; Faults []fault.Spec; Obs *obs.Recorder }`
  - `func NewPump(cfg PumpConfig) (*Pump, error)`
  - `func (*Pump) Step() error`, `FrameCounter() int`, `Loops() uint64`, `InitialSequenceNumber() uint16`
  - On `Server`: `func (*Server) Pump(id string) *Pump`

- [ ] **Step 1: Write the failing test**

`internal/rtsp/pump_test.go`:

```go
package rtsp

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// recorder captures what a pump wrote, so pump tests need no socket.
type recorder struct {
	pkts []rtp.Packet
}

func (r *recorder) WritePacketRTP(_ *description.Media, pkt *rtp.Packet) error {
	r.pkts = append(r.pkts, *pkt)
	return nil
}

func newTestPump(t *testing.T, s seed.Seed, faults []fault.Spec, rec *obs.Recorder) (*Pump, *recorder) {
	t.Helper()
	w := &recorder{}
	p, err := NewPump(PumpConfig{
		CameraID: "cam",
		Media:    testMedia(t),
		Medi:     &description.Media{Type: description.MediaTypeVideo},
		Writer:   w,
		Seed:     s,
		Faults:   faults,
		Obs:      rec,
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	return p, w
}

func TestStepWritesOneAccessUnit(t *testing.T) {
	rec := obs.New(10)
	p, w := newTestPump(t, seed.Seed(1), nil, rec)

	if err := p.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(w.pkts) == 0 {
		t.Fatal("Step wrote no packets")
	}
	if got := p.FrameCounter(); got != 1 {
		t.Errorf("FrameCounter = %d, want 1", got)
	}
	if got := rec.Counters("cam").FramesServed; got != 1 {
		t.Errorf("FramesServed = %d, want 1", got)
	}
	// One access unit shares one timestamp across however many packets it
	// fragments into.
	ts := w.pkts[0].Timestamp
	for i, pkt := range w.pkts {
		if pkt.Timestamp != ts {
			t.Fatalf("packet %d timestamp %d differs from %d within one access unit", i, pkt.Timestamp, ts)
		}
	}
	// The final packet of an access unit carries the marker bit.
	if !w.pkts[len(w.pkts)-1].Marker {
		t.Error("last packet of the access unit has no marker bit")
	}
}

// Spec section 8.3 as corrected: the initial sequence number IS reproducible.
func TestSequenceNumbersAreSeedDeterministic(t *testing.T) {
	collect := func() []uint16 {
		p, w := newTestPump(t, seed.Seed(0x3f2a9c81).Camera(2), nil, obs.New(10))
		for i := 0; i < 10; i++ {
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		out := make([]uint16, len(w.pkts))
		for i, pkt := range w.pkts {
			out[i] = pkt.SequenceNumber
		}
		return out
	}

	a, b := collect(), collect()
	if len(a) != len(b) {
		t.Fatalf("packet counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sequence number %d differs: %d vs %d", i, a[i], b[i])
		}
	}

	// Different cameras must not start at the same sequence number.
	root := seed.Seed(0x3f2a9c81)
	p0, _ := newTestPump(t, root.Camera(0), nil, obs.New(1))
	p1, _ := newTestPump(t, root.Camera(1), nil, obs.New(1))
	if p0.InitialSequenceNumber() == p1.InitialSequenceNumber() {
		t.Error("two cameras share an initial sequence number")
	}
}

// Spec section 10.4: on loop the pump continues timestamps, so a looping fixture
// does not accidentally resemble the timestamp-discontinuity fault.
func TestLoopContinuesTimestamps(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(10)
	p, w := newTestPump(t, seed.Seed(1), nil, rec)

	// Two full passes plus one frame, so the seam is crossed and passed.
	for i := 0; i < 2*len(m.AUs)+1; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
	}

	if got := p.Loops(); got != 2 {
		t.Errorf("Loops = %d, want 2", got)
	}
	if got := rec.Counters("cam").LoopsCompleted; got != 2 {
		t.Errorf("LoopsCompleted = %d, want 2", got)
	}

	// Collapse packets to one timestamp per access unit, in order.
	var ts []uint32
	for _, pkt := range w.pkts {
		if len(ts) == 0 || ts[len(ts)-1] != pkt.Timestamp {
			ts = append(ts, pkt.Timestamp)
		}
	}
	if len(ts) != 2*len(m.AUs)+1 {
		t.Fatalf("distinct timestamps = %d, want %d", len(ts), 2*len(m.AUs)+1)
	}

	// Timestamps must never go backwards.
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("timestamp went backwards at %d: %d then %d", i, ts[i-1], ts[i])
		}
	}

	// Across the seam the step must be exactly one frame duration -- that is what
	// "no discontinuity" means here.
	seam := len(m.AUs)
	if got := int64(ts[seam]) - int64(ts[seam-1]); got != m.FrameDuration() {
		t.Errorf("seam delta = %d, want %d (one frame duration)", got, m.FrameDuration())
	}
}

// The seam is real: a configured fault is decided and recorded on every frame,
// while deliberately changing nothing on the wire in this version.
func TestFaultDecisionsAreRecordedButNotApplied(t *testing.T) {
	rec := obs.New(1000)
	// Rate 1 fires on every frame, which makes the assertion unambiguous.
	p, w := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, rec)

	const frames = 5
	for i := 0; i < frames; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}

	if got := rec.Counters("cam").FaultsFired; got != frames {
		t.Fatalf("FaultsFired = %d, want %d", got, frames)
	}
	// Effect deliberately absent: every frame was still written.
	if got := rec.Counters("cam").FramesServed; got != frames {
		t.Fatalf("FramesServed = %d, want %d -- a fault must not yet drop anything", got, frames)
	}
	if len(w.pkts) == 0 {
		t.Fatal("no packets written")
	}
	ev := rec.Events()
	if len(ev) != frames {
		t.Fatalf("events = %d, want %d", len(ev), frames)
	}
	if ev[0].Kind != string(fault.KindFrameDrop) {
		t.Errorf("event kind = %q", ev[0].Kind)
	}
	if ev[0].FrameIndex != 0 || ev[frames-1].FrameIndex != frames-1 {
		t.Errorf("frame indices = %d..%d, want 0..%d", ev[0].FrameIndex, ev[frames-1].FrameIndex, frames-1)
	}
}

func TestNewPumpRejectsBadConfig(t *testing.T) {
	m := testMedia(t)
	medi := &description.Media{Type: description.MediaTypeVideo}
	good := PumpConfig{CameraID: "c", Media: m, Medi: medi, Writer: &recorder{}, Obs: obs.New(1)}

	bad := map[string]func(PumpConfig) PumpConfig{
		"no media":  func(c PumpConfig) PumpConfig { c.Media = nil; return c },
		"no writer": func(c PumpConfig) PumpConfig { c.Writer = nil; return c },
		"no obs":    func(c PumpConfig) PumpConfig { c.Obs = nil; return c },
		"no medi":   func(c PumpConfig) PumpConfig { c.Medi = nil; return c },
		"bad fault": func(c PumpConfig) PumpConfig {
			c.Faults = []fault.Spec{{Kind: "nope", Rate: 1}}
			return c
		},
	}
	for name, mutate := range bad {
		if _, err := NewPump(mutate(good)); err == nil {
			t.Errorf("%s: NewPump succeeded, want an error", name)
		}
	}
}
```

Add to `internal/rtsp/server_test.go` — media must reach a real client:

```go
func TestMediaReachesAClient(t *testing.T) {
	s := startServer(t, 2)

	for _, id := range []string{"cam-00", "cam-01"} {
		u, err := base.ParseURL(s.URL(id))
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		if err := c.Start(); err != nil {
			t.Fatalf("client start %s: %v", id, err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			c.Close()
			t.Fatalf("DESCRIBE %s: %v", id, err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			c.Close()
			t.Fatalf("SETUP %s: %v", id, err)
		}
		got := make(chan struct{}, 1)
		c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
		if _, err := c.Play(nil); err != nil {
			c.Close()
			t.Fatalf("PLAY %s: %v", id, err)
		}
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			c.Close()
			t.Fatalf("no RTP from %s within 10s", id)
		}
		c.Close()
	}

	// The stream's own counters must agree that data went out.
	if pkts, bytes := s.StreamStats("cam-00"); pkts == 0 || bytes == 0 {
		t.Errorf("StreamStats = %d packets, %d bytes; want both non-zero", pkts, bytes)
	}
}
```

That test needs two more imports in `server_test.go`: `"github.com/bluenviron/gortsplib/v5/pkg/description"` and `"github.com/pion/rtp"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/rtsp/ -v`
Expected: FAIL — `undefined: NewPump`, `undefined: PumpConfig`, `undefined: Pump`.

- [ ] **Step 3: Write the pump**

`internal/rtsp/pump.go`:

```go
package rtsp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// detailDecidedNotApplied is recorded against a fault that was decided but whose
// effect this version does not implement. It must read as deliberate, because a
// camera that is lying on purpose and a camera that is broken being
// indistinguishable would make this tool useless.
const detailDecidedNotApplied = "decided; effect not implemented in this version"

// packetWriter is the narrow slice of ServerStream the pump needs.
//
// Writing through an interface keeps pump tests hermetic: they assert exact
// sequence numbers and timestamps with no socket, and no server to start.
type packetWriter interface {
	WritePacketRTP(*description.Media, *rtp.Packet) error
}

// PumpConfig configures a Pump.
type PumpConfig struct {
	CameraID string
	Media    *media.Media
	Medi     *description.Media
	Writer   packetWriter
	Seed     seed.Seed
	Faults   []fault.Spec
	Obs      *obs.Recorder
}

// Pump paces one camera's media onto its stream.
//
// It is not safe for concurrent use: one goroutine drives one pump. Step is the
// unit of work, so a deterministic test can call it directly and needs no clock.
type Pump struct {
	id     string
	medi   *description.Media
	w      packetWriter
	enc    *rtph264.Encoder
	aus    []media.AccessUnit
	obs    *obs.Recorder
	faults *fault.Engine

	initialSeq uint16
	frameDur   int64

	idx          int
	frameCounter int
	loops        uint64
	// tsOffset accumulates across loops so timestamps continue rather than
	// restart. It is int64 and converted on use: RTP timestamps are 32-bit and
	// wrapping is correct behaviour, not an error.
	tsOffset int64
}

// NewPump validates cfg and returns a Pump positioned at the first access unit.
func NewPump(cfg PumpConfig) (*Pump, error) {
	switch {
	case cfg.Media == nil:
		return nil, errors.New("rtsp: pump has no media")
	case cfg.Medi == nil:
		return nil, errors.New("rtsp: pump has no media description")
	case cfg.Writer == nil:
		return nil, errors.New("rtsp: pump has no writer")
	case cfg.Obs == nil:
		return nil, errors.New("rtsp: pump has no recorder")
	case len(cfg.Media.AUs) == 0:
		return nil, errors.New("rtsp: pump media has no access units")
	}

	eng, err := fault.New(cfg.Seed.Stream("fault"), cfg.Faults)
	if err != nil {
		return nil, err
	}

	// Derive SSRC and the initial sequence number from the seed.
	//
	// The sequence number survives to the client and is therefore reproducible.
	// The SSRC does not: ServerStream overwrites it with a per-format value of
	// its own. It is set anyway so that a writer which does not overwrite it --
	// including the test recorder -- still behaves deterministically, and so the
	// intent is legible if upstream ever stops overwriting.
	r := cfg.Seed.Stream("media").Rand()
	ssrc := uint32(r.Uint64())
	initialSeq := uint16(r.Uint64())

	enc := &rtph264.Encoder{
		PayloadType:           96,
		PacketizationMode:     1,
		SSRC:                  &ssrc,
		InitialSequenceNumber: &initialSeq,
	}
	if err := enc.Init(); err != nil {
		return nil, fmt.Errorf("rtsp: initializing RTP encoder: %w", err)
	}

	return &Pump{
		id:         cfg.CameraID,
		medi:       cfg.Medi,
		w:          cfg.Writer,
		enc:        enc,
		aus:        cfg.Media.AUs,
		obs:        cfg.Obs,
		faults:     eng,
		initialSeq: initialSeq,
		frameDur:   cfg.Media.FrameDuration(),
	}, nil
}

// FrameCounter returns how many access units have been written. It is monotonic
// across loops, which is why fault decisions key on it.
func (p *Pump) FrameCounter() int { return p.frameCounter }

// Loops returns how many complete passes over the source have finished.
func (p *Pump) Loops() uint64 { return p.loops }

// InitialSequenceNumber returns the seed-derived first RTP sequence number.
func (p *Pump) InitialSequenceNumber() uint16 { return p.initialSeq }

// Step writes exactly one access unit.
func (p *Pump) Step() error {
	au := p.aus[p.idx]

	// The fault seam. Decisions are recorded; none of them changes what is
	// written in this version.
	for _, d := range p.faults.DecideFrame(p.frameCounter) {
		p.obs.Fault(p.id, d.FrameIndex, string(d.Kind), detailDecidedNotApplied)
	}

	pkts, err := p.enc.Encode(au.NALUs)
	if err != nil {
		return fmt.Errorf("rtsp: encoding access unit %d of %q: %w", p.idx, p.id, err)
	}

	ts := uint32(p.tsOffset + au.DTS)
	for _, pkt := range pkts {
		pkt.Timestamp = ts
		if err := p.w.WritePacketRTP(p.medi, pkt); err != nil {
			return fmt.Errorf("rtsp: writing RTP for %q: %w", p.id, err)
		}
	}

	p.obs.Frame(p.id)
	p.frameCounter++
	p.idx++

	if p.idx == len(p.aus) {
		// Loop seam: carry the timestamp forward by one frame past the last
		// access unit, so looping the fixture is indistinguishable from a
		// continuous source. Restarting at zero here would fabricate the
		// timestamp-discontinuity fault that is supposed to be opt-in.
		p.tsOffset += au.DTS + p.frameDur
		p.idx = 0
		p.loops++
		p.obs.Loop(p.id)
	}
	return nil
}

// interval returns the wall-clock spacing between access units.
func (p *Pump) interval() time.Duration {
	const clockRate = 90000
	if p.frameDur <= 0 {
		return time.Second / 30
	}
	return time.Duration(p.frameDur) * time.Second / clockRate
}

// run drives the pump until ctx is cancelled.
//
// A write error is logged by the caller and does not stop the pump: a reader
// disconnecting mid-write must not take the camera down.
func (p *Pump) run(ctx context.Context, ck clock.Clock, onErr func(error)) {
	tk := ck.NewTicker(p.interval())
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			if err := p.Step(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
```

- [ ] **Step 4: Wire pumps into the server**

In `internal/rtsp/server.go`, add to the `Server` struct:

```go
	pumps  map[string]*Pump
	cancel context.CancelFunc
	wg     sync.WaitGroup
```

Initialize `pumps: make(map[string]*Pump, len(cfg.Cameras))` in `New`.

In `Start`, immediately after the stream-initialization loop and before `s.srv = srv`:

```go
	ctx, cancel := context.WithCancel(context.Background())
	for _, id := range s.order {
		cam := s.cams[id]
		pump, err := NewPump(PumpConfig{
			CameraID: cam.id,
			Media:    cam.cfg.Media,
			Medi:     cam.medi,
			Writer:   cam.stream,
			Seed:     cam.cfg.Seed,
			Faults:   cam.cfg.Faults,
			Obs:      s.cfg.Obs,
		})
		if err != nil {
			cancel()
			for _, done := range s.order {
				if c := s.cams[done]; c.stream != nil {
					c.stream.Close()
				}
			}
			srv.Close()
			return fmt.Errorf("rtsp: building pump for %q: %w", id, err)
		}
		s.pumps[id] = pump
	}
	s.cancel = cancel

	for _, id := range s.order {
		pump := s.pumps[id]
		camID := id
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			pump.run(ctx, s.ck, func(err error) {
				s.log.Warn("pump write failed", "camera", camID, "err", err)
			})
		}()
	}
```

In `Close`, stop the drivers before closing streams:

```go
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
```

and after releasing the lock, wait for the goroutines. Because `Close` holds `s.mu` and the pumps do not take it, `s.wg.Wait()` is safe to call while holding the lock — but keep it after `s.srv = nil` so a concurrent `Close` is a no-op. Add:

```go
	s.wg.Wait()
```

Add the accessor tests need:

```go
// Pump returns a camera's pump, or nil. Intended for deterministic stepping in
// tests; the driver goroutine owns it otherwise.
func (s *Server) Pump(id string) *Pump {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pumps[id]
}
```

Add `"context"` to the imports.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/rtsp/ -v`
Expected: PASS, including `TestMediaReachesAClient`.

If `-race` reports a race between `Close` and a pump goroutine writing to a closed stream, do not silence it by dropping `wg.Wait()`: cancel the context first, wait, and only then close the streams.

- [ ] **Step 6: Commit**

```bash
git add internal/rtsp
git commit -m "Add media pump with deterministic sequence numbers

Step writes exactly one access unit, which is what makes determinism testable
without a virtual clock: fault decisions key on a frame counter, so a test steps
the pump directly and asserts an exact sequence.

The pump writes through a narrow interface so its tests need no socket. Initial
sequence numbers derive from the seed and do survive to the client; SSRC is set
from the seed too but ServerStream overwrites it, so it is excluded from replay
assertions.

On loop the timestamp carries forward by one frame duration, so a looping
fixture is not mistaken for the timestamp-discontinuity fault."
```

---

### Task 8: Public API

Spec §9's surface, minus everything the ONVIF slice owns. Every element here traces to a stated need in spec §2.9: silence by default, `Port: 0` with read-back, first-class inspection instead of a leaked pointer, a stable spec type, and errors that distinguish unsupported from uninitialised.

**Files:**
- Create: `spec.go`, `errors.go`, `camfarm.go`
- Test: `camfarm_test.go`
- Modify: `doc.go`

**Interfaces:**
- Consumes: `internal/rtsp`, `internal/media`, `internal/obs`, `internal/seed`, `internal/fault`.
- Produces: the exported surface listed below.

Two deliberate deviations from spec §9, both improvements to record:

- **`StartT` takes an interface, not `*testing.T`.** `type TB interface { Cleanup(func()); Fatalf(string, ...any); Helper() }` is satisfied by `*testing.T` and `*testing.B` without this library importing `testing`, which would otherwise register test flags in every consumer's binary.
- **`Replay` is not implemented here.** It is signatured against a spec *file* and this slice has no file format. `Spec.Hash()` and `Fleet.ReplayLine()` ship instead, which is what spec §8.2's reporting requirement actually needs.

- [ ] **Step 1: Write the failing test**

`camfarm_test.go`:

```go
package camfarm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func minimalSpec() Spec {
	return Spec{
		Seed: 0x3f2a9c81,
		Cameras: []CameraSpec{
			{ID: "front-door"},
			{ID: "lobby"},
		},
	}
}

func TestStartServesTheFleetAndClosesCleanly(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if got := f.Addr().RTSP; got == nil || got.Port == 0 {
		t.Fatalf("RTSP addr = %v, want a resolved ephemeral port", got)
	}

	list := f.List()
	if len(list) != 2 {
		t.Fatalf("List = %d cameras, want 2", len(list))
	}
	for _, st := range list {
		if !strings.HasPrefix(st.RTSPURL, "rtsp://") {
			t.Errorf("%s: RTSPURL = %q", st.ID, st.RTSPURL)
		}
		if st.Width != 320 || st.Height != 240 {
			t.Errorf("%s: geometry = %dx%d, want 320x240 from the bundled fixture", st.ID, st.Width, st.Height)
		}
		if st.Codec != "H264" {
			t.Errorf("%s: codec = %q", st.ID, st.Codec)
		}
	}

	// Close must be idempotent: a t.Cleanup and an explicit Close both fire.
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCameraLookup(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if cam.ID() != "front-door" {
		t.Errorf("ID = %q", cam.ID())
	}
	if !strings.HasSuffix(cam.RTSPURL(), "/front-door") {
		t.Errorf("RTSPURL = %q", cam.RTSPURL())
	}

	if _, err := f.Camera("nope"); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("unknown camera: err = %v, want ErrUnknownCamera", err)
	}
}

func TestStatsReportSeedAndProgress(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	st := cam.Stats()
	if st.Seed == 0 {
		t.Error("Stats.Seed is zero")
	}
	if st.Seed == f.Seed() {
		t.Error("camera seed equals the root seed; it must be derived")
	}
	// No faults are configured, so none can have fired.
	if st.FaultsFired != 0 {
		t.Errorf("FaultsFired = %d, want 0", st.FaultsFired)
	}
}

// Spec section 7.7: faults are catalogued and validated in this version, and
// refused rather than silently ignored.
func TestFaultsAreRefusedNotIgnored(t *testing.T) {
	s := minimalSpec()
	s.Cameras[0].Faults = []FaultSpec{{Kind: "frame_drop", Rate: 0.5}}

	_, err := Start(context.Background(), s)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// The message must name the kind, so a caller knows what was refused.
	if !strings.Contains(err.Error(), "frame_drop") {
		t.Errorf("err = %v, want it to name the fault kind", err)
	}

	// An unknown kind is a different failure from an unimplemented one.
	s.Cameras[0].Faults = []FaultSpec{{Kind: "not-a-fault", Rate: 0.5}}
	if _, err := Start(context.Background(), s); err == nil {
		t.Fatal("unknown fault kind was accepted")
	}
}

func TestInjectAndClear(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Inject: err = %v, want ErrUnsupported", err)
	}
	// Clearing nothing succeeds: it is not an error to ask for the state that
	// already holds.
	if err := cam.Clear(); err != nil {
		t.Errorf("Clear: %v", err)
	}
}

func TestSpecValidation(t *testing.T) {
	cases := map[string]Spec{
		"no cameras":   {Seed: 1},
		"empty id":     {Seed: 1, Cameras: []CameraSpec{{ID: ""}}},
		"duplicate id": {Seed: 1, Cameras: []CameraSpec{{ID: "a"}, {ID: "a"}}},
		"slash in id":  {Seed: 1, Cameras: []CameraSpec{{ID: "a/b"}}},
		"bad source":   {Seed: 1, Cameras: []CameraSpec{{ID: "a", Source: SourceSpec{Kind: "nonsense"}}}},
		"file without path": {Seed: 1, Cameras: []CameraSpec{
			{ID: "a", Source: SourceSpec{Kind: SourceFile}},
		}},
		"advertised geometry differs from source": {Seed: 1, Cameras: []CameraSpec{
			{ID: "a", Video: VideoSpec{Width: 1920, Height: 1080}},
		}},
	}
	for name, s := range cases {
		if _, err := Start(context.Background(), s); err == nil {
			t.Errorf("%s: Start succeeded, want an error", name)
		}
	}
}

// Spec section 8.2: a failing test must be able to print one line that
// reproduces the run.
func TestReplayLineIsStableAndSpecific(t *testing.T) {
	f1, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f1.Close() })

	line := f1.ReplayLine()
	if !strings.Contains(line, "seed=") || !strings.Contains(line, "spec=sha256:") {
		t.Fatalf("ReplayLine = %q", line)
	}

	// The same spec must hash the same, and a changed spec must not.
	h1 := minimalSpec().Hash()
	if h1 != minimalSpec().Hash() {
		t.Fatal("Hash is not stable for an identical spec")
	}
	changed := minimalSpec()
	changed.Cameras[0].ID = "side-door"
	if changed.Hash() == h1 {
		t.Fatal("Hash did not change when a camera ID did")
	}
	// A logger is not part of the fleet's identity.
	withLog := minimalSpec()
	withLog.Log = discardLoggerForTest()
	if withLog.Hash() != h1 {
		t.Fatal("Hash changed when only the logger was set")
	}
}

func TestStartTCleansUp(t *testing.T) {
	var cleanups []func()
	fake := &fakeTB{onCleanup: func(fn func()) { cleanups = append(cleanups, fn) }}

	f := StartT(fake, minimalSpec())
	if f.Addr().RTSP == nil {
		t.Fatal("StartT did not bind")
	}
	if len(cleanups) != 1 {
		t.Fatalf("registered %d cleanups, want 1", len(cleanups))
	}
	cleanups[0]()

	// After cleanup the fleet is closed; a second Close must still be safe.
	if err := f.Close(); err != nil {
		t.Errorf("Close after cleanup: %v", err)
	}
}

type fakeTB struct {
	onCleanup func(func())
	failed    string
}

func (f *fakeTB) Cleanup(fn func())                  { f.onCleanup(fn) }
func (f *fakeTB) Helper()                            {}
func (f *fakeTB) Fatalf(format string, a ...any)     { f.failed = format }
```

Add this helper at the bottom of `camfarm_test.go`:

```go
func discardLoggerForTest() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
```

with imports `"io"` and `"log/slog"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -v`
Expected: FAIL — package does not build (`undefined: Start`, `undefined: Spec`).

- [ ] **Step 3: Write the errors**

`errors.go`:

```go
package camfarm

import "errors"

// The error taxonomy. These are kept distinct because conflating them is a known
// pain for consumers of virtual cameras: a caller that cannot tell "this build
// cannot do that" from "this is not ready yet" has to guess.
var (
	// ErrUnknownCamera means no camera with the given ID exists in the fleet.
	ErrUnknownCamera = errors.New("camfarm: unknown camera")

	// ErrUnsupported means this build cannot do what was asked. It is not a
	// malfunction and not a transient condition.
	ErrUnsupported = errors.New("camfarm: unsupported")

	// ErrNotReady means the fleet or camera has not finished initialising.
	ErrNotReady = errors.New("camfarm: not ready")

	// ErrFaultActive means the request was refused *because* a fault is
	// deliberately in effect.
	//
	// This must never read as a malfunction. "This camera is lying on purpose"
	// and "this camera is broken" being indistinguishable would make the whole
	// tool useless.
	ErrFaultActive = errors.New("camfarm: refused by an active fault")
)
```

- [ ] **Step 4: Write the spec**

`spec.go`:

```go
package camfarm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/0x524a/camfarm/internal/fault"
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

// VideoSpec describes what a camera advertises.
//
// Zero fields mean "derive from the source". A value that disagrees with the
// source is a deliberate lie about capabilities, which is a catalogued fault
// rather than a configuration option, so this version rejects it.
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

		if c.Video.Codec != "" && c.Video.Codec != "H264" {
			return s, fmt.Errorf("%w: camera %q advertises codec %q; only H264 is implemented",
				ErrUnsupported, c.ID, c.Video.Codec)
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
```

- [ ] **Step 5: Write the fleet**

`camfarm.go`:

```go
package camfarm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
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
	Seed               uint64
	FrameIndex         int
	FramesServed       uint64
	LoopsCompleted     uint64
	FaultsFired        uint64
	OutboundRTPPackets uint64
	OutboundBytes      uint64
	Readers            int
}

// Fleet is a running set of synthetic cameras.
type Fleet struct {
	spec     Spec
	specHash string
	log      *slog.Logger
	rec      *obs.Recorder
	srv      *rtsp.Server

	mu       sync.Mutex
	closed   bool
	cameras  map[string]*Camera
	order    []string
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
	}

	// Parse each distinct source once and share it. This is the scale lever:
	// M cameras on one source cost one copy of the media plus M encoder states.
	loaded := make(map[SourceSpec]*media.Media)
	root := seed.Seed(valid.Seed)

	cfg := rtsp.Config{
		Host:  valid.Listen.Host,
		Port:  valid.Listen.RTSPPort,
		Log:   log,
		Obs:   f.rec,
	}

	for i, cs := range valid.Cameras {
		m, ok := loaded[cs.Source]
		if !ok {
			src, err := newSource(cs.Source)
			if err != nil {
				return nil, err
			}
			m, err = src.Load()
			if err != nil {
				return nil, fmt.Errorf("camfarm: camera %q: %w", cs.ID, err)
			}
			loaded[cs.Source] = m
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

	if ctx != nil && ctx.Done() != nil {
		watchCtx, cancel := context.WithCancel(ctx)
		f.stopWatch = cancel
		go func() {
			<-watchCtx.Done()
			_ = f.Close()
		}()
	}

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

// checkAdvertised refuses a VideoSpec that disagrees with the source.
//
// Advertising a capability the stream does not deliver is a catalogued fault,
// not a configuration option, so it is refused here rather than silently
// honoured.
func checkAdvertised(cs CameraSpec, m *media.Media) error {
	if cs.Video.Width != 0 && cs.Video.Width != m.Width {
		return fmt.Errorf("%w: camera %q advertises width %d but its source is %d wide; a deliberate mismatch is the sps_resolution_lie fault, not yet implemented",
			ErrUnsupported, cs.ID, cs.Video.Width, m.Width)
	}
	if cs.Video.Height != 0 && cs.Video.Height != m.Height {
		return fmt.Errorf("%w: camera %q advertises height %d but its source is %d high; a deliberate mismatch is the sps_resolution_lie fault, not yet implemented",
			ErrUnsupported, cs.ID, cs.Video.Height, m.Height)
	}
	if cs.Video.FPS != 0 && cs.Video.FPS != m.FPS {
		return fmt.Errorf("%w: camera %q advertises %v fps but its source is %v; a deliberate mismatch is a catalogued fault, not yet implemented",
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
	f.log.Info("fleet closed")
	return nil
}

// ID returns the camera's operator-chosen identifier.
func (c *Camera) ID() string { return c.id }

// RTSPURL returns the camera's stream URL.
func (c *Camera) RTSPURL() string { return c.fleet.srv.URL(c.id) }

// Stats returns the camera's counters.
func (c *Camera) Stats() Stats {
	counters := c.fleet.rec.Counters(c.id)
	pkts, bytes := c.fleet.srv.StreamStats(c.id)

	frameIndex := 0
	if p := c.fleet.srv.Pump(c.id); p != nil {
		frameIndex = p.FrameCounter()
	}

	return Stats{
		Seed:               uint64(c.seed),
		FrameIndex:         frameIndex,
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
```

- [ ] **Step 6: Update the package doc**

Replace the final paragraph of `doc.go`:

```go
// This version serves RTSP media for a fleet of cameras and carries the seed and
// fault-injection seams throughout, with every catalogued fault deliberately
// inert: a fault named in a spec is refused rather than silently ignored. The
// control plane and the fault effects follow. The architecture is recorded in
// docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md.
```

- [ ] **Step 7: Run the tests**

Run: `go test -race . -v`
Expected: PASS.

Note: `SourceSpec` is used as a map key in `Start`. It is a comparable struct of two strings, so this compiles; if a future field makes it non-comparable, key the map on a derived string instead.

- [ ] **Step 8: Commit**

```bash
git add spec.go errors.go camfarm.go camfarm_test.go doc.go
git commit -m "Add public fleet API

Start and StartT bring up a fleet on an ephemeral port, silent by default, with
the address read back rather than guessed. Stats replaces the internal pointer a
consumer of the upstream virtual camera had to reach for.

StartT takes an interface rather than *testing.T so this library does not import
testing. Faults named in a spec are refused with ErrUnsupported rather than
accepted and ignored: a test that asked for misbehaviour and got none, silently,
is worse than one that fails to start. Advertised video that disagrees with the
source is refused for the same reason -- that mismatch is a catalogued fault, not
a configuration option."
```

---

### Task 9: External verification, CI, and documentation

Spec §10.7 requires an external cross-check because `rtspeek` stops at DESCRIBE and cannot validate the media path: it verifies reachability and SDP parseability, never SETUP, PLAY, or a single RTP packet. `ffprobe` decodes for real, so it is the oracle that can catch a stream that parses but does not play.

**Files:**
- Create: `integration_test.go`
- Create: `README.md`
- Modify: `.github/workflows/ci.yml`
- Modify: `docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md`

- [ ] **Step 1: Write the failing integration test**

`integration_test.go`:

```go
package camfarm

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// TestFFprobeDecodesEveryCamera is the external oracle. A stream that an
// independent decoder cannot play is broken however well it parses.
func TestFFprobeDecodesEveryCamera(t *testing.T) {
	f := StartT(t, Spec{
		Seed: 0x3f2a9c81,
		Cameras: []CameraSpec{
			{ID: "front-door"},
			{ID: "lobby"},
		},
	})

	for _, st := range f.List() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, "ffprobe",
			"-v", "error",
			"-rtsp_transport", "tcp",
			"-i", st.RTSPURL,
			"-select_streams", "v:0",
			"-show_entries", "stream=codec_name,width,height,pix_fmt",
			"-read_intervals", "%+#20",
			"-of", "json",
		).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: ffprobe failed: %v\n%s\n%s", st.ID, err, out, f.ReplayLine())
		}

		var got struct {
			Streams []struct {
				CodecName string `json:"codec_name"`
				Width     int    `json:"width"`
				Height    int    `json:"height"`
				PixFmt    string `json:"pix_fmt"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: parsing ffprobe output: %v\n%s", st.ID, err, out)
		}
		if len(got.Streams) != 1 {
			t.Fatalf("%s: ffprobe saw %d video streams, want 1\n%s", st.ID, len(got.Streams), out)
		}
		s := got.Streams[0]
		if s.CodecName != "h264" {
			t.Errorf("%s: ffprobe decoded codec %q, want h264", st.ID, s.CodecName)
		}
		// The decoded geometry must match what the fleet advertised. A mismatch
		// here would be the sps_resolution_lie fault firing by accident, which
		// is exactly the failure mode this project exists to make deliberate.
		if s.Width != st.Width || s.Height != st.Height {
			t.Errorf("%s: ffprobe decoded %dx%d but the fleet advertised %dx%d",
				st.ID, s.Width, s.Height, st.Width, st.Height)
		}
	}
}

// A fleet must survive more cameras than a developer would open by hand. This
// is not the measured ceiling spec section 10.2 calls for -- that needs a
// benchmark -- but it proves the model holds well past one.
func TestTwentyFiveCamerasOnOneListener(t *testing.T) {
	const n = 25

	spec := Spec{Seed: 0x3f2a9c81}
	for i := 0; i < n; i++ {
		spec.Cameras = append(spec.Cameras, CameraSpec{ID: fmt.Sprintf("cam-%02d", i)})
	}
	f := StartT(t, spec)

	list := f.List()
	if len(list) != n {
		t.Fatalf("List = %d, want %d", len(list), n)
	}

	// One listener, so every camera shares a port and differs only by path.
	port := f.Addr().RTSP.Port
	seen := make(map[string]bool, n)
	for _, st := range list {
		if seen[st.RTSPURL] {
			t.Fatalf("duplicate URL %q", st.RTSPURL)
		}
		seen[st.RTSPURL] = true
	}
	if got := f.Addr().RTSP.Port; got != port {
		t.Fatal("the fleet moved ports mid-test")
	}

	// Every camera must be independently and deterministically seeded.
	seeds := make(map[uint64]string, n)
	for _, st := range list {
		cam, err := f.Camera(st.ID)
		if err != nil {
			t.Fatalf("Camera(%q): %v", st.ID, err)
		}
		s := cam.Stats().Seed
		if prev, dup := seeds[s]; dup {
			t.Fatalf("%s and %s share seed %#x", st.ID, prev, s)
		}
		seeds[s] = st.ID
	}
}

// The whole point: the same seed and spec give the same camera seeds, so a
// recorded failure can be reconstructed.
func TestSameSeedReproducesCameraSeeds(t *testing.T) {
	spec := Spec{
		Seed:    0xdeadbeefcafe,
		Cameras: []CameraSpec{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}

	collect := func() (map[string]uint64, string) {
		f := StartT(t, spec)
		out := map[string]uint64{}
		for _, st := range f.List() {
			cam, err := f.Camera(st.ID)
			if err != nil {
				t.Fatalf("Camera: %v", err)
			}
			out[st.ID] = cam.Stats().Seed
		}
		return out, f.SpecHash()
	}

	first, hash1 := collect()
	second, hash2 := collect()

	if hash1 != hash2 {
		t.Errorf("spec hash differs between runs: %s vs %s", hash1, hash2)
	}
	for id, want := range first {
		if got := second[id]; got != want {
			t.Errorf("%s: seed %#x then %#x", id, want, got)
		}
	}

	// A different root seed must move every camera.
	other := spec
	other.Seed = spec.Seed + 1
	f := StartT(t, other)
	for _, st := range f.List() {
		cam, err := f.Camera(st.ID)
		if err != nil {
			t.Fatalf("Camera: %v", err)
		}
		if cam.Stats().Seed == first[st.ID] {
			t.Errorf("%s kept seed %#x across a root-seed change", st.ID, first[st.ID])
		}
	}
}

// Frames must actually be served over time, not just on the first tick.
func TestFramesAccumulate(t *testing.T) {
	f := StartT(t, Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a"}}})

	cam, err := f.Camera("a")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var first uint64
	for {
		first = cam.Stats().FramesServed
		if first > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first == 0 {
		t.Fatalf("no frames served within 10s\n%s", f.ReplayLine())
	}

	for {
		if cam.Stats().FramesServed > first || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := cam.Stats().FramesServed; got <= first {
		t.Fatalf("frames stalled at %d\n%s", got, f.ReplayLine())
	}

	// No faults were configured, so none may have fired.
	if got := cam.Stats().FaultsFired; got != 0 {
		t.Errorf("FaultsFired = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails on a machine without ffmpeg, and passes with it**

Run: `go test -race -run TestFFprobeDecodesEveryCamera . -v`
Expected: PASS locally (ffprobe 6.1.1 is installed). The point of this step is to confirm the test genuinely exercises ffprobe: temporarily rename the binary on `PATH`, or run `PATH=/nonexistent go test -run TestFFprobe . -v`, and confirm the test **fails** rather than skipping. It must never skip — spec §10.7.

- [ ] **Step 3: Install ffmpeg in CI**

In `.github/workflows/ci.yml`, add this step to **both** the `build` and `race` jobs, immediately after `actions/checkout`:

```yaml
      # ffprobe is the external decoder the media tests cross-check against.
      # It is installed rather than skipped around: a test that quietly skips
      # when its oracle is missing is worse than no test.
      #
      # This does not compromise the cgo-free claim. ffprobe runs as a
      # subprocess; nothing links against it.
      - name: install ffmpeg
        run: |
          sudo apt-get update
          sudo apt-get install -y --no-install-recommends ffmpeg
          ffprobe -version | head -1
```

- [ ] **Step 4: Verify CI locally, both jobs**

```bash
CGO_ENABLED=0 gofmt -l .
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
CGO_ENABLED=1 go test -race ./...
```

Expected: `gofmt -l .` prints nothing; every other command exits zero. Fix anything that does not before proceeding — the project's own constraint is that CI is green from the first commit, and there is no remote yet to hide behind.

- [ ] **Step 5: Write the README**

`README.md`. Constraints: no badge (no remote has ever run the workflow), no coverage number, no mention of the control plane as though it exists, and no ONVIF claim of any kind in this slice.

```markdown
# camfarm

`camfarm` is a working name and not final.

A synthetic RTSP camera farm for testing video-analytics pipelines without
physical hardware. It serves N fake cameras from one process, and it is built so
that misbehaviour can be injected deterministically: a fault fires because a
seed said it would, and the same seed and fleet spec reproduce it.

## Status

Early. This version serves media and nothing else:

- **Works:** N RTSP cameras behind one listener, each on its own path; a bundled
  fixture or an MPEG-TS file as the source; media parsed once and shared across
  cameras; seed-derived per-camera RTP sequence numbers; inspection counters.
- **Not built yet:** the camera control plane, device discovery, and every
  fault's *effect*. The fault catalogue is defined and validated, and the seams
  it will act through are in place, but no fault changes a byte on the wire. A
  spec that names a fault is **refused**, not silently ignored.

## Use as a library

```go
f, err := camfarm.Start(ctx, camfarm.Spec{
	Seed:    0x3f2a9c81,
	Cameras: []camfarm.CameraSpec{{ID: "front-door"}, {ID: "lobby"}},
})
if err != nil {
	return err
}
defer f.Close()

cam, _ := f.Camera("front-door")
fmt.Println(cam.RTSPURL()) // rtsp://127.0.0.1:41569/front-door
```

In a test, `camfarm.StartT(t, spec)` binds an ephemeral port, stays silent, and
closes itself when the test ends.

## Scope — what this does not do

- **It simulates. It is not a conformance-tested implementation of anything, and
  it does not claim to be.** Testing a client against this farm tells you your
  client works against *this farm*. It does not tell you your client is correct
  against real hardware, and it certainly does not tell you anything about your
  own conformance to any specification.
- **The bundled video is a fixture, not a sample.** It is a two-second synthetic
  test pattern at 320×240, generated by `scripts/gen-fixture.sh`. It exists so
  that tests need no media sourced before they run. It is not representative of
  real camera output in bitrate, motion, noise, or encoder behaviour.
- **Determinism has stated limits.** The decision sequence is reproducible from a
  seed; wall-clock timings are not, because serving a real client happens in real
  time. Two specific carve-outs:
  - **RTP SSRC is not reproducible.** The underlying RTSP library assigns each
    stream its own SSRC and overwrites whatever the encoder set, so SSRC is
    excluded from replay assertions. Sequence numbers *are* reproducible.
  - **A live upstream source is outside the guarantee.** Not implemented in this
    version; when it is, its bytes will differ run to run, so only the fault
    *decisions* over it will be reproducible, not the media.
- **No network-path impairment.** Packet loss, bandwidth ceilings, and reordering
  are properties of the path, not of the sender; they need `tc`/`netem` and
  elevated privileges, and they are not reproducible from a seed. `camfarm` owns
  only what a camera itself chooses not to send, or sends late.
- **Not a benchmark result.** No cameras-per-core or memory-per-camera figure is
  published, because none has been measured yet. When one is, it will come from a
  committed benchmark, not an estimate.

## Requirements

Go 1.26 or later. No cgo, and no system packages at runtime. `ffmpeg` is needed
only to run the test suite, which cross-checks the served streams with `ffprobe`,
and to regenerate the fixture.

## Licence

MIT. See [LICENSE](LICENSE).
```

- [ ] **Step 6: Back-port the corrections into the design spec**

Edit `docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md`. These are corrections established by running code, so record them as corrections rather than quietly rewriting:

1. In §8.3, replace carve-out 1. It currently says SSRC comes from `crypto/rand` "with no exported field". Correct it: `rtph264.Encoder.SSRC` and `InitialSequenceNumber` *are* exported and settable, and the initial sequence number is preserved end to end and therefore reproducible; the SSRC is not, because `ServerStream` overwrites it at `server_stream_format.go:103`. Also note there is no `InitialTimestamp` field, so per-packet assignment is the only timestamp lever.
2. In §2.9 and §9, note that `Server.NetListener()` exists and returns the live listener, so port-0 read-back needs no workaround; it panics before a successful `Start()`.
3. In §7.3, correct the "wrong realm" row: `Server.AuthMethods` is exported and real, but there is no `AuthRealm` — the realm is the hardcoded constant `serverAuthRealm = "ipcam"` (`server.go:19`), so that fault is reachable only through the `OnResponse` rewrite.
4. In §9, note that `Stats()` cannot forward a reader count from `ServerStream`, which exposes none; the count is tracked in the RTSP handler.
5. In §5, record that `mediacommon/v2` has no Annex-B stream reader and that `AnnexB.Unmarshal` aliases its input buffer, which is why the fixture is MPEG-TS and the parser copies.
6. Resolve open item 3 in §13: the fixture is a synthetic pattern generated by a committed script, carries no third-party content, and is redistributable under MIT. Note that regeneration is not byte-reproducible and the committed artifact is authoritative.
7. Update §2.7's version note: latest is `gortsplib/v5 v5.6.5`, whose own `go.mod` declares `go 1.26.0`.
8. Correct the `onvif-mcp` sentence in `CLAUDE.md` as §2.9 and §13 item 2 already require: describe it as a candidate consumer, not as that project's test strategy.

- [ ] **Step 7: Final verification**

```bash
grep -rn "t.Skip" --include=*.go . && echo "FOUND SKIPS -- fix them" || echo "no skipped tests"
grep -rni "onvif" --include=*.go . && echo "FOUND ONVIF IN CODE -- out of scope for this slice" || echo "no onvif in code"
grep -n "badge\|shields.io\|coverage" README.md && echo "CHECK: no badge or coverage claim is allowed yet" || echo "no badge, no coverage claim"
CGO_ENABLED=0 go test ./... && CGO_ENABLED=1 go test -race ./...
```

Expected: no skips, no ONVIF in code, no badge, both test runs green.

- [ ] **Step 8: Commit**

```bash
git add integration_test.go README.md .github/workflows/ci.yml docs/superpowers/specs/2026-09-05-camfarm-architecture-design.md CLAUDE.md
git commit -m "Add external verification, CI ffmpeg, and README

ffprobe is the oracle: it decodes the served streams for real, which the
DESCRIBE-only client in a sibling repository cannot do. It is installed in CI
rather than skipped around, because a test that quietly skips when its oracle is
missing is worse than no test.

The README carries no badge and no coverage number, because no workflow has run
yet, and states the scope limits plainly -- including that the bundled fixture is
a fixture and that SSRC is outside the replay guarantee.

The design spec is corrected where running the code contradicted it: NetListener
exists, the SSRC carve-out had the right conclusion for the wrong reason, there
is no configurable auth realm, and the fixture's provenance is now settled."
```

---

## Self-review

**Spec coverage.** Decision 3 (`MediaSource`, pure Go, `CGO_ENABLED=0`) is Task 3 plus the CI check in Task 9. Decision 4's catalogue is named in Task 5 with effects deferred, as §7.7 phases it. Decision 5's mechanics are Tasks 1, 5, 7 and the determinism tests in Task 9. Decision 6's scale model is Tasks 6 and 7; the *measured* ceiling it demands is explicitly not delivered — that needs a committed benchmark and is listed as follow-on work below. Decision 9's library-first shape is Task 8. Decisions 1, 2, 7 and 8 are ONVIF, discovery and the config file, all out of this slice by the scope statement above.

**Deviations from spec §9, all deliberate and recorded in Task 8:** `StartT` takes an interface instead of `*testing.T`; `Replay` is replaced by `Spec.Hash()` and `Fleet.ReplayLine()`; the ONVIF-shaped `Camera` methods are absent rather than stubbed.

**Type consistency.** `media.Media`/`media.AccessUnit` flow unchanged from Task 3 through Tasks 6, 7 and 8. `seed.Seed` derivation is `root.Camera(i).Stream(label)` everywhere. `fault.Spec`/`fault.Kind` in Task 5 are what `rtsp.CameraConfig.Faults` and `camfarm.FaultSpec` map onto. `obs.Recorder`'s method set is fixed in Task 4 and used unchanged afterwards. `Pump.FrameCounter()` is what `Stats.FrameIndex` reads.

**Known risks the executor should expect.**

1. The fixture constants in Task 3 (`30` access units, keyframes at `[0, 15]`) were measured against ffmpeg 6.1.1. A different ffmpeg will produce a different encode. The committed fixture is authoritative; if regeneration changes the facts, update the constants deliberately.
2. `Virtual.Stop` writing `stopped` outside `v.mu` is a genuine race if a ticker is stopped concurrently with `Advance`. Task 2 Step 4 says how to fix it properly rather than silencing it.
3. `Close` ordering in Task 7 must cancel, wait, then close streams. Getting it wrong shows up as a `-race` report or a write to a closed stream, not as a flake.

**Follow-on work this slice deliberately leaves open**, in the order the design spec argues for it: the ONVIF control plane and its transport (spec §3); the six phase-2 fault effects (§7.7); the WS-Discovery responder plus its non-multicast fallback (§10.5); the declarative spec file and `Replay` (§10.6, §8.2); and the committed benchmark that turns spec §10.2's design target into a measured, publishable ceiling.
