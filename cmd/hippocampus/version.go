package main

import (
	"runtime/debug"
	"strings"
)

// versionInfo is the build identification derived from the Go module version and the VCS settings
// the toolchain embeds at build time (runtime/debug.ReadBuildInfo). It is logged at startup,
// surfaced by --version, reported in the /healthz body, and set as the OTEL service.version
// resource attribute, so a running instance can always be tied back to the build it came from.
type versionInfo struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
	Modified bool   `json:"modified,omitempty"`
}

// readVersionInfo reads the embedded build information. When it is unavailable (e.g. a binary built
// without module support) it returns a version of "unknown" rather than an empty string, so the
// startup log and /healthz body always carry a value.
func readVersionInfo() versionInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return versionInfo{Version: "unknown"}
	}

	return versionInfoFrom(info)
}

// versionInfoFrom extracts the version fields from a debug.BuildInfo. It is split out from
// readVersionInfo so it can be unit-tested against a stubbed BuildInfo.
func versionInfoFrom(info *debug.BuildInfo) versionInfo {
	out := versionInfo{Version: info.Main.Version}

	// A binary built from a working tree (go build, go run) reports "(devel)" or an empty main
	// version; the VCS settings below still identify the exact commit.
	if out.Version == "" {
		out.Version = "unknown"
	}

	for _, setting := range info.Settings {
		switch setting.Key {

		case "vcs.revision":
			out.Revision = setting.Value

		case "vcs.time":
			out.Time = setting.Value

		case "vcs.modified":
			out.Modified = setting.Value == "true"

		}
	}

	return out
}

// String renders the version as a single human-readable line for the startup log and --version
// output, e.g. "v1.2.3 (revision abc1234, built 2026-07-16T00:00:00Z, modified)".
func (v versionInfo) String() string {
	var parts []string

	if v.Revision != "" {
		revision := v.Revision
		if len(revision) > 12 {
			revision = revision[:12]
		}

		parts = append(parts, "revision "+revision)
	}

	if v.Time != "" {
		parts = append(parts, "built "+v.Time)
	}

	if v.Modified {
		parts = append(parts, "modified")
	}

	if len(parts) == 0 {
		return v.Version
	}

	return v.Version + " (" + strings.Join(parts, ", ") + ")"
}
