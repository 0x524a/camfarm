# WS-Discovery Responder Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Make the fake fleet discoverable: an opt-in WS-Discovery responder answers Probe messages for every camera and sends Hello/Bye as cameras come and go.

**Architecture:** One `discovery.Responder` (onvif-go v1.2.0) per fleet, owned by `internal/onvif.Server`, which already owns the camera set, the device URLs and the listener lifecycle. Each camera registers a `discovery.Device` built from `cam.srv.DiscoveryDevice()` with its XAddr overridden to camfarm's real `…/onvif/{id}/device` URL (onvif-go computes its XAddr from a port camfarm never binds). Opt-in via `ListenSpec.Discovery`, a `--discovery` flag, and the library API.

**Tech Stack:** Go 1.26, `github.com/0x524a/onvif-go` v1.2.0 (`discovery.Responder`, `discovery.DiscoverWithOptions` with `ProbeAddress` for unicast tests).

**Spec:** user-approved design in conversation (no separate spec file): opt-in; `--discovery multicast|unicast:ADDR` plus `--discovery-interface`; one responder per fleet; Hello on register / Bye on unregister including rollback; endpoint ref = the same seed-derived `urn:uuid:` that `GetEndpointReference` returns; `MaxResponseDelay` 0; multicast on a loopback listen host is refused with a clear error.

## Global Constraints

- No cgo, `CGO_ENABLED=0`; Go 1.26; new code matches surrounding style and comment density.
- Off by default: no behaviour change when `Listen.Discovery` is nil. `omitempty` JSON; an old spec must hash/replay identically (check how the replay line is built and keep it unchanged for specs without discovery).
- Determinism: endpoint refs seed-derived (already); responder replies immediately (`MaxResponseDelay` 0). Arrival timing is wall-clock and the README must say so.
- It simulates; never claim ONVIF conformance. README Scope must stay honest about multicast limits (Docker, CI runners, network namespaces) and that CI only exercises unicast mode.
- golangci-lint v2.13.2 requires `_, _ =` on `fmt.Fprint*`. Shell guard refuses compound git commands and heredoc appends: use editor tools.
- Tests: `go test ./... -race -count=1`, `golangci-lint run`, e2e with `PLAYWRIGHT_DRIVER_PATH=/tmp/pwdrv go test -tags e2e ./internal/dashboard/`.

## Review Focus

- Responder lifecycle: start only after the ONVIF listener has bound; stop on `Close` with no goroutine leak; `Close` twice is safe.
- AddCamera rollback after a registered camera must Unregister (Bye) — no phantom device.
- RemoveCamera of a never-registered camera / discovery disabled must not panic.
- Port-0 and listen-host handling: the advertised XAddr uses the real bound ONVIF address, never `:0` or onvif-go's unbound port.
- Credentialed cameras: discovery works without auth (as in reality); the XAddr must not embed credentials.
- Probe with Types/Scopes filters that do not match returns nothing (use the responder's own behaviour; test one non-matching probe).

---

### Task 1: Responder inside `internal/onvif`

**Files:** Modify `internal/onvif/server.go` (Config, New, Start, Close, AddCamera, RemoveCamera), create `internal/onvif/discovery.go` and `internal/onvif/discovery_test.go`.

**Interfaces:**
- Produces: `type DiscoveryConfig struct { Multicast bool; Interface string; ListenAddr string; AnnounceAddr string }` (exactly one mode: `Multicast` true, or `ListenAddr` set for unicast; `Interface` only with multicast; validate in `New` and return clear errors); `Config.Discovery *DiscoveryConfig`; `(*Server) DiscoveryAddr() net.Addr` (nil when disabled or not started).
- Consumes: `cam.srv.DiscoveryDevice()`, `discovery.NewResponder`, `Responder.Register/Unregister/Start/Addr`, `ResponderConfig{NetworkInterface, ListenAddr, AnnounceAddr, Ready}`.

- [ ] **Step 1: Failing tests** (`discovery_test.go`, package `onvif`): start a test server (see `startTestServer` in `server_test.go`) with `Discovery{ListenAddr:"127.0.0.1:0"}` and 3 cameras; use `discovery.DiscoverWithOptions(ctx, timeout, &discovery.DiscoverOptions{ProbeAddress: srv.DiscoveryAddr().String()})`; assert 3 devices found, each `XAddrs[0]` equals that camera's `Endpoint(id)+"/device"`, each endpoint ref equals `"urn:uuid:"+EndpointUUID(seed)`, and an authenticated `GetDeviceInformation` SOAP request to the XAddr succeeds (with the camera's digest credentials for a credentialed camera, 401 without). Then `AddCamera` a 4th → discovered; `RemoveCamera` one → no longer discovered; `Close` → probe times out/empty and the test's goroutine count returns to baseline. Config validation tests: both modes set, neither mode set, Interface with unicast → errors from `New`. Discovery nil → `DiscoveryAddr()` nil and AddCamera/RemoveCamera still work.
- [ ] **Step 2: Run, expect FAIL** (undefined `DiscoveryConfig`).
- [ ] **Step 3: Implement.** Responder created in `New` when configured, started in `Start` after the HTTP listener is bound (use `Ready` to wait until bound, bounded by a timeout; start in a goroutine with a context cancelled in `Close`, surface a `Start` error to the caller rather than losing it), cameras registered after start and on `AddCamera`; unregistered on `RemoveCamera`, and on `AddCamera` rollback paths. Device = `cam.srv.DiscoveryDevice()` with `XAddrs = []string{s.deviceURL(id)}`; do not mutate the shared value returned if it is shared. Guard all responder access with the server's existing lock discipline (read how `cameraByID`/AddCamera lock). `Close` stops the responder (Bye) and waits for its goroutine.
- [ ] **Step 4:** `go test ./internal/onvif -race -count=3`; commit `onvif: WS-Discovery responder for the fleet's cameras`.

