// Package buildinfo reports which build of camfarm is running.
//
// Release builds stamp Version, Commit and Date with -ldflags -X (see
// .goreleaser.yaml and the Dockerfile). A build that was not stamped, such as
// `go build` in a checkout or `go install`, falls back to what the Go
// toolchain recorded in the binary.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at link time. Empty means "not stamped".
var (
	Version string
	Commit  string
	Date    string
)

// String is the one-line description printed by --version.
func String() string {
	bi, _ := debug.ReadBuildInfo()
	return describe(Version, Commit, Date, bi, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
}

// describe is String with its inputs passed in so a test can pin them.
func describe(version, commit, date string, bi *debug.BuildInfo, goVersion, platform string) string {
	if bi != nil {
		if version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = s.Value
				}
			case "vcs.time":
				if date == "" {
					date = s.Value
				}
			}
		}
	}
	if version == "" {
		version = "dev"
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	out := "camfarm " + version
	if commit != "" {
		out += " commit " + commit
	}
	if date != "" {
		out += " built " + date
	}
	return fmt.Sprintf("%s (%s %s)", out, goVersion, platform)
}
