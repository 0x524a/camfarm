package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
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
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
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
