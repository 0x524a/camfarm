# Fault Catalogue Slice B (Control Plane) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the control-plane faults `rtsp_error`, `teardown` and `sdp_lie` to CamFarm, decided by a new per-camera control-plane engine, with the same catalogue, API, UI, journal and README treatment as slice A.

**Architecture:** Frame-clocked faults stay in the pump. Control-plane faults are decided on gortsplib connection goroutines by a mutex-guarded `fault.ControlEngine` (one per camera, owned by the RTSP server). Handlers (`OnDescribe`, `OnSetup`, `OnPlay`) and the `OnResponse` finaliser ask it once per request; `teardown` is triggered by the pump calling a per-frame hook. The catalogue gains an enum parameter type and the journal gains a `Clock` field.

**Tech Stack:** Go 1.26, gortsplib v5.6.5, mediacommon v2.9.4, Playwright (e2e).

**Spec:** `docs/superpowers/specs/2026-10-09-fault-catalogue-design.md` (sections 1, 2.3-2.5, 3, 3.1, 3.2, 4, 5, 6, 7). Depends on slice A being merged (catalogue, `validateFault`, `Stats.FaultCounts`, `obs.FaultTally`, generic fault form).

## Global Constraints

- `CGO_ENABLED=0` keeps building; the six cross-compile jobs stay green.
- Determinism is the product: every decision is a pure function of (camera seed, kind, clock value, params). Each kind draws from `seed.Stream(string(kind))`. Draws are unconditional (spec 1.1). Wall-clock, scheduling and map order are never inputs.
- Control-plane determinism holds for a single client only (spec 1.1). The README must say so.
- Real sockets in tests, no wire mocks; `-race` clean; fixed seeds.
- Each fault models a real field bug, stated in its catalogue `Models` string.
- Every task leaves `go build ./...`, `go vet ./...`, `go test ./...` green at its commit; e2e only where the task touches the dashboard page.
- `golangci-lint` v2.13.2 with errcheck: `fmt.Fprint*` results are assigned to `_, _`.
- Lock discipline (spec 4): the engine mutex is never held across a call into gortsplib or the pump; `Fleet.mu` before `events.mu` is unchanged.
- No coverage figure or badge not produced by CI. README keeps its "simulates, not ONVIF conformant" Scope section. No employer or client names anywhere.
- Display name CamFarm in prose; module path, packages and binary stay lowercase `camfarm`.
- Kind names follow the spec: `rtsp_error`, `teardown`, `sdp_lie`. See Ruling R1 below.

## Rulings made while planning (the executor may overturn with evidence)

- **R1 (names).** Slice A's catalogue holds placeholder kinds `teardown_mid_session`, `sdp_codec_lie`, `sps_resolution_lie`, `accept_then_silence`, all unimplemented. The spec names the implemented kinds `teardown` and `sdp_lie` (with `mode` covering the two lies). Task 1 replaces `teardown_mid_session`, `sdp_codec_lie` and `sps_resolution_lie` with `teardown`, `sdp_lie` and adds `rtsp_error`. `accept_then_silence` stays, unimplemented (spec section 8), and becomes the permanent "unimplemented kind" example in tests. Cost if wrong: a rename, no behaviour.
- **R2 (apply clock for control kinds).** A control-plane change takes effect on the camera's next request, not at a frame boundary. The journal `Clock` is `request`, `FrameIndex` carries the number of requests (all methods) the camera had answered when the change landed. Cost if wrong: journal semantics only.
- **R3 (sdp_lie mechanism).** Rewrite in `OnResponse` (the finaliser), because gortsplib sets the SDP body, Content-Type and Content-Base after the handler returns. Task 0 confirms; if blocked the plan escalates.

## Review Focus

- A fault set mixing media and control kinds (`frame_drop` + `rtsp_error`) must route each kind to its engine and replace per kind, not wholesale. Pinned in Task 3.
- `Clear` must clear both engines; a stale control fault after Clear would keep failing requests. Pinned in Task 3.
- `rtsp_error` with `rate` 0 and no `onlyRequest` must never fire; `onlyRequest` with `rate` set must ignore `rate`. Pinned in Task 2.
- `teardown` racing `RemoveCamera` or fleet `Close` must neither panic nor hang. Pinned in Task 5.
- `sdp_lie` must leave SETUP/PLAY working and must not affect live readers; Clear returns the honest SDP on the next DESCRIBE. Pinned in Task 6.

