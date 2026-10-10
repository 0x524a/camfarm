package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/dashboard"
)

const usage = `CamFarm runs a simulated RTSP/ONVIF camera fleet with a web dashboard.

SECURITY: the dashboard listens on loopback by default and has no
authentication, no Host-header check and no CSRF protection. Its API can start
servers, and with file sources make this process read local paths. The
dashboard also displays cameras' Digest credentials. Do not expose it to a
network you do not trust.

Flags:
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("camfarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	specPath := fs.String("spec", "", "path to a JSON fleet spec")
	seedFlag := fs.Uint64("seed", 0, "root seed (drawn at random and printed when omitted)")
	cameras := fs.Int("cameras", 1, "number of cameras cam-1..N on the bundled fixture (ignored with --spec)")
	httpAddr := fs.String("http-addr", "127.0.0.1:8080", "dashboard listen address")
	listenHost := fs.String("listen-host", "", "host the RTSP and ONVIF listeners bind (default 127.0.0.1)")
	rtspPort := fs.Int("rtsp-port", 0, "RTSP port (0 picks a free one)")
	onvifPort := fs.Int("onvif-port", 0, "ONVIF port (0 picks a free one)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var spec camfarm.Spec
	if *specPath != "" {
		b, err := os.ReadFile(*specPath)
		if err != nil {
			return fmt.Errorf("reading spec: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return fmt.Errorf("parsing spec %s: %w", *specPath, err)
		}
	} else {
		if *cameras < 1 {
			return errors.New("--cameras must be at least 1")
		}
		for i := 1; i <= *cameras; i++ {
			spec.Cameras = append(spec.Cameras, camfarm.CameraSpec{ID: fmt.Sprintf("cam-%d", i)})
		}
	}
	switch {
	case set["seed"]:
		spec.Seed = *seedFlag
	case *specPath == "":
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fmt.Errorf("drawing a seed: %w", err)
		}
		spec.Seed = binary.BigEndian.Uint64(b[:])
	}
	if set["listen-host"] {
		spec.Listen.Host = *listenHost
	}
	if set["rtsp-port"] {
		spec.Listen.RTSPPort = *rtspPort
	}
	if set["onvif-port"] {
		spec.Listen.ONVIFPort = *onvifPort
	}

	ln, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return fmt.Errorf("dashboard listener: %w", err)
	}
	defer ln.Close()

	// Detached from ctx so shutdown can close the HTTP server before the fleet.
	fleet, err := camfarm.Start(context.WithoutCancel(ctx), spec)
	if err != nil {
		return err
	}
	defer fleet.Close()

	fmt.Fprintln(stderr, fleet.ReplayLine())
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			fmt.Fprintln(stderr, "warning: the dashboard is not bound to loopback and has no authentication")
		}
	}

	srv := &http.Server{Handler: dashboard.Handler(fleet, dashboard.Options{}), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(stdout, "dashboard: http://%s/\n", ln.Addr())

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	// Close the HTTP server before the fleet (deferred) so no handler touches a
	// closed fleet. SSE streams never end on their own, so after the grace
	// period force-close them.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
	}
	return nil
}
