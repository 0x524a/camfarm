# ONVIF control plane, slice 1: Device and Media, with HTTP Digest auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an ONVIF Device+Media control plane to camfarm, so real ONVIF
clients can discover a camera's capabilities and get back the real RTSP URL
camfarm already serves.

**Architecture:** A new `internal/onvif/` package owns its own HTTP transport
(one listener, one `*http.ServeMux`, path-routed per camera) and calls
unmodified `onvif-go/server` `Handle*` methods through the `[]byte` body seam.
camfarm never calls onvif-go's own `Start()` or `soap.Handler.ServeHTTP`.

**Tech Stack:** Go 1.26, `github.com/0x524a/onvif-go/server` (new dependency),
`net/http`, `encoding/xml`.

**Spec:** `docs/superpowers/specs/2026-09-09-onvif-control-plane-design.md`

## Global Constraints

- Never call onvif-go's `Server.Start(ctx)` or `soap.Handler.ServeHTTP` — camfarm owns the transport.
- One `*server.Server` (onvif-go) per camera, constructed in `internal/onvif.New`.
- `UpdateStreamURI` called immediately after construction with the camera's real `RTSPURL()`.
- `SupportPTZ: false`, `SupportImaging: false`, `SupportEvents: false` on every onvif-go `Config`.
- Digest nonces are deterministic: derived from the fleet seed + camera seed, never `crypto/rand`.
- Realm is the fixed string `"camfarm"`.
- No new error values in the public `errors.go` taxonomy.
- Testing is real HTTP + hand-built SOAP envelopes only — never call onvif-go `Handle*` methods directly from a test.
- MIT license, personal-project framing, no employer/client names anywhere (per `CLAUDE.md`).

---

## Task 1: Public API additions — `AuthSpec`, `ListenSpec.ONVIFPort`, onvif-go dependency

**Files:**
- Modify: `spec.go`
- Modify: `go.mod`, `go.sum`
- Test: `spec_test.go` (create if it does not exist, else extend)

**Interfaces:**
- Produces: `AuthSpec{Username, Password string}`, `CameraSpec.Auth AuthSpec`, `ListenSpec.ONVIFPort int`.

- [ ] **Step 1: Add the dependency**

```bash
cd /home/ritwik/devBed/rj/camfarm
go get github.com/0x524a/onvif-go@none 2>/dev/null; go mod edit -require=github.com/0x524a/onvif-go@v0.0.0
go mod tidy
```

If `onvif-go` is not published to a reachable proxy, add a `replace` directive pointing at the local checkout instead:

```bash
go mod edit -replace github.com/0x524a/onvif-go=/home/ritwik/devBed/rj/onvif-go
go mod edit -require=github.com/0x524a/onvif-go@v0.0.0-00010101000000-000000000000
go mod tidy
```

- [ ] **Step 2: Write the failing test**

```go
// spec_test.go
package camfarm

import "testing"

func TestAuthSpecDefaultIsOpen(t *testing.T) {
	s := Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a"}}}
	valid, err := s.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if valid.Cameras[0].Auth != (AuthSpec{}) {
		t.Errorf("Auth = %+v, want zero value", valid.Cameras[0].Auth)
	}
}

func TestAuthSpecPartialCredentialsRefused(t *testing.T) {
	s := Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a", Auth: AuthSpec{Username: "admin"}}}}
	if _, err := s.validate(); err == nil {
		t.Fatal("validate: want error for username set without password")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./... -run TestAuthSpec -v`
Expected: FAIL — `AuthSpec` undefined.

- [ ] **Step 4: Implement**

In `spec.go`, add near `VideoSpec`:

```go
// AuthSpec configures a camera's ONVIF HTTP Digest challenge.
//
// The zero value means open — consistent with the pattern VideoSpec already
// establishes ("zero fields mean whatever"), and consistent with RTSP itself
// having no auth today. A camera only challenges when both fields are
// non-empty.
type AuthSpec struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}
```

Add to `CameraSpec`:

```go
type CameraSpec struct {
	ID     string      `json:"id"`
	Source SourceSpec  `json:"source,omitempty"`
	Video  VideoSpec   `json:"video,omitempty"`
	Faults []FaultSpec `json:"faults,omitempty"`
	Auth   AuthSpec    `json:"auth,omitempty"`
}
```

Add to `ListenSpec`:

```go
type ListenSpec struct {
	Host     string `json:"host,omitempty"`
	RTSPPort int    `json:"rtspPort,omitempty"`
	// ONVIFPort of zero requests an ephemeral port for the ONVIF HTTP
	// listener, readable back via Camera.ONVIFEndpoint.
	ONVIFPort int `json:"onvifPort,omitempty"`
}
```

In `validate()`, inside the per-camera loop (after the fault loop), add:

```go
if (c.Auth.Username == "") != (c.Auth.Password == "") {
	return s, fmt.Errorf("camfarm: camera %q sets one of auth username/password but not both", c.ID)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./... -run TestAuthSpec -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add spec.go go.mod go.sum spec_test.go
git commit -m "Add AuthSpec, ListenSpec.ONVIFPort, and the onvif-go dependency"
```

---

## Task 2: `internal/onvif/envelope.go` — SOAP parsing and writing

**Files:**
- Create: `internal/onvif/envelope.go`
- Test: `internal/onvif/envelope_test.go`

**Interfaces:**
- Produces: `parseAction(soapBody []byte) (name string, payload []byte, err error)`, `writeResponse(w http.ResponseWriter, content any)`, `writeFault(w http.ResponseWriter, code, reason string)`, `const soapContentType`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/onvif/envelope_test.go
package onvif

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseActionSOAP11(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl">
      <ProfileToken>profile1</ProfileToken>
    </GetStreamUri>
  </soap:Body>
</soap:Envelope>`)

	name, payload, err := parseAction(body)
	if err != nil {
		t.Fatalf("parseAction: %v", err)
	}
	if name != "GetStreamUri" {
		t.Errorf("name = %q, want GetStreamUri", name)
	}
	if !strings.Contains(string(payload), "profile1") {
		t.Errorf("payload = %q, want it to contain profile1", payload)
	}
}

func TestParseActionSOAP12(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope">
  <soap:Body>
    <GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/>
  </soap:Body>
</soap:Envelope>`)

	name, _, err := parseAction(body)
	if err != nil {
		t.Fatalf("parseAction: %v", err)
	}
	if name != "GetDeviceInformation" {
		t.Errorf("name = %q, want GetDeviceInformation", name)
	}
}

func TestParseActionMalformed(t *testing.T) {
	if _, _, err := parseAction([]byte("not xml")); err == nil {
		t.Fatal("parseAction: want error for malformed input")
	}
}

