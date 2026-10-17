package onvif

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x524a/onvif-go/discovery"

	"github.com/0x524a/camfarm/internal/seed"
)

const probeTimeout = 300 * time.Millisecond

func discoCam(id string, n int64) CameraConfig {
	return CameraConfig{ID: id, Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/" + id, Seed: seed.Seed(n)}
}

func newDiscoServer(t *testing.T, cams ...CameraConfig) *Server {
	t.Helper()
	srv, err := New(Config{Host: "127.0.0.1", Cameras: cams, Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// probe sends one unicast probe and returns what answered, keyed by endpoint
// ref. The responder replies immediately, so a short window is enough.
func probe(t *testing.T, addr net.Addr) map[string]*discovery.Device {
	t.Helper()
	devs, err := discovery.DiscoverWithOptions(context.Background(), probeTimeout,
		&discovery.DiscoverOptions{ProbeAddress: addr.String()})
	if err != nil {
		t.Fatalf("DiscoverWithOptions: %v", err)
	}
	out := make(map[string]*discovery.Device, len(devs))
	for _, d := range devs {
		out[d.EndpointRef] = d
	}
	return out
}

// authedDeviceInfo issues GetDeviceInformation with Digest credentials.
func authedDeviceInfo(t *testing.T, url, user, pass string) *http.Response {
	t.Helper()
	first := soapRequest(t, url, "GetDeviceInformation")
	_ = first.Body.Close()
	if first.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", first.StatusCode)
	}
	nonce := extractNonce(t, first.Header.Get("WWW-Authenticate"))
	path := strings.TrimPrefix(url, "http://")
	path = path[strings.Index(path, "/"):]

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/></soap:Body></soap:Envelope>`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", soapContentType)
	req.Header.Set("Authorization", digestAuthorization(user, pass, "POST", path, authRealm, nonce))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated POST: %v", err)
	}
	return resp
}

func TestDiscoveryAdvertisesEveryCamera(t *testing.T) {
	cams := []CameraConfig{discoCam("a", 1), discoCam("b", 2), discoCam("c", 3)}
	cams[2].Username, cams[2].Password = "admin", "secret"
	srv := newDiscoServer(t, cams...)

	if srv.DiscoveryAddr() == nil {
		t.Fatal("DiscoveryAddr nil after Start")
	}
	found := probe(t, srv.DiscoveryAddr())
	if len(found) != 3 {
		t.Fatalf("found %d devices, want 3: %v", len(found), found)
	}
	for _, cc := range cams {
		ref := "urn:uuid:" + EndpointUUID(cc.Seed)
		d, ok := found[ref]
		if !ok {
			t.Fatalf("camera %s (%s) not discovered", cc.ID, ref)
		}
		want := srv.Endpoint(cc.ID) + "/device"
		if len(d.XAddrs) != 1 || d.XAddrs[0] != want {
			t.Errorf("%s XAddrs = %v, want [%s]", cc.ID, d.XAddrs, want)
		}
		// Real bound ONVIF address: never port 0, and no embedded credentials.
		if strings.Contains(d.XAddrs[0], ":0/") || strings.Contains(d.XAddrs[0], "@") {
			t.Errorf("%s XAddr %q has port 0 or credentials", cc.ID, d.XAddrs[0])
		}

		var resp *http.Response
		if cc.Username != "" {
			unauth := soapRequest(t, d.XAddrs[0], "GetDeviceInformation")
			_ = unauth.Body.Close()
			if unauth.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s unauthenticated status = %d, want 401", cc.ID, unauth.StatusCode)
			}
			resp = authedDeviceInfo(t, d.XAddrs[0], cc.Username, cc.Password)
		} else {
			resp = soapRequest(t, d.XAddrs[0], "GetDeviceInformation")
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), cc.ID) {
			t.Errorf("%s GetDeviceInformation via XAddr: status %d body %q", cc.ID, resp.StatusCode, body)
		}
	}
}

func TestDiscoveryTracksAddAndRemove(t *testing.T) {
	srv := newDiscoServer(t, discoCam("a", 1), discoCam("b", 2), discoCam("c", 3))

	if err := srv.AddCamera(discoCam("d", 4)); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	found := probe(t, srv.DiscoveryAddr())
	d, ok := found["urn:uuid:"+EndpointUUID(seed.Seed(4))]
	if len(found) != 4 || !ok {
		t.Fatalf("after add: found %d, d present=%v", len(found), ok)
	}
	if d.XAddrs[0] != srv.Endpoint("d")+"/device" {
		t.Errorf("added camera XAddr = %v", d.XAddrs)
	}

	if err := srv.RemoveCamera("b"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	found = probe(t, srv.DiscoveryAddr())
	if _, still := found["urn:uuid:"+EndpointUUID(seed.Seed(2))]; still || len(found) != 3 {
		t.Fatalf("after remove: found %d, removed camera still present=%v", len(found), still)
	}

	// A failed AddCamera must leave no phantom device behind.
	if err := srv.AddCamera(discoCam("a", 9)); err == nil {
		t.Fatal("duplicate AddCamera: want error")
	}
	bad := discoCam("e", 5)
	bad.Media = nil
	if err := srv.AddCamera(bad); err == nil {
		t.Fatal("AddCamera without media: want error")
	}
	found = probe(t, srv.DiscoveryAddr())
	if len(found) != 3 {
		t.Fatalf("after failed adds: found %d, want 3", len(found))
	}
}

func TestDiscoveryRemoveUnknownDoesNotPanic(t *testing.T) {
	srv := newDiscoServer(t, discoCam("a", 1))
	if err := srv.RemoveCamera("nope"); err == nil {
		t.Fatal("RemoveCamera unknown: want error")
	}
	// Add then remove, then remove again (never registered any more).
	if err := srv.AddCamera(discoCam("x", 7)); err != nil {
		t.Fatal(err)
	}
	if err := srv.RemoveCamera("x"); err != nil {
		t.Fatal(err)
	}
	if err := srv.RemoveCamera("x"); err == nil {
		t.Fatal("second RemoveCamera: want error")
	}
}

// closed reports whether ch is closed, without blocking.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// responderOf returns the server's current responder state (nil if none).
func responderOf(srv *Server) *responderState {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.responder
}

func TestDiscoveryCloseStopsResponderAndReleasesSocket(t *testing.T) {
	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	st := responderOf(srv)
	addr := srv.DiscoveryAddr()
	if got := probe(t, addr); len(got) != 1 {
		t.Fatalf("before Close found %d, want 1", len(got))
	}

	srv.Close()
	srv.Close() // idempotent

	// Scoped to this server's responder, not a process-wide goroutine count
	// that other parallel tests would disturb.
	if !closed(st.done) {
		t.Error("responder goroutine still running after Close returned")
	}
	if srv.DiscoveryAddr() != nil {
		t.Error("DiscoveryAddr non-nil after Close")
	}
	if got := probe(t, addr); len(got) != 0 {
		t.Errorf("after Close found %d devices, want none", len(got))
	}
	// The socket is released: the same UDP port can be bound again.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.ListenUDP("udp", addr.(*net.UDPAddr))
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery port still bound after Close: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentCloseWaitsForStop(t *testing.T) {
	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	st := responderOf(srv)

	const n = 8
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			srv.Close()
			// Every caller, not just the first, returns only once stopped.
			if !closed(st.done) {
				t.Error("Close returned before the responder stopped")
			}
			if srv.Addr() != nil || srv.DiscoveryAddr() != nil {
				t.Error("server still reports addresses after Close returned")
			}
		}()
	}
	close(gate)
	wg.Wait()
}

// freeUDPAddr returns a loopback UDP address that was free a moment ago.
func freeUDPAddr(t *testing.T) string {
	t.Helper()
	c := localUDP(t)
	addr := c.LocalAddr().String()
	_ = c.Close()
	return addr
}

func TestCloseDuringStartAbortsCleanly(t *testing.T) {
	addr := freeUDPAddr(t)
	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{ListenAddr: addr}})
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan struct{})
	srv.startWindowHook = func() {
		go func() { srv.Close(); close(closeDone) }()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			srv.mu.RLock()
			aborted := srv.startAborted
			srv.mu.RUnlock()
			if aborted {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("Close never registered itself against the in-flight Start")
	}

	if err := srv.Start(); err == nil {
		t.Fatal("Start raced by Close: want an error, got success")
	}
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	if srv.Addr() != nil || srv.DiscoveryAddr() != nil || responderOf(srv) != nil {
		t.Error("aborted Start left state behind")
	}
	// No leak: the discovery port is free again, and the server restarts.
	c, err := net.ListenUDP("udp", mustUDPAddr(t, addr))
	if err != nil {
		t.Fatalf("discovery port still bound after aborted Start: %v", err)
	}
	_ = c.Close()
	srv.startWindowHook = nil
	if err := srv.Start(); err != nil {
		t.Fatalf("Start after an aborted Start: %v", err)
	}
	srv.Close()
}

func mustUDPAddr(t *testing.T, s string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Start and Close racing without a hook: whichever order wins, the server ends
// up fully stopped and nothing deadlocks (run under -race).
func TestStartCloseRaceLeavesNothingRunning(t *testing.T) {
	for i := 0; i < 20; i++ {
		srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1)},
			Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"}})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = srv.Start() }()
		go func() { defer wg.Done(); srv.Close() }()
		wg.Wait()
		st := responderOf(srv)
		srv.Close()
		if st != nil && !closed(st.done) {
			t.Fatal("responder still running after Close")
		}
		if srv.Addr() != nil || srv.DiscoveryAddr() != nil {
			t.Fatal("server not fully stopped")
		}
	}
}

// stubRegistrar fails Register on demand and records calls.
type stubRegistrar struct {
	failRegister bool
	registered   []string
	unregistered []string
}

func (r *stubRegistrar) Register(d *discovery.Device) error {
	if r.failRegister {
		return errors.New("stub: register failed")
	}
	r.registered = append(r.registered, d.EndpointRef)
	return nil
}
func (r *stubRegistrar) Unregister(ref string) { r.unregistered = append(r.unregistered, ref) }
func (r *stubRegistrar) Addr() net.Addr        { return nil }

func TestAddCameraRollsBackWhenRegisterFails(t *testing.T) {
	srv := startTestServer(t, discoCam("a", 1))
	stub := &stubRegistrar{failRegister: true}
	doneCh := make(chan struct{})
	close(doneCh)
	srv.mu.Lock()
	srv.responder = &responderState{resp: stub, cancel: func() {}, done: doneCh, errCh: make(chan error, 1)}
	srv.mu.Unlock()
	t.Cleanup(func() {
		srv.mu.Lock()
		srv.responder = nil // the stub has no real responder to stop
		srv.mu.Unlock()
	})

	if err := srv.AddCamera(discoCam("b", 2)); err == nil {
		t.Fatal("AddCamera with a failing Register: want error")
	}
	if _, ok := srv.cameraByID("b"); ok {
		t.Error("failed AddCamera left the camera in the fleet")
	}
	if srv.Endpoint("b") != "" {
		t.Error("failed AddCamera left an endpoint")
	}
	if len(stub.unregistered) != 0 {
		t.Errorf("nothing was registered, yet Unregister was called: %v", stub.unregistered)
	}

	// The ID is reusable afterwards: no stale state blocks a retry.
	stub.failRegister = false
	if err := srv.AddCamera(discoCam("b", 2)); err != nil {
		t.Fatalf("retry AddCamera: %v", err)
	}
	if len(stub.registered) != 1 {
		t.Errorf("registered = %v, want exactly the retried camera", stub.registered)
	}
}

func TestBaseURLEscapesZoneAndID(t *testing.T) {
	zoned := &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 8080, Zone: "eth0"}
	if got, want := baseURL(zoned, "", "cam"), "http://[fe80::1%25eth0]:8080/onvif/cam"; got != want {
		t.Errorf("zoned baseURL = %q, want %q", got, want)
	}
	plain := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 80}
	if got, want := baseURL(plain, "", "a b%c"), "http://192.0.2.7:80/onvif/a%20b%25c"; got != want {
		t.Errorf("escaped baseURL = %q, want %q", got, want)
	}
	// The result must parse back to the same host and zone.
	u, err := url.Parse(baseURL(zoned, "", "cam"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "fe80::1%eth0" {
		t.Errorf("parsed hostname = %q, want fe80::1%%eth0", u.Hostname())
	}
}

const (
	actionHelloURI = "http://schemas.xmlsoap.org/ws/2005/04/discovery/Hello"
	actionByeURI   = "http://schemas.xmlsoap.org/ws/2005/04/discovery/Bye"
	readWindow     = 3 * time.Second
)

func rawProbe(types string) string {
	typesEl := ""
	if types != "" {
		typesEl = `<d:Types>` + types + `</d:Types>`
	}
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" ` +
		`xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" ` +
		`xmlns:dn="http://www.onvif.org/ver10/network/wsdl" xmlns:x="urn:example:other">` +
		`<s:Header><a:MessageID>urn:uuid:11111111-1111-1111-1111-111111111111</a:MessageID>` +
		`<a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To>` +
		`<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action></s:Header>` +
		`<s:Body><d:Probe>` + typesEl + `</d:Probe></s:Body></s:Envelope>`
}

func localUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDiscoveryProbeTypeFiltering(t *testing.T) {
	srv := newDiscoServer(t, discoCam("a", 1))
	ref := "urn:uuid:" + EndpointUUID(seed.Seed(1))
	conn := localUDP(t)
	dst := srv.DiscoveryAddr().(*net.UDPAddr)
	buf := make([]byte, 65535)

	// Positive controls: the same envelope and socket, matching types and none.
	for _, types := range []string{"dn:NetworkVideoTransmitter", ""} {
		if _, err := conn.WriteToUDP([]byte(rawProbe(types)), dst); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(readWindow))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("probe with Types %q got no reply: %v", types, err)
		}
		if !strings.Contains(string(buf[:n]), ref) {
			t.Errorf("reply to Types %q lacks endpoint ref %s: %s", types, ref, buf[:n])
		}
	}

	// Negative: only the type differs, and now nothing answers.
	if _, err := conn.WriteToUDP([]byte(rawProbe("x:Thermostat")), dst); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(probeTimeout))
	if n, _, err := conn.ReadFromUDP(buf); err == nil {
		t.Fatalf("non-matching probe was answered: %s", buf[:n])
	}
}

