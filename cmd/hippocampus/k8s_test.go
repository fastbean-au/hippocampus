package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// TestKubernetesOverlaysPinTheImage (TODO-3 item 170): every workload named
// ghcr.io/fastbean-au/hippocampus:latest, so a node that pulled the image afresh could run a newer
// build than the rest - and a newer build migrates the schema forward, after which a rollback is
// refused with ErrSchemaTooNew. Each overlay pins the tag with kustomize's images stanza, to the
// newest version CHANGELOG.md has released, which scripts/release.sh bumps as it cuts one.
func TestKubernetesOverlaysPinTheImage(t *testing.T) {
	changelog, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("reading the changelog: %s", err)
	}

	released := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindSubmatch(changelog)
	if released == nil {
		t.Fatal("no released version in CHANGELOG.md")
	}

	want := string(released[1])

	overlays, err := filepath.Glob(filepath.Join("..", "..", "deploy", "k8s", "overlays", "*", "kustomization.yaml"))
	if err != nil || len(overlays) == 0 {
		t.Fatalf("found no overlays: %v", err)
	}

	for _, path := range overlays {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %s", path, err)
		}

		var kustomization struct {
			Images []struct {
				Name   string `yaml:"name"`
				NewTag string `yaml:"newTag"`
			} `yaml:"images"`
		}

		if err := yaml.Unmarshal(source, &kustomization); err != nil {
			t.Fatalf("parsing %s: %s", path, err)
		}

		pinned := ""

		for _, image := range kustomization.Images {
			if image.Name == "ghcr.io/fastbean-au/hippocampus" {
				pinned = image.NewTag
			}
		}

		if pinned != want {
			t.Errorf("%s pins the hippocampus image to %q, want the newest release %q", path, pinned, want)
		}
	}
}

// TestPinK8sImageScriptBumpsEveryOverlay runs the script release.sh calls against a copy of the
// overlays, since nothing else executes it until the day a release depends on it.
func TestPinK8sImageScriptBumpsEveryOverlay(t *testing.T) {
	root := t.TempDir()

	overlays, err := filepath.Glob(filepath.Join("..", "..", "deploy", "k8s", "overlays", "*", "kustomization.yaml"))
	if err != nil || len(overlays) == 0 {
		t.Fatalf("found no overlays: %v", err)
	}

	for _, path := range overlays {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %s", err)
		}

		target := filepath.Join(root, "deploy", "k8s", "overlays", filepath.Base(filepath.Dir(path)), "kustomization.yaml")

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir: %s", err)
		}

		if err := os.WriteFile(target, source, 0o600); err != nil {
			t.Fatalf("write: %s", err)
		}
	}

	for range 2 { // twice: the second run proves it idempotent
		out, err := exec.Command(filepath.Join("..", "..", "scripts", "pin-k8s-image.sh"), "v9.8.7", root).CombinedOutput()
		if err != nil {
			t.Fatalf("pin-k8s-image.sh: %s\n%s", err, out)
		}
	}

	pinned, err := filepath.Glob(filepath.Join(root, "deploy", "k8s", "overlays", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("glob: %s", err)
	}

	for _, path := range pinned {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %s", err)
		}

		if !regexp.MustCompile(`name: ghcr\.io/fastbean-au/hippocampus\s*\n\s+newTag: "9\.8\.7"`).Match(source) {
			t.Errorf("%s was not pinned to 9.8.7:\n%s", path, source)
		}
	}
}
