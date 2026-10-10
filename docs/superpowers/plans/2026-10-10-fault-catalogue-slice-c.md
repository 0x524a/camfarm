# Fault Catalogue Slice C (Timing) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the timing faults `jitter`, `burst_delivery`, `pts_dts_mismatch` and `pts_delayed` to CamFarm's pump, with seeded, reproducible decisions and wire tests.

**Architecture:** `Pump.Step` is split into a hermetic `plan` (decide, encode, stamp; no clock) and an `emit` (write packets). `Step` keeps its contract (plan then emit immediately) so existing tests need no clock. The run loop adds the one new seam: it waits until each planned frame's send deadline using a new `clock.After`. `burst_delivery` needs no timer: held frames are simply not emitted until the flush frame's own tick. Timestamp faults add into the stamped RTP timestamp.

**Tech Stack:** Go 1.26, gortsplib v5.6.5, internal `clock` package (virtual clock for tests).

**Spec:** `docs/superpowers/specs/2026-10-09-fault-catalogue-design.md` (sections 1, 1.1, 2.6-2.9, 3, 3.1, 5, 6, 7). Depends on slice A merged (catalogue, `Engine`, `obs.FaultTally`, generic fault form). Independent of slice B except that both extend the catalogue; if B is merged first, rebase Task 2 (it only appends kinds and params).

## Global Constraints

- `CGO_ENABLED=0` keeps building; the six cross-compile jobs stay green.
- Determinism is the product: each kind draws from `seed.Stream(string(kind))`; draws are unconditional; decisions are pure functions of (seed, kind, frame counter, params). `pts_dts_mismatch` and `pts_delayed` use no randomness.
- The delay values and hold boundaries are reproducible; arrival times measured by a client are wall-clock and are not (spec 2.6). The README must say so.
- Frames are always sent in order. RTP sequence numbers stay contiguous for all four kinds; only timing and timestamps change.
- Real sockets in tests, no wire mocks, `-race` clean, fixed seeds. Wire tests assert loose bounds, never exact arrival times.
- Every task leaves `go build ./...`, `go vet ./...`, `go test ./...` green; e2e only where the dashboard page changes.
- `golangci-lint` v2.13.2 with errcheck: `fmt.Fprint*` results assigned to `_, _`.
- No coverage figure or badge not produced by CI; README Scope section unchanged; no employer or client names.
- Parameter ranges verbatim from the spec: `jitter.maxMs` 1-5000; `burst_delivery.rate` [0,1], `holdFrames` integer 2-300; `pts_dts_mismatch.reorderDepth` integer 1-4; `pts_delayed.delayMs` 1-60000, `driftPpm` 1-100000, at least one required.

## Rulings made while planning

- **R1 (hermetic split).** Pacing lives only in `Pump.run`. `Step` never sleeps and ignores delays except to record them, so every decision is testable without a clock. Cost if wrong: none to behaviour.
- **R2 (drift baseline).** `driftPpm` lag counts frames since the fault was installed (engine-local counter), so injecting mid-stream starts the drift at zero instead of stepping by the accumulated lag. `delayMs` is the only one-time step (spec 2.9). Cost if wrong: lag values differ from a "since stream start" reading; pinned by test.
- **R3 (jitter on held frames).** A held frame's own jitter draw is consumed and discarded; the flush frame (last of the hold) carries its deadline plus its own jitter (spec 2.7). Cost if wrong: one test expectation.
- **R4 (timestamp faults and dropped frames).** The timestamp engine is consulted every `Step`, including frames removed by `frame_drop`/starvation, so its counters stay frame-clocked. Cost if wrong: only visible when combining faults.
- **R5 (compose order).** Per frame, in order: frame faults (drop, starvation) -> encode -> packet loss -> timestamp faults -> burst hold/flush -> jitter delay. Documented in the README.

## Review Focus

