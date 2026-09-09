# ONVIF control plane, slice 1: Device and Media, with HTTP Digest auth

Status: approved design, not yet implemented.
Supersedes nothing. Extends `2026-09-05-camfarm-architecture-design.md` §3 (reuse strategy),
§9 (public API), §10.1 (packages), §10.7 (testing strategy) and §11 (naming and public
claims). Narrows the master design's ONVIF architecture to its first vertical slice, the same
way `2026-09-06-multi-container-h265-design.md` narrowed §5's media architecture.

## 0. What this slice adds

1. **Device service.** `GetDeviceInformation`, `GetCapabilities`, `GetSystemDateAndTime`,
   `GetServices`, `SystemReboot` (accepted, no-op) — depend on `onvif-go/server` unmodified,
   per the already-settled decision in §3 of the master design.
2. **Media service.** `GetProfiles`, `GetStreamURI`, `GetSnapshotURI`, `GetVideoSources` —
   same dependency. `GetStreamURI` returns the *real* RTSP URL already served by phase 2's
   `internal/rtsp`, which is the concrete payoff of this slice: it is the one call that
   connects the ONVIF half to the RTSP half.
3. **Transport, owned natively.** New `internal/onvif/`: SOAP envelope parsing, per-camera
   dispatch behind one HTTP listener, and HTTP Digest authentication gating every operation
   when a camera's credentials are set. None of this is imported; §3 of the master design
   already decided camfarm owns the transport under every reuse option.
4. **Public surface additions.** `AuthSpec` on `CameraSpec`, `Camera.ONVIFEndpoint()`.

### 0.1 What it deliberately does not add

- **No PTZ.** The master design's quarantine decision (§3) stands: PTZ needs a native, seeded
  motion model, which is real work deserving its own slice, not a corner of this one.
- **No Imaging.** `onvif-go`'s `imaging.go` has no determinism hazard (unlike `ptz.go`), but
  including it here would make this slice "Device, Media, and whatever else happened to be
  easy" instead of one clean vertical. Next slice.
- **No WS-Discovery.** Phase 4, unrelated seam.
- **No fault effects on the ONVIF path.** The catalogue stays exactly as refused as it is
  today; this slice adds no new fault kind and wires no interception point.
- **No WS-Security `UsernameToken`.** Profile S's scheme, and Profile S is deprecated (§2.10
  of the master design). Building it next to Digest, which Profile T mandates, buys nothing.

## 1. Motivation

Phase 2 (RTSP + media) is complete: camfarm serves real, controllable H.264/H.265 streams.
Without an ONVIF control plane, nothing that discovers or drives cameras via ONVIF — which is
most of the software this project exists to test — can find or use the fleet. `GetStreamURI`
is the single call that makes the two already-built halves cohere: a real ONVIF client asks
for it and gets back a live camfarm RTSP URL, not a placeholder.

## 2. Depending on `onvif-go/server`, made concrete

The master design's §3 decision — depend, quarantine PTZ, never modify upstream, own the
transport — is not reopened here. This section is that decision's mechanics for this slice.

**One `*server.Server` value per camera**, constructed via `server.New(&server.Config{...})`
inside `Start()`. Its own `.Start(ctx)` is never called — camfarm owns the `net.Listener` and
`http.ServeMux`; the onvif-go `Server` value is used only for its `Handle*` methods and the
internal state it keeps (`streams`, `ptzState`, `imagingState` — the latter two populated by
`server.New` but inert this slice, since nothing calls PTZ or Imaging operations against them).

**`UpdateStreamURI` is called immediately after construction**, once per profile, with
`camera.RTSPURL()` — the address `internal/rtsp` actually bound. Without this, `GetStreamURI`
returns onvif-go's synthesized `rtsp://host:8554/streamN` guess, which is wrong and would make
this slice a lie rather than a bridge.

**Config fields set explicitly:** `SupportPTZ: false`, `SupportImaging: false` — `GetCapabilities`
must advertise only what this slice serves. `Config.Username`/`Password` are left at whatever
`onvif-go` defaults to and are never read by any code path camfarm exercises: they gate its own
`soap.Handler.ServeHTTP`, which this design never calls (§2.2 of the master design identifies
that path as the broken one anyway).

**The seam:** `unmarshalBody`'s `[]byte` branch, present in all fourteen body-consuming
handlers (media.go:372-391 upstream, per §2.2). camfarm's dispatcher extracts the SOAP body's
raw inner XML bytes for the matched action and passes them as `body interface{}` holding a
`[]byte`. No-parameter operations (`GetDeviceInformation`, `GetCapabilities`,
`GetSystemDateAndTime`, `GetServices`, `SystemReboot`, `GetVideoSources`) are called with `nil`.

## 3. Transport package: `internal/onvif/`

- **`server.go`** — one `http.ServeMux`, one HTTP listener, camera dispatch by path segment,
  mirroring `internal/rtsp`'s one-listener-many-cameras shape (§10.2 of the master design).
- **`envelope.go`** — parses the incoming SOAP envelope, extracts the body's first child
  element's local name as the action name and its raw bytes as the payload.
- **`dispatch.go`** — a table mapping action name to the matching onvif-go `Handle*` method,
  scoped per camera's `*server.Server` value.
- **`auth.go`** — HTTP Digest challenge and verification, in front of dispatch.

