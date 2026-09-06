# camfarm architecture design

Date: 2026-09-05
Status: design approved, implementation not started

`camfarm` is a working name. It is unverified for availability or collision and must be settled
before any public push. One constraint now applies to the choice: see
[Naming and public claims](#11-naming-and-public-claims).

---

## 1. What this project is

A synthetic camera farm, written in Go, for testing video-analytics pipelines without physical
hardware. It serves N fake RTSP streams and answers the ONVIF® control-plane requests that
camera-discovery code uses to find and drive a fleet. Its distinguishing feature is **deterministic
fault injection**: a fault fires because a seed said it would, at a reproducible point in the
stream, and the same seed and fleet spec reproduce it exactly.

The common improvisation for this need is `mediamtx` plus ffmpeg loops. That produces streams but
offers neither fault injection nor a control plane, and a large share of real bugs live in those two
places.

### 1.1 Why fault injection is the product

Cameras in the field misdescribe their capabilities, drop frames, starve keyframes, present odd auth
challenges, and tear down mid-session. A rig that cannot reproduce those conditions cannot catch the
bugs they cause.

The sharpest available argument for this project came from a sibling repository working around a
defect in the virtual camera it uses as a test fixture. That camera advertises an events endpoint it
never registers, so the consumer disabled event support in its harness with this comment:

> Left false deliberately: the virtual camera advertises an events endpoint it never registers,
> which would mislead tests.

**An accidental fault is worse than none, because it misleads.** A farm that injects that same fault
*on request*, and only on request, is the useful version. That is this project's thesis in one line.

---

### 1.2 Where each open decision is settled

`CLAUDE.md` lists nine open architectural decisions. All nine are resolved below. They are ordered
here by dependency rather than by number, since decision 1 gates the ONVIF side and the reuse
strategy shapes everything after it.

| `CLAUDE.md` decision | Settled in | Outcome |
|---|---|---|
| 1. Reuse strategy for `onvif-go/server` | [§3](#3-decision-1--reuse-strategy-for-onvif-goserver) | Depend, and quarantine PTZ |
| 2. Scope: RTSP only, or plus control plane | [§4](#4-decision-2--scope) | Both, in v1; happy path first |
| 3. Where video comes from, and CGO | [§5](#5-decision-3--where-video-comes-from), [§10.0](#100-dependencies-and-build) | Pluggable `MediaSource`, pure Go, `CGO_ENABLED=0` |
| 4. Fault-injection catalogue | [§7](#7-decision-4--the-fault-catalogue) | Catalogued by seam; six in phase 2 |
| 5. Determinism and reproducibility | [§8](#8-decision-5--determinism-mechanics) | Decisions keyed on seed and event counter, never wall clock |
| 6. Scale model | [§10.2](#102-decision-6--scale-model) | One process, shared parsed media; ceiling measured |
| 7. Discovery | [§10.5](#105-decision-7--discovery) | Responder plus non-multicast fallback |
| 8. Configuration surface | [§10.6](#106-decision-8--configuration-surface) | Declarative spec and runtime API, one type |
| 9. Library or binary | [§6](#6-decision-9--library-and-binary) | Library first; binary, API, UI layered on it |

## 2. Evidence base

Every decision below rests on reading the actual code and, where behaviour was in question, running
it. Four parallel investigations covered `onvif-go/server/`, `rtspeek`, `onvif-go/discovery/` plus
the sibling projects, and the current state of `gortsplib` and the ONVIF specifications. Findings
that contradicted prior assumptions are recorded as such rather than quietly dropped.

### 2.1 What `onvif-go/server/` actually contains

2,880 lines of production code and 3,512 lines of tests, covering Device, Media, PTZ and Imaging.
Twenty-two exported types in `types.go`, and nineteen `Handle*` methods across the four services.

**It is not a working ONVIF device today.** The SOAP body is declared `Content interface{}` with
`xml:",omitempty"` (`internal/soap/soap.go:31`). `encoding/xml` cannot unmarshal into an
`interface{}` field, so after `xml.Unmarshal` the body is always `nil`. Dispatch then passes that nil
to the handler (`server/soap/handler.go:94`), and every handler needing a parameter fails with `EOF`.

Driven over `httptest` with real SOAP, the split is exact:

- **Eight operations work** — the ones taking no request parameters: `GetDeviceInformation`,
  `GetCapabilities`, `GetSystemDateAndTime`, `GetServices`, `SystemReboot`, `GetProfiles`,
  `GetVideoSources`, `GetOptions`.
- **Twelve return a SOAP fault** — `GetStreamURI`, `GetSnapshotURI`, all seven PTZ operations,
  `GetImagingSettings`, `SetImagingSettings`, `Move`.

`AbsoluteMove` called directly in Go sets pan to 42; the same call over HTTP leaves it at 0.

This survives because **no test in that repository runs a client against the server**. All 3,512
lines of server tests call `Handle*` directly with bytes the broken layer never produces. CI runs
`go test -race ./...` and is green, because the one untested seam is the broken one.

### 2.2 The seam that makes reuse viable

`MessageHandler` is `func(body interface{}) (interface{}, error)`, and `unmarshalBody`
(`server/media.go:372-391`) opens with a first-class `[]byte` path:

```go
if b, ok := body.([]byte); ok {
    bodyXML = b
} else {
    bodyXML, err = xml.Marshal(body)
}
```

That path is present in all fourteen body-consuming handlers. The broken envelope is reached only
through `soap.Handler.ServeHTTP`, which a caller owning its own transport never invokes.

Verified against **unmodified** upstream: a custom envelope with `xml:",innerxml"`, a custom
dispatcher, and raw `[]byte` handed to the exported handlers brings **all twelve previously-faulting
operations to 200 OK**, mutates state through the wire, injects a capability lie through the same
seam, and runs **25 cameras with distinct identities behind one listener**.

### 2.3 Determinism hazards in that package

No `math/rand`, no `crypto/rand`, no `init()`. Two problems instead:

- **Package-global mutexes** (`ptz.go:201`, `imaging.go:207`). State is per-`Server`, but the locks
  are package-level, so every camera in a process serializes PTZ and imaging through one mutex.
- **Three untracked, uncancellable goroutines with hardcoded sleeps** — 500ms in `AbsoluteMove` and
  `RelativeMove`, 1s in `GotoPreset` (`ptz.go:270`, `:319`, `:501`). They *are* the PTZ motion
  model; `Moving` flags are cleared nowhere else. Plus `time.Now()` at nine sites including
  `UTCTime` in every `GetStatus` response, making responses non-byte-reproducible.

An interception hook can rewrite response bytes but cannot reach a goroutine already mutating state
behind a package-global mutex. **Seeded determinism for PTZ is unreachable without rewriting all 533
lines of `ptz.go`.**

Also relevant: `UpdateStreamURI` (`server.go:287`) writes the stream map with no mutex while
`media.go:273` reads it — a live data race on exactly the mid-test mutation a fleet orchestrator
needs.

### 2.4 Auth in that package

SHA-1 WS-Security `UsernameToken` only, compared with `==` rather than in constant time, and skipped
entirely when username or password is empty. Measured: a replayed nonce returns 200 (no replay
cache), and a `Created` timestamp of 1999 returns 200 (no skew check).

There is **no realm concept anywhere**, no `PasswordText` branch, no HTTP Basic or Digest path, and
`WWW-Authenticate` is never emitted. Realm and Basic faults are unreachable without owning the auth
layer.

### 2.5 `testdata/` is useful, with a limit

Eight real cameras (3 AXIS, 3 Bosch, 2 Reolink), 16 files, 180 KB, paired request and response XML.
The responses carry precisely the texture hand-written Go structs will not invent: `BitrateLimit:
2147483647`, 8192×1728, `UseCount: 3`, multicast at `0.0.0.0` port 0 TTL 5, an
`<Extension><Rotate>`.

Two limits. Twelve operations, all read-only — no PTZ moves, no `SetImagingSettings`, no auth
challenges, no failure responses. And the XML is not byte-faithful: namespace declarations were
round-tripped into pseudo-attributes (`_xmlns:tt="…"`) rather than real `xmlns:` declarations, so it
needs normalising before being emitted. Useful as golden fixtures for realism; not a specification.

### 2.6 Media prior art: there is none

No `testdata/`, no `.h264`/`.ts`/`.mp4`, no ffmpeg invocation anywhere in `rtspeek`. Its SPS is a
4-byte **stub** — plausible header (`0x67` = SPS, profile_idc 66, level 3.1) but truncated far short
of decodable, so `h264.SPS.Unmarshal` fails and resolution is silently omitted rather than asserted.

`rtspeek` is also a shallow client: **OPTIONS and DESCRIBE only, never SETUP or PLAY**. It verifies
reachability, SDP parseability and codec classification, and nothing about RTP, transport or
teardown. It is a useful smoke consumer, not a test oracle. camfarm's own tests need a real
`gortsplib` client driving SETUP and PLAY, with `ffprobe` as an external cross-check.

Concrete requirements it does pin down: TCP accept on the advertised host:port (there is a preflight
`net.Dial` before any protocol, and **port defaults to 554** when absent, so nonstandard ports must
appear in the advertised URI); at least one media, H.264 or H.265 for video (**MJPEG video is
explicitly rejected**); an SPS long enough to unmarshal if resolution should be reported; one
DESCRIBE retry on a Digest 401, only when credentials are embedded in the URL.

### 2.7 `gortsplib` v4 is a dead branch

`v4.16.3` is a deliberate tombstone. The module contains three files; `gortsplib.go` is one line —
`package gortsplib` — and the README reads, in full: *"This version of gortsplib is deprecated.
Switch to the latest one."* Anyone on the v4 line who runs `go get -u` gets an empty package and a
compile failure.

Latest is **`gortsplib/v5 v5.6.5`**. There is no v6. Its own `go.mod` declares `go 1.26.0` — not
merely "≥ 1.25" as first estimated, so camfarm's `go` directive tracks it at `1.26.0`.

**camfarm starts on v5.** Note `NewServerStream` is gone: v5 constructs `&ServerStream{Server, Desc}`
then calls `Initialize()`.

### 2.8 Where WS-Discovery stands

`onvif-go/discovery/` is a probe-**sending** client, confirmed by reading it: one write, and it is a
Probe (`discovery.go:136-139`); the read loop only parses; it discards the source address
(`discovery.go:152`), so it could not reply even if asked. No `Hello`, no `Bye`, no `ProbeMatch`
construction, no responder anywhere on the account.

Reusable: `resolveNetworkInterface` and `ListNetworkInterfaces`, the latter already reporting `Up`
and `Multicast` flags — exactly the check a responder needs before binding. The `ProbeMatch` structs
are unmarshal-shaped and carry no namespace prefixes.

Written new: envelope and header construction, `RelatesTo` correlation, `AppSequence`, inbound
`Probe` parsing with Types and Scopes match rules, unicast reply to the probe source, and the
bounded random reply delay.

Three findings that became determinism requirements:

- `generateUUID` (`discovery.go:229-240`) is **not a UUID** — `fmt.Sprintf("%d-%d-%d-%d-%d", nanos,
  secs, …)`. A device's `EndpointReference/Address` must be a **stable** `urn:uuid` per camera,
  because clients dedup on it; an unstable one makes one camera look like many. Derive from seed and
  camera index.
- `AppSequence.InstanceId` conventionally derives from boot time. It must come from the seed, or
  replaying a run yields different sequence values.
- The bounded random reply delay must be seeded per camera.

A warning: this client **never checks `RelatesTo`**. A responder tested only against it would pass
with a blank `RelatesTo` and a malformed UUID. Verifying correctness requires an external client or
hand-written assertions.

Also: `server/` has **no `Scopes` concept at all**. Per-camera scopes are net-new state camfarm owns.

### 2.9 What the sibling projects actually expect

Both `framelag` and `onvif-mcp` are documentation only — no Go, no `go.mod`.

**`framelag` assumes nothing** about camfarm's API, and says so: *"Do not assume its API; it does not
exist yet."* It leaves one boundary open, quoted from its own decision 7:

> **Network impairment.** Whether the tool itself measures under induced loss, jitter, and bandwidth
> limits (`tc`/`netem`), or whether impairment belongs entirely in `camfarm` and `framelag` only
> measures whatever conditions it is handed. Argue the boundary rather than assuming it.

Answered in [§7.4](#74-the-framelag-boundary).

**`onvif-mcp` does not plan to use camfarm at all.** `grep -ri camfarm` across its 681-line design
doc and 4,531-line plan returns zero hits. Its no-hardware strategy is three hermetic tiers going
directly to `onvif-go/server` plus hand-authored `httptest` fault fixtures.

**This means camfarm's own `CLAUDE.md` currently overstates the relationship** — it says camfarm "is
how that project gets integration-tested without physical hardware," which is not that project's
plan. camfarm is a *candidate* consumer, not its current test strategy. That sentence should be
corrected.

The larger finding is that `onvif-mcp` specified camfarm's library API without knowing it existed.
It hand-rolls a wrapper because the virtual camera is awkward to drive from a test, and marks it for
deletion:

> It exists to work around upstream `onvif-go` issue #63: `server.Start` binds a fixed port with no
> ephemeral-port option, blocks without a readiness signal, and prints banners to stdout
> unconditionally. … **Once that issue is resolved this package should be deleted.**

Its wrapper API — `Start(t *testing.T) *Cam`, `Endpoint()`, `Username()`, `Password()`,
`ProfileToken()`, `PresetToken()`, `Server()` — is the shape [§9](#9-public-api-surface) adopts.
**`camfarm.StartT(t)` should be able to delete that package outright**, which is a falsifiable
success criterion for the API.

Six further requirements fall out of its workarounds:

| Requirement | Origin |
|---|---|
| Silent by default, never stdout | Named three times; its `make demo` runs the camera as a separate process because "stdout banners would corrupt the stdio JSON-RPC stream" |
| `Port: 0` with address **read-back** | It hand-rolls `freePort` with a documented race, "unavoidable until issue #63 allows Port 0 with address read-back" — **but `gortsplib.Server.NetListener()` already exists and returns the live listener**, so camfarm needs no such workaround: it panics if called before a successful `Start()`, so callers read it back after, not instead of, binding |
| First-class inspection API | It reaches for `Server() *server.Server`, a leaked pointer, purely to assert PTZ state. "The decisive capability is `GetPTZState`: a test can assert that a dry-run left the camera **unmoved**" |
| Faults recoverable in-process | "**per-entry mutex, not `sync.Once`.** `sync.Once` … cannot retry, so a camera that was down at startup would remain permanently unusable until the process restarted" |
| Stable public spec type | "If `DefaultConfig` field names differ from those above, correct" — an API it cannot rely on |
| Errors distinguishing unsupported from uninitialised | Upstream #64 conflates them, forcing it to derive confidence elsewhere |

`grep t.Skip` across both its documents returns zero hits. It engineered around every gap rather
than skipping tests. That is the bar to match.

### 2.10 ONVIF profiles, current state

**Profile S is deprecated.** Last conformance submission 2027-03-31, because it mandates
WS-UsernameToken, which ONVIF states is "no longer consistent with current cybersecurity
recommendations." **Profile T is the target; S is legacy.** In ONVIF's specification language,
"Conditional" means *shall implement if supported in any way, including proprietarily* — not
optional.

Two facts that contradict common assumption: in Profile S, **H.264 is conditional and MJPEG over
RTSP is mandatory**. In Profile T, HTTP Digest is mandatory and WS-UsernameToken is absent entirely.

| | Profile S | Profile T |
|---|---|---|
| Auth | WS-UsernameToken mandatory | **HTTP Digest mandatory**, WS-UsernameToken absent |
| Media service | Media | **Media2** |
| Capabilities | `GetCapabilities` | `GetServices` + `GetServiceCapabilities`; no `GetCapabilities` |
| Events | WS-BaseNotification **and** PullPoint | **PullPoint only**, `MaxPullPoints` ≥ 2 |
| Video codec | MJPEG mandatory, H.264 conditional | **H.264 or H.265**, at least one |
| Metadata | configuration only | **streaming mandatory**, including multicast |
| Configuration | ~20 typed operations | generic `AddConfiguration` / `RemoveConfiguration` |
| Reboot | `Reboot` | `SystemReboot` |

**Consequence: `onvif-go/server` is aligned with the deprecated profile.** It implements
WS-UsernameToken and no HTTP Digest — exactly what Profile T drops and replaces. This does not
change the reuse decision, but it moves camfarm's own auth layer from optional polish onto the
critical path.

**Events are mandatory in both profiles and implemented in neither.** That gap cost `onvif-mcp` a
shipped feature: *"the virtual camera used for testing implements no events service, so it cannot be
covered without hardware."* A real consumer is blocked on a capability camfarm can provide.

---

## 3. Decision 1 — reuse strategy for `onvif-go/server`

**Decision: depend, and quarantine PTZ.**

- **Depend** on `onvif-go/server` for the Device, Media and Imaging handlers plus roughly 1,900
  lines of response structs.
- **Own the transport**: envelope, action extraction, dispatch, auth, faults, namespaces, per-camera
  identity behind one listener.
- **Implement PTZ natively** in camfarm against a seeded clock, never importing `ptz.go`.
- **Do not modify the upstream repository.** The design targets unmodified upstream, which
  §2.2 proves works. The upstream defects are recorded separately as a list; acting on them is out
  of scope for this project.

**Why depend.** The `[]byte` seam is a real, load-bearing, tested input path rather than an accident
being exploited, and it delivers both things extraction was supposed to buy: full transport control,
and a per-operation interception point for fault injection — both proven against unmodified
upstream. Reimplementing duplicates 1,900 lines of working response types for no benefit.

**Why quarantine PTZ.** §2.3: seeded determinism there is unreachable without rewriting the file, and
`ptz.go` is the only file with that problem. Quarantine is cheaper and more honest than forking the
package to fix one file.

**Two things to stay honest about.** First, this imports response *types* and state logic, not a
working ONVIF server — the transport is real work owned under every option, so it is not a cost of
this choice. Second, because upstream CI is green on a path now known to be broken, camfarm needs its
own integration tests over **every** imported handler to detect drift. If those tests ever cost more
to maintain than the 1,900 lines they guard, revisit. That is a future trigger, not today's decision.

---

## 4. Decision 2 — scope

**Decision: N cameras, both RTSP and the ONVIF control plane, in the first usable version. Happy
path first; faults are phase two.**

The control plane is the entire differentiator against `mediamtx` plus ffmpeg loops, so deferring it
would defer the reason the project exists.

**Constraint on the phasing:** the seed plumbing and the injection seams are part of the v1
architecture even though every fault is a no-op in v1. Determinism is not retrofittable, and a seam
added later would not be reachable from the code paths built without it.

---

## 5. Decision 3 — where video comes from

**Decision: a pluggable `MediaSource` interface. Pure Go. `CGO_ENABLED=0` holds.**

Implementations for the first version:

1. **Local file** — parsed to access units, looped.
2. **Upstream RTSP pull** — pulled and republished as a camera feed. This requires a `gortsplib`
   *client* as well as a server.

The interface is the deliverable; further sources are additive.

**No live encoding, and MJPEG would not have helped.** Pure-Go H.264 encoding is the hard part, so a
synthesise-and-encode path would have landed on MJPEG — which `rtspeek` explicitly rejects as a video
format (§2.6), making it useless to the clients this farm exists to test.

**`mediacommon/v2` has no Annex-B stream reader.** It ships `fmp4`, `mp4`, `mpegts` and `pmp4` under
`pkg/formats/`, and nothing for raw Annex-B. That absence is why the bundled fixture is packaged as
MPEG-TS rather than a raw `.h264` stream — there is a reader for the former and none for the latter.
`h264.AnnexB.Unmarshal` exists for the small case where Annex-B bytes are already in hand (RTP
packetization from access units already extracted from MPEG-TS), but it aliases its input: the final
line of its implementation is `(*a)[i] = buf[positions[i].start:positions[i].end]`, a slice of the
caller's buffer, not a copy. `copyNALUs` (`internal/media/mpegts.go:112`) copies each NALU out of
that buffer before handing it to the pump — deliberate insurance, not a fix for a hazard that is live
today. At the pinned `mediacommon`/`go-astits` versions each PES payload already arrives through a
fresh per-call allocation, so nothing currently aliases a reused buffer; the copy guards against a
future dependency upgrade that starts reusing one, since `mediacommon` still ships
`NextBytesNoCopy` and its own (now-deprecated) `mpegts/buffered_reader.go` shows it once had a
buffer-reusing reader on this same path. Because the parsed `*Media` is shared read-only across every
camera in the fleet, that failure mode would be silent corruption rather than a crash — worth one
extra allocation per NALU at load time to rule out in advance.

**One committed fixture is required, not optional.** §2.6 establishes there is no media anywhere on
the account to inherit. Without a bundled default, camfarm's own tests and every consumer's CI need
media sourced before any test runs, which contradicts the project's purpose. Scope: a few hundred
kilobytes, low resolution, a few seconds, clearly licensed, used only as the default and self-test
source.

---

## 6. Decision 9 — library and binary

**Decision: library first, binary and control API on top, UI last.** All three drive the same core;
nothing is reachable from only one of them.

Ordering matters: the Go API is the contract, the HTTP control API is a client of it, and the UI is a
client of that. A capability reachable only through the UI would make this a demo rather than a test
tool.

---

## 7. Decision 4 — the fault catalogue

Every entry names the class of field bug it reproduces. Candidates without such a story are excluded.

### 7.1 Media path — the RTP writer, seeded

| Fault | Field bug class |
|---|---|
| Keyframe starvation | A pipeline joins mid-stream, never receives an IDR, shows black or garbage until restarted. The "works on camera restart, breaks on reconnect" bug. |
| Frame drop at a set rate | Decoders and trackers assuming contiguous frames. Real cameras drop under CPU and network load. |
| Timestamp discontinuity | Naive `dt` computation yields a negative or enormous delta — divide-by-zero, reordering, latency spikes. Real cause: clock resync or an NTP step. |
| Mid-stream resolution or bitrate change | Decoders allocating buffers once from the first SPS; downstream stages assuming constant geometry. Real cause: adaptive-bitrate cameras. |
| Sender-side jitter and burst delay | Fixed-size jitter buffers overflowing; latency measurement assuming smooth arrival. |

### 7.2 Control-plane lies

| Fault | Field bug class | Reachability |
|---|---|---|
| SDP misdescribes the codec (advertise H.265, send H.264) | Pipeline selects its decoder from SDP, gets the wrong one, produces garbage. Real firmware bug. | No hook needed — the write path indexes only by payload type and never validates payload shape against the declared format |
| SPS claims a resolution the frames do not match | Buffers sized from SDP then overflow or crop. Self-consistent SDP, mismatched pixels — what a real lying camera looks like. | No hook needed — `format.H264.FMTP()` derives `sprop-parameter-sets` and `profile-level-id` purely from the bytes supplied |
| ONVIF advertises framerate or bitrate never delivered | Capacity planning from encoder configuration; false "underperforming stream" alarms. | Control plane only — neither value appears in SDP, so this fault requires both halves running |
| Advertised-but-dead endpoint | Client trusts the capability list, then hangs or errors on first use. §1.1. | Control plane |

### 7.3 Session, transport and auth

All reachable. The root cause is one hook: `gortsplib` sets `Content-Base`, `Content-Type`, `CSeq`,
`Server`, `RTP-Info`, `Transport` and the SDP body *after* the handler returns, so handler return
values lose. But `OnResponse(sc, res)` receives the same live `*base.Response` pointer after all of
that and immediately before the write, and `WriteResponse` is a bare `res.Marshal()`. **Implementing
`ServerHandlerOnResponse` makes every header-level lie reachable** without owning the wire.

| Fault | Field bug class | Mechanism |
|---|---|---|
| Malformed `Content-Base` | SETUP URL resolution breaks. `gortsplib`'s own client has three hard error paths for this, so it genuinely breaks real clients. | `OnResponse` — the server overwrites it unconditionally on every 200 DESCRIBE, so overwrite it back at write time |
| Wrong or omitted `RTP-Info` | Initial sequence and timestamp synchronisation. | `OnResponse`; `delete` for omission |
| Teardown mid-session | Reconnect logic, resource leaks, fleet-wide thundering-herd reconnects. | `ServerSession.Close()` from any handler ctx |
| TCP reset | Clients that cannot distinguish a clean close from an abort; retry backoff. | `ServerConn.NetConn()`, assert `*net.TCPConn`, `SetLinger(0)`, `Close()` |
| Accept-then-silence | Dial succeeds, protocol never progresses. **Already client-verified in `rtspeek`.** | Handler withholds a response |
| Close-after-OPTIONS | Mid-handshake disconnect. **Already client-verified in `rtspeek`.** | Handler closes the conn |
| Transport refusal (force TCP interleaved, refuse UDP) | Clients that do not fall back when their preferred transport is rejected. | SETUP response |
| Basic when Digest expected | Clients that refuse to downgrade, or silently leak credentials. | `Server.AuthMethods`, exported |
| Wrong realm, stale nonce | Credential caches keyed on realm; clients that loop or fail permanently. | `OnResponse` rewriting `WWW-Authenticate`, or returning a hand-built 401 and never calling `VerifyCredentials` — **correction:** `Server.AuthMethods` is exported and real, but there is no configurable `AuthRealm` anywhere in gortsplib v5.6.5; the realm is the hardcoded constant `serverAuthRealm = "ipcam"` (`server.go:19`), so this fault is reachable only through the `OnResponse` rewrite, never through a library option |

### 7.4 The framelag boundary

**camfarm owns sender-side impairment. `tc`/`netem` owns path impairment.**

What camfarm chooses not to send, or sends late, is a decision it makes — therefore seeded,
reproducible, and requiring no privileges. That covers frame drop, jitter, burst delay and
starvation. Packet loss on the wire, bandwidth ceilings and reordering are properties of the path:
`netem` needs `CAP_NET_ADMIN`, breaks the CI story, and is not reproducible from a seed.

Neither project needs the other's mechanism.

### 7.5 Discovery faults

| Fault | Field bug class |
|---|---|
| No reply spread — N cameras answer one probe simultaneously | Client-side probe storms and dropped `ProbeMatches`. The realistic fleet-scale stress case. |
| `AppSequence` never increments, or `InstanceId` resets mid-session | Client cannot distinguish a reboot from a re-announce, producing duplicate camera entries. |
| Unstable `EndpointReference` UUID | One camera appears as many, because clients dedup on it. |

### 7.6 Fenced to a later tier

Byte-level corruption — truncated messages, illegal framing, and a bogus `Content-Length` on a
bodyless response. `Marshal` always emits well-formed RTSP and force-recomputes `Content-Length`
whenever a body is present, so these need a mangling `net.Conn` injected via `Server.Listen`.

That tool sits **below** RTSP framing, so it is blind to message boundaries and will corrupt
interleaved RTP on the same socket. It needs its own design pass and must not be folded into the
response-level faults. The bug class is real — parsers that mis-frame on partial reads — but it is
second-tier.

### 7.7 Phasing

**v1 ships the seams with every fault a no-op**: the seed threaded through, `OnResponse` hooked, the
frame-path interceptor present, the inspection API reporting zero faults fired.

**Phase 2 lands six**, chosen for being real, reachable without the later tier, and cheapest to
verify: keyframe starvation, frame drop, teardown mid-session, accept-then-silence, the SDP codec
lie, and the SPS resolution lie. Two already have client-side tests written by `rtspeek`.

---

## 8. Decision 5 — determinism mechanics

**The core rule: every fault decision is a pure function of `(seed, camera index, event counter)`.**
Never wall-clock time, never goroutine scheduling order, never map iteration order.

Per-camera seeds derive from a **stable camera index assigned at spec-parse time**, not from
position in a map: `camSeed = splitmix64(rootSeed, index)`.

Seed streams split per concern, so adding a fault type does not shift another's sequence:

```
rootSeed ──▶ camSeed[i] ──▶ mediaSeed, faultSeed, discoverySeed, authSeed
```

Each camera holds its own generator. Nothing shared: no cross-camera coupling, no lock contention on
randomness.

### 8.1 What determinism does and does not mean

Serving a real client cannot be fast-forwarded; pacing must be real time. So determinism does **not**
mean identical wall-clock timings. It means **the decision sequence is identical**, because decisions
key on frame index and event counters rather than elapsed time.

The `Clock` interface exists so tests run the pump in virtual time — fast and fully deterministic —
while a live fleet runs in real time and makes the same decisions in the same order.

This is also precisely why `onvif-go`'s PTZ is quarantined: its sleep goroutines key motion
completion on wall time, the pattern this rule forbids.

### 8.2 Replay workflow

Every fault firing records `{seed, cameraID, eventCounter, faultKind}` through the inspection API. A
failing test prints one line:

```
camfarm: seed=0x3f2a9c81 spec=sha256:1b9e…  (replay: camfarm.Replay(seed, specPath))
```

The spec is hashed and the hash logged, so a replay **verifies** it is the same fleet rather than
assuming it. Replaying a different spec under the same seed is an error, not a silent divergence.

### 8.3 Two carve-outs, stated in the README

1. **Correction, from running code.** `rtph264.Encoder.SSRC` and `InitialSequenceNumber` *are*
   exported and settable — the prediction that they were not was wrong. Both halves below are now
   **confirmed** by implementation rather than predicted:
   - **Sequence number is reproducible.** The pump sets `InitialSequenceNumber` from the per-camera
     seed, and `ServerStream`'s write path never rewrites `pkt.SequenceNumber`; it only reads the
     value back afterward, to compute the `RTP-Info` header (`server_stream_format.go:173`). The
     seed-derived initial sequence number survives to the client unchanged.
   - **SSRC is not reproducible.** `ServerStream` overwrites it unconditionally on every write —
     `pkt.SSRC = ssf.localSSRC` at `server_stream_format.go:103` — regardless of what the encoder
     set. SSRC is therefore excluded from replay assertions; the pump sets it from the seed anyway,
     so a writer that does not overwrite it (including the test recorder) still behaves
     deterministically.
   - There is also no `InitialTimestamp` field on the encoder, so per-packet timestamp assignment
     (as the pump already does, via `pkt.Timestamp = ts`) is the only lever over timestamps.
2. **Live upstream-RTSP sources are outside the guarantee.** Their bytes differ run to run. Faults
   over such a source remain seeded in their *decisions*, but the media is not reproducible. The
   fault engine warns when a non-deterministic source is combined with faults.

---

## 9. Public API surface

```go
func Start(ctx context.Context, spec Spec) (*Fleet, error)
func StartT(t *testing.T, spec Spec) *Fleet   // t.Cleanup, ephemeral ports, silent

type Spec struct {
    Seed    uint64
    Cameras []CameraSpec
    Listen  ListenSpec    // Port 0 by default; read back via Fleet.Addr()
    Log     *slog.Logger  // nil = silent; never stdout
}

type CameraSpec struct {
    ID     string        // operator-chosen: "front-door", "lobby-ptz"
    Source SourceSpec    // file path | upstream RTSP URL | extensible
    Video  VideoSpec     // advertised codec, resolution, framerate, bitrate
    Auth   AuthSpec
    Faults []FaultSpec   // v1: parsed and validated, no-op
}

func (*Fleet) Camera(id string) (*Camera, error)  // ErrUnknownCamera
func (*Fleet) List() []Status
func (*Fleet) Addr() Addrs                        // bound ports, read back
func (*Fleet) Close() error

func (*Camera) RTSPURL() string
func (*Camera) ONVIFEndpoint() string
func (*Camera) Credentials() (user, pass string)
func (*Camera) ProfileToken() string              // guaranteed PTZ-capable
func (*Camera) PresetToken() string               // guaranteed to exist
func (*Camera) Stats() Stats                      // frames, faults fired, teardowns
func (*Camera) Inject(FaultSpec) error            // mid-test mutation
func (*Camera) Clear() error

// Replay reconstructs a fleet from a recorded seed and spec, verifying the spec
// hash matches the one the seed was recorded against. See §8.2.
func Replay(ctx context.Context, seed uint64, specPath string) (*Fleet, error)
```

Every element traces to a stated need in §2.9. Operator-chosen IDs and `ErrUnknownCamera` mirror the
consumer's own registry. `Port: 0` with read-back, readiness and silence are its issue-#63
workarounds — read-back is `gortsplib.Server.NetListener()`, which already exists (§2.9); no
workaround was needed. The `ProfileToken` and `PresetToken` guarantees replace configuration it
mutates by hand. `Stats()` replaces the leaked `*server.Server` pointer with a first-class,
seed-correlated read. **Correction:** `Stats()` cannot forward a reader count from `ServerStream`,
which exposes none; the implemented `Stats.Readers` is instead tracked by the RTSP handler itself, on
every `SETUP`/session-close, and read back under the same lock as the rest of its state.

**Error taxonomy**, kept distinct because conflation is a known consumer pain (upstream #64):

| Error | Meaning |
|---|---|
| `ErrUnknownCamera` | No camera with that ID |
| `ErrUnsupported` | This build cannot do that |
| `ErrNotReady` | Not initialised yet |
| `ErrFaultActive` | Refused *because* a fault is deliberately in effect |

The last must never read as a malfunction. "This camera is lying on purpose" and "this camera is
broken" being indistinguishable would make the tool useless.

**Recoverability.** A camera knocked down by a teardown fault comes back within the same process,
via a per-camera mutex, never `sync.Once` (§2.9).

**Stability contract.** `Spec`, `Fleet`, `Camera`, `Stats` and the error values are stable.
Everything under `internal/` may change without a major version.

---

## 10. Architecture

### 10.0 Dependencies and build

| | |
|---|---|
| RTSP | `github.com/bluenviron/gortsplib/v5` — v4 is a tombstoned dead branch (§2.7) |
| Media formats | `github.com/bluenviron/mediacommon/v2` — NAL handling, SPS parsing |
| ONVIF handlers | `github.com/0x524a/onvif-go` — `server/` only; Device, Media, Imaging (§3) |
| Go directive | **≥ 1.25**, required by `gortsplib/v5` |
| CGO | **`CGO_ENABLED=0`**, enforced in CI. Every dependency above is pure Go |

`server/`'s own imports are stdlib plus its root package plus `internal/soap`, so importing it links
none of the heavier graph in that module. `internal/soap` is unreachable by Go's internal rule, which
is harmless here: camfarm owns the transport and never references those types (§2.2).

A scratch container and a CI runner both work with no system packages. This is a decided property,
not a hoped-for one, and the CI workflow asserts it.

**`CGO_ENABLED=0` is a property of the distributed artifact, not of the test tooling.** The
distinction matters because `go test -race` *requires* cgo, so asserting both in one CI job is
impossible. CI therefore runs two jobs: a `build` job with `CGO_ENABLED=0` proving the artifact and
its tests need no cgo, and a separate `race` job with `CGO_ENABLED=1` running the detector. Needing
cgo to run a build-time debugging tool says nothing about whether the shipped binary needs it.

This contradiction was found by running the workflow rather than reasoning about it, which is the
standard the project holds itself to (§12).

### 10.1 Packages

```
camfarm/                 public API: Start, StartT, Fleet, Camera, Spec
internal/fleet/          registry, lifecycle, listener ownership
internal/media/          MediaSource interface, file and rtsp-pull impls, AU parsing
internal/rtsp/           gortsplib v5 server, ServerHandler including OnResponse
internal/onvif/          SOAP transport: envelope, dispatch, auth, faults; native PTZ
internal/discovery/      WS-Discovery responder and directory fallback
internal/fault/          catalogue, seeded decision engine, injection points
internal/clock/          Clock interface; virtual and real implementations
internal/obs/            inspection: counters, event log, seed correlation
internal/httpapi/        control API and embedded read-only UI
```

### 10.2 Decision 6 — scale model

**One process. One RTSP server with N `ServerStream`s, dispatched in `OnDescribe` by `ctx.Path`. One
HTTP listener with a per-camera mux.** Proven at 25 cameras behind a single listener (§2.2), which
also removes any dependence on upstream's one-port-per-`Server` behaviour.

**The scale lever is sharing parsed media, not sharing streams.** A file source is parsed to access
units once; that slice is immutable and shared by every camera using it. Per-camera state is only the
encoder — its own SSRC, sequence number and timestamp base — plus its position in the loop. M cameras
on one file cost one copy of the media plus M small encoder states.

**The ceiling is measured, not asserted.** A committed benchmark reports cameras-per-core and
memory-per-camera, and the documented ceiling is whatever CI and a laptop actually show. Design
target is the 50–100 range; the number in the README comes from a run, never from an estimate.

### 10.3 Data flow

```
MediaSource ──parse once──▶ []AccessUnit (immutable, shared)
                                  │
                    per-camera Pump (clock-driven)
                                  │
                        fault interceptor (frame index)
                                  │
                 rtph264/rtph265.Encoder (per camera, seeded)
                                  │
                        ServerStream.WritePacketRTP
```

Control flows the other way: control API → `Fleet` → `Camera` → ONVIF state, fault engine, or pump.

### 10.4 Implementation traps, designed around and found the hard way

The first two were designed around before any code was written. The next three were found by
building and running Task 6 and 7's code, and are recorded as corrections for the same reason as
§8.3 and §7.3: running code disagreed with the design, and the design lost.

- **Setup ordering.** `ServerStream.Initialize()` requires an already-started server, but `Handler`
  must be live before the first connection and `Start()` launches the accept loop immediately.
  Setup holds a mutex across the whole sequence, as upstream's own example does. `rtspeek` assigns
  `Handler` after `Start()` in all three of its server tests — a real data race, and the pattern to
  avoid.
- **Loop seam.** On EOF the pump rewinds while **continuing** timestamps, so a looping fixture does
  not accidentally resemble the timestamp-discontinuity fault injected deliberately.
- **Trap A: lock-across-library-call deadlock.** `gortsplib.Server.Close()` tears down every live
  session, and each teardown invokes the session-closed handler camfarm registers. A `Close()` that
  holds the server's own write lock while calling into `gortsplib.Server.Close()` therefore waits on
  a handler that is itself blocked waiting for that same lock — a deadlock that only manifests when a
  session is open at close time, which is exactly the case a quick manual test skips. The same shape
  showed up independently in the start-time error-rollback path, which also calls back into the
  library while holding the lock it needs to roll back under. Resolution in both places: snapshot
  what is needed under the lock, release the lock explicitly, then call into the library.
- **Trap B: fixing Trap A exposed a data race.** Once `Close()` could actually complete, its clearing
  of each camera's stream pointer began racing an *unlocked* read of that same field by request
  handlers — handlers that had resolved a camera under a read lock, released it, and then read the
  camera's fields with no lock held at all. Trap A's fix made this race newly reachable by making
  `Close()` finish instead of hang. Resolution: the lookup returns values already resolved under the
  read lock, not a pointer for the caller to dereference later, closing the window rather than
  narrowing it. The general lesson generalizes past this one fix: resolving one concurrency defect
  routinely unmasks another it was hiding, so a concurrency fix earns a re-review, not a checkmark.
- **Trap C: no test-only pump-stepping accessor.** Pumps are constructed inside the same locked
  sequence that launches their driver goroutine, so any accessor that handed one back would hand back
  a pump whose driver is already running — and a pump is single-goroutine by design, not safe to step
  from two places at once. Resolution: no such accessor exists. The observable counters a test needs
  are already exposed, correctly synchronised, through the inspection recorder; tests that need
  determinism at the pump level construct a pump directly instead of reaching into a running fleet
  for one.

### 10.5 Decision 7 — discovery

**A WS-Discovery responder, plus a non-multicast directory fallback.** The fallback is a fleet
endpoint listing every camera's device-service address, so a client in Docker or CI finds the fleet
with multicast entirely unavailable.

**Discovery is a convenience, never a dependency.** No test in camfarm or in any consumer requires
multicast to pass. This is a deliberate response to a known "works on a laptop, fails in CI" trap
rather than something to discover later.

Determinism requirements from §2.8: stable per-camera `urn:uuid` derived from seed and index; seeded
`AppSequence.InstanceId`; seeded reply delay. Correctness must be verified against an external
client, because the local one does not check `RelatesTo`.

### 10.6 Decision 8 — configuration surface

**Both, with one source of truth.** A declarative fleet spec describes the fleet up front; the
control API mutates it at runtime. The spec type *is* the API's payload type, so the two cannot
drift.

The UI is a client of the control API. In v1 it is a **read-only dashboard**: per-camera state,
source, advertised codec and resolution, connected clients, frames served, faults fired. Assets
embedded via `embed.FS`, so a single static binary with `CGO_ENABLED=0` still holds.

### 10.7 Testing strategy

- **Unit and determinism tests.** A fixed seed produces a fixed event sequence; asserted directly.
  Virtual clock throughout, so these are fast and hermetic.
- **Integration tests over every imported upstream handler**, driven over real HTTP with real SOAP.
  Non-negotiable given §2.1: upstream CI is green on a path known to be broken, so camfarm's tests
  are the only drift detector.
- **A real `gortsplib` client driving SETUP and PLAY**, plus `ffprobe` as an external cross-check.
  `rtspeek` stops at DESCRIBE and cannot validate the media path.
- **`go test -race` in CI**, in its own job for the cgo reason given in §10.0. Mid-test mutation of a
  running fleet is a core feature, so racing is a real risk here rather than a formality.
- **No skipped tests.** Engineer around gaps rather than skipping, matching the bar in §2.9.
- **Real sockets over mocks**, following the account's existing instinct: ephemeral ports,
  `203.0.113.1` (TEST-NET-1) for timeouts, `.invalid` for DNS failure.

---

## 11. Naming and public claims

### 11.1 The ONVIF® trademark constraints are binding now

Two independent gates. **Conformance claims** require membership, a passing official test-tool run
and a generated Declaration of Conformance; the test tools are members-only, so that door is closed
to this project entirely. **Trademark rules bind everyone** — the prohibited-terms list is worded
"No one may use…", and the Conformance Process Specification explicitly covers non-members using
ONVIF trademarks.

The governing permission: non-conformant products *"may not reference ONVIF profiles or add-ons, but
may reference the use of ONVIF specifications or protocols as long as the reference does not claim or
lead the public to believe, or create a direct or indirect inference, that a non-conformant product
has met the ONVIF conformance requirements."*

| Phrasing | Ruling |
|---|---|
| "ONVIF-compatible" | **Prohibited** — verbatim on the prohibited list, with compliant, certified, interoperable, registered, tested, "Complies with ONVIF", "Compatible with ONVIF protocol" |
| "implements a subset of Profile T" | **Impermissible** — references a profile, forbidden for non-conformant products. The profile name is the problem, not "subset" |
| "simulates an ONVIF device" | **Avoid.** Not enumerated, so this is inference: "an ONVIF device" reads as a conformance-defined category, and indirect inference is barred |
| "speaks ONVIF" | Permitted but weak — names no specification, so it does less than the sanctioned form |

**The sanctioned construction, to be quoted rather than improvised:**

> "Implements/supports ONVIF [protocol or specification name and reference name of service
> specification document] – not ONVIF profile or add-on conformant."

**The affirmative obligation:**

> "A non-conformant product that supports other ONVIF conformant products must make clear publicly
> that the product is not ONVIF conformant or has not met the ONVIF conformance requirements."

The policy specifies no location for that disclaimer, only "publicly". Placement in the top-level
README near the first ONVIF reference is a recommendation, not a quoted rule. The trigger is
conditioned on "supports other ONVIF conformant products", which this project plainly does by
existing to be talked to by real clients, so the obligation is treated as applying rather than
argued.

Further requirements: **`ONVIF®` with the superscript symbol on first appearance, per artifact** —
README, documentation site and package description each need it. Naming the specification documents
is **required**, not merely permitted, since it is the placeholder in the sanctioned form. Operation
names and section numbers carry no restriction. **No logo and no profile symbol**, ever; artwork is
members-only. No forward-looking phrasing such as "conformance candidate".

No mandated trademark-attribution line was found in the public policy. One may exist in the
members-only brand document. Including such a line voluntarily is conventional and harmless but
cannot be cited as required.

### 11.2 Consequence for the permanent name

The word mark must not appear in the project's official name. **Any candidate name embedding "onvif"
is ruled out.** `camfarm` remains a placeholder pending a check for availability, connotation and
collision.

### 11.3 Scope — what this does not do

The README carries this section explicitly. Contents:

- Not conformance-tested, and cannot be: the test tools are members-only. Uses the sanctioned
  disclaimer wording.
- Describes what it implements by **specification name**, never by profile.
- Lists what is not faithfully emulated, per service, honestly and specifically.
- States the determinism carve-outs from §8.3 rather than burying them.
- States that the fixture media is a fixture: bundled video is a minimal default, not
  representative of real camera output.

Overclaiming here is actively harmful. A user could test a client against this farm and wrongly
conclude something about their own conformance.

---

## 12. Hard constraints carried forward

- **Personal project framing only.** No employer or client named anywhere in code, comments, commit
  messages, documentation or issues.
- **It simulates.** It is not conformance-tested and must never claim to be. §11.
- **MIT licence**, matching `onvif-go`.
- **Default branch `main`.**
- **CI green from the first commit.** No badge not backed by a currently-passing workflow. No
  coverage percentage anywhere that CI itself did not produce. A project that is itself a test tool
  has less excuse than most to get this wrong.
- **All changes confined to this repository.** Sibling repositories are read-only prior art for this
  work. Upstream defects are recorded as a list for separate action.
- Prefer a small set of faithfully-simulated behaviours over a large set of approximate ones.

---

## 13. Open items, honestly listed

1. **The permanent name.** Unresolved, and now constrained by §11.2.
2. **`CLAUDE.md` overstates the `onvif-mcp` relationship** (§2.9). Needs correcting to describe that
   project as a candidate consumer.
3. **Resolved. The bundled fixture's provenance and licence.** It is a synthetic `testsrc2` pattern
   (320×240, 2 seconds, H.264) generated by the committed `scripts/gen-fixture.sh`, encoded with
   ffmpeg 6.1.1. It contains no third-party footage and is redistributable under MIT alongside the
   code. Regeneration is **not** byte-reproducible — a different ffmpeg build will encode differently
   — so the committed artifact, not the script, is the authoritative fixture; the script documents
   how it was produced, not a promise that re-running it reproduces the same bytes.
4. **Events service.** Mandatory in both profiles, implemented nowhere, and a real consumer is
   blocked on it (§2.10). Not in v1 scope, but the highest-value candidate for what follows, and the
   architecture should not preclude it.
5. **Profile T's Media2 migration.** The imported handlers are Media (v1), not Media2. What that
   implies for a T-shaped operation set is not yet designed.
6. **The measured scale ceiling.** Unknown by construction until the benchmark exists (§10.2).