func TestWriteResponseWrapsContent(t *testing.T) {
	type stub struct {
		XMLName struct{} `xml:"http://example.com/wsdl StubResponse"`
		Value   string   `xml:"Value"`
	}

	rec := httptest.NewRecorder()
	writeResponse(rec, &stub{Value: "hello"})

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<soap:Envelope") || !strings.Contains(body, "StubResponse") || !strings.Contains(body, "hello") {
		t.Errorf("body = %q, missing envelope/content", body)
	}
}

func TestWriteFaultShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFault(rec, "Client", "Action not supported")

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 (a fault envelope, not a bare HTTP error)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "soap:Fault") || !strings.Contains(body, "soap:Client") || !strings.Contains(body, "Action not supported") {
		t.Errorf("body = %q, missing fault shape", body)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/onvif/... -v`
Expected: FAIL — package does not exist / functions undefined.

- [ ] **Step 3: Implement**

```go
// internal/onvif/envelope.go
package onvif

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

const soapContentType = `application/soap+xml; charset=utf-8`

// parseAction extracts the SOAP body's first child element's local name (the
// action) and its raw inner XML, by tokenizing rather than unmarshaling into
// a namespace-pinned struct. Matching only the local name is what lets this
// accept SOAP 1.1 and SOAP 1.2 envelopes alike, and is why this package owns
// its own transport instead of reusing onvif-go's client-side Envelope type.
func parseAction(soapBody []byte) (name string, payload []byte, err error) {
	dec := xml.NewDecoder(bytes.NewReader(soapBody))
	depth := 0
	bodyDepth := -1

	for {
		tok, terr := dec.Token()
		if terr != nil {
			return "", nil, fmt.Errorf("onvif: no SOAP action found: %w", terr)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "Body" {
				bodyDepth = depth
				continue
			}
			if bodyDepth != -1 && depth == bodyDepth+1 {
				var raw struct {
					Inner []byte `xml:",innerxml"`
				}
				if err := dec.DecodeElement(&raw, &t); err != nil {
					return "", nil, fmt.Errorf("onvif: decoding action %q: %w", t.Name.Local, err)
				}
				return t.Name.Local, raw.Inner, nil
			}
		case xml.EndElement:
			depth--
		}
	}
}

// writeResponse marshals content (one of onvif-go's own *XxxResponse types)
// and wraps it in a hand-written SOAP 1.1 envelope.
//
// The envelope is written by hand rather than through encoding/xml:
// marshaling an outer struct whose elements need the literal "soap:" prefix
// real ONVIF clients expect fights encoding/xml's own prefix generation, and
// content's own XMLName tag already marshals correctly on its own — the two
// concerns don't need to share one call.
func writeResponse(w http.ResponseWriter, content any) {
	inner, err := xml.Marshal(content)
	if err != nil {
		writeFault(w, "Server", "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", soapContentType)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, xml.Header+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">`+
		`<soap:Body>%s</soap:Body></soap:Envelope>`, inner)
}

// writeFault sends a SOAP fault envelope. Always HTTP 200: a real ONVIF
// client expects a fault envelope on the wire, and a bare HTTP error code is
// not legible to it as "not supported" the way a fault code is.
func writeFault(w http.ResponseWriter, code, reason string) {
	w.Header().Set("Content-Type", soapContentType)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, xml.Header+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">`+
		`<soap:Body><soap:Fault><faultcode>soap:%s</faultcode><faultstring>%s</faultstring></soap:Fault></soap:Body></soap:Envelope>`,
		code, xmlEscape(reason))
}

func xmlEscape(s string) string {
	var buf strings.Builder
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/onvif/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/onvif/envelope.go internal/onvif/envelope_test.go
git commit -m "Add ONVIF SOAP envelope parsing and writing"
```

---

## Task 3: `internal/onvif/auth.go` — deterministic HTTP Digest

**Files:**
- Create: `internal/onvif/auth.go`
- Test: `internal/onvif/auth_test.go`

**Interfaces:**
- Consumes: `seed.Seed` and `seed.Seed.Stream(label string) Seed` from `github.com/0x524a/camfarm/internal/seed` (already exists).
- Produces: `newDigestAuth(username, password string, s seed.Seed) *digestAuth`, `(*digestAuth) challenge(w http.ResponseWriter)`, `(*digestAuth) verify(r *http.Request) bool`, `const authRealm = "camfarm"`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/onvif/auth_test.go
package onvif

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

// digestAuthorization builds a client-side Authorization header value for
// qop="auth", mirroring RFC 2617 §3.2.2.
func digestAuthorization(username, password, method, uri, realm, nonce string) string {
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	nc := "00000001"
	cnonce := "clienttestnonce"
	response := md5Hex(strings.Join([]string{ha1, nonce, nc, cnonce, "auth", ha2}, ":"))
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		username, realm, nonce, uri, nc, cnonce, response)
}

func md5HexLocal(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestDigestNoAuthorizationChallenges(t *testing.T) {
	d := newDigestAuth("admin", "secret", seed.Seed(1))
	req := httptest.NewRequest("POST", "/onvif/cam/device", nil)
	if d.verify(req) {
		t.Fatal("verify: want false with no Authorization header")
	}
}

func TestDigestCorrectCredentialsSucceed(t *testing.T) {
	d := newDigestAuth("admin", "secret", seed.Seed(1))

	rec := httptest.NewRecorder()
	d.challenge(rec)
	nonce := extractNonce(t, rec.Header().Get("WWW-Authenticate"))

	req := httptest.NewRequest("POST", "/onvif/cam/device", nil)
	req.Header.Set("Authorization", digestAuthorization("admin", "secret", "POST", "/onvif/cam/device", authRealm, nonce))

	if !d.verify(req) {
		t.Fatal("verify: want true for correct credentials")
	}
}

func TestDigestWrongPasswordFails(t *testing.T) {
	d := newDigestAuth("admin", "secret", seed.Seed(1))

	rec := httptest.NewRecorder()
	d.challenge(rec)
	nonce := extractNonce(t, rec.Header().Get("WWW-Authenticate"))

	req := httptest.NewRequest("POST", "/onvif/cam/device", nil)
	req.Header.Set("Authorization", digestAuthorization("admin", "wrong", "POST", "/onvif/cam/device", authRealm, nonce))

	if d.verify(req) {
		t.Fatal("verify: want false for wrong password")
	}
}

func TestDigestStaleNonceFails(t *testing.T) {
	d := newDigestAuth("admin", "secret", seed.Seed(1))

	rec := httptest.NewRecorder()
	d.challenge(rec)
	staleNonce := extractNonce(t, rec.Header().Get("WWW-Authenticate"))

	// A second challenge rotates the nonce, making the first one stale.
	d.challenge(httptest.NewRecorder())

	req := httptest.NewRequest("POST", "/onvif/cam/device", nil)
	req.Header.Set("Authorization", digestAuthorization("admin", "secret", "POST", "/onvif/cam/device", authRealm, staleNonce))

	if d.verify(req) {
		t.Fatal("verify: want false for a stale (superseded) nonce")
	}
}

func TestDigestReusedNonceFailsSecondTime(t *testing.T) {
	d := newDigestAuth("admin", "secret", seed.Seed(1))

	rec := httptest.NewRecorder()
	d.challenge(rec)
	nonce := extractNonce(t, rec.Header().Get("WWW-Authenticate"))

	authz := digestAuthorization("admin", "secret", "POST", "/onvif/cam/device", authRealm, nonce)

	req1 := httptest.NewRequest("POST", "/onvif/cam/device", nil)
	req1.Header.Set("Authorization", authz)
	if !d.verify(req1) {
		t.Fatal("verify: want true on first use")
	}

	req2 := httptest.NewRequest("POST", "/onvif/cam/device", nil)
	req2.Header.Set("Authorization", authz)
	if d.verify(req2) {
		t.Fatal("verify: want false on reuse — this design tracks no nc across requests")
	}
}

func TestDigestNonceSequenceDeterministic(t *testing.T) {
	d1 := newDigestAuth("admin", "secret", seed.Seed(42))
	d2 := newDigestAuth("admin", "secret", seed.Seed(42))

	rec1 := httptest.NewRecorder()
	d1.challenge(rec1)
	rec2 := httptest.NewRecorder()
	d2.challenge(rec2)

	n1 := extractNonce(t, rec1.Header().Get("WWW-Authenticate"))
	n2 := extractNonce(t, rec2.Header().Get("WWW-Authenticate"))
	if n1 != n2 {
		t.Errorf("nonce = %q vs %q, want equal for equal seeds", n1, n2)
	}
}

func extractNonce(t *testing.T, wwwAuth string) string {
	t.Helper()
	const key = `nonce="`
	i := strings.Index(wwwAuth, key)
	if i < 0 {
		t.Fatalf("WWW-Authenticate %q has no nonce", wwwAuth)
	}
	rest := wwwAuth[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("WWW-Authenticate %q has an unterminated nonce", wwwAuth)
	}
	return rest[:j]
}

