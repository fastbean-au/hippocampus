package ratelimit

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a hand-driven time source, so a refill can be asserted rather than slept through.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// newTestLimiter builds a limiter on a hand-driven clock, with its buckets started at that clock's
// time so the first request does not credit an accidental refill from the wall clock New used.
func newTestLimiter(t *testing.T, cfg Config) (*Limiter, *clock) {
	t.Helper()

	limiter, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to build the limiter: %s", err.Error())
	}

	c := newClock()
	limiter.now = c.Now

	if limiter.global != nil {
		limiter.global.last = c.Now()
	}

	for _, b := range limiter.tiers {
		b.last = c.Now()
	}

	return limiter, c
}

func TestGlobalCeilingAdmitsABurstThenRefills(t *testing.T) {
	limiter, c := newTestLimiter(t, Config{Global: Rule{RequestsPerSecond: 2, Burst: 3}})

	for i := range 3 {
		if decision := limiter.Arrival(); !decision.Allowed {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}

	decision := limiter.Arrival()

	if decision.Allowed {
		t.Fatal("the fourth request was admitted with an exhausted burst of three")
	}

	if decision.Scope != ScopeGlobal {
		t.Errorf("refusal reports scope '%s', expected '%s'", decision.Scope, ScopeGlobal)
	}

	// At two per second the next token is half a second away.
	if decision.RetryAfter <= 0 || decision.RetryAfter > time.Second {
		t.Errorf("retry-after is %s, expected a wait under a second at 2/s", decision.RetryAfter)
	}

	c.advance(time.Second)

	for i := range 2 {
		if decision := limiter.Arrival(); !decision.Allowed {
			t.Fatalf("request %d after a second's refill was refused", i+1)
		}
	}

	if limiter.Arrival().Allowed {
		t.Error("a third request was admitted after only two tokens had refilled")
	}
}

// The burst is capped at the configured allowance however long the bucket has been idle, or a
// caller could bank a day of quiet and spend it in one second.
func TestGlobalCeilingDoesNotBankIdleTime(t *testing.T) {
	limiter, c := newTestLimiter(t, Config{Global: Rule{RequestsPerSecond: 1, Burst: 2}})

	c.advance(time.Hour)

	for i := range 2 {
		if decision := limiter.Arrival(); !decision.Allowed {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}

	if limiter.Arrival().Allowed {
		t.Error("an hour of idleness banked more than the configured burst")
	}
}

func TestPerClientBucketsAreIndependent(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{PerClient: Rule{RequestsPerSecond: 1, Burst: 1}})

	if decision := limiter.Principal("client:a", ""); !decision.Allowed {
		t.Fatal("client a's first request was refused")
	}

	decision := limiter.Principal("client:a", "")

	if decision.Allowed {
		t.Fatal("client a's second request was admitted on a burst of one")
	}

	if decision.Scope != ScopeClient {
		t.Errorf("refusal reports scope '%s', expected '%s'", decision.Scope, ScopeClient)
	}

	// The whole point of the level: one caller exhausting its allowance must not affect another.
	if decision := limiter.Principal("client:b", ""); !decision.Allowed {
		t.Error("client b was refused because client a had exhausted its own bucket")
	}
}

// An empty key means the caller could not be identified at all; there is no bucket to charge, so
// the per-client level abstains rather than putting every such request in one shared bucket.
func TestAnUnidentifiedCallerSkipsThePerClientLevel(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{PerClient: Rule{RequestsPerSecond: 1, Burst: 1}})

	for i := range 5 {
		if decision := limiter.Principal("", ""); !decision.Allowed {
			t.Fatalf("request %d from an unidentified caller was refused", i+1)
		}
	}

	if limiter.Clients() != 0 {
		t.Errorf("an unidentified caller was tracked: %d clients", limiter.Clients())
	}
}

func TestTierLimitAppliesAcrossClientsOfThatTier(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{
		Tiers: map[string]Rule{"reader": {RequestsPerSecond: 1, Burst: 2}},
	})

	if decision := limiter.Principal("client:a", "reader"); !decision.Allowed {
		t.Fatal("the first reader request was refused")
	}

	if decision := limiter.Principal("client:b", "reader"); !decision.Allowed {
		t.Fatal("the second reader request, from another client, was refused")
	}

	decision := limiter.Principal("client:c", "reader")

	if decision.Allowed {
		t.Fatal("a third reader request was admitted on a tier burst of two")
	}

	if decision.Scope != ScopeTier {
		t.Errorf("refusal reports scope '%s', expected '%s'", decision.Scope, ScopeTier)
	}

	// A tier with no rule is unlimited, which is how admin is left free to answer an incident.
	for i := range 5 {
		if decision := limiter.Principal("client:d", "admin"); !decision.Allowed {
			t.Fatalf("admin request %d was refused by a tier that has no rule", i+1)
		}
	}
}

