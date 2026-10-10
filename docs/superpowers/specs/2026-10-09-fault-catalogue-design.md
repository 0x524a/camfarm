# Fault catalogue: media-path and control-plane faults — design

Date: 2026-10-09. Status: approved design, not yet implemented.

This is the design for the next two slices after slice 3 (dashboard, binary and the first fault
effect, `frame_drop`). It depends on slice 3 being merged: it reuses the pump's fault seam,
`Pump.SetFaults`, the journal, the change feed and the dashboard's fault control.

One design, two slices:

- **Slice A (media path):** the catalogue schema, `GET /api/v1/faults`, the generic fault UI,
  `packet_loss` and `keyframe_starvation`.
- **Slice B (control plane):** a per-camera control-plane engine, `rtsp_error`, `teardown` and
  `sdp_lie`.

Each slice gets its own plan and review. The model below is shared so that all eight kinds
(including `frame_drop`) behave consistently.

Naming: the display name is CamFarm (page, README prose, `--help`); module path, packages and
binary stay lowercase `camfarm`.

Constraints inherited from `CLAUDE.md`: `CGO_ENABLED=0` keeps building; determinism is the
product (every decision traces to a seed and a named clock, never to wall-clock or goroutine
scheduling); real sockets in tests, no wire mocks; each fault must map to a real class of field
bug; no coverage figure or badge not produced by CI; the README keeps its "simulates, not ONVIF
conformant" Scope section; no employer or client names anywhere.

## 1. The model

A fault has a **kind**, a typed **parameter set**, one **deterministic clock**, and one **real bug
it models**. A camera can carry several faults at once, at most one per kind; injecting a kind that
is already present replaces it (existing behaviour of `Camera.Inject`).

| Kind | Slice | Params | Clock and draws | Models |
|---|---|---|---|---|
| `frame_drop` (exists) | — | `rate`, `protectKeyframes` | frame counter; 1 draw per frame | encoder skipping frames |
| `packet_loss` | A | `rate`, `meanBurst`, `protectKeyframes` | frame counter plus packet index; **1 draw per packet**, unconditional | lossy UDP path |
| `keyframe_starvation` | A | `keepEvery` (integer ≥ 2) | keyframe counter; **no randomness** | long-GOP camera; a late joiner cannot start |
| `rtsp_error` | B | `on` (describe, setup, play), `status` (default 503), `rate`, optional `onlyRequest` | per-camera request counter per method; 1 draw per matching request | flaky camera answering 5xx |
| `teardown` | B | `afterFrames`, `mode` (close, reset), `rate` | frames since the reader joined; 1 draw per PLAY | camera dropping sessions; TCP reset |
| `sdp_lie` | B | `mode` (codec, sprop, resolution, contentBase), `rate`, optional `onlyRequest` | per-camera DESCRIBE counter; 1 draw per DESCRIBE | SDP that misdescribes the stream |

All other kinds already in the `fault` package keep returning `ErrUnsupported`, naming the kind,
until their effect exists.

### 1.1 Determinism rules (apply to every kind)

- Each kind draws from its own stream, `seed.Stream(kind)`, derived from the camera seed, exactly
  as `fault.New` does today. A newly injected fault therefore always starts the same sequence for
  the same camera.
- Draws are **unconditional**: the number of draws consumed depends only on the clock (frames,
  packets, requests), never on earlier outcomes, so the sequence cannot shift because of a
  neighbour's decision.
- A decision is a pure function of (camera seed, kind, clock value, params). Wall-clock time,
  goroutine scheduling and map order are never inputs.
- The honest limit, stated rather than hidden: runtime inject and clear land at a clock value that
  depends on when a human clicked. The journal records that value, so a run can be replayed from
  the seed plus the journal; the click timing itself cannot be derived from a seed (as with
  `frame_drop`). For control-plane kinds, the sequence is deterministic only when a single client
  drives the camera, because the order of requests from several concurrent connections is not
  controlled by this program.

