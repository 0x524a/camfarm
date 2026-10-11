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
	"strconv"
	"strings"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/dashboard"
	"github.com/0x524a/camfarm/internal/library"
)

const usage = `CamFarm runs a simulated RTSP/ONVIF camera fleet with a web dashboard.

SECURITY: the dashboard listens on loopback by default and has no
authentication, no Host-header check and no CSRF protection. Its API can start
servers, and with file sources make this process read local paths. The
dashboard also displays cameras' Digest credentials. Do not expose it to a
network you do not trust. --media-dir makes every video file under that
directory listable and servable, and uploads (capped by --max-upload) write to the
first writable one;
--allow-ui-paths lets anyone who can reach the dashboard point it at any
readable directory.

Seed: --seed accepts decimal or 0x-prefixed hex (a leading 0 means octal, per
Go's flag parsing). Without --seed, a --spec file's own seed is used (0 if the
file has none); with neither, a random seed is drawn and printed on stderr.
--cameras and --spec are mutually exclusive.

Flags:
`

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// exitError carries an exit code from run to main. quiet means the message was
// already printed (by the flag package) and main must not print it again.
type exitError struct {
	code  int
	err   error
	quiet bool
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, a ...any) error {
	return &exitError{code: 2, err: fmt.Errorf(format, a...)}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("camfarm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	specPath := fs.String("spec", "", "path to a JSON fleet spec")
	seedFlag := fs.Uint64("seed", 0, "root seed (drawn at random and printed when omitted)")
	cameras := fs.Int("cameras", 1, "number of cameras cam-1..N on the bundled fixture (cannot be combined with --spec)")
	httpAddr := fs.String("http-addr", "127.0.0.1:8080", "dashboard listen address")
	listenHost := fs.String("listen-host", "", "host the RTSP and ONVIF listeners bind (default 127.0.0.1)")
	rtspPort := fs.Int("rtsp-port", 0, "RTSP port (0 picks a free one)")
	onvifPort := fs.Int("onvif-port", 0, "ONVIF port (0 picks a free one)")
	discoveryFlag := fs.String("discovery", "", "answer WS-Discovery probes: \"multicast\" or \"unicast:HOST:PORT\" (empty leaves the spec as is).\n"+
		"multicast needs a non-loopback --listen-host and often fails in Docker and CI; use unicast there.\n"+
		"replaces the spec file's whole discovery block; unicast takes no interface")
	discoveryIface := fs.String("discovery-interface", "", "interface (name or IP) that joins the multicast group; only with multicast discovery")
	var mediaDirs stringList
	fs.Var(&mediaDirs, "media-dir", "directory of video files for the media library; repeatable.\n"+
		"the first writable one receives dashboard uploads. Files are validated once, at startup, on rescan and on upload")
	allowUIPaths := fs.Bool("allow-ui-paths", false, "let the dashboard add more media directories by typed path (any directory this process can read)")
	maxUpload := fs.String("max-upload", "2GiB", "largest accepted dashboard upload, e.g. 500MB or 2GiB")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &exitError{code: 2, err: err, quiet: true}
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	for name, port := range map[string]int{"rtsp-port": *rtspPort, "onvif-port": *onvifPort} {
		if port < 0 || port > 65535 {
			return usageErr("--%s %d out of range 0..65535", name, port)
		}
	}
	if _, _, err := net.SplitHostPort(*httpAddr); err != nil {
		return usageErr("invalid --http-addr %q: %v", *httpAddr, err)
	}
	maxBytes, err := parseSize(*maxUpload)
	if err != nil {
		return usageErr("invalid --max-upload %q: %v", *maxUpload, err)
	}
	for _, d := range mediaDirs {
		if d == "" {
			return usageErr("--media-dir must not be empty")
		}
	}
	if set["cameras"] && *specPath != "" {
		return usageErr("--cameras and --spec are mutually exclusive")
	}

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
			return usageErr("--cameras must be at least 1")
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
	if err := applyDiscovery(&spec, *discoveryFlag, *discoveryIface); err != nil {
		return usageErr("%v", err)
	}

	lib, err := library.New(library.Options{Roots: mediaDirs, AllowUIPaths: *allowUIPaths, MaxUpload: maxBytes})
	if err != nil {
		return usageErr("--media-dir: %v", err)
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

	_, _ = fmt.Fprintln(stderr, fleet.ReplayLine())
	if a := fleet.DiscoveryAddr(); a != "" {
		d := spec.Listen.Discovery
		_, _ = fmt.Fprintln(stdout, discoveryLine(d != nil && d.Mode == camfarm.DiscoveryMulticast, a))
	}
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			_, _ = fmt.Fprintln(stderr, "warning: the dashboard is not bound to loopback and has no authentication")
		}
	}

	// Request contexts derive from bctx; cancelling it on shutdown ends
	// long-lived SSE streams so Shutdown does not wait out its full timeout.
	bctx, bcancel := context.WithCancel(context.Background())
	defer bcancel()
	srv := &http.Server{
		Handler:           dashboard.Handler(fleet, dashboard.Options{Library: lib}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second, // bounds slow request bodies; SSE is response-only
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return bctx },
	}
	srv.RegisterOnShutdown(bcancel)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	_, _ = fmt.Fprintf(stdout, "dashboard: http://%s/\n", ln.Addr())

	// Scan in the background; wait for it on the way out so nothing writes to
	// stderr after run returns.
	scanCtx, scanCancel := context.WithCancel(ctx)
	scanDone := make(chan struct{})
	defer func() { scanCancel(); <-scanDone }()
	go func() {
		defer close(scanDone)
		if err := lib.Scan(scanCtx, "", false); err != nil && scanCtx.Err() == nil {
			_, _ = fmt.Fprintf(stderr, "warning: scanning the media library: %v\n", err)
		}
	}()

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

// applyDiscovery folds the --discovery and --discovery-interface flag values
// into spec.Listen.Discovery. Empty values leave the spec alone, so a flag
// overrides a spec file only when given. The wildcard and loopback host rules
// are camfarm's own validation, applied at Start.
// wsDiscoveryGroup is the WS-Discovery multicast group and port. onvif-go keeps
// the same value in an unexported constant, so it cannot be imported.
const wsDiscoveryGroup = "239.255.255.250:3702"

// discoveryLine is the startup line naming where discovery listens. In
// multicast mode the bound socket is 0.0.0.0:3702, but the group is what a
// reader needs; in unicast mode it is the bound address.
func discoveryLine(multicast bool, boundAddr string) string {
	if multicast {
		return "discovery: udp://" + wsDiscoveryGroup + " (multicast)"
	}
	return "discovery: udp://" + boundAddr
}

func applyDiscovery(spec *camfarm.Spec, mode, iface string) error {
	const forms = `--discovery must be "multicast" or "unicast:HOST:PORT"`
	switch {
	case mode == "":
	case mode == camfarm.DiscoveryMulticast:
		spec.Listen.Discovery = &camfarm.DiscoverySpec{Mode: camfarm.DiscoveryMulticast}
	case strings.HasPrefix(mode, camfarm.DiscoveryUnicast+":"):
		addr := strings.TrimPrefix(mode, camfarm.DiscoveryUnicast+":")
		// An empty host binds all interfaces, as in the spec's addr field.
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("invalid --discovery %q: %w; %s", mode, err, forms)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("invalid --discovery %q: port %q is not a number from 0 to 65535; %s", mode, port, forms)
		}
		spec.Listen.Discovery = &camfarm.DiscoverySpec{Mode: camfarm.DiscoveryUnicast, Addr: addr}
	default:
		return fmt.Errorf("invalid --discovery %q; %s", mode, forms)
	}
	if iface == "" {
		return nil
	}
	if spec.Listen.Discovery == nil || spec.Listen.Discovery.Mode != camfarm.DiscoveryMulticast {
		return errors.New("--discovery-interface only applies to multicast discovery (use --discovery multicast)")
	}
	spec.Listen.Discovery.Interface = iface
	return nil
}
