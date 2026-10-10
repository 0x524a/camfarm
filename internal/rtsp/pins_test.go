package rtsp

import (
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/0x524a/camfarm/internal/fault"
)

// The tests in this file pin the gortsplib behaviour the faults depend on, one
// per mechanism, against the real implementation. A gortsplib upgrade that
// changes the mechanism fails here first, with a name that says which one.

// Pins that a body and Content-Base rewritten in OnResponse reach a real
// client library, which parses them as if the camera had meant them.
func TestPinOnResponseRewriteReachesTheClientLibrary(t *testing.T) {
	describe := func(t *testing.T, mode string) (media0 format.Format, baseHost string) {
		s, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindSDPLie, Mode: mode}})
		u, err := base.ParseURL(s.URL("cam-00"))
		if err != nil {
			t.Fatal(err)
		}
		c, _ := newTestClient(u)
		if err := c.Start(); err != nil {
			t.Fatalf("client start: %v", err)
		}
		defer c.Close()
		desc, _, err := c.Describe(u)
		if err != nil {
			t.Fatalf("DESCRIBE: %v", err)
		}
		if len(desc.Medias) == 0 || len(desc.Medias[0].Formats) == 0 {
			t.Fatal("no media in description")
		}
		return desc.Medias[0].Formats[0], desc.BaseURL.Host
	}
	if f, _ := describe(t, "codec"); f == nil {
		t.Fatal("no format")
	} else if _, ok := f.(*format.H265); !ok {
		t.Fatalf("codec lie: first format is %T, want *format.H265", f)
	}
	if _, host := describe(t, "contentBase"); host != "wrong.invalid" {
		t.Fatalf("contentBase lie: client base host = %q", host)
	}
}

// Pins that closing the underlying TCP conn with linger 0 reaches a real
// client as a connection reset.
func TestPinNetConnResetReachesClient(t *testing.T) {
	s, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 3, Mode: "reset"}})
	c := playClient(t, s.URL("cam-00"), func() {})
	select {
	case err := <-c.Done():
		t.Logf("client Wait error: %v", err)
		if err == nil || !strings.Contains(err.Error(), "reset") {
			t.Fatalf("want a connection reset, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client never saw the connection end")
	}
}
