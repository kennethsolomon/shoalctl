// Package buildinfo names the running shoalctl build: the version, commit and
// date a release build injects with -ldflags, or what the Go toolchain
// recorded for a go install or go build.
package buildinfo

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Unknown is the value of any field nothing recorded.
const Unknown = "dev"

// Info describes one build.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Resolve fills what the release build did not inject (an empty string or
// Unknown) from the toolchain's build info: the module version of a
// go install <module>@<version> (for a build inside a git checkout, the
// pseudo-version Go stamps, "+dirty" when it had uncommitted changes), and
// vcs.revision and vcs.time. A "(devel)" module version, a missing field and
// a missing build info all stay Unknown.
func Resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	info := Info{Version: orUnknown(version), Commit: orUnknown(commit), Date: orUnknown(date)}
	if bi == nil {
		return info
	}
	if info.Version == Unknown && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == Unknown && s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == Unknown && s.Value != "" {
				info.Date = s.Value
			}
		}
	}
	return info
}

// Current is Resolve with the running binary's build info.
func Current(version, commit, date string) Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return Resolve(version, commit, date, bi)
}

// String is the one line shoalctl version prints.
func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return fmt.Sprintf("%s (commit %s, built %s)", i.Version, commit, i.Date)
}

func orUnknown(s string) string {
	if s == "" {
		return Unknown
	}
	return s
}
