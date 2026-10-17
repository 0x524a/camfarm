package camfarm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

func TestAdvertiseHostValidation(t *testing.T) {
	bad := []struct {
		name, host, adv, want string
		disco                 *DiscoverySpec
	}{
		{"wildcard v4 without advertise", "0.0.0.0", "", "advertiseHost", nil},
		{"wildcard v6 without advertise", "::", "", "advertiseHost", nil},
		{"bracketed wildcard without advertise", "[::]", "", "advertiseHost", nil},
		{"blank advertise", "", "   ", "blank", nil},
		{"advertise with scheme", "", "rtsp://camfarm.test", "bare host", nil},
		{"advertise with path", "", "camfarm.test/x", "bare host", nil},
		{"advertise with port", "", "camfarm.test:8554", "port", nil},
		{"advertise ipv4 with port", "", "192.0.2.1:8554", "port", nil},
		{"advertise bracketed ipv6 with port", "", "[::1]:8554", "port", nil},
		{"advertise wildcard v4", "", "0.0.0.0", "wildcard", nil},
		{"advertise wildcard v6", "", "::", "wildcard", nil},
		{"advertise bracketed wildcard", "", "[::]", "wildcard", nil},
		{"multicast with loopback advertise", "0.0.0.0", "127.0.0.1", "loopback", &DiscoverySpec{Mode: "multicast"}},
		{"multicast with localhost advertise", "0.0.0.0", "localhost", "loopback", &DiscoverySpec{Mode: "multicast"}},
		{"multicast with trailing-dot localhost advertise", "0.0.0.0", "localhost.", "loopback", &DiscoverySpec{Mode: "multicast"}},
		{"multicast with uppercase localhost advertise", "0.0.0.0", "LOCALHOST", "loopback", &DiscoverySpec{Mode: "multicast"}},
		{"multicast with loopback bind and routable advertise", "127.0.0.1", "192.0.2.1", "loopback", &DiscoverySpec{Mode: "multicast"}},
		{"advertise angle brackets", "", "a<b>", "hostname", nil},
		{"advertise comma", "", "a,b", "hostname", nil},
		{"advertise semicolon", "", "a;b", "hostname", nil},
		{"advertise non-ascii", "", "caf\u00e9.test", "ASCII", nil},
		{"advertise unbalanced open bracket", "", "[2001:db8::1", "brackets", nil},
		{"advertise unbalanced close bracket", "", "2001:db8::1]", "brackets", nil},
		{"advertise bracketed hostname", "", "[camfarm.test]", "IPv6", nil},
		{"advertise bracketed ipv4", "", "[192.0.2.1]", "IPv6", nil},
		{"advertise ipv6 zone", "", "fe80::1%eth0", "zone", nil},
		{"advertise shorthand ipv4", "", "127.1", "numeric", nil},
		{"advertise leading hyphen label", "", "-a.test", "hostname", nil},
		{"advertise trailing hyphen label", "", "a-.test", "hostname", nil},
		{"advertise underscore with space", "", "my cam", "hostname", nil},
		{"advertise empty label", "", "a..test", "hostname", nil},
		{"advertise long label", "", strings.Repeat("a", 64) + ".test", "hostname", nil},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := minimalSpec()
			s.Listen.Host, s.Listen.AdvertiseHost, s.Listen.Discovery = tc.host, tc.adv, tc.disco
			_, err := s.validate()
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	good := []struct {
		name, host, adv string
		disco           *DiscoverySpec
	}{
		{"no advertise", "", "", nil},
		{"hostname advertise", "", "camfarm.test", nil},
		{"wildcard bind with advertise", "0.0.0.0", "camfarm.test", nil},
		{"v6 wildcard bind with advertise", "::", "192.0.2.1", nil},
		{"ipv6 advertise", "", "2001:db8::1", nil},
		{"bracketed ipv6 advertise", "", "[2001:db8::1]", nil},
		{"unicast discovery on wildcard bind", "0.0.0.0", "camfarm.test", &DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}},
		{"multicast on wildcard bind", "0.0.0.0", "192.0.2.1", &DiscoverySpec{Mode: "multicast"}},
		{"unicast with loopback bind and advertise", "127.0.0.1", "camfarm.test", &DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}},
		{"uppercase hostname", "", "CAMFARM.TEST", nil},
		{"trailing-dot hostname", "", "camfarm.test.", nil},
		{"underscore-free single label", "", "camfarm", nil},
		{"underscore in label", "", "my_cam", nil},
		{"underscore and digit suffix", "", "camfarm_1", nil},
		{"leading underscore", "", "_x", nil},
		{"compose network name", "", "web_1.net_default", nil},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			s := minimalSpec()
			s.Listen.Host, s.Listen.AdvertiseHost, s.Listen.Discovery = tc.host, tc.adv, tc.disco
			if _, err := s.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestAdvertiseHostEmptyLeavesHashUnchanged(t *testing.T) {
	const wantHash = "b080afcfab9da1c905fd17b60c370d0a00c4f07cc0963d7ba295f9bd14e989db"
	if got := minimalSpec().Hash(); got != wantHash {
		t.Fatalf("Hash = %s, want the pre-existing %s", got, wantHash)
	}
	s := minimalSpec()
	s.Listen.AdvertiseHost = "camfarm.test"
	if s.Hash() == wantHash {
		t.Fatal("setting AdvertiseHost did not change the hash")
	}
}

func TestAdvertiseHostURLsUseAdvertisedHostAndRealPorts(t *testing.T) {
	s := minimalSpec()
	s.Listen.Host = "127.0.0.1"
	s.Listen.AdvertiseHost = "camfarm.test"
	f := StartT(t, s)
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}

	rtspPort := f.Addr().RTSP.Port
	if got, want := cam.RTSPURL(), "rtsp://camfarm.test:"+strconv.Itoa(rtspPort)+"/front-door"; got != want {
		t.Fatalf("RTSPURL = %q, want %q", got, want)
	}
	eu, err := url.Parse(cam.ONVIFEndpoint())
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	if eu.Hostname() != "camfarm.test" || eu.Port() == "" || eu.Port() == "0" {
		t.Fatalf("ONVIFEndpoint = %q, want camfarm.test with the real port", cam.ONVIFEndpoint())
	}

	// Still reachable on the bind address.
	for _, addr := range []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(rtspPort)), net.JoinHostPort("127.0.0.1", eu.Port())} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial bind address %s: %v", addr, err)
		}
		_ = c.Close()
	}
	if got, want := cam.LocalRTSPURL(), "rtsp://127.0.0.1:"+strconv.Itoa(rtspPort)+"/front-door"; got != want {
		t.Fatalf("LocalRTSPURL = %q, want %q", got, want)
	}

	// ONVIF answers carry the advertised host. The request goes to the bind
	// address because camfarm.test does not resolve.
	local := "http://" + net.JoinHostPort("127.0.0.1", eu.Port()) + eu.Path
	const deviceNS = "http://www.onvif.org/ver10/device/wsdl"
	const mediaNS = "http://www.onvif.org/ver10/media/wsdl"

	resp, body := soapPost(t, local+"/device", deviceNS, "GetCapabilities", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), cam.ONVIFEndpoint()+"/device") {
		t.Errorf("GetCapabilities status %d, body %s, want XAddr %s", resp.StatusCode, body, cam.ONVIFEndpoint()+"/device")
	}
	resp, body = soapPost(t, local+"/device", deviceNS, "GetServices", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), cam.ONVIFEndpoint()+"/media") {
		t.Errorf("GetServices status %d, body %s, want XAddr %s", resp.StatusCode, body, cam.ONVIFEndpoint()+"/media")
	}
	resp, body = soapPost(t, local+"/media", mediaNS, "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 || !strings.Contains(string(body), cam.RTSPURL()) {
		t.Errorf("GetStreamUri status %d, body %s, want %s", resp.StatusCode, body, cam.RTSPURL())
	}
	resp, body = soapPost(t, local+"/media", mediaNS, "GetSnapshotUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "camfarm.test") {
		t.Errorf("GetSnapshotUri status %d, body %s, want the advertised host", resp.StatusCode, body)
	}

	// A camera added at runtime is advertised the same way.
	added, err := f.AddCamera(CameraSpec{ID: "garage"})
	if err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if !strings.HasPrefix(added.RTSPURL(), "rtsp://camfarm.test:") || !strings.HasPrefix(added.ONVIFEndpoint(), "http://camfarm.test:") {
		t.Errorf("added camera URLs = %q, %q", added.RTSPURL(), added.ONVIFEndpoint())
	}
}

