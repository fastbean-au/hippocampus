// Package ratelimit implements the service's request throttling: a hierarchy of token buckets that
// admits a request only when every level that applies to it has a token to spend.
//
// The hierarchy has three levels, each independently optional, and a request must pass all of the
// ones that are configured:
//
//   - global - one bucket for the whole instance. It is the denial-of-service ceiling: it bounds
//     what the process will attempt at all, whoever is asking, and is the only level that can be
//     enforced before a caller has been identified.
//   - tier - one bucket per authorisation tier (reader/writer/admin). It bounds a class of caller
//     rather than an individual, which is how "readers may poll, writers may not flood" is
//     expressed. A tier left unset is unlimited, which is why the shipped configuration leaves
//     admin unset: an operator answering an incident should not be queued behind a limit meant for
//     application traffic.
//   - client - one bucket per principal. It is the fairness level: it stops one caller consuming
//     the global allowance and starving everyone else.
//
// The package is transport-agnostic on purpose. It knows nothing of gRPC, HTTP, or of how a
// principal is identified - the caller passes an already-resolved key and tier name - so the two
// enforcement adapters in cmd/hippocampus share one set of buckets and one decision, and neither
// transport can drift into enforcing a different policy.
package ratelimit

import (
	"fmt"
	"strings"
	"time"
)

// Default bounds for the per-client table. They are generous enough that a real deployment's
// callers all fit, and small enough that the table cannot become the memory-exhaustion surface the
// rest of the package exists to prevent.
const (
	defaultMaxClients = 10000
	defaultClientIdle = 5 * time.Minute
)

// Scope names the level of the hierarchy that denied a request. It is reported back to the caller
// so a rejection can be logged and counted by which limit was hit, without the metric ever carrying
// the principal itself (which would be unbounded cardinality).
type Scope string

const (
	// ScopeNone is the zero value, carried by an allowed decision.
	ScopeNone Scope = ""
	// ScopeGlobal is the instance-wide ceiling.
	ScopeGlobal Scope = "global"
	// ScopeTier is the per-authorisation-tier bucket.
	ScopeTier Scope = "tier"
	// ScopeClient is the per-principal bucket.
	ScopeClient Scope = "client"
)

// Rule is one bucket's shape: a sustained rate, and how much of it may be spent at once. A
// non-positive RequestsPerSecond means the level is not configured, and so imposes no limit at all.
type Rule struct {
	RequestsPerSecond float64
	Burst             int
}

// configured reports whether the rule imposes a limit.
func (r Rule) configured() bool {
	return r.RequestsPerSecond > 0
}

// burst resolves the burst allowance, defaulting to one second's worth of the rate (at least one
// request) when it is left unset. A default of zero would be a bucket that can never hold a token,
// i.e. a limit that rejects everything - not a sensible reading of "I set a rate and not a burst".
func (r Rule) burst() float64 {
	if r.Burst > 0 {
		return float64(r.Burst)
	}

	if r.RequestsPerSecond < 1 {
		return 1
	}

	return r.RequestsPerSecond
}

// Config is the whole rate-limiting configuration, as main.go assembles it from viper.
type Config struct {
	// Global is the instance-wide ceiling, enforced at the edge before a caller is identified.
	Global Rule
	// PerClient is the per-principal rule; every principal gets its own bucket of this shape.
	PerClient Rule
	// Tiers maps an authorisation tier name (reader/writer/admin) to its rule. A tier absent here
	// is unlimited.
	Tiers map[string]Rule
	// MaxClients bounds how many principals are tracked at once (0 selects the default).
	MaxClients int
	// ClientIdle is how long a principal's bucket is kept after its last request (0 selects the
	// default).
	ClientIdle time.Duration
}

// Decision is the answer to "may this request proceed": whether it may, which level said no, and
// how long the caller should wait before trying again.
type Decision struct {
	Allowed    bool
	Scope      Scope
	RetryAfter time.Duration
}

// allowed is the answer for a request no configured level objected to.
var allowed = Decision{Allowed: true}

// Limiter holds the buckets. It is safe for concurrent use and immutable in shape after
// construction, so one instance serves both transports.
type Limiter struct {
	global  *bucket
	clients *clientTable
	tiers   map[string]*bucket

	// now is the clock, replaced in tests so a refill can be observed without sleeping through it.
	now func() time.Time
}

