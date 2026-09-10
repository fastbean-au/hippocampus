package main

import (
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/hippocampus"
)

// validateCallbackConfig's own rules, none of which any other test in this package reaches - the
// whole function short-circuits on callbacks.enabled, and nothing else here enables it.
//
// Every rule below exists because its failure is silent at runtime. A missing URL records a row per
// forgotten memory into a queue that can never drain; an unbounded queue grows on disk while being
// excluded from the capacity target; a negative bound reads like a setting and behaves like "off".
// None of them stops the service serving, which is precisely why they are refused at startup.

// callbackConfig sets the keys a valid callback configuration needs, and clears them afterwards.
func callbackConfig(t *testing.T) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	setStartupDefaults()
	viper.Set("storage.directory", t.TempDir())
	viper.Set("callbacks.enabled", true)
	viper.Set("callbacks.url", "https://hooks.internal/forgotten")
}

// TestValidateCallbackConfig_Disabled is the short-circuit: none of the rules below applies to a
// deployment that is not sending callbacks, so a stray key must not fail its startup.
func TestValidateCallbackConfig_Disabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("callbacks.url", "")
	viper.Set("callbacks.maxRows", -1)

	if err := validateCallbackConfig(); err != nil {
		t.Errorf("a disabled sink must not be validated: %s", err)
	}
}

// TestValidateCallbackConfig_URL covers the two rules on the destination. The scheme check is the
// one worth having: an address with no scheme is what an operator naturally writes, and without it
// the sink would be accepted at startup and fail every delivery afterwards.
func TestValidateCallbackConfig_URL(t *testing.T) {
	callbackConfig(t)

	if err := validateCallbackConfig(); err != nil {
		t.Fatalf("expected a valid callback configuration, got %s", err)
	}

	viper.Set("callbacks.url", "   ")

	if err := validateCallbackConfig(); err == nil || !strings.Contains(err.Error(), "callbacks.url is required") {
		t.Errorf("expected a missing URL to be refused, got %v", err)
	}

	viper.Set("callbacks.url", "hooks.internal/forgotten")

	if err := validateCallbackConfig(); err == nil || !strings.Contains(err.Error(), "http://") {
		t.Errorf("expected a schemeless URL to be refused, got %v", err)
	}
}

// TestValidateCallbackConfig_NegativeBounds walks every bound. Zero means "no bound" throughout, so
// only a negative value is a mistake - and it is the sort that reads like a setting.
func TestValidateCallbackConfig_NegativeBounds(t *testing.T) {
	keys := []string{
		"callbacks.timeoutSeconds",
		"callbacks.maxBodyBytes",
		"callbacks.maxIdsPerDelivery",
		"callbacks.maxRows",
		"callbacks.maxAgeHours",
		"callbacks.batchSize",
		"callbacks.retryBaseBackoffSeconds",
		"callbacks.retryMaxBackoffSeconds",
		"callbacks.atRiskLimit",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			callbackConfig(t)
			viper.Set(key, -1)

			err := validateCallbackConfig()
			if err == nil {
				t.Fatalf("expected a negative %s to be refused", key)
			}

			if !strings.Contains(err.Error(), key) {
				t.Errorf("the error does not name %s: %s", key, err)
			}
		})
	}
}

// TestValidateCallbackConfig_AtRiskMargin covers the one bound whose sign matters for a reason
// beyond tidiness: the margin RAISES the threshold the at-risk scan selects on, so a negative one
// would omit exactly the memories the next cycle is about to delete - a warning that goes quiet
// when it is most needed.
func TestValidateCallbackConfig_AtRiskMargin(t *testing.T) {
	callbackConfig(t)

	viper.Set("callbacks.atRiskMargin", 0.1)

	if err := validateCallbackConfig(); err != nil {
		t.Fatalf("expected a positive margin to be valid, got %s", err)
	}

	viper.Set("callbacks.atRiskMargin", -0.1)

	err := validateCallbackConfig()
	if err == nil {
		t.Fatal("expected a negative at-risk margin to be refused")
	}

	if !strings.Contains(err.Error(), "callbacks.atRiskMargin") {
		t.Errorf("the error does not name the key: %s", err)
	}
}

// TestValidateCallbackConfig_Warnings covers the four arms that warn rather than refuse. Each is a
// configuration that works and costs something its author may not have intended - an unbounded
// queue, a whole memory body per delivery, an extra consolidation preview per sleep cycle, or a
// signature over a body travelling in the clear - so the check must pass while still executing
// them.
func TestValidateCallbackConfig_Warnings(t *testing.T) {
	callbackConfig(t)

	// No caps at all: the queue grows without bound on an unreachable receiver, and it is excluded
	// from the capacity target but not from the disk.
	viper.Set("callbacks.maxRows", 0)
	viper.Set("callbacks.maxAgeHours", 0)

	// Bodies carried whole, with nothing bounding one.
	viper.Set("callbacks.includeBodies", true)
	viper.Set("callbacks.maxBodyBytes", 0)

	// The extra preview per cycle.
	viper.Set("callbacks.events.memoriesAtRisk", true)

	if err := validateCallbackConfig(); err != nil {
		t.Errorf("the warned-about configuration must still be valid, got %s", err)
	}

	// A signature over plain http: the body is not altered, and it is also not private.
	viper.Set("callbacks.url", "http://hooks.internal/forgotten")
	viper.Set("callbacks.signingSecret", "s3cret")

	if err := validateCallbackConfig(); err != nil {
		t.Errorf("a signed plain-http sink must be valid, got %s", err)
	}
}

