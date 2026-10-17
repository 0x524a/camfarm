# Host metrics

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