---

## File structure

- `internal/fault/catalogue.go`: new kinds, `ParamEnum`, `Param.Options`, params `on`, `status`, `onlyRequest`, `afterFrames`, `mode`.
- `internal/fault/fault.go`: `Spec` gains `On`, `Status`, `OnlyRequest`, `AfterFrames`, `Mode`; `Known` etc.
- `internal/fault/control.go` (new): `ControlEngine`, request decisions.
- `internal/rtsp/control.go` (new): server-side wiring: per-camera engine, session registry for teardown, SDP rewriting.
- `internal/rtsp/server.go`, `pump.go`: routing, hooks.
- `spec.go`, `camfarm.go`, `events.go`: public fields, `Clock` in the journal, routing of Inject/Clear.
- `internal/dashboard/static/index.html`, `e2e_test.go`: enum rendering and one e2e.
- `README.md`, `readme_test.go`.

---

### Task 0: Spike - confirm the two gortsplib mechanisms

**Files:**
- Create: `internal/rtsp/spike_test.go` (kept as a pinning test, see Step 4)

**Interfaces:**
- Produces: a verdict recorded in the ledger/Ruling: (a) `OnResponse` can rewrite the DESCRIBE body and `Content-Base`; (b) `ServerConn.NetConn()` exists and `SetLinger(0)` + `Close()` yields an RST at a real client.