// TestValidateCallbackConfig_BacklogPolicy covers the one callback setting that decides what the
// deployment is willing to LOSE. A misspelling here fails in the direction nobody would guess -
// viper hands back a string nothing matches, which resolves to the default, which is the policy
// whose whole point is that it discards - so it is refused rather than defaulted.
func TestValidateCallbackConfig_BacklogPolicy(t *testing.T) {
	callbackConfig(t)

	for _, name := range hippocampus.BacklogPolicyNames() {
		viper.Set("callbacks.backlogPolicy", name)

		if err := validateCallbackConfig(); err != nil {
			t.Errorf("the shipped policy %q is refused: %s", name, err)
		}
	}

	viper.Set("callbacks.backlogPolicy", "keep")

	err := validateCallbackConfig()
	if err == nil {
		t.Fatal("an unknown backlog policy was accepted, and would silently mean abandon")
	}

	// The message has to name the alternatives, or an operator learns only that their value is
	// wrong and goes round the restart loop guessing.
	for _, name := range hippocampus.BacklogPolicyNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name the %q policy: %s", name, err)
		}
	}
}

// TestValidateCallbackConfig_StallNeedsACap is the one combination here that is refused rather than
// warned about, because it degrades to a policy that does not exist: the caps are exempted (so
// nothing trims the queue) AND the bound meant to replace them can never fire.
func TestValidateCallbackConfig_StallNeedsACap(t *testing.T) {
	callbackConfig(t)

	viper.Set("callbacks.backlogPolicy", "stall")
	viper.Set("callbacks.maxRows", 0)
	viper.Set("callbacks.maxAgeHours", 0)
	viper.Set("callbacks.maxBytes", 0)

	if err := validateCallbackConfig(); err == nil {
		t.Fatal("stall with no cap was accepted: nothing would ever stall and nothing would be trimmed")
	}

	// Any one of the three is enough - they are alternatives, not a set.
	for _, key := range []string{"callbacks.maxRows", "callbacks.maxAgeHours", "callbacks.maxBytes"} {
		viper.Set(key, 1)

		if err := validateCallbackConfig(); err != nil {
			t.Errorf("stall with %s set is refused: %s", key, err)
		}

		viper.Set(key, 0)
	}

	// And "retain" needs no cap at all: it never stalls, so there is nothing for a cap to be judged
	// against.
	viper.Set("callbacks.backlogPolicy", "retain")

	if err := validateCallbackConfig(); err != nil {
		t.Errorf("retain with no cap is refused: %s", err)
	}
}

// TestConfigProblems_TombstoneBounds covers the forgotten log's two bounds through configProblems,
// so the wiring into the startup check is covered along with the rules. Zero disables a bound; a
// negative one is a typo whose only symptom would otherwise be a store filling up.
func TestConfigProblems_TombstoneBounds(t *testing.T) {
	for _, key := range []string{
		"consolidation.tombstones.maxRows",
		"consolidation.tombstones.maxAgeInDays",
	} {
		t.Run(key, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			setStartupDefaults()
			viper.Set("storage.directory", t.TempDir())

			if problems := configProblems(); len(problems) != 0 {
				t.Fatalf("expected the defaults alone to be valid, got %v", problems)
			}

			viper.Set(key, -1)

			problems := configProblems()
			if len(problems) != 1 {
				t.Fatalf("expected exactly one problem for a negative %s, got %v", key, problems)
			}

			if !strings.Contains(problems[0].Error(), key) {
				t.Errorf("the problem does not name %s: %s", key, problems[0].Error())
			}
		})
	}
}

// TestValidateTopologyConfig_Heartbeat covers the one bound the peer registry added. Zero is
// meaningful - it switches the registry off - which is exactly why a negative value must be refused
// rather than accepted as another way of saying the same thing.
func TestValidateTopologyConfig_Heartbeat(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("topology.enabled", true)
	viper.Set("topology.heartbeatSeconds", 0)

	if err := validateTopologyConfig(); err != nil {
		t.Fatalf("a zero heartbeat switches the registry off and must be valid, got %s", err)
	}

	viper.Set("topology.heartbeatSeconds", -1)

	err := validateTopologyConfig()
	if err == nil {
		t.Fatal("expected a negative heartbeat to be refused")
	}

	if !strings.Contains(err.Error(), "topology.heartbeatSeconds") {
		t.Errorf("the error does not name the key: %s", err)
	}
}

// TestValidateTopologyComponents_Unreadable covers the unmarshal arm: a topology.components that is
// not a list of components at all is refused rather than silently read as none, which would present
// as a deployment nobody had declared anything for.
func TestValidateTopologyComponents_Unreadable(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("topology.components", "not-a-list")

	if err := validateTopologyComponents(); err == nil {
		t.Error("expected an unreadable topology.components to be refused")
	}
}

// TestValidateHealthURL_Unparseable covers the parse arm, which url.Parse reaches only for the
// handful of addresses it genuinely refuses - most malformed ones come back as a path, which is
// what the scheme check below it is for.
func TestValidateHealthURL_Unparseable(t *testing.T) {
	if err := validateHealthURL("http://[::1"); err == nil {
		t.Error("expected an unparseable health URL to be refused")
	}
}