// quietWindow is how long expectAnnounce listens after the expected datagrams
// for a stray one. A duplicate is sent back to back with the original, so a
// short window is enough to catch it; a miss can only hide a bug, never raise
// a false failure.
const quietWindow = 250 * time.Millisecond

// expectAnnounce reads datagrams from conn until every ref has been seen with
// the given action URI, failing on timeout. It fails on a second datagram for
// a ref already seen, on a datagram for any other ref or action, and on any
// datagram arriving in the quiet window afterwards, so each phase must be
// exactly one announcement per ref.
func expectAnnounce(t *testing.T, conn *net.UDPConn, action string, refs ...string) {
	t.Helper()
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		want[r] = true
	}
	buf := make([]byte, 65535)
	_ = conn.SetReadDeadline(time.Now().Add(readWindow))
	for len(want) > 0 {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("waiting for %s of %v: %v", action, want, err)
		}
		msg := string(buf[:n])
		if !strings.Contains(msg, "<a:Action>"+action+"</a:Action>") {
			t.Fatalf("expected %s, got: %s", action, msg)
		}
		matched := false
		for _, r := range refs {
			if strings.Contains(msg, "<a:Address>"+r+"</a:Address>") {
				if !want[r] {
					t.Fatalf("duplicate %s for %s", action, r)
				}
				delete(want, r)
				matched = true
			}
		}
		if !matched {
			t.Fatalf("%s for an unexpected ref: %s", action, msg)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(quietWindow))
	if n, _, err := conn.ReadFromUDP(buf); err == nil {
		t.Fatalf("extra datagram after %s of %v: %s", action, refs, buf[:n])
	}
}

