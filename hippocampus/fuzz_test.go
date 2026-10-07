package hippocampus

import (
	"strings"
	"testing"
)

// FuzzRedactEndpoint (TODO-3 item 167): the topology view is reader-visible by default, and what
// makes that safe is that no address shape lets a credential through. The fuzzer chooses the parts
// and the shape; the password is then looked for in the output.
func FuzzRedactEndpoint(f *testing.F) {
	f.Add(uint8(0), "user", "s3cretPW", "db.internal:5432", "hippo")
	f.Add(uint8(1), "user", "s3cretPW", "db.internal", "hippo")
	f.Add(uint8(2), "user", "s3cretPW", "db.internal:3306", "hippo")
	f.Add(uint8(3), "user", "s3cretPW", "db.internal:3306", "hippo")
	f.Add(uint8(0), "u", "p@ss/w:rd?x", "h", "d")
	f.Add(uint8(2), "u", "pa/ss?wo@rd", "h:3306", "d")

	f.Fuzz(func(t *testing.T, shape uint8, user string, password string, host string, database string) {
		// A password short enough, or shared with the parts that are meant to be shown, cannot be
		// told apart from them in the output.
		shown := host + "/" + database + ":" + host

		if len(password) < 6 || strings.Contains(shown, password) || strings.Contains(user, password) {
			return
		}

		build := func(secret string) string {
			switch shape % 4 {

			case 0:
				return "postgres://" + user + ":" + secret + "@" + host + "/" + database + "?sslmode=require"

			case 1:
				return "host=" + host + " port=5432 dbname=" + database + " user=" + user + " password=" + secret

			case 2:
				return user + ":" + secret + "@tcp(" + host + ")/" + database + "?parseTime=true"

			default:
				return "mysql://" + host + "/" + database + "?password=" + secret

			}
		}

		raw := build(password)
		got := redactEndpoint(raw)

		if !strings.Contains(got, password) {
			return
		}

		// The output can contain the password's characters by coincidence - percent-encoding a host
		// byte, say. It leaked only if putting something else in the password's place removes it.
		// The marker is a letter the password lacks, so the control parses exactly as the original
		// does: a control character would send a URL down the other branch.
		marker := "Z"

		for _, candidate := range []string{"Z", "Q", "K", "J", "X"} {
			if !strings.Contains(password, candidate) {
				marker = candidate

				break
			}
		}

		if control := redactEndpoint(build(strings.Repeat(marker, len(password)))); strings.Contains(control, password) {
			return
		}

		t.Fatalf("redactEndpoint(%q) = %q, which still carries the password %q", raw, got, password)
	})
}

// FuzzSanitiseReportedVersion (TODO-3 item 167): a reported version arrives from a caller or a
// declared component and is displayed, so what is kept is printable ASCII, bounded, and exactly what
// was sent less surrounding space - never an edited value.
func FuzzSanitiseReportedVersion(f *testing.F) {
	for _, seed := range []string{"hippo/0.51.1", " v1 ", "a\x1bb", "é", strings.Repeat("x", 65), ""} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		got := sanitiseReportedVersion(raw)
		if got == "" {
			return
		}

		if got != strings.TrimSpace(raw) {
			t.Fatalf("kept %q from %q, which is an edited value", got, raw)
		}

		if len(got) > maxReportedVersionBytes {
			t.Fatalf("kept %d bytes, over the bound", len(got))
		}

		for i := 0; i < len(got); i++ {
			if got[i] < 0x20 || got[i] > 0x7e {
				t.Fatalf("kept %q, carrying byte %#x", got, got[i])
			}
		}
	})
}

// TestRedactEndpointKeywordDSNWithAnAt is what FuzzRedactEndpoint found: a libpq keyword DSN with an
// "@" anywhere in it - a password containing one is the realistic case - was not recognised as a
// keyword DSN and fell through to the bare-address form, which cut only up to the "@" and kept the
// rest, the tail of the password included.
func TestRedactEndpointKeywordDSNWithAnAt(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"host=db.internal port=5432 dbname=hippo user=svc password=p@ssw0rd-tail",
		"host=db.internal password=p@ssw0rd-tail sslmode=require",
		"user=me@corp host=db.internal password=hunter2-tail",
	} {
		got := redactEndpoint(raw)

		if strings.Contains(got, "tail") || strings.Contains(got, "password") {
			t.Errorf("redactEndpoint(%q) = %q, which carries part of the password", raw, got)
		}

		if !strings.HasPrefix(got, "db.internal") {
			t.Errorf("redactEndpoint(%q) = %q, want the host shown", raw, got)
		}
	}
}

// TestRedactEndpointMySQLPasswordWithSeparators: the bare-address form split on the first "/" before
// looking for the userinfo, so a password containing one kept the whole "user:pass" in the output.
func TestRedactEndpointMySQLPasswordWithSeparators(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"svc:pa/ss-tail@tcp(db.internal:3306)/hippo",
		"svc:pa?ss-tail@tcp(db.internal:3306)/hippo?parseTime=true",
	} {
		if got := redactEndpoint(raw); strings.Contains(got, "tail") || strings.Contains(got, "svc") {
			t.Errorf("redactEndpoint(%q) = %q, which carries the credentials", raw, got)
		}
	}
}

// TestRedactEndpointURLPasswordWithSeparators: an unencoded "/" or "?" in a URL's password parses into
// the path or the query, with the user name or the password's head left where the host should be.
func TestRedactEndpointURLPasswordWithSeparators(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"postgres://svc:pa/ss-tail@db.internal:5432/hippo",
		"postgres://svc:1234?tail@db.internal:5432/hippo",
	} {
		got := redactEndpoint(raw)

		if strings.Contains(got, "tail") || strings.Contains(got, "svc") || strings.Contains(got, "1234") {
			t.Errorf("redactEndpoint(%q) = %q, which carries the credentials", raw, got)
		}
	}
}
