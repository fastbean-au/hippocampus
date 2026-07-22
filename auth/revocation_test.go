package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// writeRevocationFile writes body to a fresh file in the test's temp dir and returns its path.
func writeRevocationFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "revocations.json")

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write revocation file: %s", err)
	}

	return path
}

// claimsWith builds a Claims value with the given jti, client and iat for revocation-matching
// tests.
func claimsWith(jti string, client string, iat time.Time) *Claims {
	c := &Claims{
		ClientID: client,
	}

	c.ID = jti

	if !iat.IsZero() {
		c.IssuedAt = jwt.NewNumericDate(iat)
	}

	return c
}

// TestRevocationList_RevokesByJTI verifies that a token whose jti is listed is revoked, while an
// unlisted jti is not.
func TestRevocationList_RevokesByJTI(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["abc123"]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if !list.IsRevoked(claimsWith("abc123", "client-1", time.Now())) {
		t.Error("expected a listed jti to be revoked")
	}

	if list.IsRevoked(claimsWith("other", "client-1", time.Now())) {
		t.Error("expected an unlisted jti not to be revoked")
	}
}

// TestRevocationList_RevokesByClient verifies that an unconditional client entry revokes every
// token for that client regardless of iat.
func TestRevocationList_RevokesByClient(t *testing.T) {
	path := writeRevocationFile(t, `{"clients":[{"clientId":"banned"}]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if !list.IsRevoked(claimsWith("j1", "banned", time.Now())) {
		t.Error("expected a banned client's token to be revoked")
	}

	if list.IsRevoked(claimsWith("j2", "allowed", time.Now())) {
		t.Error("expected a different client's token not to be revoked")
	}
}

// TestRevocationList_ClientIssuedBefore verifies the per-client rotation semantics: tokens issued
// before the cutoff are revoked, tokens issued at or after it are not.
func TestRevocationList_ClientIssuedBefore(t *testing.T) {
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	path := writeRevocationFile(t, `{"clients":[{"clientId":"rotated","issuedBefore":"2026-07-01T00:00:00Z"}]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if !list.IsRevoked(claimsWith("old", "rotated", cutoff.Add(-time.Second))) {
		t.Error("expected a token issued before the cutoff to be revoked")
	}

	if list.IsRevoked(claimsWith("new", "rotated", cutoff.Add(time.Second))) {
		t.Error("expected a token issued after the cutoff not to be revoked")
	}
}

// TestRevocationList_ClientIssuedBeforeFailsClosed verifies that a token matching a cutoff rule but
// carrying no iat is treated as revoked - only a token minted outside this service could lack one.
func TestRevocationList_ClientIssuedBeforeFailsClosed(t *testing.T) {
	path := writeRevocationFile(t, `{"clients":[{"clientId":"rotated","issuedBefore":"2026-07-01T00:00:00Z"}]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if !list.IsRevoked(claimsWith("no-iat", "rotated", time.Time{})) {
		t.Error("expected a token with no iat matching a cutoff rule to be revoked (fail closed)")
	}
}

// TestNewRevocationList_NonPositiveRefreshDefaults verifies that a zero or negative refresh
// interval doesn't fail construction: it falls back to the package default (30s) rather than
// never polling (0) or misbehaving on a negative ticker interval.
func TestNewRevocationList_NonPositiveRefreshDefaults(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["abc"]}`)

	list, err := NewRevocationList(path, 0)
	if err != nil {
		t.Fatalf("NewRevocationList with a zero refresh: %s", err)
	}

	list.Stop()

	list2, err := NewRevocationList(path, -time.Second)
	if err != nil {
		t.Fatalf("NewRevocationList with a negative refresh: %s", err)
	}

	list2.Stop()
}

// TestRevocationList_ReloadIfChangedMissingFile verifies that reloadIfChanged reports an error
// when the underlying file has disappeared, rather than panicking on the failed stat.
func TestRevocationList_ReloadIfChangedMissingFile(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["abc"]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove revocation file: %s", err)
	}

	if err := list.reloadIfChanged(); err == nil {
		t.Error("expected reloadIfChanged to error when the file has been removed")
	}

	if !list.IsRevoked(claimsWith("abc", "c", time.Now())) {
		t.Error("expected the last good revocation to remain enforced after the file disappears")
	}
}

// TestNewRevocationList_PathIsDirectory verifies reload's os.ReadFile failure branch: a path that
// stats successfully (it exists) but cannot be read as a file - a directory, here - fails
// construction with a read error rather than a stat error.
func TestNewRevocationList_PathIsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a-directory")

	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %s", err)
	}

	if _, err := NewRevocationList(path, time.Hour); err == nil {
		t.Error("expected construction to fail when the path is a directory")
	}
}

