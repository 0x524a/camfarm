package rtsp

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// startSDPServer is startFaultServer for a caller-chosen media, so the same
// tests can run against an H.265 camera.
func startSDPServer(t *testing.T, m *media.Media, faults []fault.Spec) (*Server, *obs.Recorder) {
	t.Helper()
	rec := obs.New(1000)
	s, err := New(Config{Host: "127.0.0.1", Obs: rec, Cameras: []CameraConfig{
		{ID: "cam-00", Media: m, Seed: seed.Seed(21), Faults: faults},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s, rec
}

// rawResponse is one RTSP response read off a plain TCP connection, so no
// client library reinterprets what the server really sent.
type rawResponse struct {
	Status string
	Header map[string]string
	Body   string
}

// rawConn speaks RTSP by hand.
type rawConn struct {
	t    *testing.T
	c    net.Conn
	r    *bufio.Reader
	cseq int
}

func dialRaw(t *testing.T, rawURL string) *rawConn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawConn{t: t, c: c, r: bufio.NewReader(c)}
}

func (rc *rawConn) do(method, target string, headers ...string) rawResponse {
	rc.t.Helper()
	rc.cseq++
	var b strings.Builder
	b.WriteString(method + " " + target + " RTSP/1.0\r\nCSeq: " + strconv.Itoa(rc.cseq) + "\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	_ = rc.c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(rc.c, b.String()); err != nil {
		rc.t.Fatal(err)
	}
	return rc.read()
}

func (rc *rawConn) read() rawResponse {
	rc.t.Helper()
	status, err := rc.r.ReadString('\n')
	if err != nil {
		rc.t.Fatalf("reading status line: %v", err)
	}
	res := rawResponse{Status: strings.TrimSpace(status), Header: map[string]string{}}
	for {
		line, err := rc.r.ReadString('\n')
		if err != nil {
			rc.t.Fatalf("reading headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, _ := strings.Cut(line, ":")
		res.Header[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if n, _ := strconv.Atoi(res.Header["content-length"]); n > 0 {
		body := make([]byte, n)
		// A Content-Length that disagrees with the body shows up here as a
		// short read or a timeout.
		if _, err := io.ReadFull(rc.r, body); err != nil {
			rc.t.Fatalf("reading %d body bytes: %v", n, err)
		}
		res.Body = string(body)
	}
	return res
}

// describeRaw sends one DESCRIBE on a fresh connection.
func describeRaw(t *testing.T, target string) rawResponse {
	t.Helper()
	return dialRaw(t, target).do("DESCRIBE", target, "Accept: application/sdp")
}

// describeCodec asks through a real client library what codec the camera claims.
func describeCodec(t *testing.T, s *Server) string {
	t.Helper()
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatal(err)
	}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	return desc.Medias[0].Formats[0].RTPMap()
}

// pendingLieCount reports how many connections hold an undelivered lie.
func (s *Server) pendingLieCount() int {
	s.lieMu.Lock()
	defer s.lieMu.Unlock()
	return len(s.pendingLie)
}

func TestSDPLieIsVisibleOnDescribeAndClearedAfter(t *testing.T) {
	srv, _ := startFaultServer(t, nil)
	honest := describeCodec(t, srv)
	if err := srv.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec", Rate: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	lied := describeRaw(t, srv.URL("cam-00")).Body
	if !strings.Contains(lied, "H265/90000") || strings.Contains(lied, "H264/90000") {
		t.Fatalf("lie not visible:\n%s", lied)
	}
	if err := srv.SetFaults("cam-00", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := describeCodec(t, srv); got != honest {
		t.Fatalf("after clear = %v, want %v", got, honest)
	}
}

func TestSDPLieEveryModeOnTheWire(t *testing.T) {
	h265m, err := (media.FixtureH265Source{}).Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		m    *media.Media
	}{{"h264", testMedia(t)}, {"h265", h265m}} {
		for _, mode := range []string{"codec", "sprop", "resolution", "contentBase"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				s, _ := startSDPServer(t, c.m, nil)
				honest := describeRaw(t, s.URL("cam-00"))
				if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindSDPLie, Mode: mode}}, nil); err != nil {
					t.Fatal(err)
				}
				lied := describeRaw(t, s.URL("cam-00")) // read honours Content-Length
				if !strings.HasPrefix(lied.Status, "RTSP/1.0 200") {
					t.Fatalf("status = %q", lied.Status)
				}
				if mode == "contentBase" {
					if !strings.Contains(lied.Header["content-base"], "wrong.invalid") || lied.Body != honest.Body {
						t.Fatalf("contentBase lie: base %q, body changed %v", lied.Header["content-base"], lied.Body != honest.Body)
					}
					return
				}
				if lied.Body == honest.Body {
					t.Fatalf("%s lie left the SDP unchanged", mode)
				}
				if lied.Header["content-base"] != honest.Header["content-base"] {
					t.Fatalf("%s lie touched Content-Base", mode)
				}
				if got, want := strings.Join(lines(lied.Body, reCtl), "|"), strings.Join(lines(honest.Body, reCtl), "|"); got != want {
					t.Fatalf("a=control changed: %q -> %q", want, got)
				}
			})
		}
	}
}