var _ = http.StatusUnauthorized // referenced indirectly via challenge()
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/onvif/... -run TestDigest -v`
Expected: FAIL — `digestAuth` undefined.

- [ ] **Step 3: Implement**

```go
// internal/onvif/auth.go
package onvif

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/0x524a/camfarm/internal/seed"
)

// authRealm is the fixed string every credentialed camera challenges under.
// One fake vendor, not a per-camera realm: nothing in this slice needs realm
// variation.
const authRealm = "camfarm"

// digestAuth is one camera's RFC 2617 Digest state.
//
// Nonces are single-use and derived from the fleet seed rather than
// crypto/rand — camfarm simulates a camera, it is not a security boundary,
// and a predictable nonce is a deliberate, documented cost of "every
// decision traces to the seed" (CLAUDE.md). No nonce-count or session
// tracking beyond "is this the one currently-valid nonce": a stale or
// reused nonce simply re-challenges.
type digestAuth struct {
	username string
	password string
	seed     seed.Seed

	mu      sync.Mutex
	counter uint64
	current string
}

func newDigestAuth(username, password string, s seed.Seed) *digestAuth {
	return &digestAuth{username: username, password: password, seed: s}
}

// challenge issues a fresh nonce, invalidating whatever nonce preceded it,
// and writes a 401 with a WWW-Authenticate header.
func (d *digestAuth) challenge(w http.ResponseWriter) {
	d.mu.Lock()
	n := d.seed.Stream(fmt.Sprintf("onvif-nonce-%d", d.counter))
	d.counter++
	nonce := fmt.Sprintf("%016x", uint64(n))
	d.current = nonce
	d.mu.Unlock()

	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", nonce="%s", qop="auth"`, authRealm, nonce))
	w.WriteHeader(http.StatusUnauthorized)
}

// verify reports whether r carries a valid Authorization header for the
// single currently-valid nonce, then consumes that nonce so a replay of the
// same header fails on its next use.
func (d *digestAuth) verify(r *http.Request) bool {
	params, ok := parseDigestHeader(r.Header.Get("Authorization"))
	if !ok {
		return false
	}

	d.mu.Lock()
	current := d.current
	d.mu.Unlock()

	if current == "" || params["nonce"] != current || params["username"] != d.username {
		return false
	}

	ha1 := md5Hex(d.username + ":" + authRealm + ":" + d.password)
	ha2 := md5Hex(r.Method + ":" + params["uri"])
	want := md5Hex(strings.Join([]string{ha1, params["nonce"], params["nc"], params["cnonce"], params["qop"], ha2}, ":"))
	if want != params["response"] {
		return false
	}

	d.mu.Lock()
	if d.current == current {
		d.current = ""
	}
	d.mu.Unlock()
	return true
}

