package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe writer the test can poll.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

var dashLine = regexp.MustCompile(`dashboard: (http://\S+)/`)

// start runs the binary's core and returns the dashboard URL, a stop function
// that cancels it and returns run's error, and the captured stderr.
func start(t *testing.T, args ...string) (url string, stop func() error, stderr *syncBuf) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &syncBuf{}, &syncBuf{}
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, args, stdout, stderr) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if m := dashLine.FindStringSubmatch(stdout.String()); m != nil {
			url = m[1]
			break
		}
		select {
		case err := <-errc:
			cancel()
			t.Fatalf("run exited early: %v\nstderr: %s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no dashboard line; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() { cancel(); result = <-errc })
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return url, stop, stderr
}

func TestRunServesTheDashboardAndShutsDownCleanly(t *testing.T) {
	url, stop, stderr := start(t, "--http-addr", "127.0.0.1:0", "--cameras", "2", "--seed", "7")

	resp, err := http.Get(url + "/api/v1/cameras")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("cameras = %d, want 2", len(list))
	}
	if !strings.Contains(stderr.String(), "seed=0x7") {
		t.Errorf("stderr should print the replay line with the seed, got %q", stderr.String())
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned %v on shutdown, want nil", err)
	}
}

var seedRe = regexp.MustCompile(`seed=(0x[0-9a-f]+)`)

func TestRunDrawsASeedWhenNoneIsGiven(t *testing.T) {
	seeds := map[string]bool{}
	for i := 0; i < 2; i++ {
		_, stop, stderr := start(t, "--http-addr", "127.0.0.1:0")
		m := seedRe.FindStringSubmatch(stderr.String())
		if m == nil {
			t.Fatalf("no replay line on stderr: %q", stderr.String())
		}
		if m[1] == "0x0" {
			t.Fatalf("drew a zero seed")
		}
		seeds[m[1]] = true
		_ = stop()
	}
	if len(seeds) != 2 {
		t.Fatalf("two runs drew the same seed: %v", seeds)
	}
}

func TestRunUsesTheSpecSeedWhenNoSeedFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(`{"seed":5,"cameras":[{"id":"a"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, stderr := start(t, "--spec", path, "--http-addr", "127.0.0.1:0")
	if !strings.Contains(stderr.String(), "seed=0x5") {
		t.Fatalf("spec seed not used: %q", stderr.String())
	}
}

func TestShutdownIsPromptWithAnOpenEventStream(t *testing.T) {
	url, stop, _ := start(t, "--http-addr", "127.0.0.1:0")
	resp, err := http.Get(url + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream: %v", err)
		}
		if strings.HasPrefix(line, ": connected") {
			break
		}
	}
	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish within 2s with an open SSE stream")
	}
}

func TestRunLoadsASpecFileAndFlagsOverrideIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(path, []byte(`{"seed":5,"cameras":[{"id":"from-file"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	url, _, stderr := start(t, "--spec", path, "--http-addr", "127.0.0.1:0", "--seed", "9")

	resp, err := http.Get(url + "/api/v1/cameras/from-file")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("camera from the spec file missing: %d", resp.StatusCode)
	}
	if !strings.Contains(stderr.String(), "seed=0x9") {
		t.Errorf("--seed did not override the spec's seed: %q", stderr.String())
	}
}

func TestRunWarnsOnANonLoopbackBind(t *testing.T) {
	_, _, stderr := start(t, "--http-addr", "0.0.0.0:0")
	if !strings.Contains(stderr.String(), "warning:") {
		t.Fatalf("no warning for a non-loopback bind: %q", stderr.String())
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "bad.json")
	unknown := filepath.Join(dir, "unknown.json")
	valid := filepath.Join(dir, "ok.json")
	for path, body := range map[string]string{
		malformed: `{"seed":`,
		unknown:   `{"seed":1,"cameras":[{"id":"a"}],"bogus":true}`,
		valid:     `{"cameras":[{"id":"a"}]}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const addr = "127.0.0.1:0"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--nope"}, "flag provided but not defined"},
		{"zero cameras", []string{"--cameras", "0", "--http-addr", addr}, "--cameras must be at least 1"},
		{"missing spec", []string{"--spec", "/no/such/file.json", "--http-addr", addr}, "reading spec"},
		{"malformed spec", []string{"--spec", malformed, "--http-addr", addr}, "parsing spec"},
		{"unknown spec field", []string{"--spec", unknown, "--http-addr", addr}, "unknown field"},
		{"bad http addr", []string{"--http-addr", "not-an-address"}, "invalid --http-addr"},
		{"cameras with spec", []string{"--spec", valid, "--cameras", "2", "--http-addr", addr}, "mutually exclusive"},
		{"port too big", []string{"--rtsp-port", "70000", "--http-addr", addr}, "out of range"},
		{"negative port", []string{"--onvif-port", "-1", "--http-addr", addr}, "out of range"},
	} {
		var out, errb bytes.Buffer
		err := run(context.Background(), tc.args, &out, &errb)
		if err == nil {
			t.Errorf("%s: run succeeded, want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error()+errb.String(), tc.want) {
			t.Errorf("%s: error %q (stderr %q) lacks %q", tc.name, err, errb.String(), tc.want)
		}
	}
}

func TestHelpStatesTheSecurityPosture(t *testing.T) {
	var out, errb bytes.Buffer
	_ = run(context.Background(), []string{"--help"}, &out, &errb)
	got := errb.String() + out.String()
	for _, want := range []string{"loopback", "authentication", "Host", "CSRF", "local", "Digest", "mutually exclusive", "0x", "octal"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage text missing %q:\n%s", want, got)
		}
	}
}
