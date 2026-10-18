package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestDescribe(t *testing.T) {
	vcs := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.3.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"},
			{Key: "vcs.time", Value: "2026-10-18T00:00:00Z"},
		},
	}
	tests := []struct {
		name                  string
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  string
	}{
		{"stamped values win", "v1.2.3", "abc", "2026-01-02", vcs,
			"camfarm v1.2.3 commit abc built 2026-01-02 (go1.26 linux/amd64)"},
		{"toolchain fills the gaps", "", "", "", vcs,
			"camfarm v0.3.0 commit 0123456789ab built 2026-10-18T00:00:00Z (go1.26 linux/amd64)"},
		{"devel module version is not a version", "", "", "",
			&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			"camfarm dev (go1.26 linux/amd64)"},
		{"no build info at all", "", "", "", nil,
			"camfarm dev (go1.26 linux/amd64)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := describe(tc.version, tc.commit, tc.date, tc.bi, "go1.26", "linux/amd64")
			if got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestStringIsNeverEmpty(t *testing.T) {
	if s := String(); len(s) < len("camfarm dev") {
		t.Fatalf("String() = %q", s)
	}
}
