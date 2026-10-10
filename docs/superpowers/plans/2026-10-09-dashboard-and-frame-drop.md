# Dashboard, Binary and frame_drop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the first real fault effect (`frame_drop`) with journaled runtime injection, a change feed, and a web dashboard plus `cmd/camfarm` binary that drive a fleet interactively.

**Architecture:** `internal/fault` decides, `internal/rtsp.Pump.Step` applies (an encoder-side drop: the access unit is never encoded, so RTP sequence numbers stay contiguous while timestamps jump). Faults swap in between frames through a mutex-guarded queue on the pump. The root package gains `Inject`/`Clear`, a journal, a `Subscribe` change feed, sentinel errors and accessors. `internal/dashboard` is a thin `net/http` layer over the public `camfarm` API (REST + SSE + an embedded vanilla-JS page); `cmd/camfarm` wires flags to it.

**Tech Stack:** Go 1.26 (`net/http` ServeMux patterns, `embed`), existing `gortsplib/v5`, new test-only dependency `github.com/playwright-community/playwright-go` (Chromium) behind an `e2e` build tag.

**Spec:** `docs/superpowers/specs/2026-10-09-dashboard-and-frame-drop-design.md`

**Precondition:** slice 2 (PR #2, `Camera.Screenshot`) must be merged to `main` first. Execute on a fresh branch from `main`.

**Code status:** Tasks 1-5 were applied, compiled and run under `-race` in a scratch copy on 2026-10-09 and the corrections found are folded in below. Tasks 6-10 have *not* been compiled. Where a snippet has a trivial compile issue (an unused import, a renamed helper), fix it without changing behaviour. Do not weaken a test to make it pass.

## Global Constraints

- `CGO_ENABLED=0` must keep building for the shipped binary and the default `go test ./...`: no cgo dependency. The Playwright dependency is imported only from files with the `e2e` build tag.
- Determinism is the product: every fault decision is a pure function of the camera seed and the monotonic frame counter. No wall-clock, goroutine-order or map-order input to a decision. Draws are unconditional.
- Tests use real sockets and real behaviour; no mocks of the RTSP or HTTP stacks. The only seam faked in tests is the dashboard's capture function (to count and block captures), never the wire.
- Reuse the error taxonomy; the root package adds exactly two sentinels, `ErrDuplicateCamera` and `ErrInvalidSpec`.
- `go test -race ./...` must pass.
- Fault kinds other than `frame_drop` stay catalogued and refused with `ErrUnsupported`.
- Loopback default bind (`127.0.0.1:8080`); no token, Host-header or CSRF guard in this slice (stated risk, spec section 5). The README and `--help` must say so.
- The README keeps its "Scope / what this does not do" section and not-ONVIF-conformant blockquote unchanged. No coverage number or badge is added by hand.
- No employer or client name anywhere. Commits use the repository's configured identity.
- Naming: the display name is **CamFarm** (UI title and heading, README prose, `--help` text). The Go module path, repo URL, package names, binary name and code identifiers stay lowercase `camfarm`.
- The UI must follow the system colour scheme via CSS tokens (light defined on bare `:root`, dark under `prefers-color-scheme`), keep a 16px side gutter, never scroll the page horizontally, and build DOM with `textContent` (never `innerHTML` with server data).

## Rulings recorded against the spec

- `fault.Engine.DecideFrame` gains a `randomAccess bool` parameter (the pump knows `AccessUnit.RandomAccess`); all existing call sites are updated in Task 1.
- A swapped-in engine is built with `fault.New(cfg.Seed.Stream("fault"), specs)` — the same seed derivation as at pump creation — so injecting the same faults into the same camera always starts the same draw sequence.
- Suppressed drops (`protect_keyframes`) are recorded through `obs.Fault` like any decision, so they count toward `FaultsFired`, consistent with the existing "decided" semantics.
- The journal records start-time cameras and faults implicitly (the spec hash covers them); only runtime add/remove/inject/clear are journaled.

## File Structure

- Modify `internal/fault/fault.go`, `internal/fault/fault_test.go` — `ProtectKeyframes`, `Suppressed`, `randomAccess`, `Implemented`.
- Modify `internal/rtsp/pump.go`, `internal/rtsp/pump_test.go` — effect and `SetFaults`.
- Modify `internal/rtsp/server.go`, `internal/rtsp/server_test.go` — `camera.pump`, `Server.SetFaults`.
- Modify `errors.go`, `spec.go`, `camfarm.go`, `camfarm_test.go` — root API.
- Create `events.go`, `events_test.go` — journal, `Subscribe`, replay line.
- Create `internal/dashboard/{dashboard.go,snapshot.go,events.go,web/index.html}` and tests.
- Create `cmd/camfarm/{main.go,run.go,run_test.go}`.
- Create `internal/dashboard/e2e_test.go` (`//go:build e2e`); modify `.github/workflows/ci.yml`, `go.mod`, `go.sum`.
- Modify `README.md`.

---

### Task 1: Fault engine — `ProtectKeyframes`, `Suppressed`, `randomAccess`

**Files:**
- Modify: `internal/fault/fault.go`
- Modify: `internal/fault/fault_test.go` (every `DecideFrame(x)` becomes `DecideFrame(x, false)`)
- Modify: `internal/rtsp/pump.go` (one-line call-site fix only, so the tree keeps building: `p.faults.DecideFrame(p.frameCounter, au.RandomAccess)`; Task 2 builds the real effect on that line)

**Interfaces:**
- Consumes: existing `fault.New`, `seed.Seed`.
- Produces: `fault.Spec.ProtectKeyframes bool`; `fault.Decision.Suppressed bool`; `func (e *Engine) DecideFrame(frameIndex int, randomAccess bool) []Decision`; `func Implemented(kind Kind) bool` (true only for `KindFrameDrop`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/fault/fault_test.go` and update the existing calls with `sed -i 's/DecideFrame(\([^)]*\))/DecideFrame(\1, false)/' internal/fault/fault_test.go`:

```go
func TestProtectKeyframesSuppressesDropOnRandomAccess(t *testing.T) {
	e, err := New(seed.Seed(9), []Spec{{Kind: KindFrameDrop, Rate: 1, ProtectKeyframes: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := e.DecideFrame(0, true)
	if len(d) != 1 || !d[0].Suppressed {
		t.Fatalf("random-access frame: got %+v, want one suppressed decision", d)
	}
	d = e.DecideFrame(1, false)
	if len(d) != 1 || d[0].Suppressed {
		t.Fatalf("non-random-access frame: got %+v, want one unsuppressed decision", d)
	}
}

func TestProtectKeyframesDoesNotChangeTheDrawSequence(t *testing.T) {
	run := func(protect bool) []int {
		e, err := New(seed.Seed(42), []Spec{{Kind: KindFrameDrop, Rate: 0.5, ProtectKeyframes: protect}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var fired []int
		for i := 0; i < 200; i++ {
			// Alternate random-access so protection actually engages.
			if len(e.DecideFrame(i, i%15 == 0)) > 0 {
				fired = append(fired, i)
			}
		}
		return fired
	}
	a, b := run(false), run(true)
	if len(a) != len(b) {
		t.Fatalf("protect changed how many decisions fire: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("protect changed the draw sequence at %d: %d vs %d", i, a[i], b[i])
		}
	}
}

func TestImplemented(t *testing.T) {
	if !Implemented(KindFrameDrop) {
		t.Error("frame_drop should be implemented")
	}
	for _, k := range []Kind{KindKeyframeStarvation, KindTeardownMidSession, KindAcceptThenSilence, KindSDPCodecLie, KindSPSResolutionLie} {
		if Implemented(k) {
			t.Errorf("%s should not be implemented yet", k)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/fault/ -count=1`
Expected: FAIL to compile (`ProtectKeyframes`, `Suppressed`, `Implemented` undefined; `DecideFrame` argument count).

- [ ] **Step 3: Implement**

In `internal/fault/fault.go`: add to `Spec`:

```go
	// ProtectKeyframes, for frame_drop, suppresses a drop that would remove a
	// random-access access unit. The draw is still consumed, so the sequence
	// does not depend on which frames happen to be keyframes.
	ProtectKeyframes bool
```

Add to `Decision`:

```go
	// Suppressed reports that the fault fired but must not be applied (for
	// example a frame_drop on a keyframe with ProtectKeyframes set).
	Suppressed bool
```

Add after `Known`:

```go
// Implemented reports whether a kind's effect exists in this version. Only
// implemented kinds are accepted by the public API; the rest stay refused.
func Implemented(kind Kind) bool { return kind == KindFrameDrop }
```

Replace `DecideFrame` with:

```go
// DecideFrame returns the faults that fire for the given frame counter.
// randomAccess reports whether the access unit at that position is a random
// access point; only ProtectKeyframes consults it.
//
// The counter is monotonic across loops of the source media, so a decision is
// tied to a position in the run rather than to a position in the file.
//
// Entries are walked in configured order, never map order, so the draw sequence
// is reproducible.
func (e *Engine) DecideFrame(frameIndex int, randomAccess bool) []Decision {
	var out []Decision
	for i := range e.entries {
		en := &e.entries[i]
		switch en.spec.Kind {
		case KindFrameDrop, KindKeyframeStarvation:
			// Draw unconditionally so the sequence does not depend on earlier
			// outcomes.
			if en.rnd.Float64() >= en.spec.Rate {
				continue
			}
			d := Decision{Kind: en.spec.Kind, FrameIndex: frameIndex}
			if en.spec.Kind == KindFrameDrop && en.spec.ProtectKeyframes && randomAccess {
				d.Suppressed = true
			}
			out = append(out, d)
		default:
			// Session, transport and control-plane faults are not frame-scoped.
			// They are catalogued and validated, and decided elsewhere when
			// their effects land.
		}
	}
	return out
}
```

- [ ] **Step 4: Run to verify pass**

Also change the call in `internal/rtsp/pump.go` (currently `p.faults.DecideFrame(p.frameCounter)`) to `p.faults.DecideFrame(p.frameCounter, au.RandomAccess)` — `au` is the access unit in scope there.

Run: `gofmt -l . ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race -count=1 ./internal/fault/ ./internal/rtsp/`
Expected: PASS and the whole tree builds (no red commit).

- [ ] **Step 5: Commit**

```bash
git add internal/fault internal/rtsp/pump.go
git commit -m "fault: add ProtectKeyframes, Suppressed and Implemented for frame_drop"
```

---

### Task 2: Pump — apply `frame_drop`, swap faults between frames

**Files:**
- Modify: `internal/rtsp/pump.go`
- Modify: `internal/rtsp/pump_test.go` (replace `TestFaultDecisionsAreRecordedButNotApplied`, add tests)

**Interfaces:**
- Consumes: Task 1's `DecideFrame(int, bool)`, `Decision.Suppressed`, `fault.New`.
- Produces: `func (p *Pump) SetFaults(specs []fault.Spec, applied func(frameIndex int)) error` — validates now, swaps in at the start of the next `Step`, calls `applied(frameCounter)` (if non-nil) from the pump goroutine when it does. Dropped frames are not encoded or written, do not call `obs.Frame`, but still advance `frameCounter`, `idx`, loop bookkeeping.

**Extra required tests (spec section 6, missing from the code below; write them in the same style):** (1) across a dropped frame the written RTP timestamp advances by exactly the dropped count plus one times `m.FrameDuration()` converted to the 90 kHz clock (assert on consecutive written timestamps, not just distinctness); (2) determinism after a mid-run swap: two pumps with the same seed, each calling `SetFaults` with the same specs at the same frame index, record identical dropped frame indices.

- [ ] **Step 1: Write the failing tests**

In `internal/rtsp/pump_test.go`, delete `TestFaultDecisionsAreRecordedButNotApplied` together with its two-line doc comment ("The seam is real: ...") and one of the two surrounding blank lines, then append the tests below (run `gofmt -l .` afterwards; `media` is already reachable via `testMedia`; no new imports are needed):

```go
// A catalogued fault whose effect does not exist yet is decided and recorded,
// and changes nothing on the wire.
func TestUnimplementedFaultIsRecordedButNotApplied(t *testing.T) {
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindKeyframeStarvation, Rate: 1}}, rec)

	const frames = 5
	for i := 0; i < frames; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if got := rec.Counters("cam").FaultsFired; got != frames {
		t.Fatalf("FaultsFired = %d, want %d", got, frames)
	}
	if got := rec.Counters("cam").FramesServed; got != frames {
		t.Fatalf("FramesServed = %d, want %d -- an unimplemented fault must not drop anything", got, frames)
	}
	if len(w.pkts) == 0 {
		t.Fatal("no packets written")
	}
	if ev := rec.Events(); ev[0].Detail != detailDecidedNotApplied {
		t.Errorf("detail = %q, want %q", ev[0].Detail, detailDecidedNotApplied)
	}
}

func TestFrameDropAtRateOneWritesNothingButAdvances(t *testing.T) {
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, rec)

	const frames = 6
	for i := 0; i < frames; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if len(w.pkts) != 0 {
		t.Fatalf("wrote %d packets, want 0 when every frame is dropped", len(w.pkts))
	}
	if got := rec.Counters("cam").FramesServed; got != 0 {
		t.Errorf("FramesServed = %d, want 0", got)
	}
	if got := rec.Counters("cam").FaultsFired; got != frames {
		t.Errorf("FaultsFired = %d, want %d", got, frames)
	}
	if got := p.FrameCounter(); got != frames {
		t.Errorf("FrameCounter = %d, want %d (a drop must not stall the position)", got, frames)
	}
	if ev := rec.Events(); ev[0].Detail != detailFrameDropApplied {
		t.Errorf("detail = %q, want %q", ev[0].Detail, detailFrameDropApplied)
	}
}

// A drop is encoder-side: RTP sequence numbers stay contiguous and the missing
// frame shows up only as a timestamp jump.
func TestFrameDropKeepsSequenceContiguousAndJumpsTimestamps(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(0xD20F), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.5}}, rec)

	steps := len(m.AUs) // stay inside one loop so timestamps are the raw DTS
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}

	dropped := map[int]bool{}
	for _, ev := range rec.Events() {
		dropped[ev.FrameIndex] = true
	}
	if len(dropped) == 0 || len(dropped) == steps {
		t.Fatalf("seed gave %d drops of %d frames; pick a seed that mixes both", len(dropped), steps)
	}

	for i := 1; i < len(w.pkts); i++ {
		if w.pkts[i].SequenceNumber != w.pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d: %d then %d", i, w.pkts[i-1].SequenceNumber, w.pkts[i].SequenceNumber)
		}
	}

	want := map[uint32]bool{}
	for i := 0; i < steps; i++ {
		if !dropped[i] {
			want[uint32(m.AUs[i].DTS)] = true
		}
	}
	got := map[uint32]bool{}
	for _, pkt := range w.pkts {
		got[pkt.Timestamp] = true
	}
	if len(got) != len(want) {
		t.Fatalf("distinct timestamps = %d, want %d", len(got), len(want))
	}
	for ts := range want {
		if !got[ts] {
			t.Errorf("missing timestamp %d of a frame that should have been written", ts)
		}
	}
}

func TestFrameDropIsSeedDeterministic(t *testing.T) {
	run := func() []int {
		rec := obs.New(1000)
		p, _ := newTestPump(t, seed.Seed(77), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.4}}, rec)
		for i := 0; i < 90; i++ {
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		var idx []int
		for _, ev := range rec.Events() {
			idx = append(idx, ev.FrameIndex)
		}
		return idx
	}
	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("no drops at rate 0.4 over 90 frames")
	}
	if len(a) != len(b) {
		t.Fatalf("runs differ: %d vs %d drops", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("drop %d differs: frame %d vs %d", i, a[i], b[i])
		}
	}
}

