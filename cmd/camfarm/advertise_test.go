package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunAdvertiseHostFlagReachesTheURLs(t *testing.T) {
	url, _, _ := start(t, "--http-addr", "127.0.0.1:0", "--listen-host", "0.0.0.0", "--advertise-host", "camfarm.test")

	resp, err := http.Get(url + "/api/v1/cameras/cam-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		RTSPURL       string `json:"rtspUrl"`
		ONVIFEndpoint string `json:"onvifEndpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(got.RTSPURL, "rtsp://camfarm.test:") {
		t.Errorf("rtspUrl = %q, want the advertised host", got.RTSPURL)
	}
	if !strings.HasPrefix(got.ONVIFEndpoint, "http://camfarm.test:") {
		t.Errorf("onvifEndpoint = %q, want the advertised host", got.ONVIFEndpoint)
	}
}

func TestRunAdvertiseHostErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"wildcard without advertise", []string{"--listen-host", "0.0.0.0"}, "advertiseHost"},
		{"advertise with port", []string{"--advertise-host", "camfarm.test:8554"}, "port"},
		{"advertise wildcard", []string{"--advertise-host", "0.0.0.0"}, "wildcard"},
	} {
		var out, errb bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := run(ctx, append([]string{"--http-addr", "127.0.0.1:0"}, tc.args...), &out, &errb)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func TestHelpMentionsAdvertiseHost(t *testing.T) {
	var out, errb bytes.Buffer
	_ = run(context.Background(), []string{"--help"}, &out, &errb)
	if got := errb.String() + out.String(); !strings.Contains(got, "-advertise-host") || !strings.Contains(got, "container") {
		t.Errorf("usage text missing --advertise-host: %q", got)
	}
}