// New validates cfg and builds the buckets it describes. It returns an error rather than silently
// dropping a level, because a rate limit that is not applied is indistinguishable from one that is
// never reached until the day it was needed.
func New(cfg Config) (*Limiter, error) {
	if err := validateRule("rateLimit.global", cfg.Global); err != nil {
		return nil, err
	}

	if err := validateRule("rateLimit.perClient", cfg.PerClient); err != nil {
		return nil, err
	}

	for name, rule := range cfg.Tiers {
		if err := validateRule("rateLimit.tiers."+name, rule); err != nil {
			return nil, err
		}
	}

	if cfg.MaxClients < 0 {
		return nil, fmt.Errorf("rateLimit.maxClients must not be negative, got %d", cfg.MaxClients)
	}

	if cfg.ClientIdle < 0 {
		return nil, fmt.Errorf("rateLimit.clientIdleSeconds must not be negative, got %v", cfg.ClientIdle)
	}

	now := time.Now()

	limiter := &Limiter{
		tiers: make(map[string]*bucket, len(cfg.Tiers)),
		now:   time.Now,
	}

	if cfg.Global.configured() {
		limiter.global = newBucket(cfg.Global.RequestsPerSecond, cfg.Global.burst(), now)
	}

	if cfg.PerClient.configured() {
		maxClients := cfg.MaxClients

		if maxClients == 0 {
			maxClients = defaultMaxClients
		}

		idle := cfg.ClientIdle

		if idle == 0 {
			idle = defaultClientIdle
		}

		limiter.clients = newClientTable(cfg.PerClient.RequestsPerSecond, cfg.PerClient.burst(), maxClients, idle)
	}

	for name, rule := range cfg.Tiers {
		if !rule.configured() {
			continue
		}

		limiter.tiers[normaliseTier(name)] = newBucket(rule.RequestsPerSecond, rule.burst(), now)
	}

	return limiter, nil
}

func validateRule(key string, rule Rule) error {
	if rule.RequestsPerSecond < 0 {
		return fmt.Errorf("%s.requestsPerSecond must not be negative, got %v", key, rule.RequestsPerSecond)
	}

	if rule.Burst < 0 {
		return fmt.Errorf("%s.burst must not be negative, got %d", key, rule.Burst)
	}

	if rule.Burst > 0 && !rule.configured() {
		return fmt.Errorf("%s.burst is set to %d without a requestsPerSecond, which would impose no limit", key, rule.Burst)
	}

	return nil
}

// normaliseTier reduces a configured tier name to the form the authorisation tiers render as, so
// "Reader" in a config file matches the "reader" a resolved tier reports.
func normaliseTier(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ArrivalActive reports whether anything is enforced before a caller is identified, so main can
// leave the edge adapters out of the chain entirely when nothing would be.
func (l *Limiter) ArrivalActive() bool {
	return l != nil && l.global != nil
}

// PrincipalActive reports whether anything is enforced once a caller is identified.
func (l *Limiter) PrincipalActive() bool {
	return l != nil && (l.clients != nil || len(l.tiers) > 0)
}

// Active reports whether the limiter enforces anything at all. A limiter that is enabled and
// enforces nothing is a configuration mistake worth a warning, not an error: it is the shape of a
// config being assembled.
func (l *Limiter) Active() bool {
	return l.ArrivalActive() || l.PrincipalActive()
}

// Arrival applies the levels that can be decided without knowing who is calling - the global
// ceiling. It is the edge check, enforced ahead of authentication so a flood of unauthenticated
// requests is bounded before the token verification it would otherwise pay for.
//
// A token it spends is not refunded when Principal then refuses the same request, unlike the refund
// between the two levels inside Principal. That is deliberate: the global bucket measures arrivals,
// which is the only thing a limit standing in front of identification can measure, and a request
// that was refused later still arrived. The consequence is worth knowing when sizing: a caller
// being throttled by its own bucket still consumes the ceiling, so a flood from one client can
// exhaust the instance's allowance even while every one of its requests is being refused. Size the
// ceiling above real peak and let the narrower levels do the shaping.
func (l *Limiter) Arrival() Decision {
	if !l.ArrivalActive() {
		return allowed
	}

	if ok, wait := l.global.take(l.now()); !ok {
		return Decision{Scope: ScopeGlobal, RetryAfter: wait}
	}

	return allowed
}

// Principal applies the levels that need the caller's identity: the per-client bucket, then the
// per-tier one. client is an opaque key (the caller decides what identifies a principal) and tier
// is the resolved authorisation tier's name, empty when the request was not authorised.
//
// The two are evaluated narrowest first, so the scope reported is always the level that actually
// refused. A token taken from the client bucket is refunded when the tier bucket then refuses,
// because the request was not served and charging for it would let a busy tier quietly consume an
// individual caller's allowance.
func (l *Limiter) Principal(client string, tier string) Decision {
	if !l.PrincipalActive() {
		return allowed
	}

	now := l.now()

	var spent *bucket

	if l.clients != nil && client != "" {
		spent = l.clients.bucketFor(client, now)

		if ok, wait := spent.take(now); !ok {
			return Decision{Scope: ScopeClient, RetryAfter: wait}
		}
	}

	if tierBucket := l.tiers[normaliseTier(tier)]; tierBucket != nil {
		if ok, wait := tierBucket.take(now); !ok {
			if spent != nil {
				spent.refund(now)
			}

			return Decision{Scope: ScopeTier, RetryAfter: wait}
		}
	}

	return allowed
}

// Clients reports how many principals currently hold a bucket, for the gauge of the same name.
func (l *Limiter) Clients() int {
	if l == nil || l.clients == nil {
		return 0
	}

	return l.clients.size()
}