**Path scheme:** one base path per camera (e.g. `/onvif/{cameraID}/device`,
`/onvif/{cameraID}/media`), matching ONVIF's convention of separate per-service URLs. The
`XAddr` fields `GetCapabilities`/`GetServices` return must be rewritten to camfarm's real bound
address and per-camera path — never onvif-go's `Config.BasePath` default, which knows nothing
about camfarm's actual listener.

**Unknown or unimplemented actions** (e.g. a `GetPresets` call, since PTZ is out of scope) get
a SOAP Fault (`soap:Client`, "Action not supported") rather than a bare HTTP error code. A real
ONVIF client expects a fault envelope on the wire; a 404 is not legible to it as "not in this
version" the way a fault code is.

## 4. HTTP Digest authentication

**`AuthSpec{Username, Password string}`**, added to `CameraSpec`. The zero value means open —
consistent with the pattern `VideoSpec` already establishes ("zero fields mean whatever"), and
consistent with RTSP itself having no auth today. A camera only challenges when both fields are
non-empty.

**Scheme:** RFC 2617 Digest, MD5, `qop="auth"`. This is the scheme real ONVIF Profile T devices
and clients actually speak in practice, chosen for compatibility over cryptographic novelty —
consistent with the project's own instinct to be device-realistic rather than aspirational.

**Nonce generation is deterministic**, derived from the fleet seed, the camera ID, and a
per-camera issued-nonce counter — never `crypto/rand`. This is the same trade already made and
recorded for WS-Discovery's reply delay and `AppSequence.InstanceId` (§2.8 of the master
design): camfarm simulates a camera, it is not a security boundary, and a predictable nonce is
a deliberate, documented cost of the "every decision traces to the seed" rule
(`CLAUDE.md`, "Determinism is the product").

**Realm** is the fixed string `"camfarm"` — one fake vendor, not a per-camera realm, since
nothing in this slice needs realm variation and per-camera realms would be an untested
dimension invented for no consumer.

**Flow:** a request without a valid `Authorization` header on a credentialed camera gets `401`
with `WWW-Authenticate: Digest realm="camfarm", nonce="...", qop="auth"`. No nonce-count or
session tracking across requests — a stale or reused nonce simply re-challenges, which is
spec-legal and keeps the state machine to "did this exact request's digest match," nothing
more. Auth failure is a pure HTTP-layer `401`, never a SOAP fault body — real Digest auth lives
below SOAP, and mixing the two layers would make failures harder to reason about, not easier.

## 5. Testing strategy

Per §10.7 of the master design, non-negotiable: **integration tests over every imported
handler, driven with real HTTP and real hand-built SOAP envelopes** — never calling `Handle*`
directly. §2.1 of the master design exists precisely because upstream's own test suite does
call handlers directly and its CI is green on a transport path proven broken; repeating that
mistake here would defeat the reason this slice owns its own transport.

- A `net/http` client sending hand-built SOAP requests (no ONVIF client library needed for six
  operations) driving `GetDeviceInformation`, `GetCapabilities`, `GetProfiles`, `GetStreamURI`
  (asserting the returned URL equals `Camera.RTSPURL()` exactly), `GetSnapshotURI`,
  `GetVideoSources`.
- Digest tests: an uncredentialed camera answers with no `Authorization` header; a credentialed
  camera 401s then 200s once the digest response is computed correctly; a wrong password 401s;
  a stale nonce 401s with a fresh challenge rather than hanging or panicking.
- A determinism test: two fleets built from the same seed issue the same nonce sequence,
  mirroring the existing `TestSameSeedReproducesCameraSeeds` pattern in `integration_test.go`.
- A mixed-fleet test: one credentialed camera, one open, one H.264, one H.265 (reusing existing
  fixture infrastructure) — `GetStreamURI` returns the correct URL per camera despite each
  camera owning its own onvif-go `*server.Server` internals.

## 6. Public API additions

```go
type AuthSpec struct {
    Username string `json:"username,omitempty"`
    Password string `json:"password,omitempty"`
}
```

- `CameraSpec.Auth AuthSpec` `json:"auth,omitempty"`
- `func (*Camera) ONVIFEndpoint() string`

No new error values. `ProfileToken()`/`PresetToken()` from §9 of the master design are not
added yet — they exist to guarantee PTZ capability, which this slice does not have.

## 7. Naming and public claims

Per §11.1 of the master design: `ONVIF®` with the superscript symbol on first appearance in the
README, and the sanctioned construction — "Implements/supports the ONVIF Device Service and
Media Service specifications — not ONVIF profile or add-on conformant" — plus the affirmative
"not ONVIF conformant" disclaimer, placed near the first ONVIF mention. Naming the two service
specification documents is required, not optional, since it is the placeholder the sanctioned
phrasing needs filled in. No profile name ("Profile T") appears in public docs as a conformance
claim; this design doc may explain that Profile T's mandate is *why* Digest was chosen, because
this document is not public marketing copy, but that reasoning does not cross into the README.

## 8. What's deferred

Named so the boundary stays legible, matching the h265 slice's own practice:

- **PTZ** — native seeded motion model, its own slice.
- **Imaging** — next slice.
- **WS-Discovery** — phase 4.
- **WS-Security `UsernameToken`** — not planned; Digest supersedes it for this project.
- **ONVIF-side fault effects** — phase 6, deferred by earlier decision.