// The refund is what stops a busy tier quietly consuming an individual caller's allowance: the
// request the tier refused was never served, so the client must not be charged for it.
func TestATierRefusalRefundsTheClientToken(t *testing.T) {
	limiter, c := newTestLimiter(t, Config{
		PerClient: Rule{RequestsPerSecond: 1, Burst: 1},
		Tiers:     map[string]Rule{"writer": {RequestsPerSecond: 1, Burst: 1}},
	})

	// Drain the tier bucket with one client, leaving that client's own bucket empty too.
	if decision := limiter.Principal("client:a", "writer"); !decision.Allowed {
		t.Fatal("the first request was refused")
	}

	// A second client is refused by the tier, not by its own bucket.
	if decision := limiter.Principal("client:b", "writer"); decision.Scope != ScopeTier {
		t.Fatalf("the second client was refused with scope '%s', expected '%s'", decision.Scope, ScopeTier)
	}

	// ...and so still holds its own token: once the tier refills, it is served immediately rather
	// than having to wait out a burst it never spent.
	c.advance(time.Second)

	if decision := limiter.Principal("client:b", "writer"); !decision.Allowed {
		t.Error("the second client's token was spent on a request the tier refused")
	}
}

func TestClientTableExpiresIdlePrincipals(t *testing.T) {
	limiter, c := newTestLimiter(t, Config{
		PerClient:  Rule{RequestsPerSecond: 1, Burst: 1},
		ClientIdle: time.Minute,
	})

	limiter.Principal("client:a", "")
	limiter.Principal("client:b", "")

	if limiter.Clients() != 2 {
		t.Fatalf("tracking %d clients, expected 2", limiter.Clients())
	}

	c.advance(2 * time.Minute)

	// The access itself expires the cold end, so the table is bounded with no sweeper goroutine.
	limiter.Principal("client:c", "")

	if limiter.Clients() != 1 {
		t.Errorf("tracking %d clients after the idle window, expected only the active one", limiter.Clients())
	}
}

func TestClientTableIsBoundedAndKeepsTheActiveCaller(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{
		PerClient:  Rule{RequestsPerSecond: 100, Burst: 100},
		MaxClients: 2,
	})

	limiter.Principal("client:a", "")
	limiter.Principal("client:b", "")
	limiter.Principal("client:c", "")

	if limiter.Clients() != 2 {
		t.Fatalf("tracking %d clients against a cap of 2", limiter.Clients())
	}

	// The caller just seen is the one an attack keeps hot, so it must be the one that survives; the
	// entry evicted is the coldest.
	if _, tracked := limiter.clients.entries["client:c"]; !tracked {
		t.Error("the most recently seen caller was evicted to honour the cap")
	}

	if _, tracked := limiter.clients.entries["client:a"]; tracked {
		t.Error("the coldest caller was kept over a newer one")
	}
}

func TestLevelsCompose(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{
		Global:    Rule{RequestsPerSecond: 10, Burst: 10},
		PerClient: Rule{RequestsPerSecond: 1, Burst: 1},
	})

	if decision := limiter.Arrival(); !decision.Allowed {
		t.Fatal("the global ceiling refused the first request")
	}

	if decision := limiter.Principal("client:a", ""); !decision.Allowed {
		t.Fatal("the per-client level refused the first request")
	}

	// The global ceiling still has headroom; the client's own does not.
	if decision := limiter.Arrival(); !decision.Allowed {
		t.Error("the global ceiling refused a second request with nine tokens left")
	}

	if decision := limiter.Principal("client:a", ""); decision.Scope != ScopeClient {
		t.Errorf("the second request was refused with scope '%s', expected '%s'", decision.Scope, ScopeClient)
	}
}