func TestProtectKeyframesKeepsRandomAccessFrames(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(3), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1, ProtectKeyframes: true}}, rec)

	steps := len(m.AUs)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}

	wantTS := map[uint32]bool{}
	for _, au := range m.AUs {
		if au.RandomAccess {
			wantTS[uint32(au.DTS)] = true
		}
	}
	if len(wantTS) == 0 {
		t.Fatal("fixture has no random-access units")
	}
	gotTS := map[uint32]bool{}
	for _, pkt := range w.pkts {
		gotTS[pkt.Timestamp] = true
	}
	if len(gotTS) != len(wantTS) {
		t.Fatalf("written frames = %d, want only the %d keyframes", len(gotTS), len(wantTS))
	}
	for ts := range gotTS {
		if !wantTS[ts] {
			t.Errorf("a non-keyframe (timestamp %d) was written", ts)
		}
	}
	suppressed := 0
	for _, ev := range rec.Events() {
		if ev.Detail == detailSuppressed {
			suppressed++
		}
	}
	if suppressed != len(wantTS) {
		t.Errorf("suppressed events = %d, want %d", suppressed, len(wantTS))
	}
}

func TestSetFaultsSwapsInAtTheNextFrameBoundary(t *testing.T) {
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(5), nil, rec)

	for i := 0; i < 2; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	written := len(w.pkts)
	if written == 0 {
		t.Fatal("no packets before the swap")
	}

	var appliedAt []int
	if err := p.SetFaults([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, func(f int) { appliedAt = append(appliedAt, f) }); err != nil {
		t.Fatalf("SetFaults: %v", err)
	}
	if len(appliedAt) != 0 {
		t.Fatal("SetFaults applied immediately; it must wait for the next Step")
	}
	if err := p.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(appliedAt) != 1 || appliedAt[0] != 2 {
		t.Fatalf("applied callbacks = %v, want [2]", appliedAt)
	}
	if len(w.pkts) != written {
		t.Fatal("the frame after the swap was written; it should have been dropped")
	}

	if err := p.SetFaults(nil, func(f int) { appliedAt = append(appliedAt, f) }); err != nil {
		t.Fatalf("SetFaults(nil): %v", err)
	}
	if err := p.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(appliedAt) != 2 || appliedAt[1] != 3 {
		t.Fatalf("applied callbacks = %v, want [2 3]", appliedAt)
	}
	if len(w.pkts) == written {
		t.Fatal("clearing the faults did not resume writing")
	}
}

func TestSetFaultsRejectsBadSpecs(t *testing.T) {
	p, _ := newTestPump(t, seed.Seed(5), nil, obs.New(10))
	if err := p.SetFaults([]fault.Spec{{Kind: "nope", Rate: 1}}, nil); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := p.SetFaults([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: 2}}, nil); err == nil {
		t.Error("rate 2 accepted")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/rtsp/ -run 'Fault|Protect|Unimplemented' -count=1`
Expected: FAIL to compile (`detailFrameDropApplied`, `detailSuppressed`, `SetFaults` undefined; `DecideFrame` arity).

- [ ] **Step 3: Implement**

In `internal/rtsp/pump.go`:

1. Add `"sync"` to the imports.
2. Next to `detailDecidedNotApplied` add:

```go
const (
	detailFrameDropApplied = "applied: access unit not encoded or sent"
	detailSuppressed       = "decided; suppressed by protect_keyframes"
)

// faultSwap is a pending replacement of a pump's fault engine.
type faultSwap struct {
	eng     *fault.Engine
	applied func(frameIndex int)
}
```

3. Add fields to `Pump` (after `faults *fault.Engine`):

```go
	faultSeed seed.Seed

	// swapMu guards pending. Everything else on a Pump is owned by the one
	// goroutine that calls Step; this queue is the only way another goroutine
	// reaches in, and it is drained between frames.
	swapMu  sync.Mutex
	pending []faultSwap
```

4. In `NewPump`, build `faultSeed := cfg.Seed.Stream("fault")`, pass it to `fault.New(faultSeed, cfg.Faults)` and store `faultSeed: faultSeed` in the returned `Pump`.
5. Add:

```go
// SetFaults replaces this camera's fault set. The specs are validated now; the
// swap itself happens at the start of the next Step, so no frame is
// half-affected. applied, if non-nil, is called from the stepping goroutine
// with the frame index the new set takes effect on. Several calls between two
// frames apply in order.
func (p *Pump) SetFaults(specs []fault.Spec, applied func(frameIndex int)) error {
	eng, err := fault.New(p.faultSeed, specs)
	if err != nil {
		return err
	}
	p.swapMu.Lock()
	p.pending = append(p.pending, faultSwap{eng: eng, applied: applied})
	p.swapMu.Unlock()
	return nil
}

func (p *Pump) applyPendingFaults() {
	p.swapMu.Lock()
	swaps := p.pending
	p.pending = nil
	p.swapMu.Unlock()
	for _, s := range swaps {
		p.faults = s.eng
		if s.applied != nil {
			s.applied(p.frameCounter)
		}
	}
}
```

6. Replace the start of `Step` through the encode/write block with:

```go
func (p *Pump) Step() error {
	p.applyPendingFaults()
	au := p.aus[p.idx]

	// The fault seam. frame_drop is applied here; kinds whose effect does not
	// exist yet are recorded as decided and change nothing.
	drop := false
	for _, d := range p.faults.DecideFrame(p.frameCounter, au.RandomAccess) {
		switch {
		case d.Suppressed:
			p.obs.Fault(p.id, d.FrameIndex, string(d.Kind), detailSuppressed)
		case d.Kind == fault.KindFrameDrop:
			p.obs.Fault(p.id, d.FrameIndex, string(d.Kind), detailFrameDropApplied)
			drop = true
		default:
			p.obs.Fault(p.id, d.FrameIndex, string(d.Kind), detailDecidedNotApplied)
		}
	}

	if !drop {
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
	}

	p.frameCounter++
	p.idx++
```

(keep the existing loop-seam block after `p.idx++` unchanged, and remove the old `p.obs.Frame(p.id)` / `p.frameCounter++` / `p.idx++` lines you replaced).

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./internal/rtsp/ && go test -race -count=1 ./internal/rtsp/ ./internal/fault/`
Expected: PASS, including the unchanged determinism and loop tests.

- [ ] **Step 5: Commit**

```bash
git add internal/rtsp/pump.go internal/rtsp/pump_test.go
git commit -m "rtsp: apply frame_drop in the pump and swap faults between frames"
```

---

### Task 3: `rtsp.Server.SetFaults`

**Files:**
- Modify: `internal/rtsp/server.go`
- Modify: `internal/rtsp/server_test.go`

**Interfaces:**
- Consumes: Task 2's `Pump.SetFaults`.
- Produces: `func (s *Server) SetFaults(id string, specs []fault.Spec, applied func(frameIndex int)) error` — `ErrNotStarted` before `Start`; `fmt.Errorf("rtsp: unknown camera %q", id)` for an unknown id (test with `strings.Contains`).

- [ ] **Step 1: Write the failing test**

Append to `internal/rtsp/server_test.go`. Add to its import block exactly `"sync/atomic"` and `"github.com/0x524a/camfarm/internal/fault"`; `errors`, `time`, `description`, `format`, `rtp` and `base` are already imported. `server.go` already imports `fault`.

```go
func TestSetFaultsStopsAndResumesDeliveryToARealClient(t *testing.T) {
	m := testMedia(t)
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(1000), Cameras: []CameraConfig{
		{ID: "cam-00", Media: m, Seed: seed.Seed(11)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
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
	var pkts atomic.Int64
	c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) { pkts.Add(1) })
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("packets to flow", func() bool { return pkts.Load() > 0 })

	applied := make(chan int, 1)
	if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, func(f int) { applied <- f }); err != nil {
		t.Fatalf("SetFaults: %v", err)
	}
	select {
	case <-applied:
	case <-time.After(10 * time.Second):
		t.Fatal("fault swap never applied")
	}
	// Nothing is written after the swap applied, so the count is now frozen.
	// Give in-flight packets time to arrive, then expect a stable count.
	time.Sleep(300 * time.Millisecond)
	frozen := pkts.Load()
	time.Sleep(500 * time.Millisecond)
	if got := pkts.Load(); got != frozen {
		t.Fatalf("packets kept arriving under frame_drop rate 1: %d -> %d", frozen, got)
	}

	cleared := make(chan int, 1)
	if err := s.SetFaults("cam-00", nil, func(f int) { cleared <- f }); err != nil {
		t.Fatalf("SetFaults(nil): %v", err)
	}
	waitFor("delivery to resume", func() bool { return pkts.Load() > frozen })
}

func TestSetFaultsUnknownCamera(t *testing.T) {
	s := startServer(t, 1)
	err := s.SetFaults("nope", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown camera") {
		t.Fatalf("err = %v, want an unknown-camera error", err)
	}
}

func TestSetFaultsBeforeStart(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(10), Cameras: []CameraConfig{
		{ID: "c", Media: testMedia(t), Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SetFaults("c", nil, nil); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
}
```

Ensure the imports `errors`, `description`, `format`, `rtp`, `fault`, `base` are present (most already are).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/rtsp/ -run SetFaults -count=1`
Expected: FAIL to compile (`s.SetFaults undefined`).

- [ ] **Step 3: Implement**

In `internal/rtsp/server.go`: add `pump *Pump` to the `camera` struct. In `Start`, inside the loop that launches pumps, set `cam.pump = pump` next to `cam.cancel = camCancel`. In `AddCamera`, set `pump: pump` in the `&camera{...}` literal. Add:

```go
// SetFaults replaces a camera's fault set on a running server. The swap takes
// effect at the camera's next frame boundary; applied, if non-nil, receives the
// frame index it took effect on.
func (s *Server) SetFaults(id string, specs []fault.Spec, applied func(frameIndex int)) error {
	s.mu.RLock()
	started := s.srv != nil
	cam, ok := s.cams[id]
	s.mu.RUnlock()
	if !started {
		return ErrNotStarted
	}
	if !ok || cam.pump == nil {
		return fmt.Errorf("rtsp: unknown camera %q", id)
	}
	return cam.pump.SetFaults(specs, applied)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/rtsp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rtsp/server.go internal/rtsp/server_test.go
git commit -m "rtsp: add Server.SetFaults for swapping a camera's faults at runtime"
```

---

### Task 4: Root API — accept `frame_drop`, `Inject`/`Clear`, sentinels, accessors

**Files:**
- Modify: `errors.go`, `spec.go`, `camfarm.go`, `camfarm_test.go`

**Interfaces:**
- Consumes: Task 3's `Server.SetFaults`, Task 1's `fault.Implemented`.
- Produces: `ErrDuplicateCamera`, `ErrInvalidSpec`; `FaultSpec.ProtectKeyframes bool` (`json:"protectKeyframes,omitempty"`); `(*Camera).Spec() CameraSpec`, `(*Camera).Auth() (user, pass string, ok bool)`, `(*Camera).Faults() []FaultSpec`; working `Inject`/`Clear`; unexported `Camera.noteFaultsApplied(action string, f FaultSpec, frameIndex int)` hook that Task 5 fills in (in this task it is an empty method).

- [ ] **Step 1: Write the failing tests**

In `camfarm_test.go`, in this order: (a) delete `TestInjectAndClear` entirely (its role is taken by `TestInjectUnimplementedKindIsUnsupported` and `TestInjectThenClearOnARunningCamera` below); (b) in `TestFaultsAreRefusedNotIgnored` and `TestAddCameraFaultRefused` change `frame_drop` to `keyframe_starvation` (the two `Kind:` literals and the `strings.Contains(err.Error(), "frame_drop")` check) — do this BEFORE appending, never as a blanket replace afterwards; (c) then append the tests below. `time` is already imported there.

```go
func TestInjectUnimplementedKindIsUnsupported(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	err := cam.Inject(FaultSpec{Kind: "keyframe_starvation", Rate: 1})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "not-a-fault", Rate: 1}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("unknown kind: err = %v, want ErrInvalidSpec", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 2}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("bad rate: err = %v, want ErrInvalidSpec", err)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSpecTimeFrameDropTakesEffect(t *testing.T) {
	s := minimalSpec()
	s.Cameras[0].Faults = []FaultSpec{{Kind: "frame_drop", Rate: 1}}
	f := StartT(t, s)
	cam, _ := f.Camera("front-door")
	other, _ := f.Camera("lobby")

	waitUntil(t, "drops to be recorded", func() bool { return cam.Stats().FaultsFired >= 3 })
	if got := cam.Stats().FramesServed; got != 0 {
		t.Errorf("FramesServed = %d, want 0 under frame_drop rate 1", got)
	}
	waitUntil(t, "the other camera to serve frames", func() bool { return other.Stats().FramesServed > 0 })
	if other.Stats().FaultsFired != 0 {
		t.Error("a fault on one camera fired on another")
	}
	if got := cam.Faults(); len(got) != 1 || got[0].Kind != "frame_drop" {
		t.Errorf("Faults() = %+v", got)
	}
}

func TestInjectThenClearOnARunningCamera(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	waitUntil(t, "frames to flow", func() bool { return cam.Stats().FramesServed > 0 })

	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	waitUntil(t, "drops to be recorded", func() bool { return cam.Stats().FaultsFired >= 3 })
	if got := cam.Faults(); len(got) != 1 {
		t.Fatalf("Faults() = %+v, want the injected fault", got)
	}

	if err := cam.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(cam.Faults()) != 0 {
		t.Fatal("Faults() not empty after Clear")
	}
	before := cam.Stats().FramesServed
	waitUntil(t, "frames to resume", func() bool { return cam.Stats().FramesServed > before })

	// Clearing with nothing active succeeds.
	if err := cam.Clear(); err != nil {
		t.Errorf("second Clear: %v", err)
	}
}

func TestInjectReplacesTheSameKind(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	_ = cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.2})
	_ = cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.9, ProtectKeyframes: true})
	got := cam.Faults()
	if len(got) != 1 || got[0].Rate != 0.9 || !got[0].ProtectKeyframes {
		t.Fatalf("Faults() = %+v, want one frame_drop at 0.9 with protectKeyframes", got)
	}
}