// TestRevocationList_PollLogsOnReloadError verifies that the background poller survives a reload
// failure (the file disappearing after a good initial load): it logs the error rather than
// crashing, and the last good revocation set stays enforced.
func TestRevocationList_PollLogsOnReloadError(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["staygone"]}`)

	list, err := NewRevocationList(path, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove revocation file: %s", err)
	}

	// Give the poller a few ticks to hit the missing file and log the failure.
	time.Sleep(100 * time.Millisecond)

	if !list.IsRevoked(claimsWith("staygone", "c", time.Now())) {
		t.Error("expected the last good revocation to remain enforced after the poller's reload fails")
	}
}

// TestRevocationList_SkipsEmptyJTI verifies that an empty string among the listed jtis is skipped
// rather than matching every claims value with an empty ID.
func TestRevocationList_SkipsEmptyJTI(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["", "real"]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if list.IsRevoked(claimsWith("", "c", time.Now())) {
		t.Error("expected an empty jti in the list not to revoke claims with an empty ID")
	}

	if !list.IsRevoked(claimsWith("real", "c", time.Now())) {
		t.Error("expected the non-empty jti to still be enforced")
	}
}

// TestRevocationList_SkipsEmptyClientID verifies that a client entry with an empty clientId is
// skipped rather than matching every claims value with an empty ClientID.
func TestRevocationList_SkipsEmptyClientID(t *testing.T) {
	path := writeRevocationFile(t, `{"clients":[{"clientId":""},{"clientId":"real"}]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if list.IsRevoked(claimsWith("j", "", time.Now())) {
		t.Error("expected an empty clientId entry not to revoke claims with an empty ClientID")
	}

	if !list.IsRevoked(claimsWith("j", "real", time.Now())) {
		t.Error("expected the non-empty clientId to still be enforced")
	}
}

// TestRevocationList_IsRevoked_NilSetAndNilClaims verifies IsRevoked's two safety fallbacks: a
// list whose set has never been loaded (the zero value) and a nil claims pointer both report not
// revoked rather than panicking.
func TestRevocationList_IsRevoked_NilSetAndNilClaims(t *testing.T) {
	var zero RevocationList

	if zero.IsRevoked(claimsWith("j", "c", time.Now())) {
		t.Error("expected a RevocationList with no loaded set to report not revoked")
	}

	path := writeRevocationFile(t, `{"jtis":["abc"]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if list.IsRevoked(nil) {
		t.Error("expected nil claims to report not revoked")
	}
}

// TestRevocationList_ReloadsOnChange verifies that a revocation added to the file after load is
// picked up by the poller without reconstructing the list.
func TestRevocationList_ReloadsOnChange(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":[]}`)

	list, err := NewRevocationList(path, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	if list.IsRevoked(claimsWith("late", "c", time.Now())) {
		t.Fatal("expected nothing revoked initially")
	}

	// Rewrite with a revoked jti and bump the mtime so the poller notices the change.
	if err := os.WriteFile(path, []byte(`{"jtis":["late"]}`), 0o600); err != nil {
		t.Fatalf("rewrite revocation file: %s", err)
	}

	future := time.Now().Add(time.Second)

	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %s", err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if list.IsRevoked(claimsWith("late", "c", time.Now())) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Error("expected the reloaded revocation to take effect within the deadline")
}

// TestNewRevocationList_BadFileFailsStartup verifies that a named-but-unparseable file fails
// construction, so a typo'd path can't silently revoke nothing.
func TestNewRevocationList_BadFileFailsStartup(t *testing.T) {
	path := writeRevocationFile(t, `{not json`)

	if _, err := NewRevocationList(path, time.Hour); err == nil {
		t.Error("expected construction to fail on an unparseable revocation file")
	}
}

// TestNewRevocationList_MissingFileFailsStartup verifies that a path that does not exist fails
// construction rather than starting up enforcing an empty list.
func TestNewRevocationList_MissingFileFailsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	if _, err := NewRevocationList(path, time.Hour); err == nil {
		t.Error("expected construction to fail on a missing revocation file")
	}
}

