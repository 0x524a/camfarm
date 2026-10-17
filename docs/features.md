# What works today

The README's Status section links here. Each item below is shipped and tested; the "Not built yet"
item lists what is not. Longer topics have their own pages: [dashboard](dashboard.md),
[faults](faults.md), [media library](library.md), [stream quality test](stream-quality.md),
[host metrics](host-metrics.md) and [WS-Discovery](discovery.md).

- **Works:** N RTSP cameras behind one listener, each on its own path; H.264 and
  H.265, served as whatever the source already is; MPEG-TS, plain MP4, and
  fragmented MP4 as sources, detected from content rather than filename; a
  bundled fixture per codec; media parsed once and shared across cameras;
  seed-derived per-camera RTP sequence numbers; inspection counters.
- **Works:** an ONVIF® Device Service and Media Service control plane
  (*not ONVIF conformant*, see the notice in the [README](../README.md#status)) behind one HTTP listener, with HTTP
  Digest authentication when a camera's spec sets credentials.
- **Works:** a Go API for mutating a running fleet: `Fleet.AddCamera` starts a new camera (RTSP,
  and its ONVIF endpoint when the camera's spec sets credentials) without restarting the process,
  and `Fleet.RemoveCamera` stops and removes one. Both apply the same validation `Start` does, so a
  spec that would be refused at startup is refused here too.
- **Works:** screenshots: `Camera.Screenshot(ctx)` returns one decoded frame
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
- **Works:** Playwright end-to-end tests of the dashboard, behind the `e2e` build tag and run in
  the `e2e` CI job. Locally, `go test -tags e2e ./internal/dashboard/` needs Chromium installed
  for Playwright.
- **Not built yet:** PTZ, Imaging, and the *effect* of the
  one remaining catalogued fault, `accept_then_silence`. A spec that names an unimplemented fault is **refused**
  (`ErrUnsupported`), not silently ignored.