## 2. Kind semantics

### 2.1 `packet_loss` (slice A)

The frame is encoded normally, so the RTP packetizer assigns sequence numbers first. Withheld
packets are then simply not written, leaving a real sequence gap and honest timestamps. This is
the counterpart of `frame_drop`, where the frame is never encoded and sequence numbers stay
contiguous.

- **Independent mode** (`meanBurst` omitted or 0): a packet is withheld when its draw `u < rate`.
- **Burst mode** (`meanBurst` > 1): a two-state loss model. In the "good" state packets pass; in
  the "bad" state packets are withheld. Let `q = 1 - 1/meanBurst` be the probability that the bad
  state continues and `p = rate · (1 - q) / (1 - rate)` the probability of entering it from the
  good state. One draw per packet decides the transition, and the packet is withheld when the state
  after the transition is bad. The long-run loss fraction equals `rate` and the mean burst length
  equals `meanBurst`. Validation requires `p ≤ 1`, that is `rate ≤ meanBurst / (1 + meanBurst)`;
  otherwise the spec is refused with `ErrInvalidSpec`. An explicit `meanBurst` must be greater
  than 1, and burst mode requires `rate` < 1 (the formula divides by `1 - rate`; independent mode
  accepts `rate` = 1).
- `protectKeyframes`: packets of a random-access access unit are never withheld. Their draws are
  still consumed, so the sequence does not depend on the flag. A protected packet is recorded as
  suppressed, as `frame_drop` does.
- **Shared across readers.** `gortsplib` marshals each packet once and fans it out, so loss applies
  to every reader of the camera. Per-reader loss would need a different write path and would break
  the "decision depends on seed and clock only" rule; it is out of scope.
- **Transport honesty.** Over UDP a withheld packet is a genuine loss. Over TCP-interleaved
  transport the client sees a sequence gap but no stall, because TCP delivers everything it is
  given. The documentation says so plainly and does not call it "TCP loss". Sender-report packet
  counts (which `gortsplib` derives from what was written, not what was withheld) are slightly
  off; that is accepted and noted.

### 2.2 `keyframe_starvation` (slice A)

We cannot re-encode, so a "starved" keyframe is **withheld**, and the P frames after it still flow
and cannot be decoded until the next kept keyframe. The engine counts random-access access units;
an access unit is delivered when `k mod keepEvery == 0`, where `k` is the zero-based count of
random-access access units since the engine was created. The count is monotonic across loops of the
source. The first keyframe is therefore delivered, so a fresh run starts decodable, and a late
joiner waits up to `keepEvery - 1` keyframe periods for a decodable start.

No randomness is involved (deliberately: the long-GOP model is fully reproducible from the keyframe
counter). In-band SPS/PPS ride inside the keyframe access unit (the media loader requires them
in-band), so a withheld keyframe also withholds them; a decoder that does not take `sprop` from the
SDP starves for parameter sets as well as for the picture. This is documented as a feature.
`Camera.Screenshot` waits for a kept keyframe, so under starvation it takes longer and is bounded
by the existing 15 s snapshot deadline.

### 2.3 `rtsp_error` (slice B)

On a matching request (`on` names DESCRIBE, SETUP or PLAY) the server answers with `status`
instead of serving it. A matching request fires when its draw is below `rate`, or, if
`onlyRequest` is set, exactly on that one-based request number for that method on that camera
(with `rate` ignored). Handlers return a `base.Response` with the chosen status and a nil stream,
which `gortsplib` supports; DESCRIBE needs a non-200 status, which an error status satisfies.
Status must be in 400–599. This models a camera that intermittently answers 503 or 454.

### 2.4 `teardown` (slice B)