func TestSpecAndAuthAccessors(t *testing.T) {
	s := Spec{Seed: 1, Cameras: []CameraSpec{{ID: "locked", Auth: AuthSpec{Username: "admin", Password: "pw"}}, {ID: "open"}}}
	f := StartT(t, s)
	locked, _ := f.Camera("locked")
	open, _ := f.Camera("open")

	if u, p, ok := locked.Auth(); !ok || u != "admin" || p != "pw" {
		t.Errorf("Auth() = %q %q %v", u, p, ok)
	}
	if _, _, ok := open.Auth(); ok {
		t.Error("open camera reports credentials")
	}
	if got := locked.Spec(); got.ID != "locked" || got.Source.Kind != SourceFixture {
		t.Errorf("Spec() = %+v", got)
	}
}

func TestSentinelErrors(t *testing.T) {
	f := StartT(t, minimalSpec())
	if _, err := f.AddCamera(CameraSpec{ID: "front-door"}); !errors.Is(err, ErrDuplicateCamera) {
		t.Errorf("duplicate: err = %v, want ErrDuplicateCamera", err)
	}
	for name, cs := range map[string]CameraSpec{
		"empty id":   {ID: ""},
		"slash":      {ID: "a/b"},
		"half auth":  {ID: "x", Auth: AuthSpec{Username: "u"}},
		"bad source": {ID: "y", Source: SourceSpec{Kind: "nonsense"}},
		"bad fault":  {ID: "z", Faults: []FaultSpec{{Kind: "nope", Rate: 1}}},
	} {
		if _, err := f.AddCamera(cs); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec", name, err)
		}
	}
}
```

Add `"time"` to the imports if absent.

- [ ] **Step 2: Run to verify failure**

Run: `go test . -run 'Inject|FrameDrop|Accessors|Sentinel' -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`errors.go`, add inside the `var (...)` block:

```go
	// ErrDuplicateCamera means a camera with that ID already exists.
	ErrDuplicateCamera = errors.New("camfarm: duplicate camera")

	// ErrInvalidSpec means a spec was malformed or out of range, as opposed to
	// merely unsupported by this build.
	ErrInvalidSpec = errors.New("camfarm: invalid spec")
```

`spec.go`:
- `FaultSpec` gains `ProtectKeyframes bool `json:"protectKeyframes,omitempty"`` (document: for frame_drop, never drops a random-access frame).
- In `validateCameraSpec`, wrap every non-`ErrUnsupported` failure so it satisfies `errors.Is(err, ErrInvalidSpec)`, using the form `fmt.Errorf("%w: camera has an empty ID", ErrInvalidSpec)` and likewise for: reserved characters, unknown source kind, FixtureWithPath, FileWithoutPath, unknown fault kind, rate out of range, half auth. The duplicate case becomes `fmt.Errorf("%w: %q", ErrDuplicateCamera, c.ID)`. Keep the `ErrUnsupported` codec message as is.
- Replace the fault loop body's tail so an *implemented* kind is accepted:

```go
	for _, f := range c.Faults {
		if !fault.Known(fault.Kind(f.Kind)) {
			return c, fmt.Errorf("%w: camera %q requests unknown fault kind %q", ErrInvalidSpec, c.ID, f.Kind)
		}
		if f.Rate < 0 || f.Rate > 1 {
			return c, fmt.Errorf("%w: camera %q fault %q has rate %v outside [0,1]", ErrInvalidSpec, c.ID, f.Kind, f.Rate)
		}
		if !fault.Implemented(fault.Kind(f.Kind)) {
			// Catalogued, validated, and refused. Accepting it silently would
			// make a test that asked for a fault pass while nothing misbehaved.
			return c, fmt.Errorf("%w: fault %q is catalogued but its effect is not implemented in this version",
				ErrUnsupported, f.Kind)
		}
	}
```

- Add the helper:

```go
// toFaultSpecs converts the public fault specs to the engine's.
func toFaultSpecs(in []FaultSpec) []fault.Spec {
	out := make([]fault.Spec, 0, len(in))
	for _, f := range in {
		out = append(out, fault.Spec{Kind: fault.Kind(f.Kind), Rate: f.Rate, ProtectKeyframes: f.ProtectKeyframes})
	}
	return out
}
```

`camfarm.go`:
- `Camera` struct gains `spec CameraSpec`, `mu sync.Mutex`, `faults []FaultSpec`.
- In `Start`'s loop: set `spec: cs, faults: append([]FaultSpec(nil), cs.Faults...)` on the `&Camera{...}` and replace the "Faults is deliberately left unset" comment + `rtsp.CameraConfig` with `Faults: toFaultSpecs(cs.Faults)`.
- In `AddCamera`: same (`spec: cs`, `faults` copy, and `Faults: toFaultSpecs(cs.Faults)` in the `f.srv.AddCamera` config).
- Replace `Inject` and `Clear` and add accessors:

```go
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
func (c *Camera) Inject(f FaultSpec) error {
	if !fault.Known(fault.Kind(f.Kind)) {
		return fmt.Errorf("%w: unknown fault kind %q", ErrInvalidSpec, f.Kind)
	}
	if f.Rate < 0 || f.Rate > 1 {
		return fmt.Errorf("%w: fault %q has rate %v outside [0,1]", ErrInvalidSpec, f.Kind, f.Rate)
	}
	if !fault.Implemented(fault.Kind(f.Kind)) {
		return fmt.Errorf("%w: injecting fault %q is not implemented in this version", ErrUnsupported, f.Kind)
	}

	c.mu.Lock()
	next := make([]FaultSpec, 0, len(c.faults)+1)
	for _, ex := range c.faults {
		if ex.Kind != f.Kind {
			next = append(next, ex)
		}
	}
	next = append(next, f)
	err := c.swapFaults(next, func(frame int) { c.fleet.noteFault("inject", c.id, f, frame) })
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
	if len(c.faults) == 0 {
		return nil
	}
	err := c.swapFaults(nil, func(frame int) { c.fleet.noteFault("clear", c.id, FaultSpec{}, frame) })
	if err == nil {
		c.faults = nil
	}
	return err
}

// swapFaults hands a new fault set to the camera's pump. Caller holds c.mu.
func (c *Camera) swapFaults(next []FaultSpec, applied func(int)) error {
	if err := c.fleet.srv.SetFaults(c.id, toFaultSpecs(next), applied); err != nil {
		if !c.fleet.srv.Has(c.id) {
			return fmt.Errorf("%w: %q", ErrUnknownCamera, c.id)
		}
		return err
	}
	return nil
}
```

- Add `func (f *Fleet) noteFault(action, camera string, spec FaultSpec, frame int) {}` in `camfarm.go` as an empty method with a one-line doc comment; Task 5 replaces its body. Add `fault` to the root imports if absent (`internal/fault`).

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: PASS everywhere (including previously existing fault-refusal tests, now using `keyframe_starvation`).

- [ ] **Step 5: Commit**

```bash
git add errors.go spec.go camfarm.go camfarm_test.go
git commit -m "Accept frame_drop in specs, implement Inject/Clear, add sentinels and accessors"
```

---

### Task 5: Journal, `Subscribe` change feed, replay line

**Files:**
- Create: `events.go`, `events_test.go`
- Modify: `camfarm.go` (`Fleet` fields; `noteFault` body; publish from `AddCamera`/`RemoveCamera`; `Close`; `ReplayLine`)

**Interfaces:**
- Consumes: Task 4's `noteFault` hook.
- Produces:

```go
type EventType string
const (
	EventCameraAdded   EventType = "camera_added"
	EventCameraRemoved EventType = "camera_removed"
	EventFaultInjected EventType = "fault_injected"
	EventFaultCleared  EventType = "fault_cleared"
)
type Event struct {
	Seq        uint64     `json:"seq"`
	Type       EventType  `json:"type"`
	Camera     string     `json:"camera"`
	Fault      *FaultSpec `json:"fault,omitempty"`
	FrameIndex int        `json:"frameIndex"` // -1 for add/remove
}
type JournalEntry struct { Seq uint64; Action string; Camera string; Fault FaultSpec; FrameIndex int } // Action: add|remove|inject|clear
func (f *Fleet) Subscribe(ctx context.Context) <-chan Event
func (f *Fleet) Journal() []JournalEntry
```

