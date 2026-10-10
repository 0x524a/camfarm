<p align="center"><img src="docs/assets/camfarm-icon.svg" alt="CamFarm icon" width="96" height="96"></p>

# CamFarm

[![ci](https://github.com/0x524a/camfarm/actions/workflows/ci.yml/badge.svg)](https://github.com/0x524a/camfarm/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/0x524a/camfarm/graph/badge.svg)](https://codecov.io/gh/0x524a/camfarm)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=0x524a_camfarm&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=0x524a_camfarm)

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
  `cameras/{id}/faults`, and the `events` stream). Seeds are decimal strings in the API. Flags:
  `--spec`, `--seed`, `--cameras` (mutually exclusive with `--spec`), `--http-addr`,
  `--listen-host`, `--rtsp-port`, `--onvif-port`; run with `-h` for details. **Security:** the
  dashboard can start servers and, with file sources, make the process read local paths, and the
  page shows cameras' Digest credentials. It listens on loopback by default and has *no*
  authentication, token, Host-header check or CSRF protection, so a malicious web page can reach a
  loopback API through DNS rebinding or CSRF. Do not expose it.
- **Works:** one fault, `frame_drop`: a seeded fraction of access units is never encoded or sent,
  so RTP sequence numbers stay contiguous and the missing frame appears only as a timestamp jump
  (an encoder skipping frames, not network loss). It can be set in a spec, or injected into and
  cleared from a running camera; `protectKeyframes` keeps random-access frames. Runtime injections
  and clears are journaled, and the fleet replay line gets a ` journal=<n>:<12hex>` suffix hashing
  that journal. Honest limit: which frame an interactive click lands on is recorded but cannot be
  derived from the seed, because it depends on when the click arrived.
- **Works:** Playwright end-to-end tests of the dashboard, behind the `e2e` build tag and run in
  the `e2e` CI job. Locally, `go test -tags e2e ./internal/dashboard/` needs Chromium installed
  for Playwright.
- **Not built yet:** device discovery (WS-Discovery), PTZ, Imaging, and the *effect* of every fault
  other than `frame_drop`. The rest of the fault catalogue is defined and validated, and the seams
  it acts through are in place, but those faults change nothing on the wire. A spec that names an
  unimplemented fault is **refused** (`ErrUnsupported`), not silently ignored.

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
  advertise them, so a conformant client should not expect them. WS-Discovery
  (multicast device discovery) is not implemented; a client must be given the
  ONVIF endpoint directly (`Camera.ONVIFEndpoint()`).
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
- **No network-path impairment.** Packet loss, bandwidth ceilings, and reordering
  are properties of the path, not of the sender; they need `tc`/`netem` and
  elevated privileges, and they are not reproducible from a seed. CamFarm owns
  only what a camera itself chooses not to send, or sends late.
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