// A camera added while Start is bringing the responder up must still be
// announced exactly once: the responder's own startup Hello and Register's
// Hello must not both cover it.
func TestDiscoveryAddDuringStartAnnouncesOnce(t *testing.T) {
	sink := localUDP(t)
	refOf := func(n int64) string { return "urn:uuid:" + EndpointUUID(seed.Seed(n)) }

	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1), discoCam("b", 2)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0", AnnounceAddr: sink.LocalAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	addErr := make(chan error, 1)
	srv.startWindowHook = func() {
		// Fires inside the window, so AddCamera must wait it out.
		go func() { addErr <- srv.AddCamera(discoCam("c", 3)) }()
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	select {
	case err := <-addErr:
		if err != nil {
			t.Fatalf("AddCamera during Start: %v", err)
		}
	case <-time.After(readWindow):
		t.Fatal("AddCamera never completed")
	}

	// Exactly one Hello each for the startup pair, then exactly one for c.
	// The two phases cannot be told apart on the wire, so check the union.
	expectAnnounce(t, sink, actionHelloURI, refOf(1), refOf(2), refOf(3))
	if _, ok := srv.cameraByID("c"); !ok {
		t.Fatal("camera c missing")
	}
	if got := probe(t, srv.DiscoveryAddr()); len(got) != 3 {
		t.Errorf("found %d devices, want 3", len(got))
	}
}

