# Host metrics (API only)

Status: design, awaiting review. Sub-project 2 of 3 of the dashboard work
(1: media library, merged; 3: dashboard redesign and UI, not yet specced).

## Intent

While the farm streams, the user needs to see how loaded the machine is, to tell
whether a pipeline problem comes from the farm, the pipeline or the host. This
sub-project adds host CPU, memory and GPU readings, plus camfarm's own process
usage, as an API. The panel that displays them belongs to sub-project 3.

Success: `GET /api/v1/metrics` and `event: metrics` on `/api/v1/events` return the
sample below on Linux, and a missing or broken `nvidia-smi` never degrades the
CPU, memory or process readings.

Decisions taken with the user: host plus camfarm's own process (not per-camera
cost); a background sampler that pushes over the existing event stream; the
"video-relevant" field set; hand-rolled `/proc` readers on Linux, with GPU
stats by shelling out to `nvidia-smi`.

## Non-goals

- The panel UI (sub-project 3).
- Server-side history or charts, per-core CPU, disk I/O, network throughput,
  pressure-stall information.
- Per-camera CPU attribution (not measurable inside one process).
- CPU and memory readings on non-Linux hosts. They report `unavailable`.
- Any claim about GPU behaviour on live hardware (see Testing).

## Package

`internal/hostmetrics`. It has no dependency on the fleet. It touches no journal,
clock or RTSP client, publishes nothing to `Fleet.publish`, and cannot change a
replay hash. It has its own small event bus, with the same drop-the-slow-client
behaviour as `internal/quality`.

## Sample shape

Every section is independent: a section that cannot be read is
`{"available": false, "reason": "..."}` and the others are unaffected.

```json
{
  "time": "2026-10-12T10:00:01Z",
  "cpu":     {"available": true, "usagePercent": 41.2, "logicalCPUs": 32,
              "load1": 3.1, "load5": 2.8, "load15": 2.2, "limitCPUs": null},
  "memory":  {"available": true, "totalBytes": 0, "usedBytes": 0, "availableBytes": 0,
              "swapTotalBytes": 0, "swapUsedBytes": 0, "limitBytes": null},
  "gpu":     {"available": false, "reason": "nvidia-smi failed: Driver/library version mismatch",
              "devices": []},
  "process": {"available": true, "cpuPercent": 6.3, "rssBytes": 0,
              "goroutines": 84, "openFDs": 212}
}
```

(Byte values above are placeholders for the shape, not example readings.)

- `cpu.usagePercent` is host-wide, from the delta of two `/proc/stat` reads.
  `process.cpuPercent` is the delta of `/proc/self/stat`. Both are `null` in the first
  sample after the sampler wakes (no baseline yet).
- `cpu.limitCPUs` and `memory.limitBytes` come from the cgroup (`cpu.max`,
  `memory.max`) when one is set, else `null`. Limits are read from the cgroup v2 root only:
  right inside a container with its own cgroup namespace, `null` otherwise and on cgroup v1. Usage stays host-wide; the UI can
  show the limit beside it.
- `gpu.devices[]`, one per GPU: `index`, `name`, `uuid`, `gpuUtilPercent`,
  `memUtilPercent`, `memUsedBytes`, `memTotalBytes`, `encoderUtilPercent`,
  `decoderUtilPercent`, `temperatureC`, `powerWatts`, `powerLimitWatts`.
  `gpuUtilPercent` is nvidia-smi's `utilization.gpu`: the share of time a kernel
  was running, not true SM occupancy (that needs DCGM). The API docs say so. A field
  reported as `[N/A]`, `N/A` or `Not Supported` is `null`, never 0.
- `reason` values: `nvidia-smi not found`, `nvidia-smi failed: <first stderr line,
  at most 200 characters>`, `timed out`, `unparseable output`, `nvidia-smi reported no devices`,
  `pending` (before the first GPU reading lands), `unsupported platform`. For the Linux
  CPU, memory and process sections the reason is the underlying read error text.

## Sources