// TestRevocationList_BadReloadKeepsLastGood verifies that once a good list is loaded, a subsequent
// unparseable write does not clear the enforced revocations.
func TestRevocationList_BadReloadKeepsLastGood(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["staygone"]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	// Corrupt the file, then force a reload directly; it must error and leave the set intact.
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("corrupt revocation file: %s", err)
	}

	if err := list.reload(); err == nil {
		t.Error("expected reload to error on a corrupt file")
	}

	if !list.IsRevoked(claimsWith("staygone", "c", time.Now())) {
		t.Error("expected the last good revocation to remain enforced after a bad reload")
	}
}

// stubVerifier is a Verifier that returns fixed claims (or an error) for use in decorator tests.
type stubVerifier struct {
	claims *Claims
	err    error
}

func (s *stubVerifier) Verify(token string) (*Claims, error) {
	return s.claims, s.err
}

// TestNewRevokingVerifier_NilListPassthrough verifies that wrapping with a nil list returns the
// inner verifier unchanged, so callers can wrap unconditionally.
func TestNewRevokingVerifier_NilListPassthrough(t *testing.T) {
	inner := &stubVerifier{claims: claimsWith("j", "c", time.Now())}

	if got := NewRevokingVerifier(inner, nil); got != inner {
		t.Error("expected a nil revocation list to yield the inner verifier unchanged")
	}
}

// TestRevokingVerifier_RejectsRevoked verifies that a token which verifies but whose claims are
// revoked is rejected, while a non-revoked one passes through with its claims.
func TestRevokingVerifier_RejectsRevoked(t *testing.T) {
	path := writeRevocationFile(t, `{"jtis":["revoked-jti"]}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	revoked := NewRevokingVerifier(&stubVerifier{claims: claimsWith("revoked-jti", "c", time.Now())}, list)

	if _, err := revoked.Verify("token"); err == nil {
		t.Error("expected a revoked token to be rejected")
	}

	allowed := NewRevokingVerifier(&stubVerifier{claims: claimsWith("fine", "c", time.Now())}, list)

	if _, err := allowed.Verify("token"); err != nil {
		t.Errorf("expected a non-revoked token to pass through, got: %s", err)
	}
}

// TestRevokingVerifier_PropagatesInnerError verifies that an inner verification failure is returned
// unchanged and the revocation check is never consulted.
func TestRevokingVerifier_PropagatesInnerError(t *testing.T) {
	path := writeRevocationFile(t, `{}`)

	list, err := NewRevocationList(path, time.Hour)
	if err != nil {
		t.Fatalf("NewRevocationList: %s", err)
	}
	defer list.Stop()

	inner := &stubVerifier{err: errInvalidForTest}
	v := NewRevokingVerifier(inner, list)

	if _, err := v.Verify("token"); err != errInvalidForTest {
		t.Errorf("expected the inner error to propagate, got: %v", err)
	}
}

var errInvalidForTest = &stubError{"invalid"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }
