package camfarm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0x524a/onvif-go/discovery"
)

func unicastSpec() Spec {
	s := minimalSpec()
	s.Listen.Discovery = &DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}
	return s
}

func probeFleet(t *testing.T, f *Fleet) map[string]*discovery.Device {
	t.Helper()
	devs, err := discovery.DiscoverWithOptions(context.Background(), 500*time.Millisecond,
		&discovery.DiscoverOptions{ProbeAddress: f.DiscoveryAddr()})
	if err != nil {
		t.Fatalf("DiscoverWithOptions: %v", err)
	}
	out := make(map[string]*discovery.Device, len(devs))
	for _, d := range devs {
		out[d.EndpointRef] = d
	}
	return out
}

func TestDiscoverySpecValidation(t *testing.T) {
	tests := []struct {
		name string
		host string
		d    DiscoverySpec
		want string
	}{
		{"unknown mode", "", DiscoverySpec{Mode: "broadcast"}, "mode"},
		{"empty mode", "", DiscoverySpec{}, "mode"},
		{"unicast needs addr", "", DiscoverySpec{Mode: "unicast"}, "addr"},
		{"multicast forbids addr", "192.0.2.1", DiscoverySpec{Mode: "multicast", Addr: "127.0.0.1:0"}, "addr"},
		{"interface only multicast", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0", Interface: "eth0"}, "interface"},
		{"announce only unicast", "192.0.2.1", DiscoverySpec{Mode: "multicast", AnnounceAddr: "127.0.0.1:1"}, "announceAddr"},
		{"multicast default host", "", DiscoverySpec{Mode: "multicast"}, "listen.host"},
		{"multicast loopback", "127.0.0.1", DiscoverySpec{Mode: "multicast"}, "listen.host"},
		{"multicast ::1", "::1", DiscoverySpec{Mode: "multicast"}, "listen.host"},
		{"multicast localhost", "localhost", DiscoverySpec{Mode: "multicast"}, "listen.host"},
		{"wildcard v4", "0.0.0.0", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}, "listen.host"},
		{"wildcard v6", "::", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}, "listen.host"},
		{"wildcard bracketed", "[::]", DiscoverySpec{Mode: "multicast"}, "listen.host"},
		{"multicast wildcard v4", "0.0.0.0", DiscoverySpec{Mode: "multicast"}, "listen.host"},
		{"multicast bracketed ::1", "[::1]", DiscoverySpec{Mode: "multicast"}, "loopback"},
		{"multicast 127.0.0.2", "127.0.0.2", DiscoverySpec{Mode: "multicast"}, "loopback"},
		{"host with port", "192.0.2.1:8554", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}, "listen.host"},
		{"bracketed host with port", "[::1]:8554", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}, "listen.host"},
		{"addr without port", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1"}, "listen.discovery.addr"},
		{"addr non-numeric port", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:abc"}, "listen.discovery.addr"},
		{"addr port out of range", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:70000"}, "listen.discovery.addr"},
		{"announce without port", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0", AnnounceAddr: "127.0.0.1"}, "announceAddr"},
		{"announce empty host", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0", AnnounceAddr: ":3702"}, "announceAddr"},
		{"announce non-numeric port", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0", AnnounceAddr: "127.0.0.1:x"}, "announceAddr"},
		{"announce port out of range", "", DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0", AnnounceAddr: "127.0.0.1:65536"}, "announceAddr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := minimalSpec()
			s.Listen.Host = tc.host
			d := tc.d
			s.Listen.Discovery = &d
			_, err := s.validate()
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	ok := minimalSpec()
	ok.Listen.Host = "192.0.2.1"
	ok.Listen.Discovery = &DiscoverySpec{Mode: "multicast"}
	if _, err := ok.validate(); err != nil {
		t.Fatalf("multicast on a routable host: %v", err)
	}
	if _, err := unicastSpec().validate(); err != nil {
		t.Fatalf("unicast on loopback: %v", err)
	}
}

func TestDiscoveryFindsFleetAndFollowsLifecycle(t *testing.T) {
	f, err := Start(context.Background(), unicastSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if f.DiscoveryAddr() == "" {
		t.Fatal("DiscoveryAddr is empty with discovery enabled")
	}

	check := func(devs map[string]*discovery.Device, id string, present bool) {
		t.Helper()
		cam, err := f.Camera(id)
		if err != nil {
			if present {
				t.Fatalf("Camera(%q): %v", id, err)
			}
			return
		}
		d, ok := devs["urn:uuid:"+cam.ONVIFUUID()]
		if ok != present {
			t.Fatalf("camera %q discovered = %v, want %v", id, ok, present)
		}
		if present {
			want := cam.ONVIFEndpoint() + "/device"
			if len(d.XAddrs) != 1 || d.XAddrs[0] != want {
				t.Fatalf("camera %q XAddrs = %v, want [%s]", id, d.XAddrs, want)
			}
		}
	}

	devs := probeFleet(t, f)
	if len(devs) != 2 {
		t.Fatalf("found %d devices, want 2", len(devs))
	}
	check(devs, "front-door", true)
	check(devs, "lobby", true)

	if _, err := f.AddCamera(CameraSpec{ID: "garage"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	devs = probeFleet(t, f)
	if len(devs) != 3 {
		t.Fatalf("after add: found %d devices, want 3", len(devs))
	}
	check(devs, "garage", true)

	garage, _ := f.Camera("garage")
	ref := "urn:uuid:" + garage.ONVIFUUID()
	if err := f.RemoveCamera("garage"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	devs = probeFleet(t, f)
	if _, ok := devs[ref]; ok || len(devs) != 2 {
		t.Fatalf("after remove: %d devices, removed camera still present = %v", len(devs), ok)
	}
}

func TestDiscoveryDisabledByDefault(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if got := f.DiscoveryAddr(); got != "" {
		t.Fatalf("DiscoveryAddr = %q, want empty", got)
	}
}

// Goldens captured from the behaviour before the discovery field existed: a
// spec that does not enable discovery must keep its hash and replay line.
func TestSpecWithoutDiscoveryIsByteIdentical(t *testing.T) {
	const (
		wantJSON = `{"seed":1059757185,"cameras":[{"id":"front-door","source":{},"video":{},"auth":{}},{"id":"lobby","source":{},"video":{},"auth":{}}],"listen":{}}`
		wantHash = "b080afcfab9da1c905fd17b60c370d0a00c4f07cc0963d7ba295f9bd14e989db"
		wantLine = "camfarm: seed=0x3f2a9c81 spec=sha256:b080afcfab9d"
	)
	b, err := json.Marshal(minimalSpec())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantJSON {
		t.Fatalf("JSON = %s", b)
	}
	if got := minimalSpec().Hash(); got != wantHash {
		t.Fatalf("Hash = %s", got)
	}
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if got := f.ReplayLine(); got != wantLine {
		t.Fatalf("ReplayLine = %q", got)
	}
}

func TestDiscoveryRoundTripsAndChangesHash(t *testing.T) {
	in := unicastSpec()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"discovery":{"mode":"unicast","addr":"127.0.0.1:0"}`) {
		t.Fatalf("JSON = %s", b)
	}
	var out Spec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Listen.Discovery == nil || *out.Listen.Discovery != *in.Listen.Discovery {
		t.Fatalf("round trip = %+v", out.Listen.Discovery)
	}
	if in.Hash() == minimalSpec().Hash() {
		t.Fatal("enabling discovery did not change the spec hash")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func TestDiscoveryAnnounceAddrThroughSpec(t *testing.T) {
	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	s := minimalSpec()
	s.Listen.Discovery = &DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0", AnnounceAddr: sink.LocalAddr().String()}
	f, err := Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatal(err)
	}
	want := "<a:Address>urn:uuid:" + cam.ONVIFUUID() + "</a:Address>"
	buf := make([]byte, 65535)
	_ = sink.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		n, _, err := sink.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no Hello for %s reached the announce socket: %v", cam.ONVIFUUID(), err)
		}
		msg := string(buf[:n])
		if strings.Contains(msg, "/Hello</a:Action>") && strings.Contains(msg, want) {
			return
		}
	}
}

func TestStartDiscoveryBindFailureLeaksNothing(t *testing.T) {
	taken, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	rtspPort, onvifPort := freeTCPPort(t), freeTCPPort(t)
	s := minimalSpec()
	s.Listen.RTSPPort, s.Listen.ONVIFPort = rtspPort, onvifPort
	s.Listen.Discovery = &DiscoverySpec{Mode: "unicast", Addr: taken.LocalAddr().String()}
	if f, err := Start(context.Background(), s); err == nil {
		_ = f.Close()
		t.Fatal("Start with an occupied discovery address: want an error")
	}

	// The failed Start must have released both TCP listeners: the same ports
	// are usable again.
	s.Listen.Discovery = &DiscoverySpec{Mode: "unicast", Addr: "127.0.0.1:0"}
	f, err := Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start on the same ports after a failed Start: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