- CPU, memory, process: `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`,
  `/proc/self/stat`, `/proc/self/statm`, `/proc/self/fd`, and `runtime.NumGoroutine()`.
  The `/proc` and cgroup roots are injectable. Non-Linux builds compile; a runtime `goos` check
  reports `unsupported platform` for these sections.
- GPU: one `nvidia-smi --query-gpu=index,name,uuid,utilization.gpu,utilization.memory,memory.used,memory.total,utilization.encoder,utilization.decoder,temperature.gpu,power.draw,power.limit --format=csv,noheader,nounits`
  call, 2 s timeout, output capped at 1 MiB, through an injectable runner
  (`func(ctx, name string, args ...string) ([]byte, error)`).

## Sampler

- One goroutine, 1 s tick. It is active while at least one event-stream client is
  subscribed or a `GET` happened in the last 10 s. Otherwise it idles: no reads and no
  `nvidia-smi` spawns.
- On waking it takes a baseline read; the first emitted sample has null percentages
  and the next is real. The latest sample is kept in memory.
- The GPU call runs in its own goroutine with its own timeout. A hung `nvidia-smi`
  cannot stall CPU, memory or process readings: the tick publishes them with the
  last GPU result.
- GPU backoff: after a failure the sampler retries at most every 15 s and repeats the
  last `reason` in between. `not found` is cached for the process lifetime.
- `Close()` stops the goroutine and ends subscriber channels.
- API: `Latest()`, `Touch()`, `Current(ctx)` (synchronous read), `Subscribe(ctx)`,
  `Close()`.

## HTTP

- `GET /api/v1/metrics`: marks the sampler wanted and returns the latest sample. If
  none is younger than 2 s it takes one synchronously, so a first paint is never
  empty. Read-only, so no `sameOrigin` guard. A nil `Options.Metrics` gives 404.
- `GET /api/v1/events?metrics=1` gains `event: metrics` with the full sample as `data`, merged
  alongside the fleet and quality channels. The push is opt-in: without `metrics=1` the
  handler does not subscribe to the sampler. A dropped or closed channel ends the
  stream; the client reconnects and re-lists.
- `cmd/camfarm` always creates the sampler (`defer Close()`). No flag: it costs nothing
  while unwatched.

## Errors

- A failing section never fails the request: status 200, that section unavailable.
- `/proc` parse failures mark the section unavailable with the reason; no panic.

## Security and replay notes (README)

- The endpoint exposes host details (GPU names, memory sizes, process counts). The
  dashboard has no authentication, as before; it is not protected against DNS
  rebinding.
- Metrics do not enter the journal, the fleet feed or the replay hash.

## Testing

- `/proc` fixtures inline in the tests (no `testdata/`), including a container-style cgroup and a
  malformed file.
- Captured `nvidia-smi` CSV: one GPU, two GPUs, `[N/A]` and `Not Supported` fields. For
  failures: a missing binary, a nonzero exit with the real mismatch message seen on
  the development machine, a hang, and garbage output.
- Sampler tests with a fake ticker: wake and idle, baseline then real percentages,
  GPU backoff, a slow GPU not stalling the other sections, `Close`.
- Handler and SSE tests, including the 404 without a sampler.
- One real-process test reading this machine's `/proc`, accepting either GPU outcome.
- The README states that the GPU parser is verified against captured `nvidia-smi`
  output, not live hardware. No GPU figure is claimed beyond what CI produced.

## Changes after review

The shipped code differs from the text above in these ways:

- `Sample(ctx)` is `Current(ctx)`.
- Non-Linux handling is a runtime `goos` check, not build tags; everything still compiles.
- Fixtures are inline in the tests, not under `testdata/`.
- The `reason` list gains `pending` and `nvidia-smi reported no devices`; for the Linux CPU,
  memory and process sections the reason is the underlying read error text.
- The event-stream push is opt-in via `?metrics=1`; without it the sampler is not subscribed to.
- Cgroup limits come from the cgroup v2 root only.
- GPU values printed as bracketed errors (`[Unknown Error]`, `[Insufficient Permissions]`) and
  non-finite values are `null`; degenerate `cpu.max` and non-finite `loadavg` are rejected.
- A sample older than the stored latest is never stored or published.