func TestAdvertiseHostDiscoveryXAddr(t *testing.T) {
	s := unicastSpec()
	s.Listen.AdvertiseHost = "camfarm.test"
	f := StartT(t, s)

	devs := probeFleet(t, f)
	if len(devs) != 2 {
		t.Fatalf("found %d devices, want 2", len(devs))
	}
	for _, id := range []string{"front-door", "lobby"} {
		cam, _ := f.Camera(id)
		d, ok := devs["urn:uuid:"+cam.ONVIFUUID()]
		if !ok {
			t.Fatalf("camera %q not discovered", id)
		}
		want := cam.ONVIFEndpoint() + "/device"
		if !strings.HasPrefix(want, "http://camfarm.test:") || len(d.XAddrs) != 1 || d.XAddrs[0] != want {
			t.Fatalf("camera %q XAddrs = %v, want [%s]", id, d.XAddrs, want)
		}
	}
}

func TestWildcardBindWithAdvertiseHostServes(t *testing.T) {
	s := unicastSpec()
	s.Listen.Host = "0.0.0.0"
	s.Listen.AdvertiseHost = "127.0.0.1"
	f := StartT(t, s)
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if !strings.HasPrefix(cam.RTSPURL(), "rtsp://127.0.0.1:") || strings.Contains(cam.RTSPURL(), "0.0.0.0") {
		t.Fatalf("RTSPURL = %q", cam.RTSPURL())
	}
	if !strings.HasPrefix(cam.LocalRTSPURL(), "rtsp://127.0.0.1:") {
		t.Fatalf("LocalRTSPURL = %q, want loopback for a wildcard bind", cam.LocalRTSPURL())
	}

	u, err := base.ParseURL(cam.RTSPURL())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	if _, _, err := c.Describe(u); err != nil {
		t.Fatalf("DESCRIBE %s: %v", cam.RTSPURL(), err)
	}

	resp, body := soapPost(t, cam.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 || !strings.Contains(string(body), cam.RTSPURL()) {
		t.Errorf("GetStreamUri status %d, body %s", resp.StatusCode, body)
	}
	devs := probeFleet(t, f)
	d, ok := devs["urn:uuid:"+cam.ONVIFUUID()]
	if !ok || len(d.XAddrs) != 1 || d.XAddrs[0] != cam.ONVIFEndpoint()+"/device" {
		t.Fatalf("discovery = %+v", devs)
	}
}

func TestStartRejectsWildcardWithoutAdvertiseHost(t *testing.T) {
	s := minimalSpec()
	s.Listen.Host = "0.0.0.0"
	if _, err := Start(context.Background(), s); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Start err = %v, want ErrInvalidSpec", err)
	}
}

// The in-process dials (screenshot, ONVIF snapshot) must use the bound
// address: the advertised host is for clients and may not resolve here.
func TestAdvertiseHostUnresolvableStillCapturesInProcess(t *testing.T) {
	s := minimalSpec()
	s.Listen.Host = "127.0.0.1"
	s.Listen.AdvertiseHost = "camfarm.invalid"
	f := StartT(t, s)
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if strings.Contains(cam.LocalRTSPURL(), "invalid") {
		t.Fatalf("LocalRTSPURL = %q", cam.LocalRTSPURL())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := cam.Screenshot(ctx); err != nil {
		t.Fatalf("Screenshot with an unresolvable advertised host: %v", err)
	}

	eu, err := url.Parse(cam.ONVIFEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	local := "http://" + net.JoinHostPort("127.0.0.1", eu.Port()) + eu.Path + "/snapshot"
	resp, err := http.Get(local)
	if err != nil {
		t.Fatalf("GET %s: %v", local, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("snapshot status %d type %q body %.200s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}