At PLAY, one draw decides whether this reader's session is armed (below `rate`). An armed session
is ended after `afterFrames` frames have been served since the reader joined. Frames are counted by
the pump, on its own goroutine, so the trigger is frame-clocked, not timer-clocked; the pump calls
a hook the server provides, which closes the session (`mode: close`, a clean teardown) or sets
linger 0 on the TCP connection and closes it (`mode: reset`, an actual RST). The reset path uses
`ServerConn.NetConn()` and is outside `gortsplib`'s documented API; a slice B test pins that it
still works against the pinned version.

### 2.5 `sdp_lie` (slice B)

The lie is applied to the DESCRIBE response per request, not baked into the stream, so inject and
clear take effect on the next DESCRIBE with no rebuild and no effect on live readers. The honest
SDP is generated as today and then rewritten:

- `codec`: the media's codec name in `a=rtpmap` disagrees with the stream (H264 advertised as H265
  or the reverse).
- `sprop`: the SPS/PPS in `a=fmtp` are replaced by well-formed but different parameter sets.
- `resolution`: the SPS in `sprop` is replaced by one that decodes to a different size.
- `contentBase`: the `Content-Base` header is wrong (rewritten where the response is finalised,
  because `gortsplib` sets it itself).

Control URLs (`a=control:trackID=N`) are left alone, so SETUP still finds the stream; a lie that
breaks SETUP would be a different fault. Which mechanism returns the rewritten SDP (a response with
an explicit body and a nil stream) is confirmed by a spike test at the start of slice B's plan,
because it depends on `gortsplib` behaviour at the pinned version; if that route is blocked the
plan escalates rather than falling back to a spec-time-only lie silently.

## 3. One schema for validation, API and UI (slice A)

`internal/fault` exposes a catalogue: for each kind, its name, whether it is implemented, a
one-line summary of the real bug it models, its clock, and a typed parameter list (name, type —
number, integer, boolean or enum — minimum, maximum, default, required, help text). Three consumers
read it, so they cannot drift:

1. **Validation.** Spec-time validation, `AddCamera` and `Inject` use it. A parameter that does not
   apply to the kind (for example `keepEvery` on `frame_drop`) is refused with `ErrInvalidSpec`; an
   unimplemented kind is `ErrUnsupported`, as now. This replaces the hand-written per-kind checks.
2. **`GET /api/v1/faults`.** Returns the catalogue as JSON. Writes keep the existing flat body of
   `POST /cameras/{id}/faults`.
3. **README fault table.** A test asserts the README lists exactly the catalogue's kinds and
   implemented status.

The flat `FaultSpec` (root) and `fault.Spec` gain, all `omitempty`: `meanBurst`, `keepEvery`, `on`,
`status`, `onlyRequest`, `afterFrames`, `mode`. Flat rather than per-kind nested structs, so the
journal, the replay hash and hand-written spec files stay simple.

### 3.1 Observability

`Stats` gains `FaultCounts`, a map from kind to `{Applied, Suppressed}` (frames dropped, packets
withheld, keyframes withheld, errors returned, teardowns, lies served; suppressed means decided but
spared by `protectKeyframes`). `FaultsFired` keeps its meaning. `obs.Fault` details name what
happened for each kind.

### 3.2 Journal

`JournalEntry` gains `Clock` (`frame` or `request`) so that `FrameIndex` is read correctly for
control-plane kinds, where it holds the request number. The replay-line hash covers the new field;
the journal hash is not promised stable across versions before 1.0.

## 4. The control-plane engine (slice B)

Frame-clocked faults live in the pump and stay single-goroutine. Control-plane faults are decided
on `gortsplib` connection goroutines, so they get a separate per-camera engine:

- It holds the active control-plane specs, per-method request counters and the per-kind streams,
  all guarded by one mutex (or atomics), never by the pump's ownership.
- Handlers (`OnDescribe`, `OnSetup`, `OnPlay`) and the response finaliser call it once per request.
  It bumps the counter, performs the unconditional draw for each active matching kind, and returns
  a decision.
