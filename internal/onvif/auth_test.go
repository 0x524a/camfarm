// internal/onvif/auth_test.go
package onvif

import (
	"fmt"
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
