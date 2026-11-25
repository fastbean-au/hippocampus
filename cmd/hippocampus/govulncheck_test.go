package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// govulncheckConfig is the first line govulncheck's JSON output carries, written before it fetches
// anything - so its presence says the scanner started, not that it scanned.
const govulncheckConfig = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck"}}`

// runGovulncheckScript runs scripts/govulncheck.sh with a stub govulncheck first on the PATH, and
// returns the script's exit code and combined output.
func runGovulncheckScript(t *testing.T, stub string) (int, string) {
	t.Helper()

	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on the PATH", tool)
		}
	}

	bin := t.TempDir()

	if err := os.WriteFile(filepath.Join(bin, "govulncheck"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %s", err)
	}

	cmd := exec.Command(filepath.Join("..", "..", "scripts", "govulncheck.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError

	switch {

	case err == nil:
		return 0, string(out)

	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)

	}

	t.Fatalf("govulncheck.sh did not run: %s\n%s", err, out)

	return 0, ""
}

// TestGovulncheckScriptFailsWhenTheDatabaseIsUnreachable: govulncheck writes its config record
// before fetching the vulnerability database, so a scan that never reached the database still
// produces one. The script used to take that record as proof of a report and discard the exit
// status, and a cloud run with vuln.go.dev blocked printed "no reachable vulnerabilities" for every
// module and exited 0 - a security gate passing on a scan that scanned nothing.
func TestGovulncheckScriptFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	stub := "#!/bin/sh\n" +
		"echo '" + govulncheckConfig + "'\n" +
		`echo 'govulncheck: fetching vulnerabilities: Get "https://vuln.go.dev/index/modules.json.gz": Forbidden' >&2` + "\n" +
		"exit 1\n"

	code, out := runGovulncheckScript(t, stub)

	if code != 2 {
		t.Errorf("exit %d, want 2 (could not scan):\n%s", code, out)
	}

	if strings.Contains(out, "no reachable vulnerabilities") {
		t.Errorf("reported a clean scan that never happened:\n%s", out)
	}
}

// TestGovulncheckScriptPassesACleanScan holds the other direction: a scan that completed and found
// nothing reachable still passes.
func TestGovulncheckScriptPassesACleanScan(t *testing.T) {
	stub := "#!/bin/sh\necho '" + govulncheckConfig + "'\nexit 0\n"

	code, out := runGovulncheckScript(t, stub)

	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, out)
	}
}