func parseDigestHeader(v string) (map[string]string, bool) {
	const prefix = "Digest "
	if !strings.HasPrefix(v, prefix) {
		return nil, false
	}
	out := make(map[string]string)
	for _, part := range strings.Split(v[len(prefix):], ",") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		out[part[:eq]] = strings.Trim(part[eq+1:], `"`)
	}
	for _, k := range []string{"username", "nonce", "uri", "response", "nc", "cnonce", "qop"} {
		if out[k] == "" {
			return nil, false
		}
	}
	return out, true
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/onvif/... -run TestDigest -v`
Expected: PASS

- [ ] **Step 5: Remove the unused test helper**

The `md5HexLocal` and `var _ = http.StatusUnauthorized` lines in the test file were scaffolding; delete `md5HexLocal` (unused — tests call the package's own `md5Hex`) and the `var _` line, then re-run:

Run: `go test ./internal/onvif/... -run TestDigest -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/onvif/auth.go internal/onvif/auth_test.go
git commit -m "Add deterministic HTTP Digest auth for the ONVIF transport"
```

---

## Task 4: `internal/onvif/dispatch.go` — action table and XAddr rewriting

**Files:**
- Create: `internal/onvif/dispatch.go`
- Test: `internal/onvif/dispatch_test.go`

**Interfaces:**
- Consumes: `github.com/0x524a/onvif-go/server` (`*server.Server`, `HandleGetDeviceInformation` etc., `GetCapabilitiesResponse`, `GetServicesResponse`).
- Produces: `handlers map[string]handlerFunc`, `rewriteXAddr(resp any, deviceURL, mediaURL string)`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/onvif/dispatch_test.go
package onvif

import (
	"io"
	"testing"

	onvifserver "github.com/0x524a/onvif-go/server"
)

func newTestOnvifServer(t *testing.T) *onvifserver.Server {
	t.Helper()
	srv, err := onvifserver.New(&onvifserver.Config{
		DeviceInfo: onvifserver.DeviceInfo{Manufacturer: "camfarm", Model: "synthetic"},
		Output:     io.Discard,
		Profiles: []onvifserver.ProfileConfig{{
			Token: "profile1",
			Name:  "cam",
			VideoSource: onvifserver.VideoSourceConfig{
				Token: "profile1_source", Resolution: onvifserver.Resolution{Width: 320, Height: 240}, Framerate: 30,
			},
			VideoEncoder: onvifserver.VideoEncoderConfig{
				Encoding: "H264", Resolution: onvifserver.Resolution{Width: 320, Height: 240}, Framerate: 30,
			},
		}},
	})
	if err != nil {
		t.Fatalf("onvifserver.New: %v", err)
	}
	if err := srv.UpdateStreamURI("profile1", "rtsp://127.0.0.1:9/cam"); err != nil {
		t.Fatalf("UpdateStreamURI: %v", err)
	}
	return srv
}

func TestHandlersCoverEverySlice1Action(t *testing.T) {
	want := []string{
		"GetDeviceInformation", "GetCapabilities", "GetSystemDateAndTime",
		"GetServices", "SystemReboot", "GetProfiles", "GetVideoSources",
		"GetStreamUri", "GetSnapshotUri",
	}
	for _, action := range want {
		if _, ok := handlers[action]; !ok {
			t.Errorf("handlers[%q] missing", action)
		}
	}
}

func TestHandlerGetStreamUriReturnsRealURL(t *testing.T) {
	srv := newTestOnvifServer(t)
	body := []byte(`<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><ProfileToken>profile1</ProfileToken></GetStreamUri>`)

	resp, err := handlers["GetStreamUri"](srv, body)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	got, ok := resp.(*onvifserver.GetStreamURIResponse)
	if !ok {
		t.Fatalf("response type = %T, want *GetStreamURIResponse", resp)
	}
	if got.MediaURI.URI != "rtsp://127.0.0.1:9/cam" {
		t.Errorf("URI = %q, want the real RTSP URL", got.MediaURI.URI)
	}
}

func TestRewriteXAddrOnCapabilities(t *testing.T) {
	resp := &onvifserver.GetCapabilitiesResponse{
		Capabilities: &onvifserver.Capabilities{
			Device: &onvifserver.DeviceCapabilities{XAddr: "http://wrong/device_service"},
			Media:  &onvifserver.MediaCapabilities{XAddr: "http://wrong/media_service"},
		},
	}
	rewriteXAddr(resp, "http://real/onvif/cam/device", "http://real/onvif/cam/media")

	if resp.Capabilities.Device.XAddr != "http://real/onvif/cam/device" {
		t.Errorf("Device.XAddr = %q", resp.Capabilities.Device.XAddr)
	}
	if resp.Capabilities.Media.XAddr != "http://real/onvif/cam/media" {
		t.Errorf("Media.XAddr = %q", resp.Capabilities.Media.XAddr)
	}
}

func TestRewriteXAddrOnServices(t *testing.T) {
	resp := &onvifserver.GetServicesResponse{
		Service: []onvifserver.Service{
			{Namespace: "http://www.onvif.org/ver10/device/wsdl", XAddr: "http://wrong/device_service"},
			{Namespace: "http://www.onvif.org/ver10/media/wsdl", XAddr: "http://wrong/media_service"},
		},
	}
	rewriteXAddr(resp, "http://real/onvif/cam/device", "http://real/onvif/cam/media")

	if resp.Service[0].XAddr != "http://real/onvif/cam/device" {
		t.Errorf("Service[0].XAddr = %q", resp.Service[0].XAddr)
	}
	if resp.Service[1].XAddr != "http://real/onvif/cam/media" {
		t.Errorf("Service[1].XAddr = %q", resp.Service[1].XAddr)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/onvif/... -run "TestHandlers|TestHandlerGetStreamUri|TestRewriteXAddr" -v`
Expected: FAIL — `handlers` and `rewriteXAddr` undefined.

- [ ] **Step 3: Implement**

```go
// internal/onvif/dispatch.go
package onvif

import (
	onvifserver "github.com/0x524a/onvif-go/server"
)

// handlerFunc adapts one onvif-go Handle* method to the []byte body seam
// this package always uses (media.go's unmarshalBody accepts []byte
// directly; the no-parameter operations ignore body entirely).
type handlerFunc func(*onvifserver.Server, []byte) (any, error)

// handlers maps the SOAP wire action name — the ONVIF Media WSDL spells
// GetStreamUri/GetSnapshotUri with a lowercase "Uri", which is what a real
// onvif.Client sends — to the onvif-go method that answers it.
var handlers = map[string]handlerFunc{
	"GetDeviceInformation": func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetDeviceInformation(nil) },
	"GetCapabilities":      func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetCapabilities(nil) },
	"GetSystemDateAndTime": func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetSystemDateAndTime(nil) },
	"GetServices":          func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetServices(nil) },
	"SystemReboot":         func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleSystemReboot(nil) },
	"GetProfiles":          func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetProfiles(nil) },
	"GetVideoSources":      func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetVideoSources(nil) },
	"GetStreamUri":         func(s *onvifserver.Server, body []byte) (any, error) { return s.HandleGetStreamURI(body) },
	"GetSnapshotUri":       func(s *onvifserver.Server, body []byte) (any, error) { return s.HandleGetSnapshotURI(body) },
}

// rewriteXAddr rewrites onvif-go's own XAddr fields, which are computed from
// a Config.Host/Port camfarm never binds (camfarm owns the listener; see
// server.go's New), to the camera's real per-service URL under this
// fleet's ONVIF listener.
func rewriteXAddr(resp any, deviceURL, mediaURL string) {
	switch r := resp.(type) {
	case *onvifserver.GetCapabilitiesResponse:
		if r.Capabilities.Device != nil {
			r.Capabilities.Device.XAddr = deviceURL
		}
		if r.Capabilities.Media != nil {
			r.Capabilities.Media.XAddr = mediaURL
		}
	case *onvifserver.GetServicesResponse:
		for i := range r.Service {
			switch r.Service[i].Namespace {
			case "http://www.onvif.org/ver10/device/wsdl":
				r.Service[i].XAddr = deviceURL
			case "http://www.onvif.org/ver10/media/wsdl":
				r.Service[i].XAddr = mediaURL
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/onvif/... -v`
Expected: PASS (all tests from Tasks 2-4 green)

- [ ] **Step 5: Commit**

```bash
git add internal/onvif/dispatch.go internal/onvif/dispatch_test.go
git commit -m "Add the ONVIF action dispatch table and XAddr rewriting"
```

---

## Task 5: `internal/onvif/server.go` — the transport itself

**Files:**
- Create: `internal/onvif/server.go`
- Test: `internal/onvif/server_test.go`

