package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// TestVersionInfoFrom verifies the version fields are extracted from the embedded build info and
// that the rendered string carries the version plus the VCS details.
func TestVersionInfoFrom(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef1234567890"},
			{Key: "vcs.time", Value: "2026-07-16T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}

	v := versionInfoFrom(info)

	if v.Version != "v1.2.3" {
		t.Errorf("expected version v1.2.3, got %q", v.Version)
	}

	if v.Revision != "abcdef1234567890" {
		t.Errorf("expected the full revision, got %q", v.Revision)
	}

	if v.Time != "2026-07-16T00:00:00Z" {
		t.Errorf("expected the vcs time, got %q", v.Time)
	}

	if !v.Modified {
		t.Error("expected modified to be true")
	}

	s := v.String()

	for _, want := range []string{"v1.2.3", "abcdef123456", "built 2026-07-16T00:00:00Z", "modified"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected version string %q to contain %q", s, want)
		}
	}

	// The revision is truncated to 12 characters in the rendered string.
	if strings.Contains(s, "abcdef1234567890") {
		t.Errorf("expected the revision to be truncated in %q", s)
	}
}

// TestVersionInfoFrom_Empty verifies that a build with no main version falls back to "unknown"
// rather than an empty string, and that a bare version renders without the parenthetical.
func TestVersionInfoFrom_Empty(t *testing.T) {
	v := versionInfoFrom(&debug.BuildInfo{})

	if v.Version != "unknown" {
		t.Errorf("expected an empty main version to fall back to 'unknown', got %q", v.Version)
	}

	if s := v.String(); s != "unknown" {
		t.Errorf("expected a bare version string with no VCS details, got %q", s)
	}
}

// TestReadVersionInfo is a smoke test over the real runtime/debug.ReadBuildInfo() wiring (the `go
// test` binary always has build info available, so this exercises the ok-branch of readVersionInfo
// and its delegation to versionInfoFrom) plus the buildVersion ldflags-override branch, which wins
// over whatever the module version resolved to.
func TestReadVersionInfo(t *testing.T) {
	if v := readVersionInfo(); v.Version == "" {
		t.Error("expected a non-empty version from readVersionInfo()")
	}

	restore := buildVersion
	t.Cleanup(func() { buildVersion = restore })

	buildVersion = "v9.9.9-test"

	if v := readVersionInfo(); v.Version != "v9.9.9-test" {
		t.Errorf("expected buildVersion to override the resolved version, got %q", v.Version)
	}
}
