package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestComposePortsBindAnAddress (TODO-3 item 168): a published port with no address binds every host
// interface, and a container runtime's published ports go around a host firewall such as ufw - so
// with auth off, as these stacks run, the whole API was open to the network the host sits on. Every
// published port names an address, ${PUBLISH_ADDRESS:-127.0.0.1} unless it is loopback outright.
func TestComposePortsBindAnAddress(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "compose", "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %s", err)
	}

	files = append(files, filepath.Join("..", "..", "docker-compose.yaml"))

	published := regexp.MustCompile(`(?m)^\s+- "([^"]*:\d+)"`)
	bare := regexp.MustCompile(`^\d+:\d+$`)

	checked := 0

	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %s", path, err)
		}

		for _, match := range published.FindAllStringSubmatch(string(source), -1) {
			checked++

			if bare.MatchString(match[1]) {
				t.Errorf("%s publishes %q on every interface; prefix it with ${PUBLISH_ADDRESS:-127.0.0.1}:", path, match[1])
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no published ports - the pattern no longer matches the compose files")
	}
}