`ReplayLine` appends ` journal=<n>:<12 hex>` when the journal is non-empty, where the hex is the SHA-256 of the JSON of the entries.

- [ ] **Step 1: Write the failing tests**

`events_test.go`:

```go
package camfarm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

func TestSubscribeDeliversEventsInOrder(t *testing.T) {
	f := StartT(t, minimalSpec())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := f.Subscribe(ctx)

	if _, err := f.AddCamera(CameraSpec{ID: "extra"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	cam, _ := f.Camera("extra")
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	waitUntil(t, "inject to be journaled", func() bool { return len(f.Journal()) >= 2 })
	if err := cam.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	waitUntil(t, "clear to be journaled", func() bool { return len(f.Journal()) >= 3 })
	if err := f.RemoveCamera("extra"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	want := []EventType{EventCameraAdded, EventFaultInjected, EventFaultCleared, EventCameraRemoved}
	var last uint64
	for _, w := range want {
		e := recv(t, ch)
		if e.Type != w || e.Camera != "extra" {
			t.Fatalf("event = %+v, want %s for extra", e, w)
		}
		if e.Seq <= last {
			t.Fatalf("seq %d not increasing after %d", e.Seq, last)
		}
		last = e.Seq
	}
}

func TestSubscribeChannelClosesWhenContextEnds(t *testing.T) {
	f := StartT(t, minimalSpec())
	ctx, cancel := context.WithCancel(context.Background())
	ch := f.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("got an event instead of a close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("channel not closed after ctx cancel")
	}
}

func TestStalledSubscriberIsDroppedWithoutBlocking(t *testing.T) {
	f := StartT(t, minimalSpec())
	ch := f.Subscribe(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			f.publish(Event{Type: EventCameraAdded, Camera: "x", FrameIndex: -1})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publish blocked on a subscriber that never reads")
	}
	// The stalled subscriber's channel ends up closed once drained.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stalled subscriber was not closed")
		}
	}
}

func TestReplayLineCarriesTheJournal(t *testing.T) {
	f := StartT(t, minimalSpec())
	before := f.ReplayLine()
	if strings.Contains(before, "journal=") {
		t.Fatalf("fresh fleet replay line has a journal: %q", before)
	}
	if _, err := f.AddCamera(CameraSpec{ID: "extra"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	after := f.ReplayLine()
	if !strings.Contains(after, "journal=1:") {
		t.Fatalf("replay line = %q, want journal=1:<hash>", after)
	}
	if !strings.HasPrefix(after, before) {
		t.Errorf("the seed and spec hash prefix changed: %q vs %q", after, before)
	}
}

func TestJournalRecordsFrameIndexForFaults(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	waitUntil(t, "frames", func() bool { return cam.Stats().FramesServed > 3 })
	_ = cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.5})
	waitUntil(t, "inject journaled", func() bool { return len(f.Journal()) == 1 })

	e := f.Journal()[0]
	if e.Action != "inject" || e.Camera != "front-door" || e.FrameIndex < 3 {
		t.Fatalf("journal entry = %+v", e)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test . -run 'Subscribe|Journal|ReplayLine|Stalled' -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`events.go`:

```go
package camfarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

// EventType names a change to a running fleet.
type EventType string

// Event types.
const (
	EventCameraAdded   EventType = "camera_added"
	EventCameraRemoved EventType = "camera_removed"
	EventFaultInjected EventType = "fault_injected"
	EventFaultCleared  EventType = "fault_cleared"
)

// Event is one change to a running fleet.
type Event struct {
	Seq    uint64     `json:"seq"`
	Type   EventType  `json:"type"`
	Camera string     `json:"camera"`
	Fault  *FaultSpec `json:"fault,omitempty"`
	// FrameIndex is the frame a fault change took effect on; -1 for add/remove.
	FrameIndex int `json:"frameIndex"`
}

// JournalEntry records one runtime change. Start-time cameras and faults are
// not journaled: the spec hash already identifies them.
type JournalEntry struct {
	Seq    uint64    `json:"seq"`
	Action string    `json:"action"` // add, remove, inject, clear
	Camera string    `json:"camera"`
	Fault  FaultSpec `json:"fault"`
	// FrameIndex is when a fault change landed. It depends on when the change
	// was requested, which is the limit of replaying an interactive run.
	FrameIndex int `json:"frameIndex"`
}

const subscriberBuffer = 64

// subscriber is one Subscribe call. end closes both channels exactly once; the
// caller must have removed s from events.subs under events.mu first. done lets
// the per-subscription goroutine exit even when its ctx never ends.
type subscriber struct {
	ch   chan Event
	done chan struct{}
}

func (s *subscriber) end() {
	close(s.ch)
	close(s.done)
}

// events holds the fleet's change feed and journal.
type events struct {
	mu      sync.Mutex
	seq     uint64
	journal []JournalEntry
	subs    map[*subscriber]struct{}
	closed  bool
}

func (e *events) init() { e.subs = make(map[*subscriber]struct{}) }

// publish assigns the next sequence number, records the event in the journal
// and delivers it to subscribers without ever blocking: a subscriber whose
// buffer is full is dropped and its channel closed, so a stalled dashboard
// cannot stall the fleet. Subscribers re-list on reconnect, so a dropped
// subscriber misses nothing it cannot recover.
func (f *Fleet) publish(ev Event) {
	e := &f.ev
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	ev.Seq = e.seq

	action := map[EventType]string{
		EventCameraAdded: "add", EventCameraRemoved: "remove",
		EventFaultInjected: "inject", EventFaultCleared: "clear",
	}[ev.Type]
	if action != "" {
		var fs FaultSpec
		if ev.Fault != nil {
			fs = *ev.Fault
		}
		e.journal = append(e.journal, JournalEntry{
			Seq: ev.Seq, Action: action, Camera: ev.Camera, Fault: fs, FrameIndex: ev.FrameIndex,
		})
	}

	for s := range e.subs {
		select {
		case s.ch <- ev:
		default:
			delete(e.subs, s)
			s.end()
		}
	}
}

// Subscribe returns a channel of fleet changes, closed when ctx ends, when the
// fleet closes, or if the subscriber falls too far behind.
func (f *Fleet) Subscribe(ctx context.Context) <-chan Event {
	s := &subscriber{ch: make(chan Event, subscriberBuffer), done: make(chan struct{})}
	e := &f.ev
	e.mu.Lock()
	if e.closed {
		s.end()
		e.mu.Unlock()
		return s.ch
	}
	e.subs[s] = struct{}{}
	e.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
		case <-s.done:
			return
		}
		e.mu.Lock()
		if _, ok := e.subs[s]; ok {
			delete(e.subs, s)
			s.end()
		}
		e.mu.Unlock()
	}()
	return s.ch
}

// closeSubscribers ends every subscription. Called from Close.
func (f *Fleet) closeSubscribers() {
	e := &f.ev
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for s := range e.subs {
		delete(e.subs, s)
		s.end()
	}
}

// Journal returns the runtime changes made so far, oldest first.
func (f *Fleet) Journal() []JournalEntry {
	e := &f.ev
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]JournalEntry(nil), e.journal...)
}

// journalSuffix is appended to the replay line once the journal is non-empty.
func (f *Fleet) journalSuffix() string {
	j := f.Journal()
	if len(j) == 0 {
		return ""
	}
	b, _ := json.Marshal(j)
	sum := sha256.Sum256(b)
	return fmt.Sprintf(" journal=%d:%s", len(j), hex.EncodeToString(sum[:])[:12])
}

// noteFault records a fault change that took effect on frame.
func (f *Fleet) noteFault(action, camera string, spec FaultSpec, frame int) {
	ev := Event{Camera: camera, FrameIndex: frame}
	if action == "inject" {
		ev.Type = EventFaultInjected
		ev.Fault = &spec
	} else {
		ev.Type = EventFaultCleared
	}
	f.publish(ev)
}
```

In `camfarm.go`: add `ev events` to `Fleet`; call `f.ev.init()` in `Start` right after the `Fleet` literal is built; give Task 4's empty `func (c *Camera) noteFaultsApplied(action string, f FaultSpec, frameIndex int) {}` (already in `camfarm.go`; there is NO `Fleet.noteFault` stub) the body `c.fleet.noteFault(action, c.id, f, frameIndex)` and keep its doc comment accurate (do not take `c.mu` or `Fleet.mu` inside it; `Inject` never waits on the callback, which runs on the pump goroutine); in `AddCamera` after the camera is registered (just before the `f.log.Info("camera added"...)`) call `f.publish(Event{Type: EventCameraAdded, Camera: cs.ID, FrameIndex: -1})`; in `RemoveCamera` after both servers are removed and before the log, call `f.publish(Event{Type: EventCameraRemoved, Camera: id, FrameIndex: -1})` (publish even if one side errored — the camera is gone from the fleet — so place the publish right after the map delete, before the server removals); in `Close` call `f.closeSubscribers()` after `stop()`; change `ReplayLine` to:

```go
	return fmt.Sprintf("camfarm: seed=%#x spec=sha256:%s", f.spec.Seed, f.specHash[:12]) + f.journalSuffix()
```

Note: `publish` takes `f.ev.mu`, never `f.mu`; the `AddCamera` call happens while holding `f.mu`. That lock order (fleet.mu → ev.mu) is the only one; `noteFault` runs on the pump goroutine and takes only `ev.mu`.

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add events.go events_test.go camfarm.go
git commit -m "Add fleet change feed, runtime journal and journaled replay line"
```

---

### Task 6: Dashboard — REST API and snapshots

**Files:**
- Create: `internal/dashboard/dashboard.go`, `internal/dashboard/snapshot.go`, `internal/dashboard/web/index.html` (a minimal placeholder replaced in Task 8), `internal/dashboard/dashboard_test.go`

**Interfaces:**
- Consumes: the public `camfarm` API from Tasks 4–5.
- Produces:

```go
type Options struct {
	// SnapshotTimeout bounds one capture; zero means defaultSnapshotTimeout (10s).
	SnapshotTimeout time.Duration
	// MaxConcurrentSnapshots caps captures in flight across cameras; zero means 4.
	MaxConcurrentSnapshots int
	// capture is the capture seam; nil means Camera.Screenshot. Tests only.
	capture func(context.Context, *camfarm.Camera) (image.Image, error)
}
func Handler(f *camfarm.Fleet, opt Options) http.Handler
```

Routes implemented here: `GET /api/v1/cameras`, `POST /api/v1/cameras`, `GET /api/v1/cameras/{id}`, `DELETE /api/v1/cameras/{id}`, `GET /api/v1/cameras/{id}/snapshot.jpg`, `POST /api/v1/cameras/{id}/faults`, `DELETE /api/v1/cameras/{id}/faults`, `GET /`.

- [ ] **Step 1: Write the failing tests**

`dashboard_test.go` (package `dashboard`):

```go
package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
)

func newServer(t *testing.T, opt Options) (*httptest.Server, *camfarm.Fleet) {
	t.Helper()
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, opt))
	t.Cleanup(ts.Close)
	return ts, f
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestListAndGetCameras(t *testing.T) {
	ts, _ := newServer(t, Options{})

	resp := do(t, "GET", ts.URL+"/api/v1/cameras", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0]["id"] != "front-door" {
		t.Fatalf("list = %v", list)
	}
	for _, k := range []string{"rtspUrl", "onvifEndpoint", "seed", "codec", "width", "height"} {
		if _, ok := list[0][k]; !ok {
			t.Errorf("camera JSON has no %q: %v", k, list[0])
		}
	}

	resp = do(t, "GET", ts.URL+"/api/v1/cameras/front-door", "")
	if resp.StatusCode != 200 {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/api/v1/cameras/nope", "")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown camera status = %d, want 404", resp.StatusCode)
	}
}

func TestAddAndRemoveCamera(t *testing.T) {
	ts, f := newServer(t, Options{})

	resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"lobby","auth":{"username":"a","password":"b"}}`)
	if resp.StatusCode != 201 {
		t.Fatalf("add status = %d, want 201", resp.StatusCode)
	}
	cam, err := f.Camera("lobby")
	if err != nil {
		t.Fatalf("camera not added: %v", err)
	}
	if _, _, ok := cam.Auth(); !ok {
		t.Error("credentials not applied")
	}

	if resp = do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"lobby"}`); resp.StatusCode != 409 {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"a/b"}`); resp.StatusCode != 422 {
		t.Errorf("invalid id status = %d, want 422", resp.StatusCode)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras", `{not json`); resp.StatusCode != 400 {
		t.Errorf("malformed body status = %d, want 400", resp.StatusCode)
	}

	if resp = do(t, "DELETE", ts.URL+"/api/v1/cameras/lobby", ""); resp.StatusCode != 204 {
		t.Errorf("delete status = %d, want 204", resp.StatusCode)
	}
	if resp = do(t, "DELETE", ts.URL+"/api/v1/cameras/lobby", ""); resp.StatusCode != 404 {
		t.Errorf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestMutationsRequireJSONContentType(t *testing.T) {
	ts, _ := newServer(t, Options{})
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/cameras", strings.NewReader(`{"id":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestSnapshotIsAJPEGOfTheRealStream(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/api/v1/cameras/front-door/snapshot.jpg", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	img, err := jpeg.Decode(resp.Body)
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("bounds = %v, want 320x240", b)
	}
	if resp = do(t, "GET", ts.URL+"/api/v1/cameras/nope/snapshot.jpg", ""); resp.StatusCode != 404 {
		t.Errorf("unknown camera snapshot status = %d, want 404", resp.StatusCode)
	}
}

