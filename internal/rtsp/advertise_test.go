package rtsp

import (
	"net"
	"strconv"
	"testing"

	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

func TestAdvertisedHostPort(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 8554}
	for _, tc := range []struct{ host, want string }{
		{"", "10.0.0.5:8554"},
		{"camfarm.test", "camfarm.test:8554"},
		{"192.0.2.1", "192.0.2.1:8554"},
		{"2001:db8::1", "[2001:db8::1]:8554"},
		{"[2001:db8::1]", "[2001:db8::1]:8554"},
	} {
		if got := advertisedHostPort(tc.host, addr); got != tc.want {
			t.Errorf("advertisedHostPort(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func startHostServer(t *testing.T, host, advertise string) *Server {
	t.Helper()
	s, err := New(Config{Host: host, AdvertiseHost: advertise, Obs: obs.New(10), Cameras: []CameraConfig{
		{ID: "cam", Media: testMedia(t), Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.URL("cam"); got != "" {
		t.Fatalf("URL before Start = %q, want empty", got)
	}
	if got := s.LocalURL("cam"); got != "" {
		t.Fatalf("LocalURL before Start = %q, want empty", got)
	}
	if err := s.Start(); err != nil {
		t.Skipf("cannot bind %q here: %v", host, err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestURLAndLocalURL(t *testing.T) {
	for _, tc := range []struct {
		name, host, adv string
		wantURLHost     string // "" means the bound address, read back
		wantLocalHost   string
	}{
		{"loopback, no advertise", "127.0.0.1", "", "", "127.0.0.1"},
		{"loopback, advertised name", "127.0.0.1", "camfarm.test", "camfarm.test", "127.0.0.1"},
		{"wildcard v4, advertised name", "0.0.0.0", "camfarm.test", "camfarm.test", "127.0.0.1"},
		{"wildcard v6, advertised v6", "::", "2001:db8::1", "2001:db8::1", "127.0.0.1"},
		{"wildcard v6, bracketed advertise", "::", "[2001:db8::1]", "2001:db8::1", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startHostServer(t, tc.host, tc.adv)
			port := strconv.Itoa(s.Addr().Port)

			wantURL := "rtsp://" + s.Addr().String() + "/cam"
			if tc.wantURLHost != "" {
				wantURL = "rtsp://" + net.JoinHostPort(tc.wantURLHost, port) + "/cam"
			}
			if got := s.URL("cam"); got != wantURL {
				t.Errorf("URL = %q, want %q", got, wantURL)
			}
			if want, got := "rtsp://"+net.JoinHostPort(tc.wantLocalHost, port)+"/cam", s.LocalURL("cam"); got != want {
				t.Errorf("LocalURL = %q, want %q", got, want)
			}
		})
	}
}