---

### Task 2: Public API (`Spec`, validation, `Start`)

**Files:** Modify `spec.go` (add `DiscoverySpec`, `ListenSpec.Discovery`), validation in `spec.go` or where `Spec` is validated, `camfarm.go` (wire into `onvif.Config`), `camfarm_test.go`.

**Interfaces:**
- Produces: `type DiscoverySpec struct { Mode string `json:"mode"` /* "multicast" | "unicast" */; Interface string `json:"interface,omitempty"`; Addr string `json:"addr,omitempty"` /* unicast listen address, e.g. 127.0.0.1:0 */; AnnounceAddr string `json:"announceAddr,omitempty"` }`; `ListenSpec.Discovery *DiscoverySpec `json:"discovery,omitempty"``; `(*Fleet) DiscoveryAddr() string` ("" when disabled).
- Validation (each returns `ErrInvalidSpec`-wrapped): mode must be `multicast` or `unicast`; unicast requires `Addr`; multicast forbids `Addr`; `Interface` only with multicast; **multicast with the default/loopback listen host (`127.0.0.1`, `::1`, `localhost`) is refused** with a message naming `listen.host`.

- [ ] **Step 1: Failing tests** in `camfarm_test.go`: each validation rejection; `Start` with `Discovery{Mode:"unicast", Addr:"127.0.0.1:0"}` and 2 cameras → `Fleet.DiscoveryAddr()` non-empty, `discovery.DiscoverWithOptions(... ProbeAddress ...)` finds both with XAddr equal to `cam.ONVIFEndpoint()+"/device"` and ref `"urn:uuid:"+cam.ONVIFUUID()`; `Fleet.AddCamera` → found; `Fleet.RemoveCamera` → gone; spec without discovery: replay line and JSON round-trip unchanged (compare with a golden produced from the current main behaviour).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement**; keep `Normalize`/default handling consistent with how `Listen` is defaulted.
- [ ] **Step 4:** `go test . -race -count=1`; commit `camfarm: opt-in WS-Discovery through the public API`.

---

### Task 3: CLI flags

**Files:** Modify `cmd/camfarm/run.go`, `cmd/camfarm/*_test.go`.

**Interfaces:** flags `--discovery` (`multicast` or `unicast:HOST:PORT`; empty = off) and `--discovery-interface` (only with multicast). They override a spec file's `listen.discovery` only when explicitly set (same `set[...]` pattern as the other listen flags). Help text names the multicast caveats briefly. Invalid values produce a clear error before anything starts.

- [ ] **Step 1: Failing tests** following the existing `run` tests: parse/override cases; `--discovery unicast:127.0.0.1:0 --cameras 2` starts, a probe finds both cameras; `--discovery multicast` without `--listen-host` errors with the loopback message; `--discovery-interface` without multicast errors.
- [ ] **Step 2: Run, expect FAIL; Step 3: implement; Step 4:** `go test ./cmd/camfarm -race -count=1`; commit `camfarm: --discovery flags`.

---

### Task 4: Docs

**Files:** Modify `README.md`, `CLAUDE.md`.

- [ ] README: add a `Works` bullet (opt-in WS-Discovery responder, multicast or unicast, Hello/Bye, seed-derived endpoint refs matching `GetEndpointReference`, flags/spec field); in Scope replace "WS-Discovery is not implemented" with an honest description: simulates a responder, not conformance-tested; matching limited to what onvif-go's responder supports (state: Probe/ProbeMatches with Types and Scopes filtering, Hello/Bye; no Resolve unless the responder does — verify against its docs); multicast caveats (Docker bridge networks, CI runners, network namespaces, firewall/IGMP, needs a non-loopback `listen.host`); CI exercises unicast mode only; reply timing is wall-clock, identities are seeded. Update the "does not do" line that says a client must be given the endpoint directly. In CLAUDE.md update "Open architectural decisions" #5 (Discovery) to say a responder shipped and what remains open, and the resolved-decisions text that lists WS-Discovery as deferred.
- [ ] Run `go test ./... -race -count=1` and the README drift test; commit `docs: WS-Discovery responder`.