// capturing returns a capture seam that counts calls and blocks until released.
func capturing(release <-chan struct{}) (func(context.Context, *camfarm.Camera) (image.Image, error), *atomic.Int32) {
	var n atomic.Int32
	return func(ctx context.Context, _ *camfarm.Camera) (image.Image, error) {
		n.Add(1)
		select {
		case <-release:
			return image.NewRGBA(image.Rect(0, 0, 4, 4)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, &n
}

func TestConcurrentSnapshotsOfOneCameraAreCoalesced(t *testing.T) {
	release := make(chan struct{})
	capture, calls := capturing(release)
	ts, _ := newServer(t, Options{capture: capture})

	const clients = 5
	var wg sync.WaitGroup
	statuses := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/api/v1/cameras/front-door/snapshot.jpg")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	// Let every request join the one in-flight capture before releasing it.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("captures = %d, want 1 shared by %d requests", got, clients)
	}
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("client %d status = %d", i, s)
		}
	}
}

func TestSnapshotTimeoutIs504(t *testing.T) {
	capture, _ := capturing(make(chan struct{})) // never released
	ts, _ := newServer(t, Options{capture: capture, SnapshotTimeout: 200 * time.Millisecond})
	start := time.Now()
	resp := do(t, "GET", ts.URL+"/api/v1/cameras/front-door/snapshot.jpg", "")
	if resp.StatusCode != 504 {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not bound the request")
	}
}

func TestSnapshotConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	capture, calls := capturing(release)
	ts, f := newServer(t, Options{capture: capture, MaxConcurrentSnapshots: 1, SnapshotTimeout: 5 * time.Second})
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "second"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}

	var wg sync.WaitGroup
	for _, id := range []string{"front-door", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/api/v1/cameras/" + id + "/snapshot.jpg")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("captures in flight = %d, want 1 under a cap of 1", got)
	}
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("total captures = %d, want 2", got)
	}
}

func TestInjectAndClearFaultOverHTTP(t *testing.T) {
	ts, f := newServer(t, Options{})
	cam, _ := f.Camera("front-door")

	resp := do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"frame_drop","rate":0.5,"protectKeyframes":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("inject status = %d", resp.StatusCode)
	}
	if got := cam.Faults(); len(got) != 1 || !got[0].ProtectKeyframes {
		t.Fatalf("Faults() = %+v", got)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"keyframe_starvation","rate":1}`); resp.StatusCode != 422 {
		t.Errorf("unsupported kind status = %d, want 422", resp.StatusCode)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras/nope/faults", `{"kind":"frame_drop","rate":1}`); resp.StatusCode != 404 {
		t.Errorf("unknown camera status = %d, want 404", resp.StatusCode)
	}
	if resp = do(t, "DELETE", ts.URL+"/api/v1/cameras/front-door/faults", ""); resp.StatusCode != 204 {
		t.Errorf("clear status = %d, want 204", resp.StatusCode)
	}
	if len(cam.Faults()) != 0 {
		t.Error("faults not cleared")
	}
}

func TestRootServesTheEmbeddedPage(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(strings.ToLower(buf.String()), "<!doctype html>") {
		t.Errorf("root is not an HTML page: %.80q", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dashboard/ -count=1`
Expected: FAIL (package does not exist).

- [ ] **Step 3: Implement**

`internal/dashboard/web/index.html` (placeholder): `<!doctype html><html lang="en"><meta charset="utf-8"><title>CamFarm</title><body><main id="app">CamFarm</main>`.

`internal/dashboard/snapshot.go`:

```go
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
```

`internal/dashboard/dashboard.go`:

```go
// Package dashboard serves an HTTP API and an embedded web page that drive a
// camfarm fleet. It is a thin layer over the public camfarm API.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"

	"github.com/0x524a/camfarm"
)

//go:embed web
var webFS embed.FS

const maxBody = 1 << 20

type server struct {
	fleet *camfarm.Fleet
	snap  *snapshotter
}

// Handler returns the dashboard for a fleet.
func Handler(f *camfarm.Fleet, opt Options) http.Handler {
	s := &server{fleet: f, snap: newSnapshotter(opt)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/fleet", s.getFleet)
	mux.HandleFunc("GET /api/v1/cameras", s.listCameras)
	mux.HandleFunc("POST /api/v1/cameras", s.addCamera)
	mux.HandleFunc("GET /api/v1/cameras/{id}", s.getCamera)
	mux.HandleFunc("DELETE /api/v1/cameras/{id}", s.removeCamera)
	mux.HandleFunc("GET /api/v1/cameras/{id}/snapshot.jpg", s.snapshot)
	mux.HandleFunc("POST /api/v1/cameras/{id}/faults", s.injectFault)
	mux.HandleFunc("DELETE /api/v1/cameras/{id}/faults", s.clearFaults)
	mux.HandleFunc("GET /api/v1/events", s.events)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embed directive guarantees the directory exists
	}
	mux.Handle("GET /", http.FileServerFS(sub))
	return mux
}

type cameraDTO struct {
	ID            string             `json:"id"`
	RTSPURL       string             `json:"rtspUrl"`
	ONVIFEndpoint string             `json:"onvifEndpoint"`
	Source        string             `json:"source"`
	Codec         string             `json:"codec"`
	Width         int                `json:"width"`
	Height        int                `json:"height"`
	FPS           float64            `json:"fps"`
	Readers       int                `json:"readers"`
	Seed          uint64             `json:"seed"`
	Username      string             `json:"username,omitempty"`
	Password      string             `json:"password,omitempty"`
	Faults        []camfarm.FaultSpec `json:"faults"`
	Stats         camfarm.Stats      `json:"stats"`
}

func (s *server) dto(cam *camfarm.Camera, st camfarm.Status) cameraDTO {
	u, p, _ := cam.Auth()
	faults := cam.Faults()
	if faults == nil {
		faults = []camfarm.FaultSpec{}
	}
	return cameraDTO{
		ID: st.ID, RTSPURL: st.RTSPURL, ONVIFEndpoint: cam.ONVIFEndpoint(),
		Source: st.Source, Codec: st.Codec, Width: st.Width, Height: st.Height, FPS: st.FPS,
		Readers: st.Readers, Seed: cam.Stats().Seed, Username: u, Password: p,
		Faults: faults, Stats: cam.Stats(),
	}
}

func (s *server) listCameras(w http.ResponseWriter, _ *http.Request) {
	statuses := s.fleet.List()
	out := make([]cameraDTO, 0, len(statuses))
	for _, st := range statuses {
		cam, err := s.fleet.Camera(st.ID)
		if err != nil {
			continue // removed between List and Camera
		}
		out = append(out, s.dto(cam, st))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) statusOf(id string) (*camfarm.Camera, camfarm.Status, error) {
	cam, err := s.fleet.Camera(id)
	if err != nil {
		return nil, camfarm.Status{}, err
	}
	for _, st := range s.fleet.List() {
		if st.ID == id {
			return cam, st, nil
		}
	}
	return nil, camfarm.Status{}, fmt.Errorf("%w: %q", camfarm.ErrUnknownCamera, id)
}

func (s *server) getCamera(w http.ResponseWriter, r *http.Request) {
	cam, st, err := s.statusOf(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(cam, st))
}

func (s *server) addCamera(w http.ResponseWriter, r *http.Request) {
	var spec camfarm.CameraSpec
	if !decodeJSON(w, r, &spec) {
		return
	}
	cam, err := s.fleet.AddCamera(spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, st, err := s.statusOf(cam.ID())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.dto(cam, st))
}

func (s *server) removeCamera(w http.ResponseWriter, r *http.Request) {
	if err := s.fleet.RemoveCamera(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) snapshot(w http.ResponseWriter, r *http.Request) {
	cam, err := s.fleet.Camera(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.snap.get(r.Context(), cam)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *server) injectFault(w http.ResponseWriter, r *http.Request) {
	cam, err := s.fleet.Camera(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var f camfarm.FaultSpec
	if !decodeJSON(w, r, &f) {
		return
	}
	if err := cam.Inject(f); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cam.Faults())
}

func (s *server) clearFaults(w http.ResponseWriter, r *http.Request) {
	cam, err := s.fleet.Camera(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := cam.Clear(); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON reads a JSON body into v, replying with the right status itself
// and returning false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErr maps a library error to an HTTP status.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, camfarm.ErrDuplicateCamera):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, camfarm.ErrUnknownCamera):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, camfarm.ErrUnsupported), errors.Is(err, camfarm.ErrInvalidSpec):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, camfarm.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "snapshot timed out")
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "request cancelled")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// Options configures the dashboard; see the field docs in the plan.
type Options struct {
	SnapshotTimeout        timeDuration
	MaxConcurrentSnapshots int
	capture                func(context.Context, *camfarm.Camera) (image.Image, error)
}
```

