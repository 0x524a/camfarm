<p align="center"><img src="docs/assets/camfarm-logo.svg" alt="CamFarm: synthetic RTSP camera farm" width="460" height="144"></p>

# CamFarm

[![ci](https://github.com/0x524a/camfarm/actions/workflows/ci.yml/badge.svg)](https://github.com/0x524a/camfarm/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/0x524a/camfarm/graph/badge.svg)](https://codecov.io/gh/0x524a/camfarm)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=0x524a_camfarm&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=0x524a_camfarm)

**Simulated fleet for pipeline testing. Not an ONVIF conformance tool.**

CamFarm is a synthetic RTSP camera farm for testing video-analytics pipelines without
physical hardware. It serves N fake cameras from one process, and it is built so
that misbehaviour can be injected deterministically: a fault fires because a
seed said it would, and the same seed and fleet spec reproduce it.

## Status

Early:

- **Works:** N RTSP cameras behind one listener, each on its own path; H.264 and
  H.265, served as whatever the source already is; MPEG-TS, plain MP4, and
  fragmented MP4 as sources, detected from content rather than filename; a
  bundled fixture per codec; media parsed once and shared across cameras;
  seed-derived per-camera RTP sequence numbers; inspection counters.
- **Works:** an ONVIF® Device Service and Media Service control plane —
  *not ONVIF conformant* (see below) — behind one HTTP listener, with HTTP
  Digest authentication when a camera's spec sets credentials.
> Implements/supports the ONVIF® Device Service and Media Service
> specifications. This project is **not ONVIF conformant**, is not tested
> against the ONVIF conformance suite, and makes no claim to any ONVIF
> profile or add-on.
- **Works:** a Go API for mutating a running fleet — `Fleet.AddCamera` starts a new camera (RTSP,
  and its ONVIF endpoint when the camera's spec sets credentials) without restarting the process,
  and `Fleet.RemoveCamera` stops and removes one. Both apply the same validation `Start` does, so a
  spec that would be refused at startup is refused here too.
- **Works:** screenshots — `Camera.Screenshot(ctx)` returns one decoded frame
  from a camera's live RTSP stream, and each camera serves it as a JPEG at
  `/onvif/{id}/snapshot` (behind Digest auth when credentialed), which
  `GetSnapshotUri` advertises. The capture dials the real RTSP wire and waits
  for the next keyframe: for the bundled fixture that is at most one keyframe
  interval (a keyframe every 15 frames). A source with no keyframe blocks until
  the caller's context ends. The pure-Go H.264 decoder handles only IDR pictures
  and P_Skip-only slices, 8-bit 4:2:0; H.264 is the only codec supported so far,
  and H.265 is not yet. Sources outside that subset may fail with an error or
  decode incorrectly. Only the bundled fixture and similarly encoded files are
  tested.
- **Works:** a web dashboard and `cmd/camfarm` binary. `go run ./cmd/camfarm --cameras 3` (or the
  built `camfarm` binary) serves a fleet and a page at `http://127.0.0.1:8080/` that lists cameras
  with copyable RTSP and ONVIF URLs, adds and removes cameras, takes on-demand snapshots, and
  injects and clears faults. Changes appear live over Server-Sent Events. The JSON API is under
  `/api/v1/` (`fleet`, `cameras`, `cameras/{id}`, `cameras/{id}/snapshot.jpg`,
  `cameras/{id}/faults`, `cameras/{id}/quality`, and the `events` stream). Seeds are decimal strings in the API. Flags:
  `--spec`, `--seed`, `--cameras` (mutually exclusive with `--spec`), `--http-addr`,
  `--listen-host`, `--rtsp-port`, `--onvif-port`, `--discovery`, `--discovery-interface`, `--media-dir`, `--allow-ui-paths`, `--max-upload`; run with `-h` for details. **Security:** the
  dashboard can start servers and, with file sources, make the process read local paths, and the
  page shows cameras' Digest credentials. It listens on loopback by default and has *no*
  authentication, token or Host-header check. Only the library's mutating endpoints (upload, scan,
  delete, roots) and starting or cancelling a stream quality test refuse cross-site browser requests (an `Origin` that differs from `Host`, or
  `Sec-Fetch-Site: cross-site`); every other endpoint accepts them, so a malicious web page can
  reach those through CSRF. No endpoint, the library included, is protected against DNS
  rebinding: under rebinding the `Origin` equals `Host`, so the check passes. Do not expose it.
- **Works:** an on-demand stream quality test — `POST /api/v1/cameras/{id}/quality` runs
  rtspeek against the camera's own RTSP stream for a few seconds and reports packet loss, jitter,
  bitrate, frame rate, keyframe and timestamp findings. The measured numbers are not reproducible
  from a seed. See [Stream quality test](#stream-quality-test).
- **Works:** host metrics — `GET /api/v1/metrics`, and an opt-in `metrics` event on
  `/api/v1/events?metrics=1`, report
  host CPU, memory, GPU (via `nvidia-smi`) and camfarm's own process usage. Not part of the replay
  hash. See [Host metrics](#host-metrics).
- **Works:** a media library — point the binary at directories of your own video
  with `--media-dir` (repeatable) and the dashboard API lists, validates and
  serves them. Each file is validated once per change: at startup, on rescan, on
  upload, and again when a camera is created from the file if the file changed
  since. The report says whether it is `valid`, `warning` (for example B-frames,
  reordered timestamps, a variable frame rate) or `invalid` and why. An invalid
  file cannot become a camera through `libraryId`; files with warnings can. A
  typed `source.path` is not validated, and can read any file this process can
  read (that behaviour is unchanged by the library).
- **Works:** ten fault effects, each tied to a real field bug (table below): the frame-path faults
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
- **Works:** an opt-in WS-Discovery responder, so an ONVIF client can find the fleet by
  probing instead of being handed an endpoint. Set `listen.discovery` in the spec (`mode`
  `multicast` or `unicast`; unicast takes `addr` and optionally `announceAddr`, multicast takes an
  optional `interface`) or pass `--discovery multicast` / `--discovery unicast:HOST:PORT` and
  `--discovery-interface` to `cmd/camfarm`. One socket answers for every camera. Hello (when a
  camera is added or the fleet starts) and Bye (when a camera is removed or the fleet is closed)
  go to the multicast group in multicast mode, or to `announceAddr` in unicast mode; a unicast
  run without `announceAddr`, which is every CLI unicast run, announces nothing. Each camera's endpoint reference is its seed-derived `urn:uuid:` that
  `GetEndpointReference` also returns, and its advertised XAddr is that camera's real ONVIF
  endpoint. `Fleet.DiscoveryAddr()` returns the bound UDP address (the binary prints
  `discovery: udp://ADDR`; in multicast mode it prints the group, `udp://239.255.255.250:3702
  (multicast)`).
- **Works:** Playwright end-to-end tests of the dashboard, behind the `e2e` build tag and run in
  the `e2e` CI job. Locally, `go test -tags e2e ./internal/dashboard/` needs Chromium installed
  for Playwright.
- **Not built yet:** PTZ, Imaging, and the *effect* of the
  one remaining catalogued fault, `accept_then_silence`. A spec that names an unimplemented fault is **refused**
  (`ErrUnsupported`), not silently ignored.

## Fault catalogue

Kept in step with the catalogue in `internal/fault` by a test that fails if this table drifts.

<!-- fault-table:start -->
| Kind | Implemented | Models |
|---|---|---|
| `frame_drop` | yes | Encoder skipping frames: a frame is never encoded, so RTP sequence numbers stay contiguous and the loss shows only as a timestamp jump. |
| `packet_loss` | yes | Lossy path: packets of an encoded frame are withheld, leaving real sequence-number gaps with honest timestamps. |
| `keyframe_starvation` | yes | Long-GOP camera: only every Nth keyframe is sent, so a client joining mid-stream cannot start decoding. |
| `rtsp_error` | yes | Flaky camera that answers 5xx or 454 to some DESCRIBE, SETUP or PLAY requests. |
| `teardown` | yes | Camera that drops live sessions; TCP reset when mode is reset. |
| `sdp_lie` | yes | SDP that misdescribes the stream: wrong codec, wrong parameter sets, wrong geometry or wrong Content-Base. |
| `jitter` | yes | Inter-arrival jitter: each frame leaves a seeded random time late (order kept), which stresses jitter-buffer sizing. |
| `burst_delivery` | yes | Bursty uplink or encoder buffering: frames are held and then released back-to-back, which overflows NVR queues. |
| `pts_dts_mismatch` | yes | B-frame camera that stamps presentation time into RTP: timestamps run out of order, so decoders report non-monotonic dts. |
| `pts_delayed` | yes | Camera whose timestamps lag capture, constantly or by drift: what a latency profiler such as framelag meets. |
| `accept_then_silence` | no | Camera that accepts the TCP connection and never answers. |
<!-- fault-table:end -->

## Bring your own video

```sh
camfarm --media-dir /srv/clips --media-dir ./more-clips
```

A Docker volume or bind mount is just a directory, so inside a container point
`--media-dir` at the mount path.

- Files with a video extension (`.ts .mp4 .m4v .mov .mkv .avi .h264 .h265 .mpg`)
  are listed when they sit up to three levels below the media directory
  (`root/a/b/file.mp4` is listed, `root/a/b/c/file.mp4` is not). Symlinks and dot-files are skipped.
  The container is judged from content, so a `.mkv` is reported as an
  unsupported container rather than ignored. Only MPEG-TS and MP4/MOV carrying
  H.264 or H.265 can be served.
- Uploads through the dashboard API (`POST /api/v1/library/upload`; the dashboard
  page has no library controls yet) go to an `uploads/` subdirectory of the first
  writable `--media-dir`, created on first use, capped by `--max-upload` (default 2GiB).
  An upload that fails validation is kept so its report stays visible; delete it with
  `DELETE /api/v1/library/files/{id}`. Only files inside `uploads/` can be deleted
  (anything you place there by hand included); your own files elsewhere in the
  directory are never removed by the API.
- `--allow-ui-paths` lets the dashboard API add directories by typed path
  (`POST /api/v1/library/roots`). It is off
  by default because the dashboard has no authentication and the option lets
  anyone who can reach it point the process at any directory it can read.
- A camera takes a library file with `{"id": "cam", "libraryId": "<id from GET
  /api/v1/library>"}`. The file is re-checked first, since it may have changed
  since the scan. Only this route validates: a camera created with a typed
  `source.path` skips the library entirely.
- If a media directory disappears, a rescan drops its files and the scan response
  lists it under `scanErrors`; the other directories keep working.

What the validator can and cannot see: it reports what camfarm's own parser
loads. B-frames are read from H.264 slice headers; for H.265 they are inferred
from timestamp reordering, because H.265 slice types are not parsed. It does not
repair or transcode anything.

## Stream quality test

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"durationSeconds": 5, "transport": "auto"}' \
  http://127.0.0.1:8080/api/v1/cameras/cam-1/quality      # start: 202 and the running test
curl http://127.0.0.1:8080/api/v1/cameras/cam-1/quality   # latest result
curl -X DELETE http://127.0.0.1:8080/api/v1/cameras/cam-1/quality   # cancel: 204
```

A test points [rtspeek](https://github.com/0x524A/rtspeek) (v1.0.0, linked as a library) at a
camera's own RTSP URL for a short window and reports what a client received: packet loss, jitter,
bitrate, frame rate, keyframes, timestamp jumps, broken frames and B-frame reordering, as findings
ranked `info`, `warn` or `error`. The URL is always the camera's own; nothing in the request can
aim it at another host. It is API-only for now: the dashboard page has no button for it yet.

- Options: `durationSeconds` from 1 to 30, default 5; `transport` `auto` (the default), `udp` or
  `tcp`. An empty body takes the defaults. The thresholds are rtspeek's own and cannot be changed,
  so results stay comparable between runs.
- **TCP hides network loss in transit** (rtspeek's own caveat): TCP retransmits, so loss between
  camera and client never shows on it. CamFarm serves RTSP over TCP-interleaved transport only
  today, so `udp` fails with a `setup_failed` finding (the camera answers 461 Unsupported
  Transport) and `auto` falls back to TCP, adding an `info` `transport_fallback` finding. A
  `packet_loss` fault still shows over TCP, because it withholds packets at the camera after they
  have been numbered: the gaps are in the sequence numbers the client receives.
- The verdict of a finished run is its worst finding: `error` if there is an error finding, `warn`
  if there is a warning, otherwise `ok` (`info` findings alone are `ok`). A run that cannot finish
  (connection refused, stream ended, timed out, cancelled) is `failed` with an `error` and no
  verdict. A cancelled or timed-out run carries no analysis.
- Results live in memory only, and only the latest per camera is kept: a new test replaces it, a
  restart loses it, and removing the camera forgets it (cancelling a test still running).
  `GET /api/v1/cameras/{id}` carries a `quality` summary (`state`, `verdict`, `finishedAt`) once a
  result exists.
- A test is a real RTSP client, so it occupies one session on the camera (its reader count rises
  by one while it runs). At most 2 run at once across the fleet (429 with `Retry-After` beyond
  that) and one per camera (409).
- `quality` events (`{camera, state, verdict}`) arrive on `/api/v1/events`. They come from the
  test runner, not the fleet's event feed, so a test publishes nothing to the fleet's feed and adds
  no journal entry. It is still an RTSP client: like `Camera.Screenshot` it counts toward the
  camera's request index and consumes control-plane fault draws, so it can move the `clock: request`
  index of a later control-plane inject and, with `rtsp_error`, `sdp_lie` or `teardown` active,
  consume an `onlyRequest: N` request.
- **The measured numbers are not reproducible from a seed.** Jitter, frame rate, bitrate and the
  loss percentage depend on timing and on load on the machine, over a window of a few seconds.
  What does reproduce is the presence of a strong seeded fault: a `packet_loss` at a rate of 0.3
  produces a `packet_loss` finding. Do not compare figures between runs as if a seed fixed them.
- `POST` and `DELETE` refuse cross-site browser requests, like the library's mutating endpoints.

## Host metrics

```sh
curl http://127.0.0.1:8080/api/v1/metrics
```

`GET /api/v1/metrics` returns one sample of the machine camfarm runs on, no older than two seconds.
The same sample arrives as a `metrics` event on `/api/v1/events`, but only when the request asks
for it with `?metrics=1` (exactly that value); without it the stream carries no metrics events and
subscribing never wakes the sampler.

```json
{
  "time": "2026-10-12T09:00:00Z",
  "cpu": {"available": true, "usagePercent": 12.5, "logicalCPUs": 16, "load1": 0.5, "load5": 0.4, "load15": 0.3, "limitCPUs": null},
  "memory": {"available": true, "totalBytes": 66560000000, "usedBytes": 26000000000, "availableBytes": 40560000000, "swapTotalBytes": 0, "swapUsedBytes": 0, "limitBytes": null},
  "gpu": {"available": true, "devices": [{"index": 0, "name": "NVIDIA GeForce RTX 4090", "uuid": "GPU-aaaa-1111", "gpuUtilPercent": 37, "memUtilPercent": 12, "memUsedBytes": 1073741824, "memTotalBytes": 25757220864, "encoderUtilPercent": 5, "decoderUtilPercent": 0, "temperatureC": 54, "powerWatts": 78.43, "powerLimitWatts": 450}]},
  "process": {"available": true, "cpuPercent": 3.1, "rssBytes": 41943040, "goroutines": 52, "openFDs": 18}
}
```

- Sections are independent. One that cannot be read has `"available": false` and a `reason`; the
  others are unaffected and the response is still 200. For `cpu`, `memory` and `process` on Linux
  the reason is the underlying read error text. For `gpu` it is one of: `nvidia-smi not found`,
  `nvidia-smi failed: <first output line>` (a driver or library mismatch prints its own message),
  `timed out`, `unparseable output`, `nvidia-smi reported no devices`, or `pending` before the
  first reading has landed.
- `null` means "not reported", never 0: the first CPU reading after the sampler wakes has no
  baseline, a limit is `null` when no cgroup limit applies, and a value `nvidia-smi` prints as
  `N/A`, `Not Supported` or a bracketed error such as `[Unknown Error]` is `null`.
- Cgroup limits are read from the cgroup v2 root only: they are right inside a container with its
  own cgroup namespace, and `null` elsewhere and on cgroup v1.
- `gpuUtilPercent` is `nvidia-smi`'s `utilization.gpu`: the share of time a kernel was running, not
  SM occupancy.
- CPU, memory and process readings are Linux only; other systems report `unsupported platform`.
  The GPU section needs `nvidia-smi` on the `PATH`.
- The sampler runs only while an event-stream client that passed `?metrics=1` is connected or the
  endpoint was fetched in the last 10 seconds; otherwise it is idle.
- **The endpoint exposes host details and the dashboard has no authentication.** Do not expose it.
  Like the rest of the dashboard, it is not protected against DNS rebinding.
- Metrics publish nothing to the fleet's feed, add no journal entry and are not part of the replay
  hash.
- The GPU parser is verified against captured `nvidia-smi` output, not live hardware.

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
- **It does not transcode or rescale.** A camera serves whatever codec and
  geometry its source already has. Serving H.265 means supplying an H.265
  source, and serving 320×240 means supplying a 320×240 source. Both would need
  an encoder, no viable pure-Go H.265 encoder exists, and cgo and a runtime
  `ffmpeg` are both excluded by design. `VideoSpec` therefore describes the
  output you expect and is refused when it disagrees with the source, rather
  than transforming anything.
- **Container support is deliberately narrow.** MPEG-TS, plain MP4, and
  fragmented MP4, all H.264 or H.265 video only; audio tracks are ignored.
  Matroska and MPEG-PS are not supported, and no pure-Go demuxer for either was
  found to be production-ready. Fragmented MP4 additionally requires each `trun`
  to carry a data-offset preset, so a producer using the base-data-offset
  convention is refused rather than mis-parsed.
- **The ONVIF control plane covers two services only.** Device and Media —
  enough for a client to discover a camera and get back its real RTSP URL.
  PTZ and Imaging are not implemented: `GetCapabilities`/`GetServices` never
  advertise them, so a conformant client should not expect them. A WS-Discovery responder
  is available but opt-in (see below); with it off, a client must be given the ONVIF endpoint
  directly (`Camera.ONVIFEndpoint()`). Each camera has a seed-derived endpoint UUID, returned by
  `GetEndpointReference` as `urn:uuid:<uuid>` (served by `onvif-go`'s own handler) and shown bare
  in the dashboard; the responder announces and answers with that same reference. The UUID is
  version 4 shaped.
- **WS-Discovery is a simulation, not a conformance-tested responder.** It is `onvif-go`'s
  responder, which supports Probe/ProbeMatches with Types and Scopes filtering (rfc3986, strcmp0
  and none match rules) plus Hello and Bye. It does not support Resolve/ResolveMatches, the ldap
  and uuid scope rules (such a probe matches nothing), the 2009/01 (WS-Discovery 1.1) namespace,
  or managed mode with a discovery proxy. A wildcard `listen.host` (`0.0.0.0`, `::`) is refused,
  because the host is advertised in each camera's XAddr and a wildcard is not an address a client
  can dial. Multicast mode also refuses a loopback `listen.host`, since it would advertise an
  address no other machine can reach; use unicast mode for loopback. Multicast depends on the
  network: it commonly does not work across Docker bridge networks, on CI runners, between network
  namespaces, or through firewalls and IGMP-snooping switches, and nothing here detects that. CI
  exercises **unicast mode only** for probe and announce behaviour. A test that starts the
  responder in multicast mode (join, add and remove a camera, close) runs only where the host has
  a non-loopback multicast-capable IPv4 interface and skips otherwise, and it does not check that
  any packet is delivered. Multicast joins the
  interface named by `--discovery-interface` (`interface` in the spec); when that is empty
  and `listen.host` is a literal IP it defaults to the interface holding that IP, and with a
  hostname it falls back to the system default-route interface. Containers on a bridge should need the
  bridge's interface (reasoned from how multicast joins work, not tested). **On the same host as the fleet, multicast discovery with `onvif-go`'s
  client (and other clients built on Go's `net.ListenMulticastUDP`) finds nothing and never sees
  Hello or Bye**, because both sides disable `IP_MULTICAST_LOOP`. This was observed by hand on one Linux
  machine (`onvif-go`'s client on the LAN interface found none of the fleet's cameras, while probing
  via `lo` and a Python prober found them); no test covers it. On one host, use the loopback interface (`lo`), a client that leaves multicast loopback
  on, or unicast mode. **Give each fleet on a shared network a distinct seed:** endpoint
  references derive only from the seed (a spec file with no seed, and a zero-value library
  `Spec`, use 0), so two fleets with the same seed advertise identical `urn:uuid:` references
  with different XAddrs, and clients keep one entry per reference. Only the endpoint references
  are seeded: WS-Discovery MessageIDs are random and the AppSequence InstanceId is wall-clock,
  so discovery traffic is not byte-replayable, and reply timing is wall-clock. Enabling
  discovery changes the spec's hash, and so the replay line.
- **The bundled video is a fixture, not a sample.** Two of them: a two-second
  synthetic test pattern at 320×240 in H.264, and its H.265 counterpart, both
  generated by `scripts/gen-fixture.sh`. They exist so that tests need no media
  sourced before they run. Neither is representative of real camera output in
  bitrate, motion, noise, or encoder behaviour.
- **Determinism has stated limits.** The decision sequence is reproducible from a
  seed; wall-clock timings are not, because serving a real client happens in real
  time. Two specific carve-outs:
  - **RTP SSRC is not reproducible.** The underlying RTSP library assigns each
    stream its own SSRC and overwrites whatever the encoder set, so SSRC is
    excluded from replay assertions. Sequence numbers *are* reproducible.
  - **A live upstream source is outside the guarantee.** Not implemented in this
    version; when it is, its bytes will differ run to run, so only the fault
    *decisions* over it will be reproducible, not the media.
- **No network-path impairment.** Bandwidth ceilings, reordering and real
  network-path effects are properties of the path, not of the sender; they need
  `tc`/`netem` and elevated privileges, and they are not reproducible from a
  seed. `packet_loss` is a *sender-side simulation* of loss (the camera chooses
  not to write packets), not a network impairment. CamFarm owns only what a
  camera itself chooses not to send, or sends late.
- **Not a benchmark result.** No cameras-per-core or memory-per-camera figure is
  published, because none has been measured yet. When one is, it will come from a
  committed benchmark, not an estimate.

## Requirements

Go 1.26 or later. No cgo, and no system packages at runtime. `ffmpeg` is needed
only to run the test suite, which cross-checks the served streams with `ffprobe`
and remuxes the bundled fixtures into MP4 to test the other container paths.
Regenerating the H.265 fixture additionally needs an `ffmpeg` built with
`libx265`; running the tests does not, because the fixture is committed.

## Licence

MIT. See [LICENSE](LICENSE).
