package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/onvif-go/discovery"
)

func TestApplyDiscovery(t *testing.T) {
	multicast := func(iface string) *camfarm.DiscoverySpec {
		return &camfarm.DiscoverySpec{Mode: "multicast", Interface: iface}
	}
	unicast := func(addr string) *camfarm.DiscoverySpec {
		return &camfarm.DiscoverySpec{Mode: "unicast", Addr: addr}
	}
	tests := []struct {
		name    string
		initial *camfarm.DiscoverySpec
		mode    string
		iface   string
		want    *camfarm.DiscoverySpec
		wantErr string
	}{
		{"nothing set", nil, "", "", nil, ""},
		{"spec kept when flags empty", unicast("1.2.3.4:5"), "", "", unicast("1.2.3.4:5"), ""},
		{"multicast flag", nil, "multicast", "", multicast(""), ""},
		{"multicast with interface", nil, "multicast", "eth0", multicast("eth0"), ""},
		{"unicast flag", nil, "unicast:127.0.0.1:0", "", unicast("127.0.0.1:0"), ""},
		{"unicast ipv6", nil, "unicast:[::1]:3702", "", unicast("[::1]:3702"), ""},
		{"flag overrides spec", multicast(""), "unicast:127.0.0.1:9", "", unicast("127.0.0.1:9"), ""},
		{"interface only on spec multicast", multicast(""), "", "wlan0", multicast("wlan0"), ""},
		{"interface alone, no discovery", nil, "", "eth0", nil, "only applies to multicast"},
		{"interface with unicast", nil, "unicast:127.0.0.1:0", "eth0", nil, "only applies to multicast"},
		{"interface alone, spec unicast", unicast("127.0.0.1:0"), "", "eth0", nil, "only applies to multicast"},
		{"unknown form", nil, "broadcast", "", nil, `"multicast" or "unicast:HOST:PORT"`},
		{"unicast missing port", nil, "unicast:127.0.0.1", "", nil, `"multicast" or "unicast:HOST:PORT"`},
		{"unicast non-numeric port", nil, "unicast:h:abc", "", nil, "port"},
		{"unicast port out of range", nil, "unicast:h:70000", "", nil, "port"},
		{"unicast empty host", nil, "unicast::80", "", nil, "missing host"},
		{"flag replaces whole block", &camfarm.DiscoverySpec{Mode: "unicast", Addr: "1.2.3.4:5", AnnounceAddr: "1.2.3.4:6"}, "unicast:127.0.0.1:9", "", unicast("127.0.0.1:9"), ""},
		{"unicast empty", nil, "unicast:", "", nil, `"multicast" or "unicast:HOST:PORT"`},
		{"multicast with suffix", nil, "multicast:eth0", "", nil, `"multicast" or "unicast:HOST:PORT"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spec camfarm.Spec
			spec.Listen.Discovery = tc.initial
			err := applyDiscovery(&spec, tc.mode, tc.iface)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := spec.Listen.Discovery
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("discovery = %+v, want %+v", got, tc.want)
			}
		})
	}
}

var discLine = regexp.MustCompile(`discovery: udp://(\S+)`)

func TestRunUnicastDiscoveryAnswersProbes(t *testing.T) {
	_, _, stdout, _ := startOut(t, "--http-addr", "127.0.0.1:0", "--cameras", "2", "--seed", "3",
		"--discovery", "unicast:127.0.0.1:0")
	m := discLine.FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("no discovery line on stdout: %q", stdout.String())
	}
	devs, err := discovery.DiscoverWithOptions(context.Background(), 500*time.Millisecond,
		&discovery.DiscoverOptions{ProbeAddress: m[1]})
	if err != nil {
		t.Fatalf("DiscoverWithOptions: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("probe found %d devices, want 2: %+v", len(devs), devs)
	}
}

func TestRunWithoutDiscoveryPrintsNoDiscoveryLine(t *testing.T) {
	_, _, stdout, _ := startOut(t, "--http-addr", "127.0.0.1:0")
	if strings.Contains(stdout.String(), "discovery:") {
		t.Fatalf("unexpected discovery line: %q", stdout.String())
	}
}

func TestRunDiscoveryFlagErrors(t *testing.T) {
	const addr = "127.0.0.1:0"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"multicast on default loopback", []string{"--discovery", "multicast"}, "listen.host"},
		{"multicast on explicit loopback", []string{"--discovery", "multicast", "--listen-host", "127.0.0.1"}, "loopback"},
		{"unicast on wildcard host", []string{"--discovery", "unicast:127.0.0.1:0", "--listen-host", "0.0.0.0"}, "listen.host"},
		{"interface without multicast", []string{"--discovery-interface", "eth0"}, "only applies to multicast"},
		{"interface with unicast", []string{"--discovery", "unicast:127.0.0.1:0", "--discovery-interface", "eth0"}, "only applies to multicast"},
		{"non-numeric port", []string{"--discovery", "unicast:h:abc"}, "port"},
		{"empty host", []string{"--discovery", "unicast::80"}, "missing host"},
		{"bad form", []string{"--discovery", "bogus"}, `"multicast" or "unicast:HOST:PORT"`},
	} {
		var out, errb bytes.Buffer
		err := run(context.Background(), append([]string{"--http-addr", addr}, tc.args...), &out, &errb)
		if err == nil {
			t.Errorf("%s: run succeeded, want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q lacks %q", tc.name, err, tc.want)
		}
		if strings.Contains(out.String(), "dashboard:") {
			t.Errorf("%s: started despite the error", tc.name)
		}
	}
}

func TestHelpMentionsDiscoveryCaveats(t *testing.T) {
	var out, errb bytes.Buffer
	_ = run(context.Background(), []string{"--help"}, &out, &errb)
	got := errb.String() + out.String()
	for _, want := range []string{"-discovery ", "-discovery-interface", "listen-host", "Docker"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage text missing %q", want)
		}
	}
}