Fix the `Options` block: use `time.Duration` (import `time`) and `image.Image` (import `image`); the `timeDuration` above is a typo to correct. Move `Options` to the top of the file with its doc comments. Add stubs for the two routes Task 7 implements so the package builds: `func (s *server) getFleet(w http.ResponseWriter, r *http.Request)` and `func (s *server) events(w http.ResponseWriter, r *http.Request)`, each replying `http.StatusNotImplemented` via `writeError`. Task 7 replaces them.

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/dashboard/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard
git commit -m "dashboard: REST API for cameras, faults and coalesced JPEG snapshots"
```

---

### Task 7: Dashboard — `/fleet` and SSE `/events`

**Files:**
- Modify: `internal/dashboard/dashboard.go` (replace the two stubs)
- Create: `internal/dashboard/events.go`
- Modify: `internal/dashboard/dashboard_test.go`

**Interfaces:**
- Consumes: `Fleet.Seed/SpecHash/ReplayLine/Journal/Subscribe`.
- Produces: `GET /api/v1/fleet` → `{"seed":..., "specHash":..., "replayLine":..., "journal":[...]}`; `GET /api/v1/events` → `text/event-stream` with `event: <type>` + `data: <Event JSON>` frames, first frame the comment `: connected`.

- [ ] **Step 1: Write the failing tests**

Append to `dashboard_test.go` (add `"bufio"`, `"strconv"` imports as needed):

```go
func TestFleetEndpoint(t *testing.T) {
	ts, _ := newServer(t, Options{})
	var body struct {
		Seed       uint64 `json:"seed"`
		SpecHash   string `json:"specHash"`
		ReplayLine string `json:"replayLine"`
		Journal    []any  `json:"journal"`
	}
	resp := do(t, "GET", ts.URL+"/api/v1/fleet", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Seed != 1 || body.SpecHash == "" || !strings.Contains(body.ReplayLine, "seed=0x1") {
		t.Errorf("fleet = %+v", body)
	}
	if body.Journal == nil {
		t.Error("journal should be an empty array, not null")
	}
}

func TestEventStreamDeliversChanges(t *testing.T) {
	ts, _ := newServer(t, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func() string {
		t.Helper()
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			return l
		case <-time.After(10 * time.Second):
			t.Fatal("timed out reading the event stream")
			return ""
		}
	}
	if l := next(); l != ": connected" {
		t.Fatalf("first line = %q, want the connected comment", l)
	}

	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"lobby"}`); r.StatusCode != 201 {
		t.Fatalf("add status = %d", r.StatusCode)
	}
	var sawEvent, sawData bool
	for !(sawEvent && sawData) {
		l := next()
		switch {
		case l == "event: camera_added":
			sawEvent = true
		case strings.HasPrefix(l, "data: ") && strings.Contains(l, `"camera":"lobby"`):
			sawData = true
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dashboard/ -run 'Fleet|EventStream' -count=1`
Expected: FAIL (501 from the stubs).

- [ ] **Step 3: Implement**

`internal/dashboard/events.go`:

```go
package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func (s *server) getFleet(w http.ResponseWriter, _ *http.Request) {
	j := s.fleet.Journal()
	writeJSON(w, http.StatusOK, map[string]any{
		"seed":       s.fleet.Seed(),
		"specHash":   s.fleet.SpecHash(),
		"replayLine": s.fleet.ReplayLine(),
		"journal":    j,
	})
}

// events streams fleet changes as Server-Sent Events. A client that falls
// behind is dropped by the fleet and sees the stream end; it reconnects and
// re-lists, which is why nothing is replayed.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := s.fleet.Subscribe(r.Context())

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			fl.Flush()
		}
	}
}
```

Delete the two stubs from `dashboard.go`. Make `getFleet` emit `[]` for an empty journal: in `getFleet`, `if j == nil { j = []camfarm.JournalEntry{} }` (import `camfarm`).

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/dashboard/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard
git commit -m "dashboard: fleet info endpoint and SSE change stream"
```

---

### Task 8: The embedded page (designed with Playwright)

**Files:**
- Modify: `internal/dashboard/web/index.html` (single file: markup, CSS, JS)

**Interfaces:**
- Consumes: the API from Tasks 6–7.
- Produces a page with these stable hooks (Task 10's tests rely on them exactly): `#replay` (replay line text), `#error` (`role="alert"`, hidden when empty), `#add-form` with inputs `#add-id`, `#add-user`, `#add-pass`, `#add-source` (`<select>`, values `fixture`/`file`), `#add-path`, submit button `#add-submit`; `#cameras tbody tr[data-id]` rows containing `.btn-snapshot`, `.btn-remove`, `img.snapshot`, `.fault-rate` (number input), `.fault-protect` (checkbox), `.btn-fault`, `.btn-clear-fault`, `.fault-active` (text of active faults), and `button.copy[data-copy]` (copies `data-copy` to the clipboard). Each row also has `data-testid` cells: `.cell-rtsp`, `.cell-onvif`.

- [ ] **Step 1: Write the page**

Write `internal/dashboard/web/index.html` with this content (the whole file):

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CamFarm</title>
<style>
:root {
  color-scheme: light;
  --bg: #f6f7f9; --panel: #ffffff; --text: #1b1f24; --muted: #5b6672;
  --line: #d9dee4; --accent: #1e5fd6; --accent-text: #ffffff;
  --danger: #b3261e; --ok: #1a7f37; --field: #ffffff;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    color-scheme: dark;
    --bg: #101317; --panel: #181c22; --text: #e8ecf1; --muted: #98a3b0;
    --line: #2b323b; --accent: #6ea0ff; --accent-text: #0b1220;
    --danger: #ff8a80; --ok: #5ac17a; --field: #11151a;
  }
}
:root[data-theme="dark"] {
  color-scheme: dark;
  --bg: #101317; --panel: #181c22; --text: #e8ecf1; --muted: #98a3b0;
  --line: #2b323b; --accent: #6ea0ff; --accent-text: #0b1220;
  --danger: #ff8a80; --ok: #5ac17a; --field: #11151a;
}
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--text);
  font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif;
  padding-inline: 16px; padding-block: 16px 32px;
}
main { max-width: 1200px; margin-inline: auto; }
h1 { font-size: 20px; margin: 0 0 4px; }
h2 { font-size: 15px; margin: 0 0 8px; }
.sub { color: var(--muted); margin: 0 0 16px; }
.panel { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 12px; margin-block: 12px; }
code, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; word-break: break-all; }
form.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 8px; align-items: end; }
label { display: flex; flex-direction: column; gap: 2px; font-size: 12px; color: var(--muted); }
input, select, button { font: inherit; }
input, select {
  background: var(--field); color: var(--text); border: 1px solid var(--line);
  border-radius: 6px; padding: 6px 8px; min-width: 0;
}
button {
  background: var(--accent); color: var(--accent-text); border: 0; border-radius: 6px;
  padding: 6px 10px; cursor: pointer;
}
button.secondary { background: transparent; color: var(--text); border: 1px solid var(--line); }
button.danger { background: transparent; color: var(--danger); border: 1px solid var(--danger); }
button:focus-visible, input:focus-visible, select:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
button.copy { padding: 1px 6px; font-size: 11px; margin-inline-start: 4px; }
.table-wrap { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; min-width: 760px; }
th, td { text-align: left; vertical-align: top; padding: 8px; border-top: 1px solid var(--line); }
th { color: var(--muted); font-weight: 600; font-size: 12px; }
td .row { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; margin-bottom: 4px; }
img.snapshot { display: block; max-width: 100%; height: auto; margin-top: 6px; border: 1px solid var(--line); border-radius: 4px; }
#error { color: var(--danger); margin-block: 8px; }
#error:empty { display: none; }
.fault-active { color: var(--ok); font-size: 12px; }
.fault-controls input[type="number"] { width: 72px; }
.empty { color: var(--muted); padding: 12px 8px; }
</style>
</head>
<body>
<main>
  <h1>CamFarm</h1>
  <p class="sub">A simulated camera fleet. It simulates cameras and is not an ONVIF-conformant implementation.</p>

  <div class="panel">
    <h2>Replay</h2>
    <code id="replay">loading…</code>
  </div>

  <div id="error" role="alert"></div>

  <div class="panel">
    <h2>Add a camera</h2>
    <form id="add-form" class="grid">
      <label>ID<input id="add-id" required autocomplete="off"></label>
      <label>Username (optional)<input id="add-user" autocomplete="off"></label>
      <label>Password (optional)<input id="add-pass" autocomplete="off"></label>
      <label>Source
        <select id="add-source">
          <option value="fixture">Bundled fixture (H.264)</option>
          <option value="file">File on this machine</option>
        </select>
      </label>
      <label id="add-path-label" hidden>File path<input id="add-path" autocomplete="off"></label>
      <button id="add-submit" type="submit">Add camera</button>
    </form>
  </div>

  <div class="panel">
    <h2>Cameras</h2>
    <div class="table-wrap">
      <table id="cameras">
        <thead><tr><th>Camera</th><th>URLs</th><th>Stream</th><th>Faults</th><th>Actions</th></tr></thead>
        <tbody></tbody>
      </table>
    </div>
    <p id="empty" class="empty" hidden>No cameras.</p>
  </div>
</main>

<script>
(function () {
  "use strict";
  var api = "/api/v1";
  var tbody = document.querySelector("#cameras tbody");
  var errorBox = document.getElementById("error");
  var cameras = [];
  var snapshotURLs = {};   // id -> object URL of the last snapshot
  var inputState = {};     // id -> {rate, protect}, survives re-renders

  function showError(msg) { errorBox.textContent = msg || ""; }

  function request(method, path, body) {
    var opts = { method: method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    return fetch(api + path, opts).then(function (r) {
      if (r.ok) { return r; }
      return r.json().catch(function () { return {}; }).then(function (j) {
        throw new Error(j.error || (r.status + " " + r.statusText));
      });
    });
  }

  function el(tag, props, children) {
    var n = document.createElement(tag);
    Object.keys(props || {}).forEach(function (k) {
      if (k === "text") { n.textContent = props[k]; }
      else if (k === "class") { n.className = props[k]; }
      else if (k.indexOf("data-") === 0 || k === "type" || k === "min" || k === "max" || k === "step" || k === "alt") { n.setAttribute(k, props[k]); }
      else { n[k] = props[k]; }
    });
    (children || []).forEach(function (c) { n.appendChild(typeof c === "string" ? document.createTextNode(c) : c); });
    return n;
  }

  function copyButton(value) {
    return el("button", { class: "copy secondary", type: "button", text: "Copy", "data-copy": value, title: "Copy " + value });
  }

  function urlLine(label, value, cls) {
    return el("div", { class: "row" }, [
      el("span", { text: label }),
      el("code", { class: cls, text: value }),
      copyButton(value)
    ]);
  }

  function faultText(c) {
    if (!c.faults.length) { return "none"; }
    return c.faults.map(function (f) {
      return f.kind + " " + f.rate + (f.protectKeyframes ? " (keyframes protected)" : "");
    }).join(", ");
  }

  function renderRow(c) {
    var st = inputState[c.id] || { rate: "0.2", protect: false };
    inputState[c.id] = st;

    var rate = el("input", { class: "fault-rate", type: "number", min: "0", max: "1", step: "0.05", value: st.rate, "aria-label": "Drop rate for " + c.id });
    rate.addEventListener("input", function () { st.rate = rate.value; });
    var protect = el("input", { class: "fault-protect", type: "checkbox", checked: st.protect, "aria-label": "Protect keyframes for " + c.id });
    protect.addEventListener("change", function () { st.protect = protect.checked; });

    var kind = el("select", { class: "fault-kind", "aria-label": "Fault kind for " + c.id }, [
      el("option", { value: "frame_drop", text: "frame_drop" }),
      el("option", { value: "keyframe_starvation", text: "keyframe_starvation (not implemented)", disabled: true }),
      el("option", { value: "packet_loss", text: "packet_loss (not implemented)", disabled: true })
    ]);

    var img = el("img", { class: "snapshot", alt: "Snapshot of " + c.id, hidden: !snapshotURLs[c.id] });
    if (snapshotURLs[c.id]) { img.src = snapshotURLs[c.id]; }

    var snapBtn = el("button", { class: "btn-snapshot secondary", type: "button", text: "Snapshot" });
    snapBtn.addEventListener("click", function () {
      snapBtn.disabled = true;
      showError("");
      fetch(api + "/cameras/" + encodeURIComponent(c.id) + "/snapshot.jpg").then(function (r) {
        if (!r.ok) { return r.json().then(function (j) { throw new Error(j.error || r.statusText); }); }
        return r.blob();
      }).then(function (blob) {
        if (snapshotURLs[c.id]) { URL.revokeObjectURL(snapshotURLs[c.id]); }
        snapshotURLs[c.id] = URL.createObjectURL(blob);
        img.src = snapshotURLs[c.id];
        img.hidden = false;
      }).catch(function (e) { showError("Snapshot failed: " + e.message); })
        .then(function () { snapBtn.disabled = false; });
    });

    var removeBtn = el("button", { class: "btn-remove danger", type: "button", text: "Remove" });
    removeBtn.addEventListener("click", function () {
      showError("");
      request("DELETE", "/cameras/" + encodeURIComponent(c.id)).then(refresh).catch(function (e) { showError(e.message); });
    });

    var applyBtn = el("button", { class: "btn-fault", type: "button", text: "Apply fault" });
    applyBtn.addEventListener("click", function () {
      showError("");
      request("POST", "/cameras/" + encodeURIComponent(c.id) + "/faults",
        { kind: kind.value, rate: parseFloat(rate.value), protectKeyframes: protect.checked })
        .then(refresh).catch(function (e) { showError(e.message); });
    });
    var clearBtn = el("button", { class: "btn-clear-fault secondary", type: "button", text: "Clear" });
    clearBtn.addEventListener("click", function () {
      showError("");
      request("DELETE", "/cameras/" + encodeURIComponent(c.id) + "/faults").then(refresh).catch(function (e) { showError(e.message); });
    });

    var urls = el("td", {}, [
      urlLine("RTSP", c.rtspUrl, "cell-rtsp"),
      urlLine("ONVIF", c.onvifEndpoint, "cell-onvif")
    ]);
    if (c.username) {
      urls.appendChild(el("div", { class: "row" }, [el("span", { text: "Auth" }), el("code", { text: c.username + " / " + c.password })]));
    }

    var tr = el("tr", { "data-id": c.id }, [
      el("td", {}, [
        el("strong", { text: c.id }),
        el("div", { class: "sub", text: c.codec + " " + c.width + "x" + c.height + " @ " + c.fps }),
        el("div", { class: "sub mono", text: "seed " + c.seed })
      ]),
      urls,
      el("td", {}, [
        el("div", { class: "mono", text: "frames " + c.stats.FramesServed + " · loops " + c.stats.LoopsCompleted }),
        el("div", { class: "mono", text: "faults fired " + c.stats.FaultsFired + " · readers " + c.readers })
      ]),
      el("td", {}, [
        el("div", { class: "fault-active", text: faultText(c) }),
        el("div", { class: "fault-controls row" }, [kind, el("label", {}, ["Rate", rate]), el("label", {}, ["Keep keyframes", protect])])
      ]),
      el("td", {}, [
        el("div", { class: "row" }, [snapBtn, applyBtn, clearBtn, removeBtn]),
        img
      ])
    ]);
    return tr;
  }

  function render() {
    // Re-rendering replaces rows, so keep any input the user is editing.
    var focused = document.activeElement;
    var keep = focused && tbody.contains(focused) ? { id: focused.closest("tr").dataset.id, cls: focused.className } : null;
    tbody.textContent = "";
    cameras.forEach(function (c) { tbody.appendChild(renderRow(c)); });
    document.getElementById("empty").hidden = cameras.length > 0;
    if (keep) {
      var row = tbody.querySelector('tr[data-id="' + CSS.escape(keep.id) + '"]');
      var again = row && row.querySelector("." + keep.cls.split(" ")[0]);
      if (again) { again.focus(); }
    }
  }

  function refresh() {
    return Promise.all([
      request("GET", "/cameras").then(function (r) { return r.json(); }),
      request("GET", "/fleet").then(function (r) { return r.json(); })
    ]).then(function (res) {
      cameras = res[0];
      document.getElementById("replay").textContent = res[1].replayLine;
      render();
    }).catch(function (e) { showError("Could not load the fleet: " + e.message); });
  }

  document.addEventListener("click", function (ev) {
    var b = ev.target.closest && ev.target.closest("button.copy");
    if (!b) { return; }
    var text = b.getAttribute("data-copy");
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () {
        var old = b.textContent; b.textContent = "Copied";
        setTimeout(function () { b.textContent = old; }, 1200);
      }).catch(function () { showError("Copy failed; select the text manually."); });
    }
  });

  var source = document.getElementById("add-source");
  source.addEventListener("change", function () {
    document.getElementById("add-path-label").hidden = source.value !== "file";
  });

  document.getElementById("add-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    showError("");
    var spec = { id: document.getElementById("add-id").value.trim() };
    var u = document.getElementById("add-user").value, p = document.getElementById("add-pass").value;
    if (u || p) { spec.auth = { username: u, password: p }; }
    if (source.value === "file") { spec.source = { kind: "file", path: document.getElementById("add-path").value }; }
    request("POST", "/cameras", spec).then(function () {
      document.getElementById("add-form").reset();
      document.getElementById("add-path-label").hidden = true;
      return refresh();
    }).catch(function (e) { showError(e.message); });
  });

  // SSE gives immediate updates on change; the interval keeps counters fresh.
  if (window.EventSource) {
    var es = new EventSource(api + "/events");
    ["camera_added", "camera_removed", "fault_injected", "fault_cleared"].forEach(function (t) {
      es.addEventListener(t, refresh);
    });
    es.onerror = function () { /* the browser reconnects; refresh covers missed events */ };
    es.onopen = refresh;
  }
  setInterval(refresh, 2000);
  refresh();
})();
</script>
</body>
</html>
```

- [ ] **Step 2: Design pass with Playwright (do not skip)**

Run the real binary-less dashboard: write a throwaway `main` or use `go test`-style helper — simplest is to run after Task 9 exists. If executing before Task 9, start a scratch server with `go run` of a 15-line `main` in a temp dir that calls `camfarm.Start` + `dashboard.Handler` on `127.0.0.1:8080` (do not commit it). Then use the Playwright MCP browser tools:
1. Navigate to `http://127.0.0.1:8080/`; take an accessibility snapshot; confirm headings, form labels and table headers read correctly.
2. Take screenshots at 1280px and 400px widths; confirm no horizontal page scroll (only the table scrolls inside `.table-wrap`), a 16px gutter, and readable text.
3. Emulate `prefers-color-scheme: dark` and take a screenshot; confirm contrast of text, buttons, inputs and the danger colour.
4. Click Snapshot on a camera, add a camera, apply and clear a fault; confirm the console shows no errors.
Fix any defect found in `index.html` and repeat. Screenshots are working material; do not commit them. Record in the commit message what you adjusted.

