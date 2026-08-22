package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestFreeTCPPort_AvoidsTheEphemeralRange is the regression test for a CI flake in which run failed
// with "bind: address already in use" on a port this helper had just reported free. The helper used
// to reserve an ephemeral port and release it, which leaves the kernel free to hand that very port
// to another package's test binary - running concurrently, as go test does - in the window before
// run binds it. Drawing from below the ephemeral range is what closes that window, so the range is
// the property worth pinning.
func TestFreeTCPPort_AvoidsTheEphemeralRange(t *testing.T) {
	port := freeTCPPort(t)

	if port < testPortRangeStart || port >= testPortRangeEnd {
		t.Fatalf("expected a port in %d-%d, got %d", testPortRangeStart, testPortRangeEnd, port)
	}

	low, ok := ephemeralPortRangeStart(t)
	if !ok {
		return
	}

	if port >= low {
		t.Errorf("port %d is inside the kernel's ephemeral range (from %d), which another process may be handed", port, low)
	}
}

// TestFreeTCPPort_NeverRepeats covers the other half: two tests in one run must not be given the
// same port, which the kernel's own allocator used to guarantee and a random draw does not.
func TestFreeTCPPort_NeverRepeats(t *testing.T) {
	seen := map[int]struct{}{}

	for range 50 {
		port := freeTCPPort(t)

		if _, repeated := seen[port]; repeated {
			t.Fatalf("port %d was issued twice", port)
		}

		seen[port] = struct{}{}
	}
}

// ephemeralPortRangeStart reports the lowest port the kernel allocates automatically, where that is
// readable without shelling out (Linux). Elsewhere it reports false and the caller skips that half.
func ephemeralPortRangeStart(t *testing.T) (int, bool) {
	t.Helper()

	if runtime.GOOS != "linux" {
		return 0, false
	}

	contents, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, false
	}

	fields := strings.Fields(string(contents))
	if len(fields) == 0 {
		return 0, false
	}

	low, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false
	}

	return low, true
}
