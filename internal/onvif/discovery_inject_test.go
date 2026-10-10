package onvif

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x524a/onvif-go/discovery"
)

// fakeResponder stands in for onvif-go's responder so tests can make the parts
// camfarm cannot reach through the real one (a Register that fails, a socket
// that never binds, a responder that dies) happen on demand.
type fakeResponder struct {
	cfg    *discovery.ResponderConfig
	regErr error
	addr   net.Addr
	run    func(ctx context.Context, cfg *discovery.ResponderConfig) error
}

func (f *fakeResponder) Register(*discovery.Device) error { return f.regErr }
func (f *fakeResponder) Unregister(string)                {}
func (f *fakeResponder) Addr() net.Addr                   { return f.addr }
func (f *fakeResponder) Start(ctx context.Context) error  { return f.run(ctx, f.cfg) }

// lockedBuf is a log sink safe to read while the server may still be logging.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// injectServer builds a unicast-discovery server whose responder is fake.
func injectServer(t *testing.T, f *fakeResponder) (*Server, *lockedBuf) {
	t.Helper()
	logs := &lockedBuf{}
	srv, err := New(Config{
		Host:      "127.0.0.1",
		Cameras:   []CameraConfig{discoCam("a", 1)},
		Discovery: &DiscoveryConfig{ListenAddr: "127.0.0.1:0"},
		Log:       slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.newResp = func(c *discovery.ResponderConfig) responder {
		f.cfg = c
		return f
	}
	return srv, logs
}

// bindThen reports the socket as bound, then runs after until the context ends.
func bindThen(after func(ctx context.Context) error) func(context.Context, *discovery.ResponderConfig) error {
	return func(ctx context.Context, cfg *discovery.ResponderConfig) error {
		close(cfg.Ready)
		return after(ctx)
	}
}

var multicastGroupAddr = &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 3702}

func TestStartFailsWhenRegisterFails(t *testing.T) {
	f := &fakeResponder{regErr: errors.New("register boom")}
	srv, _ := injectServer(t, f)
	err := srv.Start()
	if err == nil || !strings.Contains(err.Error(), "register boom") {
		if err == nil {
			srv.Close()
		}
		t.Fatalf("Start = %v, want the Register error", err)
	}
	if srv.Addr() != nil || srv.DiscoveryAddr() != nil {
		t.Error("failed Start left the server half-started")
	}
}

func TestStartFailsWhenSocketNeverBinds(t *testing.T) {
	f := &fakeResponder{run: func(ctx context.Context, _ *discovery.ResponderConfig) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	srv, _ := injectServer(t, f)
	srv.readyTimeout = 50 * time.Millisecond
	err := srv.Start()
	if err == nil || !strings.Contains(err.Error(), "not bound within 50ms") {
		if err == nil {
			srv.Close()
		}
		t.Fatalf("Start = %v, want a not-bound timeout", err)
	}
	if srv.Addr() != nil || srv.DiscoveryAddr() != nil {
		t.Error("failed Start left the server half-started")
	}
}

func TestResponderDeathAfterStartIsLoggedOnce(t *testing.T) {
	die := make(chan struct{})
	f := &fakeResponder{addr: multicastGroupAddr, run: bindThen(func(ctx context.Context) error {
		select {
		case <-die:
			return errors.New("socket gone")
		case <-ctx.Done():
			return nil
		}
	})}
	srv, logs := injectServer(t, f)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	close(die)
	// Close must not race the death: a cancelled context would make it a
	// normal stop. Wait until the death has been logged.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "stopped unexpectedly") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	srv.Close()
	out := logs.String()
	if n := strings.Count(out, "stopped unexpectedly"); n != 1 {
		t.Errorf("unexpected-stop logged %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "discovery responder stopped err") {
		t.Errorf("Close reported the same death a second time:\n%s", out)
	}
}

func TestCloseWarnsWhenResponderStopsWithError(t *testing.T) {
	f := &fakeResponder{addr: multicastGroupAddr, run: bindThen(func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("bye failed")
	})}
	srv, logs := injectServer(t, f)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if out := logs.String(); !strings.Contains(out, "discovery responder stopped") || !strings.Contains(out, "bye failed") {
		t.Errorf("Close did not warn about the responder error:\n%s", out)
	}
}

func TestStartWarnsWhenStartupHellosCannotBeConfirmed(t *testing.T) {
	silent := localUDP(t) // a unicast address that never answers the sync probe
	f := &fakeResponder{addr: silent.LocalAddr(), run: bindThen(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})}
	srv, logs := injectServer(t, f)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	if out := logs.String(); !strings.Contains(out, "could not confirm the startup Hellos") {
		t.Errorf("Start did not warn that the Hellos were unconfirmed:\n%s", out)
	}
}

func TestAwaitServingDialError(t *testing.T) {
	// A link-local address with a zone that names no interface cannot be dialled.
	err := awaitServing(&net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "nosuchif0", Port: 3702})
	if err == nil {
		t.Fatal("awaitServing with an unresolvable zone: want a dial error")
	}
	if strings.Contains(err.Error(), "no reply") {
		t.Errorf("error = %v, want a dial failure and not a probe timeout", err)
	}
}
