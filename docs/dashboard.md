# Dashboard and `cmd/camfarm`

Screenshots are in the [README](../README.md#the-dashboard). Related pages:
[media library](library.md), [stream quality test](stream-quality.md), [host metrics](host-metrics.md)
and [faults](faults.md).

- **Works:** a web dashboard and `cmd/camfarm` binary. `go run ./cmd/camfarm --cameras 3` (or the
  built `camfarm` binary) serves a fleet and a page at `http://127.0.0.1:8080/` that shows each
  camera as a card with a snapshot thumbnail and copyable RTSP and ONVIF URLs, adds and removes
  cameras, and opens a fault drawer to inject and clear faults. It also has a host metrics strip,
  a library tab (validation findings, uploads and shared paths) and a per-camera stream quality
  test. Changes appear live over Server-Sent Events. The JSON API is under
  `/api/v1/` (`fleet`, `cameras`, `cameras/{id}`, `cameras/{id}/snapshot.jpg`,
  `cameras/{id}/faults`, `cameras/{id}/quality`, and the `events` stream). Seeds are decimal strings in the API. Flags:
  `--spec`, `--seed`, `--cameras` (mutually exclusive with `--spec`), `--http-addr`,
  `--listen-host`, `--advertise-host`, `--rtsp-port`, `--onvif-port`, `--discovery`, `--discovery-interface`, `--media-dir`, `--allow-ui-paths`, `--max-upload`; run with `-h` for details. **Security:** the
  dashboard can start servers and, with file sources, make the process read local paths, and the
  page shows cameras' Digest credentials. It listens on loopback by default and has *no*
  authentication, token or Host-header check. Only the library's mutating endpoints (upload, scan,
  delete, roots) and starting or cancelling a stream quality test refuse cross-site browser requests (an `Origin` that differs from `Host`, or
  `Sec-Fetch-Site: cross-site`); every other endpoint accepts them, so a malicious web page can
  reach those through CSRF. No endpoint, the library included, is protected against DNS
  rebinding: under rebinding the `Origin` equals `Host`, so the check passes. Do not expose it.