func TestSDPLieLeavesSetupAndPlayWorking(t *testing.T) {
	s, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec"}})
	rc := dialRaw(t, s.URL("cam-00"))
	d := rc.do("DESCRIBE", s.URL("cam-00"), "Accept: application/sdp")
	if !strings.Contains(d.Body, "H265/90000") {
		t.Fatalf("lie not visible:\n%s", d.Body)
	}
	ctl := lines(d.Body, reCtl)
	if len(ctl) == 0 {
		t.Fatalf("no a=control in:\n%s", d.Body)
	}
	// SETUP against the unchanged control URL, resolved against the honest base.
	track := strings.TrimPrefix(ctl[0], "a=control:")
	setupURL := strings.TrimSuffix(s.URL("cam-00"), "/") + "/" + track
	su := rc.do("SETUP", setupURL, "Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
	if !strings.HasPrefix(su.Status, "RTSP/1.0 200") {
		t.Fatalf("SETUP = %q", su.Status)
	}
	session, _, _ := strings.Cut(su.Header["session"], ";")
	pl := rc.do("PLAY", s.URL("cam-00"), "Session: "+session)
	if !strings.HasPrefix(pl.Status, "RTSP/1.0 200") {
		t.Fatalf("PLAY = %q", pl.Status)
	}
	_ = rc.c.SetDeadline(time.Now().Add(10 * time.Second))
	b, err := rc.r.ReadByte()
	if err != nil || b != '$' {
		t.Fatalf("expected an interleaved RTP frame, got %q, %v", b, err)
	}
}

func TestSDPLieDoesNotAffectLiveReaders(t *testing.T) {
	s, rec := startFaultServer(t, nil)
	var pkts atomic.Int64
	_ = playClient(t, s.URL("cam-00"), func() { pkts.Add(1) })
	awaitCond(t, "first packets", func() bool { return pkts.Load() > 5 })
	if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec"}}, nil); err != nil {
		t.Fatal(err)
	}
	if lied := describeRaw(t, s.URL("cam-00")).Body; !strings.Contains(lied, "H265/90000") {
		t.Fatalf("lie not visible:\n%s", lied)
	}
	before := pkts.Load()
	awaitCond(t, "packets after the injection", func() bool { return pkts.Load() > before+5 })
	if got := rec.Counters("cam-00").ByKind["sdp_lie"].Applied; got != 1 {
		t.Fatalf("sdp_lie tally = %d, want 1", got)
	}
}

func TestSDPLieOnlyRequestHitsTheNthDescribe(t *testing.T) {
	s, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec", OnlyRequest: 2}})
	for i, want := range []bool{false, true, false} {
		got := strings.Contains(describeRaw(t, s.URL("cam-00")).Body, "H265/90000")
		if got != want {
			t.Fatalf("describe %d lied = %v, want %v", i+1, got, want)
		}
	}
}

// An rtsp_error wins over a lie in the same decision: the error response has no
// SDP, nothing may be left pending, and the next response must stay honest.
func TestSDPLieDoesNotLeakOrApplyAfterAnError(t *testing.T) {
	s, _ := startFaultServer(t, []fault.Spec{
		{Kind: fault.KindRTSPError, On: "describe", Status: 454, OnlyRequest: 1},
		{Kind: fault.KindSDPLie, Mode: "codec", OnlyRequest: 1},
	})
	rc := dialRaw(t, s.URL("cam-00"))
	if res := rc.do("DESCRIBE", s.URL("cam-00")); !strings.Contains(res.Status, "454") {
		t.Fatalf("status = %q, want 454", res.Status)
	}
	if n := s.pendingLieCount(); n != 0 {
		t.Fatalf("%d lies pending after an error won", n)
	}
	// The same connection's next DESCRIBE is request 2: honest.
	if res := rc.do("DESCRIBE", s.URL("cam-00")); strings.Contains(res.Body, "H265/90000") {
		t.Fatal("a lie from a failed DESCRIBE leaked into a later response")
	}
}

func TestPendingLieIsConsumedByANon200AndRemovedOnConnClose(t *testing.T) {
	s, _ := startFaultServer(t, nil)
	c1 := new(gortsplib.ServerConn)
	s.noteLie(c1, "cam-00", fault.ControlDecision{Request: 1, Lie: &fault.LieDecision{Mode: "codec"}})
	if s.pendingLieCount() != 1 {
		t.Fatal("lie not recorded")
	}
	res := &base.Response{StatusCode: base.StatusNotFound, Body: []byte("H264/90000")}
	s.OnResponse(c1, res)
	if string(res.Body) != "H264/90000" || s.pendingLieCount() != 0 {
		t.Fatalf("a non-200 must consume the lie without applying it (body %q, pending %d)", res.Body, s.pendingLieCount())
	}

	c2 := new(gortsplib.ServerConn)
	s.noteLie(c2, "cam-00", fault.ControlDecision{Request: 1, Lie: &fault.LieDecision{Mode: "codec"}})
	s.OnConnClose(&gortsplib.ServerHandlerOnConnCloseCtx{Conn: c2})
	if s.pendingLieCount() != 0 {
		t.Fatal("OnConnClose leaked a pending lie")
	}
}

func TestPendingLiesDrainAfterRealConnectionsClose(t *testing.T) {
	s, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec"}})
	for i := 0; i < 3; i++ {
		_ = describeRaw(t, s.URL("cam-00"))
	}
	awaitCond(t, "pending lies to drain", func() bool { return s.pendingLieCount() == 0 })
}