- [ ] **Step 1: Write the DESCRIBE rewrite test.** Start an `rtsp.Server` with one camera on an ephemeral port (copy the setup from `internal/rtsp/server_test.go`). Temporarily set the server's `OnResponse` to: when `res.Header["Content-Type"]` contains `application/sdp`, replace `res.Body` with `bytes.ReplaceAll(res.Body, []byte("H264/90000"), []byte("H265/90000"))` and set `res.Header["Content-Base"] = base.HeaderValue{"rtsp://wrong.invalid/"}`. A real `gortsplib.Client` calls `Start`+`Describe` and the test asserts the returned description's first format is H265 and `Content-Base` differs from the server URL.
- [ ] **Step 2: Write the reset test.** In `OnPlay` capture `ctx.Conn`; after 100 ms call `c := ctx.Conn.NetConn().(*net.TCPConn); c.SetLinger(0); c.Close()`. A real client in TCP transport reads RTP until its `Wait()` returns an error; assert the error text contains `reset` or `EOF` and record which.
- [ ] **Step 3: Run** `go test ./internal/rtsp -run Spike -race -count=3`.
- [ ] **Step 4: Outcome.** If both pass: delete the temporary handler edits but keep the two tests renamed `TestPinOnResponseCanRewriteDescribe` and `TestPinNetConnResetReachesClient`, rewritten against the real implementation in Tasks 5-6 (they are the spec's "pin" tests). If (a) fails, STOP and escalate with the observed behaviour (spec 2.5: no silent fallback). If (b) fails, rule: `mode: reset` is dropped from the catalogue enum and the spec limit is recorded in the ledger.
- [ ] **Step 5: Commit** `git commit -m "rtsp: spike for sdp rewrite and tcp reset mechanisms"` (the spike file is deleted in Task 5/6 once superseded).

---

### Task 1: Catalogue - enum params, three new kinds, spec fields

**Files:**
- Modify: `internal/fault/catalogue.go`, `internal/fault/fault.go`, `internal/fault/catalogue_test.go`
- Modify: tests that used `teardown_mid_session`, `sdp_codec_lie`, `sps_resolution_lie` as examples (`grep -rn 'teardown_mid_session\|sdp_codec_lie\|sps_resolution_lie' --include=*.go --include=*.md .`) -> `accept_then_silence` (R1).

**Interfaces:**
- Produces: `fault.ParamEnum ParamType = "enum"`; `Param.Options []string \`json:"options,omitempty"\``; kinds `KindRTSPError="rtsp_error"`, `KindTeardown="teardown"`, `KindSDPLie="sdp_lie"`; `Spec` fields `On string` (comma-free single method: `describe|setup|play`... see below), `Status int`, `OnlyRequest int`, `AfterFrames int`, `Mode string`.
- `rtsp_error.on` is an enum of `describe`, `setup`, `play`, `all`. Default `describe`. (Spec says `on` names methods; a single enum value keeps the flat spec flat; `all` covers every method.) Ruling R4: single-valued `on`.
- All three new kinds register `Implemented: false`; Tasks 4, 5 and 6 flip them.

- [ ] **Step 1: Failing tests** in `catalogue_test.go`:

```go
func TestControlKindsAreCatalogued(t *testing.T) {
	for _, k := range []fault.Kind{fault.KindRTSPError, fault.KindTeardown, fault.KindSDPLie} {
		info, ok := fault.Info(k)
		if !ok {
			t.Fatalf("%s not catalogued", k)
		}
		if info.Models == "" || info.Clock == "" || len(info.Params) == 0 {
			t.Errorf("%s: incomplete catalogue entry %+v", k, info)
		}
	}
	for _, old := range []string{"teardown_mid_session", "sdp_codec_lie", "sps_resolution_lie"} {
		if fault.Known(fault.Kind(old)) {
			t.Errorf("placeholder %q should be gone (R1)", old)
		}
	}
}

func TestEnumParamsAreValidated(t *testing.T) {
	// Run only once the kinds are implemented; here we pin the schema.
	info, _ := fault.Info(fault.KindSDPLie)
	var mode fault.Param
	for _, p := range info.Params {
		if p.Name == "mode" {
			mode = p
		}
	}
	if mode.Type != fault.ParamEnum || len(mode.Options) != 4 {
		t.Fatalf("sdp_lie.mode = %+v, want enum with codec, sprop, resolution, contentBase", mode)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (`go test ./internal/fault -run 'ControlKinds|EnumParams'`).
- [ ] **Step 3: Implement.** Params (names are the public JSON names):
  - `rtsp_error`: `on` enum {describe,setup,play,all} default describe required; `status` integer 400-599 default 503; `rate` number [0,1] default 0.2; `onlyRequest` integer 1-1000000 default 0 (0 = unset). Clock: `per-method request counter; one draw per matching request`. Models: `Flaky camera that answers 5xx or 454 to some DESCRIBE, SETUP or PLAY requests.`
  - `teardown`: `afterFrames` integer 1-100000 default 30 required; `mode` enum {close,reset} default close; `rate` number default 1. Clock: `frames since the reader joined; one draw per PLAY`. Models: `Camera that drops live sessions; TCP reset when mode is reset.`
  - `sdp_lie`: `mode` enum {codec,sprop,resolution,contentBase} default codec required; `rate` default 1; `onlyRequest` default 0. Clock: `per-camera DESCRIBE counter; one draw per DESCRIBE`. Models: `SDP that misdescribes the stream: wrong codec, wrong parameter sets, wrong geometry or wrong Content-Base.`
  - Extend `paramNames` and `Spec.param` with the five new names (strings are not float; add a `Spec.enumParam(name) (string, bool)` and check enum membership in `Validate` with `ErrBadParam: %q must be one of %v`). Enum values live in `Param.Options`; `Default` for enums is the string.
  - Remove the three placeholder constants and entries; update `knownKinds`. Update examples in other tests per R1.
- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./... -race` expect PASS.
- [ ] **Step 5: Commit** `fault: control-plane kinds, enum parameters, spec fields`.

---

### Task 2: The control-plane engine

**Files:**
- Create: `internal/fault/control.go`, `internal/fault/control_test.go`

**Interfaces:**
- Consumes: `Spec`, `Validate`, `seed.Seed.Stream(...).Rand()`.
- Produces:
  - `type Method string` with `MethodDescribe`, `MethodSetup`, `MethodPlay`.
  - `type ControlEngine struct` (mutex-guarded). `NewControlEngine(s seed.Seed) *ControlEngine`.
  - `(*ControlEngine) Set(specs []Spec) error` replaces the control kinds' specs (non-control kinds in `specs` are ignored; validated by `Validate`); resets each kind's stream and counters, so a newly injected fault starts the same sequence (spec 1.1). Returns the request total at the time of the swap via `(*ControlEngine) Requests() int`.
  - `(*ControlEngine) Decide(m Method) ControlDecision` where `ControlDecision{Request int; Error *ErrorDecision; Lie *LieDecision}`; `ErrorDecision{Status int}`; `LieDecision{Mode string}`. Called exactly once per request: bumps the per-method counter and the total, performs the unconditional draw for each active matching kind.
  - `(*ControlEngine) ArmPlay() *Arming` returns `nil` or `&Arming{AfterFrames int, Mode string}` (one draw per PLAY from the `teardown` stream).

- [ ] **Step 1: Failing tests.**

```go
func TestRTSPErrorOnlyRequestFiresExactlyThere(t *testing.T) {
	e := fault.NewControlEngine(seed.New(1))
	if err := e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 503, OnlyRequest: 3}}); err != nil {
		t.Fatal(err)
	}
	var fired []int
	for i := 1; i <= 6; i++ {
		if d := e.Decide(fault.MethodDescribe); d.Error != nil {
			fired = append(fired, i)
		}
	}
	if !reflect.DeepEqual(fired, []int{3}) {
		t.Fatalf("fired on %v, want [3]", fired)
	}
}

func TestRTSPErrorOtherMethodsDoNotAdvanceTheCounter(t *testing.T) {
	e := fault.NewControlEngine(seed.New(1))
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 454, OnlyRequest: 2}})
	e.Decide(fault.MethodSetup)
	e.Decide(fault.MethodPlay)
	if e.Decide(fault.MethodDescribe).Error != nil {
		t.Fatal("first DESCRIBE fired")
	}
	if d := e.Decide(fault.MethodDescribe); d.Error == nil || d.Error.Status != 454 {
		t.Fatalf("second DESCRIBE = %+v, want status 454", d.Error)
	}
}

func TestRTSPErrorRateZeroNeverFiresAndSeedIsReproducible(t *testing.T) {
	run := func() []bool {
		e := fault.NewControlEngine(seed.New(7))
		_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503, Rate: 0.5}})
		var out []bool
		for i := 0; i < 64; i++ {
			out = append(out, e.Decide(fault.MethodDescribe).Error != nil)
		}
		return out
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("same seed produced different sequences")
	}
	e := fault.NewControlEngine(seed.New(7))
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503}})
	for i := 0; i < 100; i++ {
		if e.Decide(fault.MethodPlay).Error != nil {
			t.Fatal("rate 0 fired")
		}
	}
}

func TestDrawsAreUnconditionalPerMatchingRequest(t *testing.T) {
	// A non-matching method must not consume draws: the sequence at the
	// matching method is identical with and without interleaved SETUPs.
	a := fault.NewControlEngine(seed.New(3))
	b := fault.NewControlEngine(seed.New(3))
	spec := []fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 503, Rate: 0.5}}
	_ = a.Set(spec)
	_ = b.Set(spec)
	for i := 0; i < 50; i++ {
		b.Decide(fault.MethodSetup)
		if (a.Decide(fault.MethodDescribe).Error != nil) != (b.Decide(fault.MethodDescribe).Error != nil) {
			t.Fatalf("diverged at %d", i)
		}
	}
}

func TestSetResetsStreamsAndEngineIsRaceFree(t *testing.T) {
	e := fault.NewControlEngine(seed.New(1))
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				e.Decide(fault.MethodDescribe)
				_ = e.Set([]fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec"}})
				e.ArmPlay()
			}
		}()
	}
	wg.Wait()
}
```

(`seed.New` stands for whatever the package's constructor is; check `internal/seed`.)

- [ ] **Step 2: Run, expect FAIL** (undefined `NewControlEngine`).
- [ ] **Step 3: Implement.** `ControlEngine{mu sync.Mutex; seed seed.Seed; specs map[Kind]*ctlEntry (iterate via a fixed kind order slice, never the map); total int; perMethod map[Method]int}`; `ctlEntry{spec Spec; rnd *rand.Rand; seen map[Method]int ...}`. `Decide`: lock; `perMethod[m]++; total++`; for kinds in fixed order [rtsp_error, sdp_lie]: if it matches the method (`rtsp_error`: `On==all || On==string(m)`; `sdp_lie`: `m==MethodDescribe`), draw `u := rnd.Float64()` unconditionally, then: if `OnlyRequest>0` fire iff `perMethod[m]==OnlyRequest` (for rtsp_error the counter is the one for `m`; for sdp_lie the DESCRIBE counter) else fire iff `u < Rate`. `rtsp_error` rate 0 with no onlyRequest never fires (`u < 0` is false). When both `rtsp_error` and `sdp_lie` fire on a DESCRIBE the error wins (a lie in a failed response has no SDP). `Set` rebuilds entries from `seed.Stream(string(kind))`.
  `ArmPlay`: under the lock, if a `teardown` entry exists, draw once; armed iff `u < Rate` (Rate default 1 when 0: treat unset `rate` as 1 for `teardown` and `sdp_lie` since their catalogue default is 1 and the JSON omits zero. State this in a comment; `Validate` still refuses an explicit out-of-range value).
- [ ] **Step 4: Run** `go test ./internal/fault -race -count=3` expect PASS.
- [ ] **Step 5: Commit** `fault: per-camera control-plane engine`.

---

### Task 3: Routing, `rtsp_error`, journal clock

**Files:**
- Create: `internal/rtsp/control.go`
- Modify: `internal/rtsp/server.go` (camera gains `ctl *fault.ControlEngine`; `SetFaults` routes; handlers consult the engine), `spec.go`, `camfarm.go` (public `On`, `Status`, `OnlyRequest`, `AfterFrames`, `Mode` fields, `toFaultSpec`), `events.go` (`JournalEntry.Clock`, replay hash), `internal/obs` usage for tallies.
- Test: `internal/rtsp/control_wire_test.go` (new), `camfarm_test.go`, `events_test.go`.

**Interfaces:**
- Consumes: `ControlEngine` (Task 2), `fault.KindRTSPError` flipped to `Implemented: true` in this task.
- Produces:
  - `Server.SetFaults(id, specs, applied func(index int, clock string))`; signature change from `applied func(frameIndex int)`. Media kinds go to the pump (applied with `"frame"`), control kinds to `cam.ctl` (applied immediately with `"request"` and `ctl.Requests()`). A set with both calls `applied` once per class present, in the order frame, request; `camfarm` publishes one change-feed event and journal entry per call. (Ruling R5: one entry per class keeps `Clock` unambiguous.) *Superseded:* the shipped order is request then frame (effect order), and only classes whose specs changed are applied, not every class present.
  - `JournalEntry.Clock string` (`frame` or `request`, `omitempty`); `Event` gains none.
  - Public `FaultSpec` fields `On string \`json:"on,omitempty"\``, `Status int`, `OnlyRequest int`, `AfterFrames int`, `Mode string`, all `omitempty`.
  - `Server` helper `(s *Server) answerError(m fault.Method, camID string, d fault.ControlDecision) *base.Response`.

- [ ] **Step 1: Failing wire test** (real client):

```go
func TestRTSPErrorOnlyRequestAnswersTheConfiguredStatus(t *testing.T) {
	srv := startFaultServer(t, "cam1") // helper from faults_wire_test.go (slice A)
	if err := srv.SetFaults("cam1", []fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 454, OnlyRequest: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	statusOf := func() base.StatusCode {
		c := gortsplib.Client{}
		u, _ := base.ParseURL(srv.URL("cam1"))
		c.Scheme, c.Host = u.Scheme, u.Host
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _, err := c.Describe(u)
		var bad liberrors.ErrClientBadStatusCode
		if errors.As(err, &bad) {
			return bad.Code
		}
		if err != nil {
			t.Fatal(err)
		}
		return base.StatusOK
	}
	if got := statusOf(); got != base.StatusOK {
		t.Fatalf("request 1 = %v, want 200", got)
	}
	if got := statusOf(); got != 454 {
		t.Fatalf("request 2 = %v, want 454", got)
	}
	if got := statusOf(); got != base.StatusOK {
		t.Fatalf("request 3 = %v, want 200", got)
	}
}

func TestSetFaultsRoutesEachKindAndClearClearsBoth(t *testing.T) {
	// frame_drop (pump) + rtsp_error (engine) injected together; Clear removes both.
}
```

  Write the second test fully: inject `frame_drop rate 1` and `rtsp_error on=all status=503 rate=1`; assert DESCRIBE fails with 503 and `obs` shows frame drops; then `SetFaults(id, nil, nil)` and assert DESCRIBE succeeds and frames flow again.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
  - Flip `rtsp_error` `Implemented` true. In `Server`, `cam.ctl = fault.NewControlEngine(seedFor(cam))`: use the same per-camera seed derivation the pump uses (`PumpConfig.Seed`); extend `CameraConfig` only if the seed is not already reachable.
  - `SetFaults`: partition `specs` by `fault.IsControl(kind)` (add `func IsControl(Kind) bool` in `fault`), always call both `pump.SetFaults(mediaSpecs, ...)` and `ctl.Set(controlSpecs)` so a clear resets both.
  - Handlers: `OnDescribe`, `OnSetup`, `OnPlay` call `cam.ctl.Decide(m)` after the camera lookup and before serving. If `d.Error != nil`: `obs.Fault(id, d.Request, "rtsp_error", fmt.Sprintf("answered %d to %s", status, m))`, `obs.FaultTally(id, "rtsp_error", 1, 0)`, return `&base.Response{StatusCode: base.StatusCode(status)}, nil, nil` (DESCRIBE/SETUP) or `, nil` (PLAY). Never hold `s.mu` across `Decide` (look the camera up with the read lock, copy the pointer, release).
  - Public layer: `toFaultSpec` copies the five new fields; `validateFault` needs no change beyond ordering (it calls `fault.Validate`); `Camera.Inject` passes an `applied` that takes `(index int, clock string)` and calls `c.fleet.noteFault(action, id, f, index, clock)`; `noteFault` stores `Clock`. `Clear` passes the same. `FaultCount` already handles any kind.
  - Replay-line hash covers `Clock`.
- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./... -race -count=2`.
- [ ] **Step 5: Commit** `rtsp: rtsp_error via the control-plane engine; journal clock`.

---

### Task 4: `teardown`

**Files:**
- Modify: `internal/rtsp/control.go` (session registry), `internal/rtsp/server.go` (OnPlay arming, OnSessionClose unregister), `internal/rtsp/pump.go` (`PumpConfig.AfterFrame func()`, called at the end of every `Step`), `internal/fault/catalogue.go` (flip `teardown` Implemented).
- Test: `internal/rtsp/teardown_wire_test.go`, `internal/rtsp/pump_test.go`.

**Interfaces:**
- Consumes: `ControlEngine.ArmPlay`, the spike's NetConn route (Task 0).
- Produces: `type armed struct{ sess *gortsplib.ServerSession; conn *gortsplib.ServerConn; remaining int; mode string }`; `(*Server) afterFrame(camID string)` wired as the pump hook; `Server.sessions` registry guarded by its own mutex `armMu` (never held while closing).

- [ ] **Step 1: Failing tests.** (a) pump test: a `Pump` with `AfterFrame` counting calls sees exactly one call per `Step`, including dropped frames. (b) wire test, both modes:

```go
func TestTeardownEndsTheSessionAfterNFrames(t *testing.T) {
	for _, mode := range []string{"close", "reset"} {
		t.Run(mode, func(t *testing.T) {
			srv := startFaultServer(t, "cam1")
			if err := srv.SetFaults("cam1", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 5, Mode: mode, Rate: 1}}, nil); err != nil {
				t.Fatal(err)
			}
			var packets atomic.Int64
			c := playClient(t, srv.URL("cam1"), func() { packets.Add(1) }) // helper: real TCP client, counts RTP packets
			select {
			case err := <-c.Done(): // helper wraps client.Wait()
				if err == nil {
					t.Fatal("session ended without error")
				}
				if mode == "reset" && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "EOF") {
					t.Fatalf("reset mode error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session not torn down")
			}
			if n := packets.Load(); n < 5 || n > 40 { // generous: frames can span packets
				t.Fatalf("got %d packets before teardown", n)
			}
		})
	}
}
```

  Add `TestTeardownRateZeroNeverArms` (rate is the catalogue default 1 when unset, so use an explicit tiny rate `1e-9` and assert no teardown within 60 frames).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** In `OnPlay` after the 200 decision: `if a := cam.ctl.ArmPlay(); a != nil { s.arm(ctx.Session, ctx.Conn, a) }`. `afterFrame(camID)`: lock `armMu`, decrement `remaining` for every armed session of that camera, collect those at 0 (remove them), unlock, then for each: tally `obs.FaultTally(id, "teardown", 1, 0)` and `obs.Fault(...)`, then `mode==close`: `sess.Close()`; `mode==reset`: `if tc, ok := conn.NetConn().(*net.TCPConn); ok { _ = tc.SetLinger(0) }; _ = conn.NetConn().Close()`. `OnSessionClose` also removes the registry entry. Guard `Close` against the session already being gone (no panic). Flip `Implemented`.
- [ ] **Step 4: Race test** `TestTeardownRacingRemoveCameraDoesNotPanicOrHang`: arm, then concurrently `RemoveCamera` and `Server.Close` while frames flow; `-race -count=20` must pass in under 10 s.
- [ ] **Step 5: Run** `go test ./... -race -count=2`. **Commit** `rtsp: teardown fault with close and reset modes`.

---

### Task 5: `sdp_lie`

**Files:**
- Modify: `internal/rtsp/control.go` (`rewriteSDP`), `internal/rtsp/server.go` (`pendingLie` map keyed by `*gortsplib.ServerConn`, `OnResponse`), `internal/fault/catalogue.go` (flip `sdp_lie`).
- Test: `internal/rtsp/sdp_lie_test.go`, `internal/rtsp/sdp_lie_wire_test.go`.

**Interfaces:**
- Consumes: `ControlDecision.Lie`.
- Produces: `func rewriteSDP(body []byte, mode string, codec media.Codec) ([]byte, error)` and `func lieContentBase(orig string) string` (pure, unit-testable).

- [ ] **Step 1: Failing unit tests** for `rewriteSDP` on a captured honest SDP (take it from a DESCRIBE against the test server, store as `testdata/describe_h264.sdp` and `describe_h265.sdp`): `codec` swaps `H264/90000` <-> `H265/90000` in `a=rtpmap`; `sprop` replaces the `sprop-parameter-sets` value with a different well-formed base64 pair (still decodes to valid SPS+PPS NAL units); `resolution` yields an SPS whose parsed width/height differ from the honest ones (parse with `h264.SPS.Unmarshal` from mediacommon); `a=control:` lines unchanged for all modes (spec 2.5); line endings preserved.
- [ ] **Step 2: Failing wire test:**

```go
func TestSDPLieIsVisibleOnDescribeAndClearedAfter(t *testing.T) {
	srv := startFaultServer(t, "cam1")
	describe := func() *description.Session { /* real client Describe, returns desc */ }
	honest := describe().Medias[0].Formats[0].Codec()
	_ = srv.SetFaults("cam1", []fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec", Rate: 1}}, nil)
	lied := describeRaw(t, srv.URL("cam1")) // raw SDP string via a plain TCP RTSP exchange so the client library does not reinterpret it
	if !strings.Contains(lied, "H265/90000") {
		t.Fatalf("lie not visible:\n%s", lied)
	}
	_ = srv.SetFaults("cam1", nil, nil)
	if got := describe().Medias[0].Formats[0].Codec(); got != honest {
		t.Fatalf("after clear = %v, want %v", got, honest)
	}
}
```

  Add `TestSDPLieLeavesSetupAndPlayWorking` (a lying SDP plus a real client that issues SETUP/PLAY by hand against the unchanged control URL still receives RTP) and `TestSDPLieDoesNotAffectLiveReaders` (a reader that joined before the injection keeps receiving packets).
- [ ] **Step 3: Run, expect FAIL.**
- [ ] **Step 4: Implement.** `OnDescribe`: on `d.Lie != nil` store `s.pendingLie[ctx.Conn] = lieInfo{mode, camID}` under its own mutex, tally `(1,0)`, `obs.Fault(...)`. `OnResponse(conn, res)`: take-and-delete `pendingLie[conn]`; if present and `res.StatusCode==200`: `res.Body = rewriteSDP(res.Body, ...)` unless `mode==contentBase`, in which case `res.Header["Content-Base"] = {lieContentBase(orig)}`. `OnConnClose` deletes the conn's entry (no leak). The resolution SPS: if `h264.SPS` has a `Marshal`, build it by editing the honest SPS's `Width`/`Height` fields; otherwise embed a constant base64 SPS for a 320x240 stream generated once by a throwaway test (record how in a comment and keep the generating test). `contentBase`: swap the host for `wrong.invalid` and keep the path. Flip `Implemented`.
- [ ] **Step 5: Run** `go test ./... -race -count=2`. **Commit** `rtsp: sdp_lie rewrites DESCRIBE per request`.
- [ ] **Step 6:** delete `spike_test.go` pins superseded by Task 4 and 5 wire tests (`TestPinOnResponseCanRewriteDescribe` becomes the codec case above; `TestPinNetConnResetReachesClient` is the reset subtest), keeping one pin test per mechanism with the real implementation. Commit `rtsp: replace spike tests with pinned implementation tests`.

---

### Task 6: Dashboard form - enum parameters

**Files:**
- Modify: `internal/dashboard/static/index.html` (generic param renderer: add `enum` -> `<select class="fault-param" data-param=...>` with `options`), `internal/dashboard/e2e_test.go`, `internal/dashboard/dashboard_test.go`, `internal/dashboard/api.go` if camera JSON needs `Clock` (journal endpoint already serialises the struct).

**Interfaces:**
- Consumes: catalogue JSON `options` (Task 1); slice A's `.fault-param[data-param=<name>]` hooks.

- [ ] **Step 1: Failing tests.** httptest: `GET /api/v1/faults` returns `rtsp_error`, `teardown`, `sdp_lie` with `implemented: true` and `sdp_lie.params[mode].options == [codec sprop resolution contentBase]`. e2e (Playwright): select `rtsp_error`, set `on` to `describe`, `status` 503, `rate` 1, Apply, assert `.fault-active` lists it, then Clear; select `accept_then_silence` and assert Apply is disabled; page has no console errors in both colour schemes (reuse the existing helper).
- [ ] **Step 2: Run** unit (FAIL), e2e with `PLAYWRIGHT_DRIVER_PATH=/tmp/pwdrv go test ./internal/dashboard -run E2E`.
- [ ] **Step 3: Implement** the `enum` branch in the renderer; unknown types still skipped with the existing notice. Apply collects selects by `data-param`; integer parsing for `status`/`onlyRequest`/`afterFrames` follows the existing integer path; empty `onlyRequest` is omitted from the body.
- [ ] **Step 4: Run all** `go test ./... -race` and the e2e. **Commit** `dashboard: enum fault parameters`.

---

### Task 7: README, drift test, docs

**Files:**
- Modify: `README.md` (fault table rows for the three kinds, "control-plane determinism holds for a single client" limit, `sdp_lie` and `teardown` limits, Status "Works" list), `readme_test.go` (drift test already compares the table to the catalogue; update expected strings), `CLAUDE.md` Resolved decisions only if the Open decision "fault-injection catalogue" is now answered (leave the open list alone otherwise), `docs/superpowers/specs/2026-10-09-fault-catalogue-design.md` status line ("slices A and B implemented").

- [ ] **Step 1:** run `go test . -run README` expecting FAIL (table lacks the new rows).
- [ ] **Step 2:** add the rows with the exact `Models` strings from Task 1 and the limits text. State plainly: `reset` mode uses `ServerConn.NetConn()` outside gortsplib's documented API and is pinned by a test; `rtsp_error` and `sdp_lie` are deterministic for a single client only.
- [ ] **Step 3:** `go test ./... -race`, `golangci-lint run`, `go vet ./...`. **Commit** `docs: control-plane faults in the README`.

---

## Self-review notes

- Spec coverage: 2.3 -> Tasks 2-3; 2.4 -> Task 4; 2.5 -> Tasks 0 and 5; 3 (flat fields) -> Tasks 1 and 3; 3.1 observability -> tallies in Tasks 3-5; 3.2 journal `Clock` -> Task 3; 4 engine -> Task 2 and the lock rule in Task 3; 5 UI -> Task 6; 6 testing bullets for rtsp_error/teardown/sdp_lie/races -> Tasks 3-5; 7 docs -> Task 7.
- Known open item: `status` of 454 at PLAY (Session Not Found) is semantically odd but spec-allowed (400-599).