- [ ] **Step 3: Run the existing tests**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./internal/dashboard/`
Expected: PASS (`TestRootServesTheEmbeddedPage` still sees `<!doctype html>`).

- [ ] **Step 4: Commit**

```bash
git add internal/dashboard/web/index.html
git commit -m "dashboard: embedded single-page UI"
```

---

### Task 9: `cmd/camfarm`

**Files:**
- Create: `cmd/camfarm/main.go`, `cmd/camfarm/run.go`, `cmd/camfarm/run_test.go`

**Interfaces:**
- Consumes: `camfarm.Start`, `dashboard.Handler`.
- Produces: `func run(ctx context.Context, args []string, stdout, stderr io.Writer) error`. On startup it prints to **stdout** a line `dashboard: http://<addr>/` once listening, and to **stderr** the replay line and, for a non-loopback `--http-addr`, a warning line starting `warning:`. Flags: `--spec`, `--seed`, `--cameras` (default 1), `--http-addr` (default `127.0.0.1:8080`), `--listen-host`, `--rtsp-port`, `--onvif-port`.

- [ ] **Step 1: Write the failing tests**

`cmd/camfarm/run_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe writer the test can poll.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string               { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

var dashLine = regexp.MustCompile(`dashboard: (http://\S+)/`)

// start runs the binary's core and returns the dashboard URL, a stop function
// that cancels it and returns run's error, and the captured stderr.
func start(t *testing.T, args ...string) (url string, stop func() error, stderr *syncBuf) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &syncBuf{}, &syncBuf{}
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, args, stdout, stderr) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if m := dashLine.FindStringSubmatch(stdout.String()); m != nil {
			url = m[1]
			break
		}
		select {
		case err := <-errc:
			cancel()
			t.Fatalf("run exited early: %v\nstderr: %s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no dashboard line; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop = func() error { cancel(); return <-errc }
	t.Cleanup(func() { cancel() })
	return url, stop, stderr
}

func TestRunServesTheDashboardAndShutsDownCleanly(t *testing.T) {
	url, stop, stderr := start(t, "--http-addr", "127.0.0.1:0", "--cameras", "2", "--seed", "7")

	resp, err := http.Get(url + "/api/v1/cameras")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("cameras = %d, want 2", len(list))
	}
	if !strings.Contains(stderr.String(), "seed=0x7") {
		t.Errorf("stderr should print the replay line with the seed, got %q", stderr.String())
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned %v on shutdown, want nil", err)
	}
}

func TestRunDrawsASeedWhenNoneIsGiven(t *testing.T) {
	_, stop, stderr := start(t, "--http-addr", "127.0.0.1:0")
	defer stop()
	if !regexp.MustCompile(`seed=0x[0-9a-f]+`).MatchString(stderr.String()) {
		t.Fatalf("no replay line on stderr: %q", stderr.String())
	}
}

func TestRunLoadsASpecFileAndFlagsOverrideIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(path, []byte(`{"seed":5,"cameras":[{"id":"from-file"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	url, stop, stderr := start(t, "--spec", path, "--http-addr", "127.0.0.1:0", "--seed", "9")
	defer stop()

	resp, err := http.Get(url + "/api/v1/cameras/from-file")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("camera from the spec file missing: %d", resp.StatusCode)
	}
	if !strings.Contains(stderr.String(), "seed=0x9") {
		t.Errorf("--seed did not override the spec's seed: %q", stderr.String())
	}
}

func TestRunWarnsOnANonLoopbackBind(t *testing.T) {
	_, stop, stderr := start(t, "--http-addr", "0.0.0.0:0")
	defer stop()
	if !strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("no warning for a non-loopback bind: %q", stderr.String())
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":  {"--nope"},
		"zero cameras":  {"--cameras", "0", "--http-addr", "127.0.0.1:0"},
		"missing spec":  {"--spec", "/no/such/file.json", "--http-addr", "127.0.0.1:0"},
		"bad http addr": {"--http-addr", "not-an-address"},
	} {
		var out, errb bytes.Buffer
		if err := run(context.Background(), args, &out, &errb); err == nil {
			t.Errorf("%s: run succeeded, want an error", name)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/camfarm/ -count=1`
Expected: FAIL to compile (`run` undefined).

- [ ] **Step 3: Implement**

`cmd/camfarm/main.go`:

```go
// Command camfarm (CamFarm) runs a simulated camera fleet with a web dashboard.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "camfarm:", err)
		os.Exit(1)
	}
}
```

`cmd/camfarm/run.go`:

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/dashboard"
)

const usage = `CamFarm runs a simulated RTSP/ONVIF camera fleet with a web dashboard.

SECURITY: the dashboard can start servers and, with file sources, make this
process read local paths. It listens on loopback by default and has no
authentication, Host-header check or CSRF protection. Do not expose it.

Flags:
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("camfarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	specPath := fs.String("spec", "", "path to a JSON fleet spec")
	seedFlag := fs.Uint64("seed", 0, "root seed (drawn at random and printed when omitted)")
	cameras := fs.Int("cameras", 1, "number of cameras cam-1..N on the bundled fixture (ignored with --spec)")
	httpAddr := fs.String("http-addr", "127.0.0.1:8080", "dashboard listen address")
	listenHost := fs.String("listen-host", "", "host the RTSP and ONVIF listeners bind (default 127.0.0.1)")
	rtspPort := fs.Int("rtsp-port", 0, "RTSP port (0 picks a free one)")
	onvifPort := fs.Int("onvif-port", 0, "ONVIF port (0 picks a free one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var spec camfarm.Spec
	if *specPath != "" {
		b, err := os.ReadFile(*specPath)
		if err != nil {
			return fmt.Errorf("reading spec: %w", err)
		}
		dec := json.NewDecoder(bytesReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return fmt.Errorf("parsing spec %s: %w", *specPath, err)
		}
	} else {
		if *cameras < 1 {
			return errors.New("--cameras must be at least 1")
		}
		for i := 1; i <= *cameras; i++ {
			spec.Cameras = append(spec.Cameras, camfarm.CameraSpec{ID: fmt.Sprintf("cam-%d", i)})
		}
	}
	switch {
	case set["seed"]:
		spec.Seed = *seedFlag
	case *specPath == "":
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fmt.Errorf("drawing a seed: %w", err)
		}
		spec.Seed = binary.BigEndian.Uint64(b[:])
	}
	if set["listen-host"] {
		spec.Listen.Host = *listenHost
	}
	if set["rtsp-port"] {
		spec.Listen.RTSPPort = *rtspPort
	}
	if set["onvif-port"] {
		spec.Listen.ONVIFPort = *onvifPort
	}

	ln, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return fmt.Errorf("dashboard listener: %w", err)
	}
	defer ln.Close()

	fleet, err := camfarm.Start(ctx, spec)
	if err != nil {
		return err
	}
	defer fleet.Close()

	fmt.Fprintln(stderr, fleet.ReplayLine())
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			fmt.Fprintln(stderr, "warning: the dashboard is not bound to loopback and has no authentication")
		}
	}

	srv := &http.Server{Handler: dashboard.Handler(fleet, dashboard.Options{}), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(stdout, "dashboard: http://%s/\n", ln.Addr())

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
	}
	return nil
}
```

Add the tiny helper `func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }` (import `bytes`) or inline `bytes.NewReader` directly.

Note for the `--cameras 0` test: the `--http-addr 127.0.0.1:0` listener is created after the camera check, so the error returns before binding. The `bad http addr` case fails at `net.Listen`.

- [ ] **Step 4: Run to verify pass**

Run: `gofmt -l . ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race -count=1 ./cmd/camfarm/`
Expected: PASS; the build leaves a `camfarm` binary only if you used `-o`; do not commit binaries.

- [ ] **Step 5: Commit**

```bash
git add cmd/camfarm
git commit -m "Add cmd/camfarm: run a fleet with the dashboard from the command line"
```

---

### Task 10: Playwright end-to-end tests and CI job

**Files:**
- Create: `internal/dashboard/e2e_test.go` (`//go:build e2e`)
- Modify: `.github/workflows/ci.yml`, `go.mod`, `go.sum`

**Interfaces:**
- Consumes: the page hooks listed in Task 8.
- Produces: tests run with `go test -tags e2e ./internal/dashboard/...`; a required CI job `e2e`.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/playwright-community/playwright-go@latest && go mod tidy`
Then install the browser locally once: `go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium`.
Because the dependency is imported only under the `e2e` tag, `go mod tidy` must be run with `-tags e2e` to keep it: `go mod tidy -e` may drop it; use `go list -tags e2e ./... >/dev/null && go mod tidy` after creating the test file in Step 2, and confirm `go.mod` still requires playwright-go. If `go mod tidy` removes it, add a `tools`-style blank import file `internal/dashboard/e2e_deps_test.go` with the same `e2e` tag.

- [ ] **Step 2: Write the tests**

`internal/dashboard/e2e_test.go`:

```go
//go:build e2e

package dashboard

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/playwright-community/playwright-go"

	"github.com/0x524a/camfarm"
)

type browserEnv struct {
	pw      *playwright.Playwright
	browser playwright.Browser
}