- `Server.SetFaults` routes media kinds to the pump and control-plane kinds to this engine; the
  "applied" callback carries the clock value (frame or request number) into the journal.
- The server keeps the `ServerSession` and connection of each reader so `teardown` can close them
  without reaching into `gortsplib` internals beyond the documented `Close` and `NetConn`.
- Lock discipline: the engine's mutex is never held across a call into `gortsplib` or the pump,
  and the fleet lock order (`Fleet.mu` before `events.mu`) is unchanged.

## 5. UI (slice A)

Each camera row's fault controls render from `GET /api/v1/faults`:

- A kind selector lists every kind; unimplemented kinds are disabled and labelled "not
  implemented"; the help text and the one-line "models" summary are shown as visible hint text.
- Parameter inputs are generated from the schema (number, integer, checkbox, select) with the
  declared bounds and defaults. Apply and Clear work per kind, with the existing in-flight guards.
- Active faults show kind, parameters and the per-kind counters from `FaultCounts`.
- Existing hooks keep their meaning: `.fault-kind`, `.fault-rate`, `.fault-protect`,
  `.fault-active`, `.btn-fault`, `.btn-clear-fault`. Generic inputs add
  `.fault-param[data-param=<name>]`; `.fault-rate` and `.fault-protect` remain the classes on the
  `rate` and `protectKeyframes` inputs. The e2e tests are updated accordingly.
- If the catalogue contains a kind or parameter type the page cannot render, the page skips it and
  shows a notice; the schema is not versioned.

## 6. Testing

Same standard as `frame_drop`: real wire, `-race`, fixed seeds.

- **`packet_loss`:** a fixed seed yields a fixed set of withheld packet indices; sequence numbers
  show gaps and timestamps stay honest; the long-run loss fraction converges to `rate` and the
  measured mean burst length to `meanBurst` (tolerances stated in the test, with a seed chosen so
  the test is exact, not statistical flakiness); `protectKeyframes` spares keyframe packets and
  records them as suppressed; validation of the `p ≤ 1` bound; a real TCP-interleaved client sees
  the gaps.
- **`keyframe_starvation`:** only every Nth keyframe is delivered; P frames continue; a late
  joining real client receives no decodable frame until a kept keyframe; `Camera.Screenshot` is
  slower under starvation but succeeds within its deadline for small N.
- **`rtsp_error`:** a real client receives the configured status on the Nth request of the chosen
  method with `onlyRequest`, and the same sequence of failing requests for a fixed seed with one
  client.
- **`teardown`:** the client's session ends after `afterFrames` frames in both modes; the reset
  mode produces a connection reset at the client; no goroutine or connection leaks.
- **`sdp_lie`:** the real DESCRIBE text shows each lie; the honest SDP returns after Clear; SETUP
  and PLAY still work with a lying SDP.
- **Catalogue:** validation, `GET /api/v1/faults` and the README table agree; a parameter that does
  not apply to its kind is refused.
- **Dashboard:** `httptest` for the catalogue endpoint and the mapped statuses; Playwright (the
  `e2e` job) applies and clears one new kind through the generic form, checks a disabled kind
  cannot be applied, and the page stays free of console errors in both colour schemes.
- **Races:** inject and clear concurrent with traffic, with `-race`; a teardown racing camera
  removal must not panic or hang.

## 7. Docs

README: a fault table generated from the catalogue, each row stating the real bug it models; the
limits stated in sections 1.1 and 2.1 (shared loss, TCP shows gaps without stall, control-plane
determinism for a single client). The Scope / not-ONVIF-conformant section is unchanged.

## 8. Out of scope (recorded for later)

`accept_then_silence` (needs a custom listener in front of `gortsplib`); the ONVIF capability lie
(a per-camera override in the ONVIF layer); jitter and burst delay; per-reader loss; modelling TCP
stall and backpressure; re-encoding of any kind; a versioned catalogue schema; dashboard
hardening, which is a separate recorded follow-up.