**Interfaces:**
- Consumes: `media.Media` (`github.com/0x524a/camfarm/internal/media`: `.Codec`, `.Width`, `.Height`, `.FPS`), `seed.Seed`, everything from Tasks 2-4.
- Produces: `Config{Host, Port string/int, Log *slog.Logger, Cameras []CameraConfig}`, `CameraConfig{ID string, Media *media.Media, RTSPURL string, Username, Password string, Seed seed.Seed}`, `New(cfg Config) (*Server, error)`, `(*Server) Start() error`, `(*Server) Close()`, `(*Server) Addr() *net.TCPAddr`, `(*Server) Endpoint(cameraID string) string`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/onvif/server_test.go
package onvif

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/seed"
)

func testMedia() *media.Media {
	return &media.Media{Codec: media.CodecH264, Width: 320, Height: 240, FPS: 30}
}

func startTestServer(t *testing.T, cams ...CameraConfig) *Server {
	t.Helper()
	srv, err := New(Config{Host: "127.0.0.1", Cameras: cams})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func soapRequest(t *testing.T, url, action string) *http.Response {
	t.Helper()
	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<` + action + ` xmlns="http://www.onvif.org/ver10/device/wsdl"/>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(url, soapContentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestServerAnswersGetDeviceInformation(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	resp := soapRequest(t, srv.Endpoint("cam")+"/device", "GetDeviceInformation")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(out, []byte("camfarm")) {
		t.Errorf("body = %q, want manufacturer camfarm", out)
	}
}

func TestServerUnknownCameraReturns404(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	resp, err := http.Get(srv.Endpoint("nope") + "/device")
	if err != nil {
		// Endpoint("nope") returns "" for an unknown camera, so build the URL
		// by hand against the real bound address instead.
		resp, err = http.Get("http://" + srv.Addr().String() + "/onvif/nope/device")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestServerUnknownActionReturnsFault(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	resp := soapRequest(t, srv.Endpoint("cam")+"/device", "GetPresets")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (a fault envelope)", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(out, []byte("soap:Fault")) {
		t.Errorf("body = %q, want a SOAP fault", out)
	}
}

func TestServerCredentialedCameraChallenges(t *testing.T) {
	srv := startTestServer(t, CameraConfig{
		ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam",
		Username: "admin", Password: "secret", Seed: seed.Seed(1),
	})

	resp := soapRequest(t, srv.Endpoint("cam")+"/device", "GetDeviceInformation")
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate header")
	}
}

func TestServerTwoCamerasIndependentEndpoints(t *testing.T) {
	srv := startTestServer(t,
		CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)},
		CameraConfig{ID: "b", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/b", Seed: seed.Seed(2)},
	)

	if srv.Endpoint("a") == srv.Endpoint("b") {
		t.Fatal("two cameras got the same endpoint")
	}
	for _, id := range []string{"a", "b"} {
		resp := soapRequest(t, srv.Endpoint(id)+"/media", "GetProfiles")
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status = %d, want 200", id, resp.StatusCode)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/onvif/... -run TestServer -v`
Expected: FAIL — `Config`, `CameraConfig`, `New` undefined.

- [ ] **Step 3: Implement**

```go
// internal/onvif/server.go

// Package onvif serves the ONVIF Device and Media control plane for a
// camfarm fleet.
//
// One onvif-go *server.Server per camera holds that camera's profile and
// stream state; camfarm owns the net.Listener, the http.ServeMux, and every
// byte of the SOAP transport (envelope.go, auth.go, dispatch.go) — onvif-go's
// own Start(ctx) and soap.Handler.ServeHTTP are never called. See
// docs/superpowers/specs/2026-09-09-onvif-control-plane-design.md.
package onvif

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	onvifserver "github.com/0x524a/onvif-go/server"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/seed"
)

// profileToken is the single media profile every camera in this slice
// exposes. Multi-profile cameras are out of scope for slice 1.
const profileToken = "profile1"

// CameraConfig describes one camera's ONVIF endpoint.
type CameraConfig struct {
	ID       string
	Media    *media.Media
	RTSPURL  string
	Username string
	Password string
	Seed     seed.Seed
}

// Config configures the ONVIF transport for a fleet.
type Config struct {
	// Host defaults to 127.0.0.1.
	Host string
	// Port of zero requests an ephemeral port, readable back via Addr.
	Port int
	// Log receives server events. Nil means silent.
	Log *slog.Logger

	Cameras []CameraConfig
}

type camera struct {
	id     string
	srv    *onvifserver.Server
	digest *digestAuth // nil means open
}

// Server serves the ONVIF control plane for a fleet, behind one HTTP
// listener shared by every camera.
type Server struct {
	cfg Config
	log *slog.Logger
	mux *http.ServeMux

	mu   sync.RWMutex
	ln   net.Listener
	http *http.Server
	cams map[string]*camera
}

// New validates cfg and builds one onvif-go *server.Server per camera.
func New(cfg Config) (*Server, error) {
	if len(cfg.Cameras) == 0 {
		return nil, fmt.Errorf("onvif: no cameras configured")
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &Server{cfg: cfg, log: log, cams: make(map[string]*camera, len(cfg.Cameras))}

	for _, cc := range cfg.Cameras {
		if cc.ID == "" {
			return nil, fmt.Errorf("onvif: camera with an empty ID")
		}
		if _, dup := s.cams[cc.ID]; dup {
			return nil, fmt.Errorf("onvif: duplicate camera ID %q", cc.ID)
		}
		if cc.Media == nil {
			return nil, fmt.Errorf("onvif: camera %q has no media", cc.ID)
		}

		srv, err := onvifserver.New(&onvifserver.Config{
			DeviceInfo: onvifserver.DeviceInfo{
				Manufacturer:    "camfarm",
				Model:           "synthetic",
				FirmwareVersion: "0.0.0",
				SerialNumber:    cc.ID,
				HardwareID:      cc.ID,
			},
			SupportPTZ:     false,
			SupportImaging: false,
			SupportEvents:  false,
			Output:         io.Discard,
			Profiles: []onvifserver.ProfileConfig{
				{
					Token: profileToken,
					Name:  cc.ID,
					VideoSource: onvifserver.VideoSourceConfig{
						Token:      profileToken + "_source",
						Name:       cc.ID,
						Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
						Framerate:  int(cc.Media.FPS),
						Bounds:     onvifserver.Bounds{Width: cc.Media.Width, Height: cc.Media.Height},
					},
					VideoEncoder: onvifserver.VideoEncoderConfig{
						Encoding:   string(cc.Media.Codec),
						Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
						Framerate:  int(cc.Media.FPS),
					},
					Snapshot: onvifserver.SnapshotConfig{Enabled: false},
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("onvif: building server for %q: %w", cc.ID, err)
		}
		if err := srv.UpdateStreamURI(profileToken, cc.RTSPURL); err != nil {
			return nil, fmt.Errorf("onvif: wiring stream URI for %q: %w", cc.ID, err)
		}

		cam := &camera{id: cc.ID, srv: srv}
		if cc.Username != "" && cc.Password != "" {
			cam.digest = newDigestAuth(cc.Username, cc.Password, cc.Seed.Stream("onvif-auth"))
		}
		s.cams[cc.ID] = cam
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/onvif/", s.handle)
	return s, nil
}

// Start binds the listener and begins serving.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("onvif: listen: %w", err)
	}
	httpSrv := &http.Server{Handler: s.mux}

	s.mu.Lock()
	s.ln = ln
	s.http = httpSrv
	s.mu.Unlock()

	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Warn("onvif server stopped", "err", err)
		}
	}()

	s.log.Info("onvif server started", "addr", ln.Addr().String(), "cameras", len(s.cams))
	return nil
}

// Close stops the server. Safe to call more than once.
func (s *Server) Close() {
	s.mu.Lock()
	httpSrv := s.http
	s.http = nil
	s.mu.Unlock()

	if httpSrv != nil {
		_ = httpSrv.Close()
	}
}

// Addr returns the bound address, or nil before a successful Start.
func (s *Server) Addr() *net.TCPAddr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return nil
	}
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return addr
}

// Endpoint returns the base ONVIF URL for a camera (e.g.
// "http://127.0.0.1:PORT/onvif/cam"), or "" if the camera is unknown or
// before a successful Start.
func (s *Server) Endpoint(cameraID string) string {
	addr := s.Addr()
	if addr == nil {
		return ""
	}
	s.mu.RLock()
	_, ok := s.cams[cameraID]
	s.mu.RUnlock()
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s/onvif/%s", addr.String(), cameraID)
}

func (s *Server) deviceURL(id string) string { return s.Endpoint(id) + "/device" }
func (s *Server) mediaURL(id string) string  { return s.Endpoint(id) + "/media" }

func (s *Server) cameraByID(id string) (*camera, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cam, ok := s.cams[id]
	return cam, ok
}

// parsePath parses "/onvif/{cameraID}/{service}" into its parts.
func parsePath(p string) (cameraID, service string, ok bool) {
	p = strings.TrimPrefix(p, "/onvif/")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// handle routes one HTTP request to its camera, gates it behind Digest auth
// when the camera is credentialed, and dispatches the SOAP action inside.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	id, service, ok := parsePath(r.URL.Path)
	if !ok || (service != "device" && service != "media") {
		http.NotFound(w, r)
		return
	}
	cam, ok := s.cameraByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cam.digest != nil && !cam.digest.verify(r) {
		cam.digest.challenge(w)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeFault(w, "Client", "failed to read request body")
		return
	}

	actionName, payload, err := parseAction(raw)
	if err != nil {
		writeFault(w, "Client", "invalid SOAP envelope")
		return
	}

	h, ok := handlers[actionName]
	if !ok {
		writeFault(w, "Client", "Action not supported")
		return
	}

	resp, err := h(cam.srv, payload)
	if err != nil {
		writeFault(w, "Server", err.Error())
		return
	}

	rewriteXAddr(resp, s.deviceURL(id), s.mediaURL(id))
	writeResponse(w, resp)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/onvif/... -v`
Expected: PASS (every test from Tasks 2-5)

- [ ] **Step 5: Commit**

```bash
git add internal/onvif/server.go internal/onvif/server_test.go
git commit -m "Add the ONVIF transport server: listener, routing, per-camera dispatch"
```

---

## Task 6: Wire `internal/onvif` into `camfarm.go`

**Files:**
- Modify: `camfarm.go`
- Test: `onvif_test.go` (new file, package `camfarm`)

**Interfaces:**
- Consumes: `onvif.Config`, `onvif.CameraConfig`, `onvif.New`, `(*onvif.Server) Start/Close/Addr/Endpoint` from Task 5.
- Produces: `func (*Camera) ONVIFEndpoint() string`.

- [ ] **Step 1: Write the failing test**

```go
// onvif_test.go
package camfarm

import (
	"net/http"
	"strings"
	"testing"
)

func TestONVIFEndpointServesGetDeviceInformation(t *testing.T) {
	f := StartT(t, Spec{Seed: 1, Cameras: []CameraSpec{{ID: "front-door"}}})

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	endpoint := cam.ONVIFEndpoint()
	if endpoint == "" {
		t.Fatal("ONVIFEndpoint is empty")
	}

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><ProfileToken>profile1</ProfileToken></GetStreamUri>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(endpoint+"/media", "application/soap+xml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCloseStopsONVIFServer(t *testing.T) {
	f, err := Start(nil, Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cam, _ := f.Camera("a")
	endpoint := cam.ONVIFEndpoint()

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := http.Get(endpoint + "/device"); err == nil {
		t.Fatal("ONVIF server still answering after Close")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run "TestONVIFEndpoint|TestCloseStopsONVIF" -v`
Expected: FAIL — `ONVIFEndpoint` undefined.

- [ ] **Step 3: Implement**

In `camfarm.go`, add the import:

```go
"github.com/0x524a/camfarm/internal/onvif"
```

Add a field to `Fleet`:

```go
type Fleet struct {
	spec     Spec
	specHash string
	log      *slog.Logger
	rec      *obs.Recorder
	srv      *rtsp.Server
	onvifSrv *onvif.Server

	mu        sync.Mutex
	closed    bool
	cameras   map[string]*Camera
	order     []string
	stopWatch func()
}
```

In `Start`, after `f.srv = srv` and before the `if ctx != nil` block, add:

```go
onvifCfg := onvif.Config{
	Host: valid.Listen.Host,
	Port: valid.Listen.ONVIFPort,
	Log:  log,
}
for i, cs := range valid.Cameras {
	cam := f.cameras[cs.ID]
	onvifCfg.Cameras = append(onvifCfg.Cameras, onvif.CameraConfig{
		ID:       cs.ID,
		Media:    cam.media,
		RTSPURL:  srv.URL(cs.ID),
		Username: cs.Auth.Username,
		Password: cs.Auth.Password,
		Seed:     cam.seed,
	})
	_ = i
}

onvifSrv, err := onvif.New(onvifCfg)
if err != nil {
	srv.Close()
	return nil, err
}
if err := onvifSrv.Start(); err != nil {
	srv.Close()
	return nil, err
}
f.onvifSrv = onvifSrv
```

(The `_ = i` discards the loop index, unused here — the camera is already looked up by ID from `f.cameras`, which was populated in the earlier loop.)

In `Close`, after `f.srv.Close()`, add:

```go
f.onvifSrv.Close()
```

Add the accessor near `RTSPURL`:

```go
// ONVIFEndpoint returns the camera's ONVIF base URL, e.g.
// "http://127.0.0.1:PORT/onvif/front-door". Append "/device" or "/media" for
// the two services this version serves.
func (c *Camera) ONVIFEndpoint() string { return c.fleet.onvifSrv.Endpoint(c.id) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run "TestONVIFEndpoint|TestCloseStopsONVIF" -v`
Expected: PASS

- [ ] **Step 5: Run the full existing suite to check for regressions**

Run: `go test ./...`
Expected: PASS (nothing in Tasks 1-6 should touch RTSP or media behavior)

- [ ] **Step 6: Commit**

```bash
git add camfarm.go onvif_test.go
git commit -m "Wire the ONVIF transport into Fleet.Start/Close and add Camera.ONVIFEndpoint"
```

---

## Task 7: Integration tests — every slice-1 operation, at the `camfarm` level

**Files:**
- Modify: `onvif_test.go`

**Interfaces:**
- Consumes: everything from Task 6.

- [ ] **Step 1: Write the failing tests**

```go
// Append to onvif_test.go

func soapPost(t *testing.T, url, ns, action, innerXML string) (*http.Response, []byte) {
	t.Helper()
	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<` + action + ` xmlns="` + ns + `">` + innerXML + `</` + action + `>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(url, "application/soap+xml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return resp, out
}

func TestEveryDeviceAndMediaOperation(t *testing.T) {
	f := StartT(t, Spec{Seed: 7, Cameras: []CameraSpec{{ID: "cam"}}})
	cam, err := f.Camera("cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	endpoint := cam.ONVIFEndpoint()
	const deviceNS = "http://www.onvif.org/ver10/device/wsdl"
	const mediaNS = "http://www.onvif.org/ver10/media/wsdl"

	cases := []struct {
		name, path, ns, action, inner, want string
	}{
		{"GetDeviceInformation", "/device", deviceNS, "GetDeviceInformation", "", "camfarm"},
		{"GetCapabilities", "/device", deviceNS, "GetCapabilities", "", endpoint + "/device"},
		{"GetSystemDateAndTime", "/device", deviceNS, "GetSystemDateAndTime", "", "UTCDateTime"},
		{"GetServices", "/device", deviceNS, "GetServices", "", endpoint + "/media"},
		{"SystemReboot", "/device", deviceNS, "SystemReboot", "", "SystemRebootResponse"},
		{"GetProfiles", "/media", mediaNS, "GetProfiles", "", "profile1"},
		{"GetVideoSources", "/media", mediaNS, "GetVideoSources", "", "GetVideoSourcesResponse"},
		{"GetSnapshotUri", "/media", mediaNS, "GetSnapshotUri", "<ProfileToken>profile1</ProfileToken>", "GetSnapshotUriResponse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := soapPost(t, endpoint+tc.path, tc.ns, tc.action, tc.inner)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body = %s, want it to contain %q", body, tc.want)
			}
		})
	}
}

func TestGetStreamUriReturnsCamfarmsRealURL(t *testing.T) {
	f := StartT(t, Spec{Seed: 7, Cameras: []CameraSpec{{ID: "cam"}}})
	cam, err := f.Camera("cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}

	resp, body := soapPost(t, cam.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), cam.RTSPURL()) {
		t.Errorf("body = %s, want it to contain the real RTSP URL %q", body, cam.RTSPURL())
	}
}
```

Add `"io"` to the import block in `onvif_test.go` if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run "TestEveryDeviceAndMediaOperation|TestGetStreamUriReturnsCamfarmsRealURL" -v`
Expected: FAIL initially only if a typo/mismatch exists — since Task 6 already wired everything, this should mostly pass immediately once compiled; treat any failure as a real bug to fix, not an expected-red step.

- [ ] **Step 3: Fix any failures found**

If a subtest fails, the likely causes are: an XAddr not rewritten (check `rewriteXAddr` covers that response type), or a wire action name mismatch (check `handlers` keys against the case's `action` field exactly, including case-sensitivity).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run "TestEveryDeviceAndMediaOperation|TestGetStreamUriReturnsCamfarmsRealURL" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add onvif_test.go
git commit -m "Add integration tests for every ONVIF Device and Media operation"
```

---

## Task 8: Integration tests — Digest at the fleet level, determinism, mixed fleet

**Files:**
- Modify: `onvif_test.go`

**Interfaces:**
- Consumes: everything from Tasks 6-7.

- [ ] **Step 1: Write the failing tests**

```go
// Append to onvif_test.go

func digestAuthorizationHeader(t *testing.T, username, password, method, uri, nonce string) string {
	t.Helper()
	h := md5.Sum([]byte(username + ":camfarm:" + password))
	ha1 := hex.EncodeToString(h[:])
	h2 := md5.Sum([]byte(method + ":" + uri))
	ha2 := hex.EncodeToString(h2[:])
	nc, cnonce := "00000001", "clienttest"
	resp := md5.Sum([]byte(strings.Join([]string{ha1, nonce, nc, cnonce, "auth", ha2}, ":")))
	response := hex.EncodeToString(resp[:])
	return fmt.Sprintf(`Digest username="%s", realm="camfarm", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		username, nonce, uri, nc, cnonce, response)
}

func extractWWWAuthNonce(t *testing.T, header string) string {
	t.Helper()
	const key = `nonce="`
	i := strings.Index(header, key)
	if i < 0 {
		t.Fatalf("WWW-Authenticate %q has no nonce", header)
	}
	rest := header[i+len(key):]
	return rest[:strings.IndexByte(rest, '"')]
}

func TestFleetLevelDigestFlow(t *testing.T) {
	f := StartT(t, Spec{Seed: 9, Cameras: []CameraSpec{
		{ID: "cam", Auth: AuthSpec{Username: "admin", Password: "secret"}},
	}})
	cam, err := f.Camera("cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	url := cam.ONVIFEndpoint() + "/device"
	uri := strings.TrimPrefix(url, "http://"+strings.SplitN(url, "/", 4)[2])

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/></soap:Body></soap:Envelope>`

	// No Authorization header: challenged.
	resp, err := http.Post(url, "application/soap+xml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	nonce := extractWWWAuthNonce(t, resp.Header.Get("WWW-Authenticate"))

	// Correct credentials against that nonce: succeeds.
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/soap+xml")
	req.Header.Set("Authorization", digestAuthorizationHeader(t, "admin", "secret", "POST", uri, nonce))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST with auth: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		out, _ := io.ReadAll(resp2.Body)
		t.Fatalf("status = %d, want 200\n%s", resp2.StatusCode, out)
	}

	// Wrong password against a fresh nonce: challenged again, not 200.
	req3, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req3.Header.Set("Content-Type", "application/soap+xml")
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()
	nonce2 := extractWWWAuthNonce(t, resp3.Header.Get("WWW-Authenticate"))

	req4, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req4.Header.Set("Content-Type", "application/soap+xml")
	req4.Header.Set("Authorization", digestAuthorizationHeader(t, "admin", "wrong", "POST", uri, nonce2))
	resp4, _ := http.DefaultClient.Do(req4)
	defer resp4.Body.Close()
	if resp4.StatusCode != 401 {
		t.Errorf("status = %d, want 401 for wrong password", resp4.StatusCode)
	}
}

func TestSameSeedReproducesNonceSequence(t *testing.T) {
	spec := Spec{Seed: 0x1234, Cameras: []CameraSpec{
		{ID: "cam", Auth: AuthSpec{Username: "admin", Password: "secret"}},
	}}

	firstNonce := func() string {
		f := StartT(t, spec)
		cam, _ := f.Camera("cam")
		resp, err := http.Post(cam.ONVIFEndpoint()+"/device", "application/soap+xml", strings.NewReader(""))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		return extractWWWAuthNonce(t, resp.Header.Get("WWW-Authenticate"))
	}

	n1 := firstNonce()
	n2 := firstNonce()
	if n1 != n2 {
		t.Errorf("nonce = %q then %q, want equal for the same seed", n1, n2)
	}
}

func TestMixedFleetGetStreamUriPerCamera(t *testing.T) {
	h265Path := filepath.Join(t.TempDir(), "h265.ts")
	if err := os.WriteFile(h265Path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write H265 fixture: %v", err)
	}

	f := StartT(t, Spec{Seed: 11, Cameras: []CameraSpec{
		{ID: "open-h264"},
		{ID: "auth-h264", Auth: AuthSpec{Username: "admin", Password: "secret"}},
		{ID: "hevc-cam", Source: SourceSpec{Kind: SourceFile, Path: h265Path}},
	}})

	for _, id := range []string{"open-h264", "auth-h264", "hevc-cam"} {
		cam, err := f.Camera(id)
		if err != nil {
			t.Fatalf("Camera(%q): %v", id, err)
		}
		if cam.ONVIFEndpoint() == "" {
			t.Errorf("%s: empty ONVIFEndpoint", id)
		}
	}

	// The open camera answers GetStreamUri directly with its own URL.
	open, _ := f.Camera("open-h264")
	resp, body := soapPost(t, open.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 || !strings.Contains(string(body), open.RTSPURL()) {
		t.Errorf("open-h264: status=%d body=%s, want 200 containing %q", resp.StatusCode, body, open.RTSPURL())
	}

	// The credentialed camera refuses the same unauthenticated request.
	auth, _ := f.Camera("auth-h264")
	resp2, _ := soapPost(t, auth.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp2.StatusCode != 401 {
		t.Errorf("auth-h264: status = %d, want 401", resp2.StatusCode)
	}
}
```

Add `"crypto/md5"`, `"encoding/hex"`, `"fmt"`, `"os"`, `"path/filepath"` to `onvif_test.go`'s import block as needed (some may already be imported by `integration_test.go` in the same package, but each file needs its own import list).

- [ ] **Step 2: Run tests to verify current state**

Run: `go test ./... -run "TestFleetLevelDigestFlow|TestSameSeedReproducesNonceSequence|TestMixedFleetGetStreamUriPerCamera" -v`
Expected: PASS if Tasks 1-7 are correct. Treat any failure as a real bug (most likely: the `uri` field in the test's Digest computation not matching the request path camfarm's `verify` reconstructs — double check both use the same path, no query string, no trailing slash mismatch).

- [ ] **Step 3: Fix any failures, then re-run**

Run: `go test ./... -v`
Expected: PASS, whole suite green.

- [ ] **Step 4: Commit**

```bash
git add onvif_test.go
git commit -m "Add fleet-level Digest, determinism, and mixed-fleet ONVIF tests"
```

---

## Task 9: README — ONVIF® naming, trademark disclaimer, scope update

**Files:**
- Modify: `README.md`

**Interfaces:** none (documentation only).

- [ ] **Step 1: Update the "Status" section**

Replace the "Not built yet" bullet's mention of "the camera control plane" with a narrowed statement, and add what is now built. Change:

```markdown
- **Not built yet:** the camera control plane, device discovery, and every
  fault's *effect*. The fault catalogue is defined and validated, and the seams
  it will act through are in place, but no fault changes a byte on the wire. A
  spec that names a fault is **refused**, not silently ignored.
```

to:

```markdown
- **Works:** an ONVIF® Device Service and Media Service control plane —
  *not ONVIF conformant* (see below) — behind one HTTP listener, with HTTP
  Digest authentication when a camera's spec sets credentials.
- **Not built yet:** device discovery (WS-Discovery), PTZ, Imaging, and every
  fault's *effect*. The fault catalogue is defined and validated, and the seams
  it will act through are in place, but no fault changes a byte on the wire. A
  spec that names a fault is **refused**, not silently ignored.
```

- [ ] **Step 2: Add the trademark disclaimer near the first ONVIF mention**

Directly below the new "Works" bullet, add:

```markdown
> Implements/supports the ONVIF® Device Service and Media Service
> specifications. This project is **not ONVIF conformant**, is not tested
> against the ONVIF conformance suite, and makes no claim to any ONVIF
> profile or add-on.
```

- [ ] **Step 3: Extend the "Scope — what this does not do" section**

Add a new bullet after the existing "Container support is deliberately narrow" bullet:

```markdown
- **The ONVIF control plane covers two services only.** Device and Media —
  enough for a client to discover a camera and get back its real RTSP URL.
  PTZ and Imaging are not implemented: `GetCapabilities`/`GetServices` never
  advertise them, so a conformant client should not expect them. WS-Discovery
  (multicast device discovery) is not implemented; a client must be given the
  ONVIF endpoint directly (`Camera.ONVIFEndpoint()`).
```

- [ ] **Step 4: Verify the README renders sensibly**

Run: `grep -c "ONVIF" README.md` and read the file once to confirm the first "ONVIF" occurrence carries the `®` symbol and the disclaimer sits immediately below it, per `CLAUDE.md`'s "not ONVIF conformant" requirement.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "Document the ONVIF Device/Media control plane in the README"
```

---

## Self-Review Notes

- **Spec coverage:** Every numbered item in §0 of the design doc has a task: Device service (Task 4/5/7), Media service (Task 4/5/7), transport (Tasks 2/3/5), public API additions (Tasks 1/6), naming (Task 9). §0.1's deferrals (PTZ, Imaging, WS-Discovery, fault effects, WS-Security UsernameToken) are respected — no task adds any of them.
- **Testing strategy (§5):** real HTTP + hand-built SOAP envelopes only, at every level (Tasks 5, 6, 7, 8) — no task calls an onvif-go `Handle*` method directly from a test file outside `internal/onvif`'s own unit tests, which call it as the production dispatcher does (through the same `[]byte` seam), not as a shortcut around the transport.
- **Type consistency:** `CameraConfig`, `Config`, `Server.Endpoint`, `handlers`, `rewriteXAddr`, `digestAuth`, `AuthSpec` are defined once (Tasks 1, 3, 4, 5) and referenced identically in every later task.
- **XAddr rewriting** is exercised both as a unit (Task 4) and end-to-end (Task 7's `GetCapabilities`/`GetServices` cases assert the rewritten URL, not onvif-go's own default).

