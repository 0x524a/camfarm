package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/0x524a/camfarm"
)

func TestFaultCatalogueEndpoint(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/api/v1/faults", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var cat []struct {
		Kind        string `json:"kind"`
		Implemented bool   `json:"implemented"`
		Models      string `json:"models"`
		Params      []struct {
			Name     string   `json:"name"`
			Type     string   `json:"type"`
			Required bool     `json:"required"`
			Options  []string `json:"options"`
		} `json:"params"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	byKind := map[string]int{}
	for i, k := range cat {
		byKind[k.Kind] = i
		if k.Models == "" {
			t.Errorf("%s has no models text", k.Kind)
		}
	}
	for _, want := range []string{"frame_drop", "packet_loss", "keyframe_starvation", "accept_then_silence"} {
		if _, ok := byKind[want]; !ok {
			t.Fatalf("catalogue lacks %q: %+v", want, cat)
		}
	}
	pl := cat[byKind["packet_loss"]]
	if !pl.Implemented {
		t.Error("packet_loss should be implemented")
	}
	names := map[string]bool{}
	for _, p := range pl.Params {
		names[p.Name] = true
	}
	for _, n := range []string{"rate", "meanBurst", "protectKeyframes"} {
		if !names[n] {
			t.Errorf("packet_loss lacks param %q", n)
		}
	}
	if cat[byKind["accept_then_silence"]].Implemented {
		t.Error("accept_then_silence should not be implemented")
	}
	for _, k := range []string{"rtsp_error", "teardown", "sdp_lie"} {
		i, ok := byKind[k]
		if !ok {
			t.Fatalf("catalogue lacks %q", k)
		}
		if !cat[i].Implemented {
			t.Errorf("%s should be implemented", k)
		}
	}
	var got []string
	for _, p := range cat[byKind["sdp_lie"]].Params {
		if p.Name == "mode" {
			got = p.Options
		}
	}
	if want := []string{"codec", "sprop", "resolution", "contentBase"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sdp_lie mode options = %v, want %v", got, want)
	}
}

func TestFaultCatalogueListsTheTimingKindsAsImplemented(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/api/v1/faults", "")
	var cat []struct {
		Kind        string `json:"kind"`
		Implemented bool   `json:"implemented"`
		Params      []struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"jitter":           {"maxMs"},
		"burst_delivery":   {"rate", "holdFrames"},
		"pts_dts_mismatch": {"reorderDepth"},
		"pts_delayed":      {"delayMs", "driftPpm"},
	}
	seen := 0
	for _, k := range cat {
		params, ok := want[k.Kind]
		if !ok {
			continue
		}
		seen++
		if !k.Implemented {
			t.Errorf("%s should be implemented", k.Kind)
		}
		var got []string
		for _, p := range k.Params {
			got = append(got, p.Name)
		}
		if !reflect.DeepEqual(got, params) {
			t.Errorf("%s params = %v, want %v", k.Kind, got, params)
		}
	}
	if seen != len(want) {
		t.Errorf("found %d of %d timing kinds", seen, len(want))
	}
}

func TestInjectRoundTripsEnumAndIntegerFaultFields(t *testing.T) {
	ts, f := newServer(t, Options{})
	resp := do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults",
		`{"kind":"rtsp_error","on":"setup","status":454,"onlyRequest":2,"rate":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inject status = %d", resp.StatusCode)
	}
	cam, _ := f.Camera("front-door")
	got := cam.Faults()
	if len(got) != 1 || got[0].On != "setup" || got[0].Status != 454 || got[0].OnlyRequest != 2 {
		t.Fatalf("faults = %+v", got)
	}
	if resp := do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"teardown","afterFrames":5,"mode":"reset"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("teardown inject status = %d", resp.StatusCode)
	}
	var out []struct {
		Faults []camfarm.FaultSpec `json:"faults"`
	}
	if err := json.NewDecoder(do(t, "GET", ts.URL+"/api/v1/cameras", "").Body).Decode(&out); err != nil || len(out) != 1 {
		t.Fatalf("decode: %v %d", err, len(out))
	}
	var td *camfarm.FaultSpec
	for i := range out[0].Faults {
		if out[0].Faults[i].Kind == "teardown" {
			td = &out[0].Faults[i]
		}
	}
	if td == nil || td.AfterFrames != 5 || td.Mode != "reset" {
		t.Fatalf("camera JSON faults = %+v", out[0].Faults)
	}
}

func TestCameraJSONCarriesPerKindFaultCounts(t *testing.T) {
	ts, _ := newServer(t, Options{})
	if resp := do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"frame_drop","rate":1}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("inject status = %d", resp.StatusCode)
	}
	waitFor(t, "frame_drop applied count", func() bool {
		resp := do(t, "GET", ts.URL+"/api/v1/cameras", "")
		var cams []struct {
			Stats struct {
				FaultCounts map[string]struct{ Applied, Suppressed uint64 }
			} `json:"stats"`
		}
		if json.NewDecoder(resp.Body).Decode(&cams) != nil || len(cams) != 1 {
			return false
		}
		return cams[0].Stats.FaultCounts["frame_drop"].Applied >= 2
	})
}
