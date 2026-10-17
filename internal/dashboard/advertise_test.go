package dashboard

import (
	"strings"
	"testing"

	"github.com/0x524A/rtspeek/pkg/rtspeek"
	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/quality"
	"net/http/httptest"
)

// The quality runner dials from inside the process, so it must get the bound
// address, not the advertised host (which here cannot resolve).
func TestStartQualityDialsTheBoundAddressNotTheAdvertisedHost(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	fleet := camfarm.StartT(t, camfarm.Spec{
		Seed:    1,
		Cameras: []camfarm.CameraSpec{{ID: "front-door"}},
		Listen:  camfarm.ListenSpec{Host: "127.0.0.1", AdvertiseHost: "camfarm.invalid"},
	})
	r := quality.New(quality.Options{Analyze: f.analyze})
	t.Cleanup(r.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Quality: r}))
	t.Cleanup(ts.Close)
	cam, _ := fleet.Camera("front-door")

	resp := do(t, "POST", qURL(ts, "front-door"), "")
	if resp.StatusCode != 202 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	u := <-f.urls
	if strings.Contains(u, "invalid") || u != cam.LocalRTSPURL() {
		t.Errorf("analysed %q, want the bound address %q", u, cam.LocalRTSPURL())
	}
	close(f.release)
}
