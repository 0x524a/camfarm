package onvif

import (
	"net"
	"strings"
	"testing"
)

func TestAwaitServingBranches(t *testing.T) {
	t.Run("multicast group returns without probing", func(t *testing.T) {
		if err := awaitServing(&net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 3702}); err != nil {
			t.Errorf("awaitServing(multicast) = %v, want nil", err)
		}
	})
	t.Run("non-UDP address", func(t *testing.T) {
		if err := awaitServing(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); err == nil {
			t.Error("want error for a non-UDP address")
		}
	})
	t.Run("nil address", func(t *testing.T) {
		if err := awaitServing(nil); err == nil {
			t.Error("want error for a nil address")
		}
	})
	t.Run("silent peer times out", func(t *testing.T) {
		silent := localUDP(t)
		err := awaitServing(silent.LocalAddr())
		if err == nil || !strings.Contains(err.Error(), "no reply") {
			t.Errorf("awaitServing(silent) = %v, want a no-reply error", err)
		}
	})
	t.Run("wildcard v4 address is probed on loopback", func(t *testing.T) {
		srv := newDiscoServer(t, discoCam("a", 1))
		port := srv.DiscoveryAddr().(*net.UDPAddr).Port
		if err := awaitServing(&net.UDPAddr{IP: net.IPv4zero, Port: port}); err != nil {
			t.Errorf("awaitServing(0.0.0.0:%d) = %v", port, err)
		}
	})
}

// multicastTestInterface returns a non-loopback, up, multicast-capable
// interface with an IPv4 address, or skips. CI runners and containers often
// have none, so the tests that use it are opportunistic.
func multicastTestInterface(t *testing.T) (name, ip string) {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interface list: %v", err)
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				return ifc.Name, ipn.IP.String()
			}
		}
	}
	t.Skip("no non-loopback multicast-capable IPv4 interface")
	return "", ""
}

// Delivery cannot be asserted from Go on the same host (both sides disable
// IP_MULTICAST_LOOP), so this covers what can be: the responder joins the group
// on the chosen interface, binds the WS-Discovery port, follows AddCamera and
// RemoveCamera, and releases everything on Close.
func TestMulticastStartBindsGroupAndCloses(t *testing.T) {
	name, ip := multicastTestInterface(t)
	cases := map[string]DiscoveryConfig{
		"host-ip-default":    {Multicast: true},
		"explicit-interface": {Multicast: true, Interface: name},
	}
	for label, d := range cases {
		t.Run(label, func(t *testing.T) {
			srv, err := New(Config{Host: ip, Cameras: []CameraConfig{discoCam("a", 1)}, Discovery: &d})
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.Start(); err != nil {
				t.Skipf("multicast join unavailable here: %v", err)
			}
			addr, ok := srv.DiscoveryAddr().(*net.UDPAddr)
			// The socket is bound to the wildcard address on the WS-Discovery
			// port and joined to the group, so Addr reports 0.0.0.0:3702, not
			// the group address.
			if !ok || addr.Port != 3702 {
				t.Errorf("DiscoveryAddr = %v, want a UDP socket on port 3702", srv.DiscoveryAddr())
			}
			if err := srv.AddCamera(discoCam("b", 2)); err != nil {
				t.Errorf("AddCamera in multicast mode: %v", err)
			}
			if err := srv.RemoveCamera("b"); err != nil {
				t.Errorf("RemoveCamera in multicast mode: %v", err)
			}
			srv.Close()
			if srv.DiscoveryAddr() != nil {
				t.Error("DiscoveryAddr non-nil after Close")
			}
		})
	}
}

// A closed port answers a connected UDP socket with an ICMP error, which is a
// non-timeout read failure; where IPv6 is unavailable the dial itself fails.
// Either way the wildcard-v6 branch must return an error and not hang.
func TestAwaitServingWildcardV6ClosedPort(t *testing.T) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	if err := awaitServing(&net.UDPAddr{IP: net.IPv6unspecified, Port: port}); err == nil {
		t.Error("awaitServing against a closed v6 port: want error")
	}
}

func TestMulticastStartFailsOnUnknownInterface(t *testing.T) {
	srv, err := New(Config{Host: "192.0.2.7", Cameras: []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{Multicast: true, Interface: "nosuchif0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err == nil {
		srv.Close()
		t.Fatal("Start with an unknown multicast interface: want error")
	}
	if srv.Addr() != nil || srv.DiscoveryAddr() != nil {
		t.Error("failed Start left the server half-started")
	}
}