func TestAnUnconfiguredLevelImposesNoLimit(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{PerClient: Rule{RequestsPerSecond: 1, Burst: 1}})

	if limiter.ArrivalActive() {
		t.Error("the arrival level reports itself active with no global rule")
	}

	for i := range 100 {
		if decision := limiter.Arrival(); !decision.Allowed {
			t.Fatalf("arrival request %d was refused with no global rule configured", i+1)
		}
	}
}

// A nil limiter is the disabled state, and every call site relies on it behaving as no limit at
// all rather than having to check first.
func TestANilLimiterAllowsEverything(t *testing.T) {
	var limiter *Limiter

	if limiter.Active() || limiter.ArrivalActive() || limiter.PrincipalActive() {
		t.Error("a nil limiter reports itself active")
	}

	if !limiter.Arrival().Allowed {
		t.Error("a nil limiter refused an arrival")
	}

	if !limiter.Principal("client:a", "reader").Allowed {
		t.Error("a nil limiter refused a principal")
	}

	if limiter.Clients() != 0 {
		t.Error("a nil limiter reports tracked clients")
	}
}

func TestBurstDefaultsToOneSecondOfRate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rule     Rule
		expected float64
	}{
		{"explicit burst wins", Rule{RequestsPerSecond: 10, Burst: 3}, 3},
		{"absent burst is one second's worth", Rule{RequestsPerSecond: 10}, 10},
		{"a sub-unit rate still admits one request", Rule{RequestsPerSecond: 0.1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if burst := tc.rule.burst(); burst != tc.expected {
				t.Errorf("burst resolved to %v, expected %v", burst, tc.expected)
			}
		})
	}
}

func TestNewRejectsIncoherentRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   Config
		contains string
	}{
		{
			name:     "negative global rate",
			config:   Config{Global: Rule{RequestsPerSecond: -1}},
			contains: "rateLimit.global.requestsPerSecond",
		},
		{
			name:     "negative per-client burst",
			config:   Config{PerClient: Rule{RequestsPerSecond: 1, Burst: -5}},
			contains: "rateLimit.perClient.burst",
		},
		{
			// A burst with no rate is a bucket that never refills: it would admit the burst once and
			// refuse for ever. Almost certainly a half-written rule, so say so at startup.
			name:     "a burst with no rate",
			config:   Config{Tiers: map[string]Rule{"reader": {Burst: 10}}},
			contains: "rateLimit.tiers.reader.burst",
		},
		{
			name:     "negative client cap",
			config:   Config{MaxClients: -1},
			contains: "rateLimit.maxClients",
		},
		{
			name:     "negative idle window",
			config:   Config{ClientIdle: -time.Second},
			contains: "rateLimit.clientIdleSeconds",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.config)
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error '%s' does not name %s", err.Error(), tc.contains)
			}
		})
	}
}

func TestTierNamesAreMatchedCaseInsensitively(t *testing.T) {
	limiter, _ := newTestLimiter(t, Config{
		Tiers: map[string]Rule{" Reader ": {RequestsPerSecond: 1, Burst: 1}},
	})

	if decision := limiter.Principal("client:a", "reader"); !decision.Allowed {
		t.Fatal("the first request was refused")
	}

	if decision := limiter.Principal("client:b", "reader"); decision.Scope != ScopeTier {
		t.Errorf("a configured ' Reader ' rule did not apply to the resolved 'reader' tier (scope '%s')", decision.Scope)
	}
}

func TestConcurrentCallersSeeOneConsistentBudget(t *testing.T) {
	const burst = 50

	limiter, _ := newTestLimiter(t, Config{Global: Rule{RequestsPerSecond: 0.0001, Burst: burst}})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)

	for range 200 {
		wg.Go(func() {
			if limiter.Arrival().Allowed {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	// The rate is low enough that no meaningful refill happens during the test, so the budget is
	// exactly the burst - no more (a race would over-admit) and no fewer.
	if granted != burst {
		t.Errorf("%d of 200 concurrent requests were admitted, expected exactly the burst of %d", granted, burst)
	}
}

// The clock can step backwards. Crediting a negative elapsed time would drain the bucket, turning a
// time adjustment into an outage.
func TestABackwardClockDoesNotDrainABucket(t *testing.T) {
	limiter, c := newTestLimiter(t, Config{Global: Rule{RequestsPerSecond: 1, Burst: 2}})

	c.advance(-time.Hour)

	for i := range 2 {
		if decision := limiter.Arrival(); !decision.Allowed {
			t.Fatalf("request %d was refused after the clock stepped backwards", i+1)
		}
	}
}