func TestDiscoveryAnnouncesHelloAndBye(t *testing.T) {
	// onvif-go's responder sends Hello at Start and on Register while running,
	// and Bye on Unregister and when stopped, to AnnounceAddr (unicast mode
	// included), so a test-owned socket sees all of them.
	sink := localUDP(t)
	refOf := func(n int64) string { return "urn:uuid:" + EndpointUUID(seed.Seed(n)) }

	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1), discoCam("b", 2)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0", AnnounceAddr: sink.LocalAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	expectAnnounce(t, sink, actionHelloURI, refOf(1), refOf(2))

	if err := srv.AddCamera(discoCam("c", 3)); err != nil {
		t.Fatal(err)
	}
	expectAnnounce(t, sink, actionHelloURI, refOf(3))

	if err := srv.RemoveCamera("b"); err != nil {
		t.Fatal(err)
	}
	expectAnnounce(t, sink, actionByeURI, refOf(2))

	srv.Close()
	expectAnnounce(t, sink, actionByeURI, refOf(1), refOf(3))
}

func TestDiscoveryRestartGetsFreshResponder(t *testing.T) {
	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1), discoCam("b", 2)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	first := srv.DiscoveryAddr().String()
	srv.Close()

	if err := srv.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer srv.Close()
	addr := srv.DiscoveryAddr()
	if addr == nil {
		t.Fatal("DiscoveryAddr nil after restart")
	}
	if addr.String() == first {
		t.Errorf("restart reused discovery address %s; want a fresh socket", first)
	}
	found := probe(t, addr)
	if len(found) != 2 {
		t.Fatalf("after restart found %d, want 2", len(found))
	}
	for id, d := range found {
		if !strings.HasPrefix(d.XAddrs[0], "http://"+srv.Addr().String()+"/onvif/") {
			t.Errorf("%s XAddr %s does not use the restarted ONVIF listener %s", id, d.XAddrs[0], srv.Addr())
		}
	}
}

