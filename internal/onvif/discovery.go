package onvif

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/0x524a/onvif-go/discovery"
)

// responderReadyTimeout bounds how long Start waits for the WS-Discovery
// socket to bind. Binding is local and synchronous, so hitting this means
// something is badly wrong rather than merely slow.
const responderReadyTimeout = 5 * time.Second

// DiscoveryConfig makes the fleet answer WS-Discovery probes. Exactly one mode
// is chosen: Multicast, which joins 239.255.255.250:3702, or ListenAddr, which
// binds a unicast UDP socket for tests and CI where multicast is unavailable.
type DiscoveryConfig struct {
	// Multicast joins the WS-Discovery multicast group.
	Multicast bool
	// Interface selects the interface (name or IP) that joins the group.
	// Multicast mode only.
	Interface string
	// ListenAddr is the UDP address ("host:port") to bind in unicast mode.
	ListenAddr string
	// AnnounceAddr is where Hello and Bye go. Defaults to the multicast group
	// in multicast mode; empty disables announcements in unicast mode.
	AnnounceAddr string
}

func (d *DiscoveryConfig) validate() error {
	switch {
	case d.Multicast && d.ListenAddr != "":
		return fmt.Errorf("onvif: discovery: Multicast and ListenAddr are mutually exclusive")
	case !d.Multicast && d.ListenAddr == "":
		return fmt.Errorf("onvif: discovery: set either Multicast or ListenAddr")
	case d.Interface != "" && !d.Multicast:
		return fmt.Errorf("onvif: discovery: Interface applies to multicast mode only")
	}
	return nil
}

// multicastInterface picks the interface the responder joins the group on.
// An explicit Interface wins. Otherwise, in multicast mode, a literal IP host
// is used (onvif-go resolves an IP to its interface), so the join happens on
// the interface the cameras are advertised from rather than the default route.
// A hostname leaves it empty, as does unicast mode.
func multicastInterface(d *DiscoveryConfig, host string) string {
	if d.Interface != "" || !d.Multicast {
		return d.Interface
	}
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return ""
}

// responderState is the running WS-Discovery responder and what Close needs to
// stop it.
type responderState struct {
	resp   *discovery.Responder
	cancel context.CancelFunc
	done   chan struct{}
	errCh  chan error
}

// startResponder binds the responder and registers every current camera. The
// caller must hold s.mu for writing and have set s.ln. Registration happens
// before Start so the responder's initial Hello covers the whole fleet.
func (s *Server) startResponder() (*responderState, error) {
	ready := make(chan struct{})
	resp := discovery.NewResponder(&discovery.ResponderConfig{
		NetworkInterface: multicastInterface(s.cfg.Discovery, s.cfg.Host),
		ListenAddr:       s.cfg.Discovery.ListenAddr,
		AnnounceAddr:     s.cfg.Discovery.AnnounceAddr,
		Ready:            ready,
	})
	for _, cam := range s.cams {
		if err := s.registerLocked(resp, cam); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	st := &responderState{resp: resp, cancel: cancel, done: make(chan struct{}), errCh: make(chan error, 1)}
	go func() {
		defer close(st.done)
		err := resp.Start(ctx)
		if err != nil && ctx.Err() == nil {
			// Died on its own rather than via stop; without this a dead
			// responder would go unnoticed until Close.
			s.log.Error("discovery responder stopped unexpectedly", "err", err)
		}
		st.errCh <- err
	}()

	timer := time.NewTimer(responderReadyTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return st, nil
	case err := <-st.errCh:
		cancel()
		<-st.done
		return nil, fmt.Errorf("onvif: discovery responder: %w", err)
	case <-timer.C:
		cancel()
		<-st.done
		return nil, fmt.Errorf("onvif: discovery responder: socket not bound within %s", responderReadyTimeout)
	}
}

// stop cancels the responder (which sends Bye for the remaining devices) and
// waits for its goroutine to exit.
func (st *responderState) stop(s *Server) {
	st.cancel()
	<-st.done
	select {
	case err := <-st.errCh:
		if err != nil {
			s.log.Warn("discovery responder stopped", "err", err)
		}
	default:
	}
}

// registerLocked advertises one camera on resp, with the device XAddr
// overridden to camfarm's own bound ONVIF listener (onvif-go's value reflects
// a port it never bound). Requires s.mu held and s.ln set.
func (s *Server) registerLocked(resp *discovery.Responder, cam *camera) error {
	// DiscoveryDevice returns a fresh value, but copy anyway so nothing
	// camfarm sets can ever leak back into onvif-go's state.
	dev := *cam.srv.DiscoveryDevice()
	dev.XAddrs = []string{fmt.Sprintf("http://%s/onvif/%s/device", s.ln.Addr().String(), cam.id)}
	if err := resp.Register(&dev); err != nil {
		return fmt.Errorf("onvif: registering %q for discovery: %w", cam.id, err)
	}
	cam.discoRef = dev.EndpointRef
	return nil
}

// DiscoveryAddr returns the bound WS-Discovery socket address, or nil when
// discovery is disabled, not started, or closed.
func (s *Server) DiscoveryAddr() net.Addr {
	s.mu.RLock()
	st := s.responder
	s.mu.RUnlock()
	if st == nil {
		return nil
	}
	return st.resp.Addr()
}
