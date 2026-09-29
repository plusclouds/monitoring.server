// Package buildinfo holds the version stamped into the binary at release
// (-ldflags "-X github.com/plusclouds/monitoring.server/internal/buildinfo.Version=…").
package buildinfo

import "runtime/debug"

var (
	Version = "dev"
	Commit  = ""
)

// CommitOrVCS returns Commit, or the VCS revision Go recorded at build time.
func CommitOrVCS() string {
	if Commit != "" {
		return Commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}