var (
	envOnce sync.Once
	env     browserEnv
	envErr  error
)

// launch starts Chromium once per test binary. A missing browser fails the
// test; it is never skipped.
func launch(t *testing.T) playwright.Browser {
	t.Helper()
	envOnce.Do(func() {
		env.pw, envErr = playwright.Run()
		if envErr != nil {
			return
		}
		env.browser, envErr = env.pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	})
	if envErr != nil {
		t.Fatalf("starting Playwright/Chromium (run `go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium`): %v", envErr)
	}
	return env.browser
}

func e2eServer(t *testing.T) (string, *camfarm.Fleet) {
	t.Helper()
	f := camfarm.StartT(t, camfarm.Spec{Seed: 0xE2E, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, Options{}))
	t.Cleanup(ts.Close)
	return ts.URL, f
}

func newPage(t *testing.T, opts ...playwright.BrowserNewContextOptions) (playwright.Page, *[]string) {
	t.Helper()
	b := launch(t)
	var o playwright.BrowserNewContextOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	ctx, err := b.NewContext(o)
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	t.Cleanup(func() { ctx.Close() })
	page, err := ctx.NewPage()
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	var consoleErrors []string
	var mu sync.Mutex
	page.On("console", func(m playwright.ConsoleMessage) {
		if m.Type() == "error" {
			mu.Lock()
			consoleErrors = append(consoleErrors, m.Text())
			mu.Unlock()
		}
	})
	page.On("pageerror", func(err error) {
		mu.Lock()
		consoleErrors = append(consoleErrors, err.Error())
		mu.Unlock()
	})
	return page, &consoleErrors
}

func expect(t *testing.T) playwright.PlaywrightAssertions {
	t.Helper()
	return playwright.NewPlaywrightAssertions(15000)
}

func TestE2ECameraTableRendersURLsAndReplayLine(t *testing.T) {
	url, _ := e2eServer(t)
	page, consoleErrors := newPage(t)
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	row := page.Locator(`#cameras tbody tr[data-id="front-door"]`)
	if err := expect(t).Locator(row).ToBeVisible(); err != nil {
		t.Fatalf("row not visible: %v", err)
	}
	if err := expect(t).Locator(row.Locator(".cell-rtsp")).ToContainText("rtsp://"); err != nil {
		t.Fatalf("RTSP URL missing: %v", err)
	}
	if err := expect(t).Locator(row.Locator(".cell-onvif")).ToContainText("/onvif/front-door"); err != nil {
		t.Fatalf("ONVIF endpoint missing: %v", err)
	}
	if err := expect(t).Locator(page.Locator("#replay")).ToContainText("seed=0xe2e"); err != nil {
		t.Fatalf("replay line missing: %v", err)
	}
	if len(*consoleErrors) != 0 {
		t.Errorf("console errors: %v", *consoleErrors)
	}
}

func TestE2EAddAndRemoveACamera(t *testing.T) {
	url, _ := e2eServer(t)
	page, _ := newPage(t)
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	if err := page.Locator("#add-id").Fill("lobby"); err != nil {
		t.Fatal(err)
	}
	if err := page.Locator("#add-submit").Click(); err != nil {
		t.Fatal(err)
	}
	lobby := page.Locator(`#cameras tbody tr[data-id="lobby"]`)
	if err := expect(t).Locator(lobby).ToBeVisible(); err != nil {
		t.Fatalf("added camera did not appear: %v", err)
	}
	if err := lobby.Locator(".btn-remove").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect(t).Locator(lobby).ToHaveCount(0); err != nil {
		t.Fatalf("removed camera still shown: %v", err)
	}
}

func TestE2EDuplicateCameraShowsAnError(t *testing.T) {
	url, _ := e2eServer(t)
	page, _ := newPage(t)
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	_ = page.Locator("#add-id").Fill("front-door")
	_ = page.Locator("#add-submit").Click()
	if err := expect(t).Locator(page.Locator("#error")).ToContainText("duplicate"); err != nil {
		t.Fatalf("no duplicate error shown: %v", err)
	}
}

func TestE2ESnapshotShowsA320x240Image(t *testing.T) {
	url, _ := e2eServer(t)
	page, _ := newPage(t)
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	row := page.Locator(`#cameras tbody tr[data-id="front-door"]`)
	if err := row.Locator(".btn-snapshot").Click(); err != nil {
		t.Fatal(err)
	}
	if _, err := page.WaitForFunction(`() => {
		const i = document.querySelector('tr[data-id="front-door"] img.snapshot');
		return i && !i.hidden && i.complete && i.naturalWidth === 320 && i.naturalHeight === 240;
	}`, nil); err != nil {
		t.Fatalf("snapshot never showed a 320x240 image: %v", err)
	}
}

func TestE2EFrameDropControlInjectsAndClears(t *testing.T) {
	url, f := e2eServer(t)
	cam, _ := f.Camera("front-door")
	page, _ := newPage(t)
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	row := page.Locator(`#cameras tbody tr[data-id="front-door"]`)
	if err := row.Locator(".fault-rate").Fill("1"); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(".fault-protect").Check(); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(".btn-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect(t).Locator(row.Locator(".fault-active")).ToContainText("frame_drop"); err != nil {
		t.Fatalf("active fault not shown: %v", err)
	}
	if got := cam.Faults(); len(got) != 1 || !got[0].ProtectKeyframes {
		t.Fatalf("library faults = %+v", got)
	}
	if err := row.Locator(".btn-clear-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect(t).Locator(row.Locator(".fault-active")).ToContainText("none"); err != nil {
		t.Fatalf("fault not cleared in the UI: %v", err)
	}
}

func TestE2EUnimplementedFaultKindsAreDisabled(t *testing.T) {
	url, _ := e2eServer(t)
	page, _ := newPage(t)
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	opt := page.Locator(`tr[data-id="front-door"] .fault-kind option[value="keyframe_starvation"]`)
	if err := expect(t).Locator(opt).ToBeDisabled(); err != nil {
		t.Fatalf("an unimplemented fault kind is selectable: %v", err)
	}
}

func TestE2ECopyButtonCopiesTheRTSPURL(t *testing.T) {
	url, f := e2eServer(t)
	cam, _ := f.Camera("front-door")
	page, _ := newPage(t)
	ctx := page.Context()
	if err := ctx.GrantPermissions([]string{"clipboard-read", "clipboard-write"}); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	row := page.Locator(`#cameras tbody tr[data-id="front-door"]`)
	if err := row.Locator(`button.copy[data-copy^="rtsp://"]`).Click(); err != nil {
		t.Fatal(err)
	}
	got, err := page.Evaluate(`navigator.clipboard.readText()`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := got.(string); s != cam.RTSPURL() {
		t.Fatalf("clipboard = %v, want %q", got, cam.RTSPURL())
	}
}

func TestE2ENarrowViewportDoesNotScrollTheWholePage(t *testing.T) {
	url, _ := e2eServer(t)
	page, _ := newPage(t)
	if err := page.SetViewportSize(400, 800); err != nil {
		t.Fatal(err)
	}
	if _, err := page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
	if err := expect(t).Locator(page.Locator(`#cameras tbody tr[data-id="front-door"]`)).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	over, err := page.Evaluate(`document.documentElement.scrollWidth > window.innerWidth`)
	if err != nil {
		t.Fatal(err)
	}
	if over == true {
		t.Fatal("the page scrolls horizontally at 400px; only the table may")
	}
}

func TestE2EDarkColourSchemeIsApplied(t *testing.T) {
	url, _ := e2eServer(t)
	bg := func(scheme playwright.ColorScheme) string {
		page, _ := newPage(t, playwright.BrowserNewContextOptions{ColorScheme: &scheme})
		if _, err := page.Goto(url + "/"); err != nil {
			t.Fatalf("Goto: %v", err)
		}
		v, err := page.Evaluate(`getComputedStyle(document.body).backgroundColor`)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := v.(string)
		return s
	}
	light, dark := bg(*playwright.ColorSchemeLight), bg(*playwright.ColorSchemeDark)
	if light == dark || strings.TrimSpace(dark) == "" {
		t.Fatalf("light (%q) and dark (%q) backgrounds should differ", light, dark)
	}
}
```

If a locator/assertion method name differs in the installed playwright-go version (for example `ToBeDisabled`, `ToHaveCount`), use the equivalent from that version; do not delete the assertion.

- [ ] **Step 3: Run the tests**

Run: `go test -tags e2e -race -count=1 ./internal/dashboard/...`
Expected: PASS. Then confirm the default build is unaffected: `go test -count=1 ./... && CGO_ENABLED=0 go build ./...`.

- [ ] **Step 4: Add the CI job**

Append to `.github/workflows/ci.yml` (same indentation as the other jobs, before the `sonar` job):

```yaml
  # Real-browser tests of the dashboard. A missing browser fails the job; the
  # tests are never skipped. They sit behind the e2e build tag so the default
  # `go test ./...` (including the cgo-free build job) needs no browser.
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          check-latest: true

      - name: install Chromium
        run: go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium

      - name: e2e tests
        run: go test -tags e2e -count=1 ./internal/dashboard/...
```

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/e2e_test.go .github/workflows/ci.yml go.mod go.sum
git commit -m "Add Playwright end-to-end tests for the dashboard and an e2e CI job"
```

---

### Task 11: README and docs

**Files:**
- Modify: `README.md`

**Interfaces:** none.

- [ ] **Step 1: Update the Status section**

In `README.md`'s Status list, add after the screenshots bullet (before **Not built yet**):

```markdown
- **Works:** a web dashboard and `cmd/camfarm` binary — `go run ./cmd/camfarm` serves a fleet
  and a page at `http://127.0.0.1:8080/` to list cameras with copyable RTSP and ONVIF URLs, add and
  remove cameras, take on-demand snapshots, and inject faults. Changes appear live over
  Server-Sent Events. **Security:** the dashboard can start servers and, with file sources, make
  the process read local paths. It listens on loopback by default and has *no* authentication,
  Host-header check or CSRF protection; a malicious web page can reach a loopback API through DNS
  rebinding or CSRF. Do not expose it.
- **Works:** one fault, `frame_drop`: a seeded fraction of access units is never encoded or sent,
  so RTP sequence numbers stay contiguous and the missing frame appears only as a timestamp jump
  (an encoder skipping frames, not network loss). It can be set in a spec, or injected into and
  cleared from a running camera; `protectKeyframes` keeps random-access frames. The fleet replay
  line includes a hash of the runtime journal, but the frame an interactive injection lands on
  depends on when it was requested. Every other catalogued fault is still **refused**.
```

Then update the **Not built yet** bullet so it says that *every other* fault's effect is not built (replace "every fault's *effect*" and the "no fault changes a byte on the wire" sentence accordingly), keeping the "a spec that names an unimplemented fault is refused, not silently ignored" sentence. Do not touch the Scope section or the blockquote. Add no badges or coverage figures.

- [ ] **Step 2: Verify**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 ./... && go test -tags e2e -count=1 ./internal/dashboard/... && CGO_ENABLED=0 go build ./...`
Expected: everything green.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "Document the dashboard, binary and frame_drop"
```

---

## Self-review

- **Spec coverage:** `frame_drop` semantics (encoder-side), `protect_keyframes`, spec-time acceptance and journaled runtime inject/clear (Tasks 1–5); change feed, sentinels, accessors, journaled replay line (Tasks 4–5); REST API, status mapping, snapshot coalescing/timeout/cap, SSE (Tasks 6–7); embedded vanilla-JS UI with Playwright-driven design pass (Task 8); binary flags, random seed printed, loopback default, graceful shutdown (Task 9); Playwright e2e under an `e2e` tag with a dedicated CI job (Task 10); README and stated security posture (Task 11). Deferred items from spec section 7 have no tasks.
- **Placeholder scan:** none. Task 6 contains one stated typo-correction instruction (`timeDuration` → `time.Duration`) for the implementer; this is deliberate wording in a single `Options` block and the correct type is given in the Interfaces section.
- **Type consistency:** `DecideFrame(int, bool)` (Task 1) is called with two arguments in Task 2; `Pump.SetFaults(specs, applied func(int))` (Task 2) matches `Server.SetFaults` (Task 3) and `Camera.swapFaults` (Task 4); `noteFault(action, camera, spec, frame)` is declared in Task 4 and given its body in Task 5; `FaultSpec.ProtectKeyframes` is used in Tasks 4, 6, 8, 10; `Options` fields match between Tasks 6, 7 and 9; DOM hooks produced in Task 8 are exactly those consumed in Task 10.
- **Known unverified:** none of this code has been compiled. Task 10's playwright-go method names should be checked against the pinned version.
