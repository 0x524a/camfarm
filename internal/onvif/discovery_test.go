package onvif

import (
	"context"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
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

func TestDiscoveryCloseStopsResponderAndLeaksNothing(t *testing.T) {
	http.DefaultClient.CloseIdleConnections()
	baseline := runtime.NumGoroutine()

	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	addr := srv.DiscoveryAddr()
	if got := probe(t, addr); len(got) != 1 {
		t.Fatalf("before Close found %d, want 1", len(got))
	}

	srv.Close()
	srv.Close() // idempotent

	if srv.DiscoveryAddr() != nil {
		t.Error("DiscoveryAddr non-nil after Close")
	}
	if got := probe(t, addr); len(got) != 0 {
		t.Errorf("after Close found %d devices, want none", len(got))
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond) // bounded settle for net/http teardown, not for correctness
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Errorf("goroutines = %d, baseline %d: responder or server leaked", n, baseline)
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

// expectAnnounce reads datagrams from conn until every ref has been seen with
// the given action URI, failing on timeout.
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
		for r := range want {
			if strings.Contains(msg, "<a:Address>"+r+"</a:Address>") {
				delete(want, r)
			}
		}
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
