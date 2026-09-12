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
