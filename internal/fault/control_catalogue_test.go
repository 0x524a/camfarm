package fault

import (
	"errors"
	"testing"
)

func TestControlKindsAreCatalogued(t *testing.T) {
	for _, k := range []Kind{KindRTSPError, KindTeardown, KindSDPLie} {
		info, ok := Info(k)
		if !ok {
			t.Fatalf("%s not catalogued", k)
		}
		if info.Models == "" || info.Clock == "" || len(info.Params) == 0 {
			t.Errorf("%s: incomplete catalogue entry %+v", k, info)
		}
	}
	for _, old := range []string{"teardown_mid_session", "sdp_codec_lie", "sps_resolution_lie"} {
		if Known(Kind(old)) {
			t.Errorf("placeholder %q should be gone", old)
		}
	}
}

func TestEnumParamsAreCatalogued(t *testing.T) {
	info, _ := Info(KindSDPLie)
	var mode Param
	for _, p := range info.Params {
		if p.Name == "mode" {
			mode = p
		}
	}
	if mode.Type != ParamEnum || len(mode.Options) != 4 {
		t.Fatalf("sdp_lie.mode = %+v, want enum with codec, sprop, resolution, contentBase", mode)
	}
}

// The new kinds are unimplemented, so Validate skips them. checkParams is the
// parameter check Validate applies once a kind flips; it is driven here with
// the real catalogue entries so enum validation is tested before then.
func TestEnumMembershipIsChecked(t *testing.T) {
	sdp, _ := Info(KindSDPLie)
	rtsp, _ := Info(KindRTSPError)
	tear, _ := Info(KindTeardown)
	cases := []struct {
		name string
		info KindInfo
		spec Spec
		want error
	}{
		{"sdp_lie ok", sdp, Spec{Kind: KindSDPLie, Mode: "sprop"}, nil},
		{"sdp_lie bad mode", sdp, Spec{Kind: KindSDPLie, Mode: "nope"}, ErrBadParam},
		{"sdp_lie mode required", sdp, Spec{Kind: KindSDPLie}, ErrBadParam},
		{"sdp_lie on does not apply", sdp, Spec{Kind: KindSDPLie, Mode: "codec", On: "play"}, ErrBadParam},
		{"rtsp_error ok", rtsp, Spec{Kind: KindRTSPError, On: "all", Status: 454, Rate: 0.5}, nil},
		{"rtsp_error bad on", rtsp, Spec{Kind: KindRTSPError, On: "teardown"}, ErrBadParam},
		{"rtsp_error status range", rtsp, Spec{Kind: KindRTSPError, On: "play", Status: 200}, ErrBadParam},
		{"teardown ok", tear, Spec{Kind: KindTeardown, AfterFrames: 10, Mode: "reset"}, nil},
		{"teardown bad mode", tear, Spec{Kind: KindTeardown, AfterFrames: 10, Mode: "codec"}, ErrBadParam},
		{"teardown afterFrames required", tear, Spec{Kind: KindTeardown}, ErrBadParam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkParams(tc.info, tc.spec); !errors.Is(err, tc.want) {
				t.Fatalf("checkParams = %v, want %v", err, tc.want)
			}
		})
	}
}
