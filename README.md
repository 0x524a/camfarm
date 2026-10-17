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

## Quick start

```sh
go run ./cmd/camfarm --cameras 3
```

The binary prints `dashboard: http://127.0.0.1:8080/`. Open it: every camera is a card with a
live snapshot and its RTSP and ONVIF URLs, each with a Copy button. Point your pipeline, `ffprobe`
or VLC at an RTSP URL from there (the RTSP and ONVIF ports are picked at random unless you pass
`--rtsp-port` and `--onvif-port`). Use your own footage with `--media-dir DIR`. Run with `-h` for
every flag.

**The dashboard has no authentication.** It can start servers, read local paths when you allow
it, and show host details and camera credentials. It listens on loopback by default. Do not
expose it. [Details](docs/dashboard.md).

## The dashboard

The screenshots below come from `TestDocsScreenshots`, a Playwright test. The committed images
were made by running it on a development machine with the command at the end of this section;
CI runs the same test on every change, which fails if the page stops showing these views, and
keeps its own images as the `dashboard-screenshots` build artifact. Nothing is mocked: the host
figures are the machine's own (the GPU tile says why it is unavailable when there is no
`nvidia-smi`), the library verdicts come from the real validator, and the stream quality result
is a real run.

![The Cameras tab: a host metrics strip, then one card per camera with a snapshot, RTSP and ONVIF URLs, an active frame_drop fault on one card and a finished stream quality test on another.](docs/assets/dashboard-cameras.png)

**Cameras.** One card per camera, with the stream's source, codec and size, fault chips, and a
stream quality test that reports frame rate, bitrate, jitter, packet loss and keyframe interval.

![The fault drawer open on a camera, with the packet_loss kind selected and its parameters shown.](docs/assets/dashboard-fault-drawer.png)

**Faults.** The drawer lists the catalogue's kinds and parameters, shows what is active on the
camera and clears it. What each fault does, and where its determinism stops, is in
[docs/faults.md](docs/faults.md).

![The Library tab in the dark theme: filter chips with counts, the media sources, an upload area and a table of files with a status pill, stream facts and findings.](docs/assets/dashboard-library.png)

**Library.** Every file in your media directories is validated once per change, and each problem
is written out, so a file that cannot be served is explained before it becomes a camera. See
[docs/library.md](docs/library.md).

To regenerate the images:

```sh
CAMFARM_DOCS_SHOTS=$PWD/docs/assets go test -tags e2e -count=1 -run TestDocsScreenshots ./internal/dashboard/
```

## Status

Early. What works, in brief:

- N RTSP cameras behind one listener, H.264 and H.265, from MPEG-TS, MP4 or fragmented MP4 sources.
- An ONVIF® Device and Media control plane, and an opt-in WS-Discovery responder.
- Ten seeded fault effects, injectable at startup or at runtime, with a replayable journal.
- A dashboard with a media library, a stream quality test and host metrics, and a JSON API.
- A Go API: `camfarm.Start(spec)` in a `TestMain`, `Fleet.AddCamera` and `Fleet.RemoveCamera`.

> Implements/supports the ONVIF® Device Service and Media Service
> specifications. This project is **not ONVIF conformant**, is not tested
> against the ONVIF conformance suite, and makes no claim to any ONVIF
> profile or add-on.

The full list, including what is not built yet, is in [docs/features.md](docs/features.md). More:
[dashboard and CLI](docs/dashboard.md), [faults](docs/faults.md), [media library](docs/library.md),
[stream quality test](docs/stream-quality.md), [host metrics](docs/host-metrics.md) and
[WS-Discovery](docs/discovery.md).

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
