# Stream quality test

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
aim it at another host. The dashboard runs it from a button on each camera card. Like the rest of
the dashboard it has no authentication, and its metrics strip exposes host details.

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
