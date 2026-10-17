# Faults: behaviour and limits

The catalogue table (kind, whether it is implemented, what field bug it models) is in the
[README](../README.md#fault-catalogue). This page holds what each fault actually does and where
its determinism stops.

- **Works:** ten fault effects, each tied to a real field bug (table in the README): the frame-path faults
  `frame_drop`, `packet_loss` and `keyframe_starvation`, the timing faults `jitter`,
  `burst_delivery`, `pts_dts_mismatch` and `pts_delayed`, and the control-plane faults
  `rtsp_error`, `teardown` and `sdp_lie`. A fault comes from a spec or is injected into and
  cleared from a running camera (`Camera.Inject`, the dashboard, `POST /api/v1/cameras/{id}/faults`).
  Runtime injections and clears are journaled, and the fleet replay line gets a
  ` journal=<n>:<12hex>` suffix hashing that journal. Per-kind effect counts are in
  `Stats.FaultCounts` and the dashboard. `GET /api/v1/faults` returns the catalogue that
  validates specs and generates the dashboard's fault form.
  Honest limits: which frame an interactive click lands on is recorded but cannot be derived
  from the seed, because it depends on when the click arrived; `packet_loss` draws only for
  frames that were actually encoded, so frames removed by `frame_drop` or `keyframe_starvation`
  consume no `packet_loss` draws and combining them makes its sequence depend on theirs;
  `FramesServed` still counts a frame whose packets were all withheld; a withheld packet is
  only a sequence gap, with no stall, because RTSP is served over TCP-interleaved transport only
  today (TCP delivers everything it is given); genuine loss arrives with UDP serving; loss applies to every reader of the camera, not per
  reader; `keyframe_starvation` also withholds the in-band SPS/PPS that travel with the withheld
  keyframe, and with `keepEvery` above about 15 (a 1s GOP at the fixture's 15fps) the 15s snapshot
  deadline is exceeded, so dashboard and ONVIF snapshots fail. For the frame-path kinds with a
  `rate` (`frame_drop`, `packet_loss`, `burst_delivery`) an omitted `rate` means 0, so the fault
  is installed but never fires: the catalogue's default rate only prefills the dashboard form and
  is not applied server-side (unlike `rtsp_error`, below).
  Timing faults (`jitter`, `burst_delivery`, `pts_dts_mismatch`, `pts_delayed`) have their own
  limits:
  - **Arrival times are wall-clock and not reproducible; the delays, hold boundaries and
    timestamps are.** The seed fixes how long each frame is delayed, which frames are held and
    which timestamps are written; when those packets actually reach a client depends on the
    scheduler and the network.
  - **Compose order**, per frame: `frame_drop` / `keyframe_starvation` (the frame is removed or
    kept), then encode, then `packet_loss`, then the timestamp faults (`pts_dts_mismatch`,
    `pts_delayed`), then `burst_delivery` hold or flush, then `jitter`.
  - `burst_delivery` and `packet_loss` interact in one way worth knowing: loss is decided at
    encode, before the hold, so each frame's loss is drawn per frame regardless of whether the
    frame is later held. A frame removed by `frame_drop` or `keyframe_starvation` while a hold is
    open shortens the burst, so a release can be smaller than `holdFrames`.
  - A change to the camera's frame-fault list while frames are held flushes them at the next
    frame. A change to control-plane faults only (`rtsp_error`, `teardown`, `sdp_lie`), or
    re-injecting an identical fault, does not touch the pump and does not flush. Frames still
    held at shutdown are discarded.
  - With `burst_delivery`, a `teardown` still counts held frames toward `afterFrames` (its
    count fires per pump frame, held ones included), so a client torn down at `afterFrames` may
    have received up to `holdFrames`-1 fewer frames: those were still in the hold buffer.
  - The `burst_delivery` tally counts held frames, not the flush frame that releases them.
  - With `burst_delivery` and `jitter` together, a held frame's deadline is that of the last frame
    of its hold plus that frame's jitter, so the held frames leave together at that time.
  - RTP sequence numbers stay contiguous for all four timing kinds; only timing and timestamps
    change.
  - `jitter` delays are relative to each frame's nominal slot (`start + k × interval`, an
    absolute schedule) and order is preserved: a frame never leaves before the one ahead of it,
    so when a delay runs past later slots those frames go out back-to-back behind it rather than
    being reordered. Every frame leaves within `maxMs` of its slot, so the long-run frame rate
    stays nominal for any `maxMs`. A pump that wakes more than max(1 s, 10 frame intervals) behind
    its schedule (a suspended process, a stalled host; never lateness jitter caused) skips the
    missed slots and restarts its schedule from now instead of sending them as one burst; below
    that it catches up back-to-back.
  - `pts_dts_mismatch` reaches clients as out-of-order RTP timestamps (sequence numbers stay in
    order). Its windows are phased on the absolute frame counter, not on the frame it was injected
    at, so injecting it at a frame that is not a window start, or clearing it mid-window, can give
    two consecutive frames the same timestamp (with `reorderDepth` 1 injected at frame 5, frame 5
    is stamped like frame 4).
  - `pts_delayed` is visible mainly in the RTCP sender report's own RTP timestamp. Measured on the
    pinned `gortsplib` v5.6.5, the server-side sender pairs the already-shifted RTP timestamp with
    the true write time, so a client that maps packets to wall-clock through the sender report
    (`Client.PacketNTP`) sees a delayed camera at the same lag as a clean one (a difference near
    0 ms). The delay shows only as the SR's RTP timestamp sitting about `delayMs` behind a clean
    camera's (about -45000 ticks at 90 kHz for 500 ms). A wire test pins this, and a `gortsplib`
    upgrade may change it, in which case that test fails loudly. `driftPpm` drift grows from the
    frame the fault was installed on. Injecting or clearing an unrelated frame fault leaves an
    unchanged `pts_delayed` drifting continuously; changing its own parameters, or clearing and
    re-injecting it, restarts the drift at zero.

  Control-plane faults have their own limits:
  - `rtsp_error`, `sdp_lie` and `teardown` are deterministic for a **single client** only: their
    request counters and draws are per camera and shared across connections, so concurrent
    clients interleave non-deterministically. `Camera.Screenshot`, the dashboard Snapshot and a
    stream quality test are RTSP clients too and consume control draws, so the single-client
    claim excludes using them concurrently with a test client.
  - The **request index** (the `clock: request` journal index and the event-log index of
    `rtsp_error` and `teardown`) counts DESCRIBE, SETUP and PLAY requests to that camera since
    its creation, including requests answered with an injected error, and excluding OPTIONS and
    TEARDOWN.
  - Omitted parameters: an `rtsp_error` without `status` answers 503, and without `rate` and
    `onlyRequest` uses the catalogue default rate 0.2 (so a rate of 0 cannot be requested; use a
    tiny rate such as 1e-9 to mean "never"). `sdp_lie` and `teardown` treat an omitted rate as 1
    (always); to not inject them, clear the fault or use a tiny rate. With `on: all`,
    `onlyRequest` counts requests of every method on one counter.
  - A control-plane fault takes effect on the **next request**, not on a frame boundary. Journal
    entries carry a `clock` of `request` or `frame`. For a set containing both classes the
    request-clock entry is journaled at once and the frame-clock entry at the next frame
    boundary, in that order. Only classes whose specs changed are journaled and restarted;
    re-injecting an identical fault is a no-op, so to restart its seeded sequence, clear and then
    inject. There is still no per-kind clear: clearing removes every fault.
  - `teardown` counts pump frames, not wall-clock time; the decision to tear down at frame N is
    deterministic, but the number of frames the client actually received can be `afterFrames`-1
    depending on scheduling (frames removed by `frame_drop` or
    `keyframe_starvation` still count), and a frame that fails to encode or write
    does not count. Its `reset` mode reaches the connection through `ServerConn.NetConn()`, which
    is outside `gortsplib`'s documented API, and is pinned by a test so a library upgrade that
    breaks it fails loudly. On a connection that is not a `*net.TCPConn` it closes the connection
    without an RST.
  - `sdp_lie` rewrites only the DESCRIBE response. Mode `codec` changes only the `a=rtpmap` line
    (the honest `fmtp` stays); `sprop` replaces the parameter sets; `resolution` substitutes one
    of two embedded SPS constants (`mediacommon` has no SPS writer); `contentBase` replaces the
    host with `wrong.invalid` and keeps the path. `a=control` lines are never altered. A lie is
    counted in the fault tally and the obs event log (the fleet journal holds only inject, clear, add and remove) only when the rewrite actually changed the response.
