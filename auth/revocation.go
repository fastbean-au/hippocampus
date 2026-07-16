package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// revocationFile is the on-disk JSON shape of a revocation list. jtis revokes individual tokens by
// their jti; clients revokes every token for a client_id, optionally only those issued before a
// cutoff (the per-client rotation move: revoke everything issued before now, then mint fresh).
type revocationFile struct {
	Jtis    []string               `json:"jtis"`
	Clients []revocationFileClient `json:"clients"`
}

type revocationFileClient struct {
	ClientID string `json:"clientId"`

	// IssuedBefore, when set, revokes only tokens whose iat is before it. An RFC 3339 timestamp.
	IssuedBefore *time.Time `json:"issuedBefore"`
}

// revocationSet is the parsed, lookup-ready form swapped in atomically on each reload. A jti in
// jtis is revoked outright; a client_id in clients is revoked either unconditionally (zero cutoff)
// or only for tokens issued before the cutoff.
type revocationSet struct {
	jtis    map[string]struct{}
	clients map[string]time.Time
}

// RevocationList holds the current revocation set and reloads it from disk when the file's mtime
// changes. It is safe for concurrent use: Verify reads the set on the request hot path via an
// atomic load while a background goroutine swaps in reloads.
type RevocationList struct {
	path string

	current atomic.Pointer[revocationSet]

	mu       sync.Mutex
	lastMod  time.Time
	stopOnce sync.Once
	stop     chan struct{}
}

// NewRevocationList loads path once (failing if it cannot be read or parsed - a named-but-broken
// revocation file must not silently revoke nothing) and starts a goroutine that reloads it whenever
// its mtime changes, polling every refresh. A later reload failure is logged and the last good set
// kept, so a transient bad write never stops enforcing the previously loaded revocations. Call Stop
// to end the poller.
func NewRevocationList(path string, refresh time.Duration) (*RevocationList, error) {
	log.Trace("func() auth.NewRevocationList")

	r := &RevocationList{
		path: path,
		stop: make(chan struct{}),
	}

	if err := r.reload(); err != nil {
		return nil, err
	}

	if refresh <= 0 {
		refresh = 30 * time.Second
	}

	go r.poll(refresh)

	return r, nil
}

// Stop ends the background reload goroutine. It is idempotent.
func (r *RevocationList) Stop() {
	r.stopOnce.Do(func() {
		close(r.stop)
	})
}

// poll reloads the file whenever its mtime advances, at the given interval, until Stop is called.
func (r *RevocationList) poll(refresh time.Duration) {
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()

	for {
		select {

		case <-r.stop:
			return

		case <-ticker.C:
			if err := r.reloadIfChanged(); err != nil {
				log.Errorf("auth: revocation reload failed, keeping last good list: %s", err.Error())
			}

		}
	}
}

// reloadIfChanged reloads only when the file's mtime differs from the last load, so an unchanged
// file costs a single stat per tick rather than a full read and parse.
func (r *RevocationList) reloadIfChanged() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("auth: stat revocation file: %w", err)
	}

	r.mu.Lock()
	unchanged := info.ModTime().Equal(r.lastMod)
	r.mu.Unlock()

	if unchanged {
		return nil
	}

	return r.reload()
}

// reload reads and parses the file and atomically swaps in the new set, recording the file's mtime
// so reloadIfChanged can skip unchanged files.
func (r *RevocationList) reload() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("auth: stat revocation file: %w", err)
	}

	raw, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("auth: read revocation file: %w", err)
	}

	var parsed revocationFile

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("auth: parse revocation file: %w", err)
	}

	set := &revocationSet{
		jtis:    make(map[string]struct{}, len(parsed.Jtis)),
		clients: make(map[string]time.Time, len(parsed.Clients)),
	}

	for _, jti := range parsed.Jtis {
		if jti == "" {
			continue
		}

		set.jtis[jti] = struct{}{}
	}

	for _, c := range parsed.Clients {
		if c.ClientID == "" {
			continue
		}

		cutoff := time.Time{}

		if c.IssuedBefore != nil {
			cutoff = *c.IssuedBefore
		}

		set.clients[c.ClientID] = cutoff
	}

	r.current.Store(set)

	r.mu.Lock()
	r.lastMod = info.ModTime()
	r.mu.Unlock()

	log.Infof("auth: revocation list loaded (%d jtis, %d clients)", len(set.jtis), len(set.clients))

	return nil
}

// IsRevoked reports whether the given claims match any revocation rule. A jti match revokes
// outright. A client match revokes unconditionally when the rule has no cutoff, or when the token's
// iat is before the cutoff; a matching token that carries no iat is treated as revoked (fail
// closed), since only a token minted outside this service could lack one.
func (r *RevocationList) IsRevoked(c *Claims) bool {
	set := r.current.Load()

	if set == nil || c == nil {
		return false
	}

	if c.ID != "" {
		if _, ok := set.jtis[c.ID]; ok {
			return true
		}
	}

	cutoff, ok := set.clients[c.ClientID]

	if !ok || c.ClientID == "" {
		return false
	}

	if cutoff.IsZero() {
		return true
	}

	if c.IssuedAt == nil {
		return true
	}

	return c.IssuedAt.Before(cutoff)
}

// revokingVerifier decorates an inner Verifier with a revocation check, so revocation composes with
// any verification scheme (hmac or idp): the signature is verified first, then the resulting claims
// are checked against the list.
type revokingVerifier struct {
	inner Verifier
	list  *RevocationList
}

// NewRevokingVerifier wraps inner so that a token which verifies but whose claims are revoked is
// rejected. It returns inner unchanged when list is nil, so callers can wrap unconditionally.
func NewRevokingVerifier(inner Verifier, list *RevocationList) Verifier {
	if list == nil {
		return inner
	}

	return &revokingVerifier{inner: inner, list: list}
}

// Verify runs the inner verification, then rejects revoked claims. A revoked token is rejected with
// the same generic error as an invalid one, so a caller cannot tell a revoked token from a
// malformed one; the specific reason is logged at Debug for operators.
func (v *revokingVerifier) Verify(token string) (*Claims, error) {
	claims, err := v.inner.Verify(token)
	if err != nil {
		return nil, err
	}

	if v.list.IsRevoked(claims) {
		log.Debugf("token rejected: revoked (jti %q, client %q)", claims.ID, claims.ClientID)

		return nil, fmt.Errorf("auth: token invalid")
	}

	return claims, nil
}