- Frame-interval units: the timestamp faults use `frameDur` ticks from the pump, not a hard-coded 3000; fixtures with other frame rates must produce the exact sequences. Pinned in Task 3 by a 25 fps fixture case.
- 32-bit RTP timestamp wrap with a negative shift (`delayMs` at the start of the stream, timestamp near 0) must wrap, not panic or clamp. Pinned in Task 3.
- `burst_delivery` hold crossing the media loop seam, and `Clear` while a hold is active (held frames must be flushed or discarded deterministically, never leaked). Pinned in Task 4.
- Pump shutdown (context cancel, `RemoveCamera`) while waiting on a jitter deadline must return promptly and not leak the goroutine. Pinned in Task 5.
- `jitter` + `burst_delivery` + `pts_*` + `packet_loss` composed under `-race` with inject/clear churn. Pinned in Task 7.

---

## File structure

- `internal/clock/clock.go`: `After(d) <-chan time.Time` on `Clock`, `Real` and `Virtual`.
- `internal/fault/catalogue.go`, `fault.go`: four kinds, params, `Spec` fields.
- `internal/fault/timing.go` (new): timing decisions on `Engine`.
- `internal/rtsp/pump.go`: `plan`/`emit` split, stamping, hold buffer, paced `run`.
- `spec.go`, `camfarm.go`: public fields.
- `internal/dashboard/static/index.html`, `e2e_test.go`, `README.md`, `readme_test.go`.

---

### Task 1: `clock.After`

**Files:**
- Modify: `internal/clock/clock.go`, `internal/clock/clock_test.go`

**Interfaces:**
- Produces: `Clock.After(d time.Duration) <-chan time.Time` (buffered 1). `Real.After` = `time.After`. `Virtual.After` fires when `Advance` carries virtual time to or past `now+d`; `d <= 0` fires immediately.

- [ ] **Step 1: Failing test**

```go
func TestVirtualAfterFiresOnAdvance(t *testing.T) {
	v := clock.NewVirtual(time.Unix(0, 0))
	ch := v.After(50 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("fired before advance")
	default:
	}
	v.Advance(49 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("fired early")
	default:
	}
	v.Advance(time.Millisecond)
	select {
	case <-ch:
	default:
		t.Fatal("did not fire at deadline")
	}
	select {
	case <-v.After(0):
	default:
		t.Fatal("zero duration must fire immediately")
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (`go test ./internal/clock`). Also add the method to every existing fake that satisfies `Clock` (`grep -rn "NewTicker(" --include=*_test.go`).
- [ ] **Step 3: Implement.** `Virtual` keeps `waiters []waiter{at time.Time; ch chan time.Time}` under `v.mu`; `Advance` (read how it already walks tickers) fires and removes due waiters after moving `now`.
- [ ] **Step 4: Run** `go test ./... -race`. **Commit** `clock: After for deadline waits`.

---

### Task 2: Catalogue and spec fields for the four kinds

**Files:**
- Modify: `internal/fault/catalogue.go`, `internal/fault/fault.go`, `spec.go`, `camfarm.go` (`toFaultSpec`), `internal/fault/catalogue_test.go`

**Interfaces:**
- Produces: kinds `KindJitter="jitter"`, `KindBurstDelivery="burst_delivery"`, `KindPTSDTSMismatch="pts_dts_mismatch"`, `KindPTSDelayed="pts_delayed"`, all `Implemented: false` here (Tasks 3-5 flip them); `Spec` and public `FaultSpec` fields `MaxMs int \`json:"maxMs,omitempty"\``, `HoldFrames int \`json:"holdFrames,omitempty"\``, `ReorderDepth int \`json:"reorderDepth,omitempty"\``, `DelayMs int \`json:"delayMs,omitempty"\``, `DriftPpm int \`json:"driftPpm,omitempty"\``.
- `Validate` extension: `pts_delayed` requires at least one of `delayMs`, `driftPpm` (`ErrBadParam`); `burst_delivery.holdFrames` required.

- [ ] **Step 1: Failing tests**

```go
func TestTimingKindsAreCatalogued(t *testing.T) {
	want := map[fault.Kind][]string{
		fault.KindJitter:          {"maxMs"},
		fault.KindBurstDelivery:   {"rate", "holdFrames"},
		fault.KindPTSDTSMismatch:  {"reorderDepth"},
		fault.KindPTSDelayed:      {"delayMs", "driftPpm"},
	}
	for k, names := range want {
		info, ok := fault.Info(k)
		if !ok || info.Models == "" || info.Clock == "" {
			t.Fatalf("%s: bad catalogue entry %+v", k, info)
		}
		var got []string
		for _, p := range info.Params {
			got = append(got, p.Name)
		}
		if !reflect.DeepEqual(got, names) {
			t.Errorf("%s params = %v, want %v", k, got, names)
		}
	}
}

func TestTimingValidation(t *testing.T) {
	// Validate skips unimplemented kinds, so this test calls the checks after
	// flipping; until then it asserts the catalogue bounds only.
	info, _ := fault.Info(fault.KindPTSDelayed)
	for _, p := range info.Params {
		if p.Required {
			t.Errorf("%s.%s must not be individually required (at least one of two)", info.Kind, p.Name)
		}
	}
}
```

  Also add (enabled in Task 5 when the kind turns on) table cases for `Validate`: `jitter{MaxMs:0}` rejected as required; `5001` rejected; `burst_delivery{HoldFrames:1}` rejected, `301` rejected; `pts_dts_mismatch{ReorderDepth:5}` rejected; `pts_delayed{}` rejected; `pts_delayed{DelayMs:60001}` rejected; `pts_delayed{DriftPpm:100000}` accepted. Put them in a helper `validateTimingTable(t)` that each of Tasks 3-5 calls for its own kinds.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** Models strings (README must match):
  - jitter: `Inter-arrival jitter: each frame leaves a seeded random time late (order kept), which stresses jitter-buffer sizing.` Clock: `frame counter; one draw per frame`.
  - burst_delivery: `Bursty uplink or encoder buffering: frames are held and then released back-to-back, which overflows NVR queues.` Clock: `frame counter; one draw per frame, unconditional`.
  - pts_dts_mismatch: `B-frame camera that stamps presentation time into RTP: timestamps run out of order, so decoders report non-monotonic dts.` Clock: `frame counter; no randomness`.
  - pts_delayed: `Camera whose timestamps lag capture, constantly or by drift: what a latency profiler such as framelag meets.` Clock: `frame counter; no randomness`.
  Required flags per the Global Constraints; `jitter.maxMs` required default 100; `holdFrames` required default 5, `rate` default 0.1; `reorderDepth` required default 1. `Spec.param` and `paramNames` gain the five names; `Validate` gets the at-least-one rule.
- [ ] **Step 4:** `go build ./... && go vet ./... && go test ./... -race`. **Commit** `fault: catalogue entries and spec fields for timing faults`.

---

### Task 3: Timestamp faults (`pts_dts_mismatch`, `pts_delayed`)

**Files:**
- Create: `internal/fault/timing.go`, `internal/fault/timing_test.go`
- Modify: `internal/rtsp/pump.go`, `internal/rtsp/pump_test.go`, `internal/fault/catalogue.go` (flip two kinds)

**Interfaces:**
- Produces: `(*Engine) TimestampShift(frameIndex int, frameDur int64) int64`, in RTP ticks, to be **added** to the stamped timestamp. Called exactly once per frame. Pure arithmetic, integers only.
  - `pts_dts_mismatch`, `W = ReorderDepth+1`, `j = frameIndex % W`: shift is `+(W-1)*frameDur` if `j == 0`, else `-frameDur`.
  - `pts_delayed`: `-DelayMs*90` (the pump's clock rate is 90000) plus `-(n * frameDur * DriftPpm / 1_000_000)` where `n` is the engine-local count of calls since this entry was installed (R2). Multiply before dividing; `int64` throughout.
  - Both add up when both are present.
- Pump: `ts := uint32(p.tsOffset + au.DTS + shift)` (the `uint32` conversion of a negative `int64` wraps, which is the required behaviour).

- [ ] **Step 1: Failing tests (fault package)**

```go
func TestReorderSequenceForDepthOne(t *testing.T) {
	e, _ := fault.New(seed.New(1), []fault.Spec{{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1}})
	const dur = 3000
	var ts []int64
	for k := 0; k < 8; k++ {
		ts = append(ts, int64(k)*dur+e.TimestampShift(k, dur))
	}
	want := []int64{1, 0, 3, 2, 5, 4, 7, 6}
	for i := range want {
		want[i] *= dur
	}
	if !reflect.DeepEqual(ts, want) {
		t.Fatalf("ts = %v, want %v", ts, want)
	}
}

func TestReorderKeepsTheSetOfTimestampsForEveryDepth(t *testing.T) {
	for depth := 1; depth <= 4; depth++ {
		e, _ := fault.New(seed.New(1), []fault.Spec{{Kind: fault.KindPTSDTSMismatch, ReorderDepth: depth}})
		const dur, n = 3600, 40 // a multiple of every window size 2..5
		got := map[int64]bool{}
		for k := 0; k < n; k++ {
			got[int64(k)*dur+e.TimestampShift(k, dur)] = true
		}
		for k := 0; k < n; k++ {
			if !got[int64(k)*dur] {
				t.Fatalf("depth %d: timestamp %d missing", depth, k)
			}
		}
	}
}

func TestDelayShiftsConstantlyAndDriftGrowsPerFrame(t *testing.T) {
	e, _ := fault.New(seed.New(1), []fault.Spec{{Kind: fault.KindPTSDelayed, DelayMs: 100, DriftPpm: 1000}})
	const dur = 3000
	if got := e.TimestampShift(0, dur); got != -9000 { // 100 ms * 90 ticks/ms, n=0 -> no drift yet
		t.Fatalf("frame 0 shift = %d, want -9000", got)
	}
	// n=1000 -> 1000*3000*1000/1e6 = 3000 ticks of extra lag.
	var last int64
	for k := 1; k <= 1000; k++ {
		last = e.TimestampShift(k, dur)
	}
	if last != -9000-3000 {
		t.Fatalf("frame 1000 shift = %d, want -12000", last)
	}
}

func TestTimestampFaultsUseNoRandomnessAndComposeByAdding(t *testing.T) {
	both, _ := fault.New(seed.New(5), []fault.Spec{
		{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1},
		{Kind: fault.KindPTSDelayed, DelayMs: 10},
	})
	if got := both.TimestampShift(0, 3000); got != 3000-900 {
		t.Fatalf("composed shift = %d, want 2100", got)
	}
}
```

- [ ] **Step 2: Failing pump tests** (hermetic, recording `packetWriter` already in `pump_test.go`): with `reorderDepth 1` the first four written timestamps are `3000, 0, 9000, 6000` (nominal DTS 0,3000,6000,9000 for a 30 fps fixture); sequence numbers are contiguous across all frames; a 25 fps fixture case (frameDur 3600) produces `3600, 0, 10800, 7200`; with `delayMs 10` on frame 0 the timestamp is `uint32(0-900) == 4294966396` (wrap pinned); a frame removed by `frame_drop` still advances the engine counter (R4): after a dropped frame, drift lag at the next frame equals what an undropped run gives.
- [ ] **Step 3: Run, expect FAIL.**
- [ ] **Step 4: Implement** `TimestampShift` on `Engine` (timing state lives on the `entry`: add `calls int`), flip both kinds to `Implemented: true`, and in `Pump.Step` call `shift := p.faults.TimestampShift(p.frameCounter, p.frameDur)` once, before the drop check (R4); use it in the stamp. Record `obs.Fault` and `FaultTally(kind, 1, 0)` for each frame where the shift is non-zero, detail `timestamp shifted by %d ticks`.
- [ ] **Step 5:** call `validateTimingTable` for the two kinds. Run `go test ./... -race -count=2`. **Commit** `rtsp: pts_dts_mismatch and pts_delayed stamp reordered and lagged timestamps`.

---

### Task 4: `burst_delivery` (hold and flush)

**Files:**
- Modify: `internal/fault/timing.go`, `internal/rtsp/pump.go`, tests in both; flip the kind.

**Interfaces:**
- Produces: `(*Engine) DecideHold(frameIndex int) HoldDecision` where `HoldDecision` is one of `HoldNone`, `HoldHeld` (withhold this frame), `HoldFlush` (this frame is the last of a hold: release the buffer then this frame). One draw per frame, unconditional, consumed during a hold too. State on the entry: `remaining int`. At a frame boundary with `remaining == 0`, a draw `< Rate` starts a hold of `HoldFrames` frames including this one: `remaining = HoldFrames`; this and the next frames return `HoldHeld` while `remaining > 1`, the frame at `remaining == 1` returns `HoldFlush`, then `remaining = 0`.
- Pump: `held [][]*rtp.Packet` (already stamped, already loss-filtered packets). `Step`: after stamping, call `DecideHold`; `HoldHeld` appends the frame's packets to `held` and writes nothing (still counts `Frame` for `FramesServed`? Ruling: `obs.Frame` is called when the frame is **emitted**, so a held frame counts at flush; stated in the test); `HoldFlush` writes `held` in order then this frame's packets, back-to-back, and clears `held`. Timestamps and sequence numbers are untouched.

- [ ] **Step 1: Failing tests (fault)**

```go
func TestHoldBoundariesAreSeededAndExactlyHoldFramesLong(t *testing.T) {
	run := func() []fault.HoldDecision {
		e, _ := fault.New(seed.New(11), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0.1, HoldFrames: 4}})
		var out []fault.HoldDecision
		for k := 0; k < 400; k++ {
			out = append(out, e.DecideHold(k))
		}
		return out
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("not reproducible")
	}
	held := 0
	for _, d := range a {
		switch d {
		case fault.HoldHeld:
			held++
		case fault.HoldFlush:
			if held != 3 {
				t.Fatalf("flush after %d held frames, want 3 (HoldFrames-1)", held)
			}
			held = 0
		}
	}
	if held != 0 && held > 3 {
		t.Fatalf("unterminated hold of %d", held)
	}
}

func TestDrawsAreConsumedDuringAHold(t *testing.T) {
	// Two engines, same seed, one with HoldFrames 2 and one with HoldFrames 9:
	// the draws they consume per frame are identical, so after the same number
	// of frames a third consumer of the same stream sees the same next value.
	// Observable form: a second kind (frame_drop) on the same engine draws the
	// same sequence whichever hold length is configured.
}
```

  Write the second test fully: build two engines with `[]Spec{{burst_delivery, HoldFrames: h}, {frame_drop, Rate: 0.5}}` for `h = 2` and `h = 9` (different streams per kind, so frame_drop is independent by construction) and assert the `frame_drop` decisions match; additionally assert the burst stream itself consumes exactly one draw per frame by comparing against a fresh `seed.Stream("burst_delivery").Rand()` read 400 times (hold starts exactly where `u < Rate` and `remaining == 0`).
- [ ] **Step 2: Failing pump tests:** (a) with a seed/rate that holds frames 5-8 (find with a loop in the test and `t.Fatalf` if none, using the helper from the fault test) the recording writer shows nothing written for those Steps and then 4 frames' packets in one flush at Step 8, with contiguous sequence numbers and the exact honest timestamps; (b) `Clear` (SetFaults to empty) while holding: held frames are **flushed at the next Step** (rule: a clear never loses frames; documented), asserted by sequence continuity; (c) a hold spanning the loop seam keeps timestamps strictly continuing across `tsOffset`.
- [ ] **Step 3: Run, expect FAIL.**
- [ ] **Step 4: Implement** and flip `Implemented`. Obs: held frames record `obs.Fault(..., "held (1 of N)")` per held frame and one flush event `released N frames`; tally `Applied` = frames held. In `applyPendingFaults`, if the old engine had a hold in progress and the new one does not, set `p.flushOnNext = true` (rule b).
- [ ] **Step 5:** `validateTimingTable`; `go test ./... -race -count=3`. **Commit** `rtsp: burst_delivery holds and flushes frames`.

---

### Task 5: `jitter` and the paced run loop

**Files:**
- Modify: `internal/fault/timing.go`, `internal/rtsp/pump.go`, tests; flip `jitter`.

**Interfaces:**
- Produces: `(*Engine) FrameDelay(frameIndex int) time.Duration` (one draw per frame, unconditional: `d = time.Duration(u * float64(MaxMs) * float64(time.Millisecond))`, truncated to whole microseconds so float noise cannot change it).
- Pump split (R1): `type plannedFrame struct{ packets []*rtp.Packet; delay time.Duration; idx int }`; `(*Pump) plan() (plannedFrame, error)` does everything in `Step` up to the write (including hold/flush: a held frame yields an empty plan with no delay); `(*Pump) emit(plannedFrame) error` writes packets and calls `obs.Frame`; `Step()` = plan then emit. Existing `Step` tests must pass unchanged.
- `run`: on each tick `pf := plan()`; if `pf.delay > 0`: `select { case <-ck.After(max(0, tickTime+delay-ck.Now())): case <-ctx.Done(): return }`; never emit earlier than the previous emission (order preserved; bunching falls out because the ticker channel buffers one tick). `tickTime` is the time of the tick being processed. The planned delay is recorded with `obs.Fault(..., "delayed %d ms")` and `FaultTally(jitter, 1, 0)` when non-zero. With burst: the flush frame's `delay` is its own jitter (R3).

- [ ] **Step 1: Failing tests**

```go
func TestJitterDelaysAreTheSeededSchedule(t *testing.T) {
	spec := []fault.Spec{{Kind: fault.KindJitter, MaxMs: 200}}
	a, _ := fault.New(seed.New(21), spec)
	b, _ := fault.New(seed.New(21), spec)
	rnd := seed.New(21).Stream("jitter").Rand()
	for k := 0; k < 200; k++ {
		da, db := a.FrameDelay(k), b.FrameDelay(k)
		want := time.Duration(rnd.Float64()*200*float64(time.Millisecond)).Truncate(time.Microsecond)
		if da != db || da != want {
			t.Fatalf("frame %d: %v, %v, want %v", k, da, db, want)
		}
		if da < 0 || da >= 200*time.Millisecond {
			t.Fatalf("frame %d delay %v out of range", k, da)
		}
	}
}
```

  Pump run-loop test with `clock.Virtual`: start `run` in a goroutine with a recording writer, `jitter maxMs 5000` (so delays exceed the interval), advance virtual time in 1 ms steps for 3 s, and assert (1) written timestamps are non-decreasing in write order and sequence numbers contiguous (order kept), (2) at least one pair of consecutive writes is less than one frame interval apart (bunching), (3) no write happened before its nominal tick time. Plus `TestRunReturnsPromptlyWhileWaitingOnADeadline`: cancel the context during a long planned delay (virtual clock never advanced) and assert `run` returns within 1 s real time and the goroutine count returns to baseline (Review Focus 4).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** the split and the paced loop, flip `jitter`. `Step`'s behaviour is unchanged when no jitter is configured; verify with the whole existing pump test file and `-count=3`.
- [ ] **Step 4:** `validateTimingTable`; `go test ./... -race -count=3`. **Commit** `rtsp: jitter delays frames on a seeded schedule`.

---

### Task 6: Wire tests with a real client

**Files:**
- Create: `internal/rtsp/timing_wire_test.go` (reuses `startFaultServer`, the client helpers from `faults_wire_test.go`)

**Interfaces:** none new.

- [ ] **Step 1: Write the tests** (no production changes expected):
  - `TestJitterSpreadsInterArrivalTimes`: run a clean camera and a `jitter maxMs 200` camera for 40 frames each over a real TCP client, record arrival gaps per frame, assert the jittered run's standard deviation of gaps exceeds the clean run's by at least 5 ms **and** the stated floor (> 8 ms); assert order preserved by RTP timestamp.
  - `TestBurstDeliveryArrivesTogether`: seed with a known hold; assert that within the flush, at least `holdFrames` frames arrive within 5 ms of each other, timestamps honest, sequence contiguous, and after the flush the average gap returns to the nominal interval within 3 frames.
  - `TestReorderedTimestampsReachTheClient`: RTP timestamps seen by a real client for `reorderDepth 1` follow `1,0,3,2,...` in frame-interval units relative to the first frame's nominal slot, sequence contiguous.
  - `TestPTSDelayedShowsInTheSenderReport`: with `delayMs 500`, read the client's RTCP sender report mapping (`Client.PacketNTP`/`PacketPTS` for the format, or the SR callback at the pinned version) and assert `NTP - PTS-derived` differs from a clean camera's by about 500 ms within +/- 150 ms. If the pinned gortsplib does not expose an SR-based mapping, assert on the raw RTCP SR packet (`OnPacketRTCP`) timestamp field instead; either way the assertion states the expected relationship between SR RTP timestamp and wall clock (spec 2.9: "stated as expected behaviour and pinned").
- [ ] **Step 2:** `go test ./internal/rtsp -run Timing -race -count=5`. If a bound flakes, widen the tolerance in the **assertion** and say why in a comment; never add sleeps.
- [ ] **Step 3: Commit** `rtsp: wire tests for timing faults`.

---

### Task 7: Public API, composition and race test

**Files:**
- Modify: `camfarm_test.go`; Create: `internal/rtsp/timing_compose_test.go`

- [ ] **Step 1: Tests.**
  - Public: `Start` with each of the four kinds in a `CameraSpec.Faults` succeeds; each rejection from the validation table (Task 2 list) returns `ErrInvalidSpec`; `Stats.FaultCounts` shows non-zero `Applied` for `pts_dts_mismatch` after a few frames (use `StartT` and wait for frames).
  - Compose: inject all four plus `packet_loss` on one camera under a virtual clock and a recording writer, run 2,000 steps with inject/clear churn from a second goroutine every 50 steps, `-race`; assert no panic, sequence numbers strictly increasing modulo gaps from `packet_loss`, and that every held frame is eventually written (total frames written + currently held == frames not dropped).
- [ ] **Step 2: Run, fix only test-side problems; any production defect found goes back to its owning task's code with a regression test.** `go test ./... -race -count=3`.
- [ ] **Step 3: Commit** `camfarm: timing faults through the public API and under composition`.

---

### Task 8: Dashboard, README, spec status

**Files:**
- Modify: `internal/dashboard/e2e_test.go`, `internal/dashboard/dashboard_test.go`, `README.md`, `readme_test.go`, `docs/superpowers/specs/2026-10-09-fault-catalogue-design.md` (status line)

- [ ] **Step 1: Failing tests.** `GET /api/v1/faults` lists the four kinds implemented with the Task 2 params. e2e: apply `jitter` with `maxMs` 100 through the generic form, see it in `.fault-active` with its counter increasing, clear it; apply `pts_delayed` with neither parameter and assert the server's 400 is shown (error text visible). README drift test expects the four rows.
- [ ] **Step 2: README.** Table rows with the exact `Models` strings; text on: arrival times are wall-clock and not reproducible while delays and hold boundaries are; the compose order (R5); `burst_delivery` + `packet_loss` interaction; `pts_delayed` is visible to RTCP-aware clients mainly through the sender report; sequence numbers stay contiguous for these four kinds.
- [ ] **Step 3:** `go test ./... -race`, `golangci-lint run`, e2e with `PLAYWRIGHT_DRIVER_PATH=/tmp/pwdrv`. **Commit** `docs: timing faults in the README and dashboard tests`.

---

## Self-review notes

- Spec coverage: 2.6 -> Tasks 1, 5; 2.7 -> Task 4; 2.8 -> Task 3; 2.9 -> Task 3 (+ SR in Task 6); 3 (flat fields) -> Task 2; 3.1 observability -> tallies in Tasks 3-5; 5 UI -> Task 8 (generic form, no new UI code expected); 6 slice C bullets -> Tasks 3-7; 7 docs -> Task 8.
- Interface consistency: `TimestampShift` (Task 3), `DecideHold`/`HoldDecision` (Task 4), `FrameDelay` (Task 5) are the only new `Engine` methods; `plan`/`emit` introduced in Task 5 wrap code added in Tasks 3-4, so Tasks 3-4 edit `Step` in place and Task 5 refactors it with all prior tests as the safety net.
- Known unverified: the SR mapping API at gortsplib v5.6.5 (Task 6 has a documented fallback); whether the bundled fixtures contain B-frames (spec says unverified; tests do not depend on it).
