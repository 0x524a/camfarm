package onvif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0x524a/onvif-go/discovery"
)

// responderReadyTimeout bounds how long Start waits for the WS-Discovery
// socket to bind. Binding is local and synchronous, so hitting this means
// something is badly wrong rather than merely slow.
const responderReadyTimeout = 5 * time.Second

// responderSyncTimeout bounds the loopback probe round trip that confirms the
// responder finished its startup Hellos.
const responderSyncTimeout = 2 * time.Second

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

// bindOrAdvertised is the host multicastInterface keys on. A specific bind IP
// wins, since that is the interface the listeners are on. Otherwise the
// advertised IP is used only if a local interface owns it (it may be a NAT or
// published address, which onvif-go cannot resolve to an interface); failing
// that it is empty, which leaves the join to the default route.
func (s *Server) bindOrAdvertised() string {
	if ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(s.cfg.Host, "["), "]")); ip != nil && !ip.IsUnspecified() {
		return s.cfg.Host
	}
	if s.cfg.AdvertiseHost == "" {
		return s.cfg.Host
	}
	adv := strings.TrimSuffix(strings.TrimPrefix(s.cfg.AdvertiseHost, "["), "]")
	if ip := net.ParseIP(adv); ip != nil && localIPOwned(ip) {
		return adv
	}
	return ""
}

// localIPOwned reports whether ip is assigned to a local interface.
func localIPOwned(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// registrar is the part of the onvif-go responder that camfarm drives after
// startup. It is an interface only so a test can inject a failing Register.
type registrar interface {
	Register(*discovery.Device) error
	Unregister(endpointRef string)
	Addr() net.Addr
}

// responder is a registrar that can also be run. *discovery.Responder is the
// only production implementation; tests substitute one to inject failures.
type responder interface {
	registrar
	Start(ctx context.Context) error
}

// responderState is the running WS-Discovery responder and what Close needs to
// stop it.
type responderState struct {
	resp   registrar
	cancel context.CancelFunc
	done   chan struct{}
	errCh  chan error
	up     atomic.Bool // set once the socket is bound; gates the "died" log
}

// newResponder builds the responder and registers every current camera. The
// caller must hold s.mu for writing and have set s.ln. Registration happens
// before the responder runs, so its own initial Hello covers the whole fleet
// and Register itself sends nothing. It returns how many cameras it registered.
func (s *Server) newResponder() (responder, <-chan struct{}, int, error) {
	ready := make(chan struct{})
	build := s.newResp
	if build == nil {
		build = func(c *discovery.ResponderConfig) responder { return discovery.NewResponder(c) }
	}
	resp := build(&discovery.ResponderConfig{
		NetworkInterface: multicastInterface(s.cfg.Discovery, s.bindOrAdvertised()),
		ListenAddr:       s.cfg.Discovery.ListenAddr,
		AnnounceAddr:     s.cfg.Discovery.AnnounceAddr,
		Ready:            ready,
	})
	for _, cam := range s.cams {
		if err := s.registerLocked(resp, cam); err != nil {
			return nil, nil, 0, err
		}
	}
	return resp, ready, len(s.cams), nil
}

// runResponder starts resp and waits until it is bound and has finished its
// startup Hellos. It must be called without s.mu held. AddCamera and
// RemoveCamera wait out this window (Server.starting), so no Register can
// overlap the responder's own startup announcements.
func (s *Server) runResponder(resp responder, ready <-chan struct{}, cams int) (*responderState, error) {
	ctx, cancel := context.WithCancel(context.Background())
	st := &responderState{resp: resp, cancel: cancel, done: make(chan struct{}), errCh: make(chan error, 1)}
	go func() {
		defer close(st.done)
		err := resp.Start(ctx)
		if err != nil && ctx.Err() == nil && st.up.Load() {
			// Died on its own after a successful start. Logged here and not
			// handed on, so Close does not report it a second time. A
			// failure before the socket was bound is returned by
			// runResponder and logged by neither.
			s.log.Error("discovery responder stopped unexpectedly", "err", err)
			err = nil
		}
		st.errCh <- err
	}()

	readyTimeout := s.readyTimeout
	if readyTimeout == 0 {
		readyTimeout = responderReadyTimeout
	}
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		st.up.Store(true)
	case err := <-st.errCh:
		cancel()
		<-st.done
		return nil, fmt.Errorf("onvif: discovery responder: %w", err)
	case <-timer.C:
		cancel()
		<-st.done
		return nil, fmt.Errorf("onvif: discovery responder: socket not bound within %s", readyTimeout)
	}

	if cams > 0 {
		if err := awaitServing(resp.Addr()); err != nil {
			s.log.Warn("discovery: could not confirm the startup Hellos finished; a camera added at this instant may be announced twice", "err", err)
		}
	}
	return st, nil
}

// probeEnvelope is a minimal WS-Discovery Probe with no Types or Scopes, which
// every registered device matches.
const probeEnvelope = `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" ` +
	`xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">` +
	`<s:Header><a:MessageID>urn:uuid:00000000-0000-0000-0000-000000000000</a:MessageID>` +
	`<a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To>` +
	`<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action></s:Header>` +
	`<s:Body><d:Probe/></s:Body></s:Envelope>`

// awaitServing blocks until the responder answers a unicast probe. onvif-go
// sends its startup Hellos before it begins reading probes and offers no other
// signal that they are done, so a reply proves they have all been sent. It
// needs a registered device to reply. In multicast mode the socket is bound to
// the group, which cannot be probed without putting a probe on the real
// network, so that mode returns without waiting.
func awaitServing(addr net.Addr) error {
	ua, ok := addr.(*net.UDPAddr)
	if !ok || ua == nil {
		return fmt.Errorf("responder has no UDP address")
	}
	if ua.IP.IsMulticast() {
		return nil
	}
	dst := *ua
	if len(dst.IP) == 0 || dst.IP.IsUnspecified() {
		dst.IP = net.IPv4(127, 0, 0, 1)
		if len(ua.IP) != 0 && ua.IP.To4() == nil {
			dst.IP = net.IPv6loopback
		}
	}
	conn, err := net.DialUDP("udp", nil, &dst)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(probeEnvelope)); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(responderSyncTimeout))
	if _, err := conn.Read(make([]byte, 65535)); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return fmt.Errorf("no reply within %s", responderSyncTimeout)
		}
		return err
	}
	return nil
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
func (s *Server) registerLocked(resp registrar, cam *camera) error {
	// DiscoveryDevice returns a fresh value, but copy anyway so nothing
	// camfarm sets can ever leak back into onvif-go's state.
	dev := *cam.srv.DiscoveryDevice()
	dev.XAddrs = []string{baseURL(s.ln.Addr(), s.cfg.AdvertiseHost, cam.id) + "/device"}
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