func TestDiscoveryStartFailureSurfacesAndRollsBack(t *testing.T) {
	taken, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{ListenAddr: taken.LocalAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err == nil {
		srv.Close()
		t.Fatal("Start with an occupied discovery port: want error")
	}
	if srv.Addr() != nil || srv.DiscoveryAddr() != nil {
		t.Error("failed Start left the server half-started")
	}
}

func TestDiscoveryDisabledByDefault(t *testing.T) {
	srv := startTestServer(t, discoCam("a", 1))
	if srv.DiscoveryAddr() != nil {
		t.Error("DiscoveryAddr non-nil with Discovery unset")
	}
	if err := srv.AddCamera(discoCam("b", 2)); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if err := srv.RemoveCamera("b"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
}

func TestBindOrAdvertisedPicksAJoinableInterface(t *testing.T) {
	for _, tc := range []struct{ name, host, adv, want string }{
		{"specific bind wins", "192.0.2.7", "198.51.100.1", "192.0.2.7"},
		{"wildcard bind, advertised not local", "0.0.0.0", "192.0.2.1", ""},
		{"wildcard bind, advertised hostname", "0.0.0.0", "camfarm.test", ""},
		{"wildcard bind, advertised local IP", "0.0.0.0", "127.0.0.1", "127.0.0.1"},
		{"no advertise keeps host", "camhost.example", "", "camhost.example"},
	} {
		s := &Server{cfg: Config{Host: tc.host, AdvertiseHost: tc.adv}}
		if got := s.bindOrAdvertised(); got != tc.want {
			t.Errorf("%s: bindOrAdvertised = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMulticastWildcardBindWithForeignAdvertiseStarts(t *testing.T) {
	multicastTestInterface(t)
	srv, err := New(Config{Host: "0.0.0.0", AdvertiseHost: "192.0.2.1", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{Multicast: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		if strings.Contains(err.Error(), "interface not found") {
			t.Fatalf("Start failed on an advertised IP no interface owns: %v", err)
		}
		t.Skipf("multicast join unavailable here: %v", err)
	}
	srv.Close()
}

func TestBaseURLAdvertiseHost(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 8080}
	for _, tc := range []struct{ adv, want string }{
		{"camfarm.test", "http://camfarm.test:8080/onvif/cam"},
		{"192.0.2.1", "http://192.0.2.1:8080/onvif/cam"},
		{"2001:db8::1", "http://[2001:db8::1]:8080/onvif/cam"},
		{"[2001:db8::1]", "http://[2001:db8::1]:8080/onvif/cam"},
	} {
		if got := baseURL(addr, tc.adv, "cam"); got != tc.want {
			t.Errorf("baseURL(%q) = %q, want %q", tc.adv, got, tc.want)
		}
	}
}

func TestMulticastInterfaceMapping(t *testing.T) {
	cases := []struct {
		name string
		d    DiscoveryConfig
		host string
		want string
	}{
		{"ipv4 literal defaults", DiscoveryConfig{Multicast: true}, "192.0.2.7", "192.0.2.7"},
		{"explicit wins", DiscoveryConfig{Multicast: true, Interface: "br0"}, "192.0.2.7", "br0"},
		{"hostname stays empty", DiscoveryConfig{Multicast: true}, "camhost.example", ""},
		{"ipv6 literal", DiscoveryConfig{Multicast: true}, "2001:db8::1", "2001:db8::1"},
		{"bracketed ipv6", DiscoveryConfig{Multicast: true}, "[2001:db8::1]", "2001:db8::1"},
		{"unicast untouched", DiscoveryConfig{ListenAddr: "127.0.0.1:0"}, "192.0.2.7", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := multicastInterface(&tc.d, tc.host); got != tc.want {
				t.Errorf("multicastInterface = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMulticastRefusesHostnameBoundToLoopback(t *testing.T) {
	srv, err := New(Config{Host: "localhost", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{Multicast: true}})
	if err != nil {
		t.Fatal(err)
	}
	err = srv.Start()
	if err == nil {
		srv.Close()
		t.Fatal("Start: want loopback error")
	}
	if !strings.Contains(err.Error(), `"localhost"`) || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %v, want it to name listen.host and loopback", err)
	}
	if srv.Addr() != nil {
		t.Error("failed Start left a listener")
	}
}

func TestDiscoveryConfigValidation(t *testing.T) {
	cases := map[string]DiscoveryConfig{
		"both modes":          {Multicast: true, ListenAddr: "127.0.0.1:0"},
		"neither mode":        {},
		"interface w/unicast": {ListenAddr: "127.0.0.1:0", Interface: "lo"},
	}
	for name, dc := range cases {
		t.Run(name, func(t *testing.T) {
			dc := dc
			if _, err := New(Config{Cameras: []CameraConfig{discoCam("a", 1)}, Discovery: &dc}); err == nil {
				t.Fatal("New: want error")
			}
		})
	}
	ok := DiscoveryConfig{Multicast: true, Interface: "lo"}
	if _, err := New(Config{Cameras: []CameraConfig{discoCam("a", 1)}, Discovery: &ok}); err != nil {
		t.Fatalf("valid multicast config rejected: %v", err)
	}
}
