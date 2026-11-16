package main

import (
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/hippocampus"
	"github.com/fastbean-au/hippocampus/notify"
)

// callbacksTLSFromViper reads the callback receiver's TLS block, honouring callbacks.tls.enabled
// (TODO-3 item 163). The key was documented and offered by the wizard but never read, so an operator
// turning the block off with insecureSkipVerify still set kept skipping verification. Explicitly
// false now discards the block, with a warning naming what it carried; explicitly true applies it.
// Unset, the block applies whenever it carries anything - which is how every configuration written
// before the key was read behaves, and dropping their trust options would silently stop a
// private-CA receiver verifying.
func callbacksTLSFromViper() notify.TLSConfig {
	config := notify.TLSConfig{
		CACertFile:         viper.GetString("callbacks.tls.caCertFile"),
		CertFile:           viper.GetString("callbacks.tls.certFile"),
		KeyFile:            viper.GetString("callbacks.tls.keyFile"),
		InsecureSkipVerify: viper.GetBool("callbacks.tls.insecureSkipVerify"),
	}

	if !viper.IsSet("callbacks.tls.enabled") || viper.GetBool("callbacks.tls.enabled") {
		return config
	}

	if config != (notify.TLSConfig{}) {
		log.Warn("callbacks.tls.enabled is false, so the trust options set beside it (caCertFile, certFile/keyFile, insecureSkipVerify) are ignored")
	}

	return notify.TLSConfig{}
}

// validateCallbackConfig checks the outbound callback settings.
//
// A missing URL is fatal rather than a warning: callbacks.enabled with nothing to send to would
// record a row per forgotten memory into a queue that can never drain, which is the one arrangement
// the feature is built to avoid. Everything else here is a bound, where a negative value is a typo
// and 0 deliberately means "no bound".
func validateCallbackConfig() error {
	if !viper.GetBool("callbacks.enabled") {
		return nil
	}

	url := strings.TrimSpace(viper.GetString("callbacks.url"))
	if url == "" {
		return fmt.Errorf("callbacks.url is required when callbacks.enabled is set (or inject it as HIPPOCAMPUS_CALLBACKS_URL)")
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("callbacks.url must be an http:// or https:// URL, got %q", url)
	}

	bounds := map[string]int{
		"callbacks.timeoutSeconds":          viper.GetInt("callbacks.timeoutSeconds"),
		"callbacks.maxBodyBytes":            viper.GetInt("callbacks.maxBodyBytes"),
		"callbacks.maxIdsPerDelivery":       viper.GetInt("callbacks.maxIdsPerDelivery"),
		"callbacks.maxRows":                 viper.GetInt("callbacks.maxRows"),
		"callbacks.maxBytes":                viper.GetInt("callbacks.maxBytes"),
		"callbacks.maxAgeHours":             viper.GetInt("callbacks.maxAgeHours"),
		"callbacks.batchSize":               viper.GetInt("callbacks.batchSize"),
		"callbacks.retryBaseBackoffSeconds": viper.GetInt("callbacks.retryBaseBackoffSeconds"),
		"callbacks.retryMaxBackoffSeconds":  viper.GetInt("callbacks.retryMaxBackoffSeconds"),
		"callbacks.atRiskLimit":             viper.GetInt("callbacks.atRiskLimit"),
	}

	for key, value := range bounds {
		if value < 0 {
			return fmt.Errorf("%s must not be negative, got %d", key, value)
		}
	}

	// A body carried without a cap makes one large memory able to fill the queue and the delivery
	// alike; a warning rather than an error, because an operator who knows their bodies are small
	// is entitled to say so.
	if viper.GetBool("callbacks.includeBodies") && viper.GetInt("callbacks.maxBodyBytes") <= 0 {
		log.Warn(
			"callbacks.includeBodies is set with no callbacks.maxBodyBytes, so a single large " +
				"memory can be carried whole into the queue and the delivery",
		)
	}

	// A negative margin would lower the bar below the one in force, so the delivery would leave out
	// memories the cycle is about to take - a warning that is silent about exactly what it exists to
	// warn about, which is worse than no warning at all.
	if viper.GetFloat64("callbacks.atRiskMargin") < 0 {
		return fmt.Errorf(
			"callbacks.atRiskMargin must not be negative, got %v: it raises the threshold the at-risk scan selects on, and lowering it would omit memories the next cycle will delete",
			viper.GetFloat64("callbacks.atRiskMargin"),
		)
	}

	// Warned about rather than refused: it is the one callback that costs a full scan per cycle, and
	// an operator who wants it on a store consolidating every few seconds is entitled to say so.
	if viper.GetBool("callbacks.events.memoriesAtRisk") {
		log.Info(
			"callbacks.events.memoriesAtRisk is set: every sleep cycle runs an extra consolidation " +
				"preview to build the warning, which costs roughly what the cycle itself does",
		)
	}

	if strings.HasPrefix(url, "http://") && viper.GetString("callbacks.signingSecret") != "" {
		log.Warn(
			"callbacks.signingSecret is set on a plain http:// callbacks.url: the signature proves " +
				"the body was not altered, but the body itself - which may carry memory bodies - " +
				"travels in the clear",
		)
	}

	if viper.GetInt("callbacks.maxRows") <= 0 && viper.GetInt("callbacks.maxAgeHours") <= 0 &&
		viper.GetInt64("callbacks.maxBytes") <= 0 {
		log.Warn(
			"callbacks are enabled with none of callbacks.maxRows, callbacks.maxAgeHours or " +
				"callbacks.maxBytes set, so an unreachable receiver will grow the queue without " +
				"bound; it is excluded from the capacity target but not from the disk",
		)
	}

	// The combination the byte cap exists for. A row cap bounds this queue's rows and says nothing
	// about its size once a row can carry five hundred memory bodies: at the defaults that is
	// maxRows x maxIdsPerDelivery x maxBodyBytes, which is a number no disk has. Warned rather than
	// refused, because an operator who knows their bodies are small is entitled to say so - and it
	// is the shape that starts cleanly and then misbehaves, which is what a startup warning is for.
	if viper.GetBool("callbacks.includeBodies") && viper.GetInt64("callbacks.maxBytes") <= 0 {
		log.Warn(
			"callbacks.includeBodies is set with no callbacks.maxBytes: callbacks.maxRows bounds " +
				"the queue's rows, not its size, and a row carrying memory bodies is not a fixed " +
				"size - so nothing bounds what an unreachable receiver can add to the disk",
		)
	}

	return validateBacklogPolicy()
}

// validateBacklogPolicy checks callbacks.backlogPolicy and the settings it changes the meaning of.
//
// Split out because it is the one callback setting that decides what the service is willing to LOSE,
// and the three values fail in three different places - the far system, this disk, the workload. A
// deployment that picked one and then configured it as though it had picked another gets the failure
// of the one it did not choose.
func validateBacklogPolicy() error {
	setting := viper.GetString("callbacks.backlogPolicy")

	policy, ok := hippocampus.ParseBacklogPolicy(setting)
	if !ok {
		return fmt.Errorf(
			"callbacks.backlogPolicy must be one of %s, got %q",
			strings.Join(hippocampus.BacklogPolicyNames(), ", "),
			setting,
		)
	}

	capped := viper.GetInt("callbacks.maxRows") > 0 ||
		viper.GetInt("callbacks.maxAgeHours") > 0 ||
		viper.GetInt64("callbacks.maxBytes") > 0

	// A stall with no cap can never fire, so it is refused rather than warned about: every other
	// misconfiguration here degrades to one of the two remaining policies, but this one degrades to a
	// policy that does not exist - the caps are retained (so nothing trims the queue) AND the bound
	// that was supposed to replace them never triggers, which is unbounded growth chosen by an
	// operator who explicitly asked for the opposite.
	if policy == hippocampus.BacklogStall && !capped {
		return fmt.Errorf(
			"callbacks.backlogPolicy is \"stall\" with none of callbacks.maxRows, callbacks.maxAgeHours " +
				"or callbacks.maxBytes set: the stall is judged against those caps, so nothing would " +
				"ever stall and nothing would ever be trimmed",
		)
	}

	// The forgotten log is the PULL path behind the push one: it records the same deletions, in
	// order, with a keyset cursor, so a receiver that was rebuilt - or whose queue an operator
	// emptied - can page GetForgottenMemories back to its cursor and catch up. Without it, a delivery
	// that never lands is unrecoverable however carefully the queue holds it, which is most of what
	// retaining it was for.
	if policy != hippocampus.BacklogAbandon && !viper.GetBool("consolidation.tombstones.enabled") {
		log.Warnf(
			"callbacks.backlogPolicy is %q with consolidation.tombstones.enabled off: the queue will "+
				"hold undelivered forget-callbacks, but nothing records them anywhere else, so a "+
				"receiver that is rebuilt or a queue that is emptied leaves those deletions "+
				"unrecoverable - enable the forgotten log as the catch-up path",
			setting,
		)
	}

	// The shape the whole policy exists for: this store points at payloads it does not hold, and the
	// callback is the only thing that will ever tell the far system to delete one. Under abandon a
	// receiver outage leaks them permanently, and the leak grows with how well the decay cycle is
	// working, which is the opposite of the direction a warning usually needs to point.
	if policy == hippocampus.BacklogAbandon && viper.GetInt64("consolidation.capacityExternalBytes") > 0 {
		log.Warn(
			"consolidation.capacityExternalBytes is set with callbacks.backlogPolicy at its default " +
				"of \"abandon\": this store is sizing storage it does not hold, but a receiver outage " +
				"long enough to reach the queue's caps discards the forget-callbacks for whatever was " +
				"deleted meanwhile, orphaning those payloads permanently - consider \"retain\" or \"stall\"",
		)
	}

	return nil
}

// callbackSinkConfig is what initNotifier needs to build the optional callback sink.
type callbackSinkConfig struct {
	Enabled bool
	Webhook notify.Config
}

// setCallbackDefaults applies the callbacks.* defaults.
func setCallbackDefaults() {
	// Outbound callbacks (see docs/operations.md). Off by default - a deployment with no receiver
	// should queue nothing at all - but every bound is defaulted anyway, on the same reasoning as
	// the forgotten log: turning it on gets a queue that is already bounded rather than one that
	// grows until a receiver's outage fills the disk. An explicit 0 on either cap disables it.
	//
	// The three deletion toggles default ON because the feature as a whole is off: an operator who
	// has configured a URL wants the callbacks, and having to then enable them individually would be
	// a second switch for the same decision. The fourth - memoriesAtRisk - defaults OFF against that
	// grain, because it is the only kind that costs a scan rather than riding on work already being
	// done: raising it runs a preview at the top of every cycle.
	viper.SetDefault("callbacks.timeoutSeconds", 10)
	viper.SetDefault("callbacks.maxBodyBytes", 65536)
	viper.SetDefault("callbacks.maxIdsPerDelivery", 500)
	viper.SetDefault("callbacks.maxRows", 1000000)
	viper.SetDefault("callbacks.maxAgeHours", 24)
	viper.SetDefault("callbacks.batchSize", 100)
	viper.SetDefault("callbacks.retryBaseBackoffSeconds", 1)
	viper.SetDefault("callbacks.retryMaxBackoffSeconds", 300)
	viper.SetDefault("callbacks.events.memoryForgotten", true)
	viper.SetDefault("callbacks.events.eventForgotten", true)
	viper.SetDefault("callbacks.events.sleepCompleted", true)
	viper.SetDefault("callbacks.events.memoriesAtRisk", false)
	viper.SetDefault("callbacks.events.memoryWrites", false)
	viper.SetDefault("callbacks.atRiskLimit", 1000)
	viper.SetDefault("callbacks.atRiskMargin", 0)

	// "abandon" is what every deployment had before the key existed, so the default changes nothing.
	// It is not the right answer for a store pointing at payloads it does not hold - see
	// validateBacklogPolicy, which says so at startup rather than choosing for the operator, because
	// the alternatives cost either this disk or the store's willingness to forget at all.
	viper.SetDefault("callbacks.backlogPolicy", "abandon")

	// callbacks.maxBytes is the one byte cap that bounds something a row cap cannot, and it is
	// still not defaulted. A delivery carries up to maxIdsPerDelivery items and, under
	// includeBodies, a body each, so maxRows bounds this queue between a few hundred megabytes of
	// bare ids and a figure with no ceiling - but how much of that a deployment can afford is a
	// property of its disk, and there is no number that is right for both a laptop store and a
	// server. Reaching this cap discards notifications nobody will ever receive, which is a policy
	// an operator should choose rather than inherit. What stands in for a default is the startup
	// warning in validateCallbackConfig, where the shape is dangerous.
}

// callbackSinkConfigFromViper reads the callbacks.* delivery keys.
func callbackSinkConfigFromViper() callbackSinkConfig {
	log.Trace("func() callbackSinkConfigFromViper")

	cfg := callbackSinkConfig{
		Enabled: viper.GetBool("callbacks.enabled"),
	}

	if cfg.Enabled {
		cfg.Webhook = notify.Config{
			URL:           viper.GetString("callbacks.url"),
			Token:         viper.GetString("callbacks.token"),
			SigningSecret: viper.GetString("callbacks.signingSecret"),
			Timeout:       time.Duration(viper.GetInt("callbacks.timeoutSeconds")) * time.Second,
			TLS:           callbacksTLSFromViper(),
		}
	}

	return cfg
}

// initNotifier builds the optional outbound callback sink. The no-op unless callbacks.enabled, and
// the no-op is not merely a disabled feature: it gates the persisted queue's recording as well as
// its dispatch, so a deployment with no receiver never writes a row. Construction validates the
// configuration and deliberately does not dial - a receiver that happens to be down at startup is
// exactly what the queue exists to survive.
func initNotifier(cfg callbackSinkConfig) (notify.Notifier, error) {
	log.Trace("func() initNotifier")

	if !cfg.Enabled {
		return notify.NewNoop(), nil
	}

	log.Debug("initialising the callback sink")

	hook, err := notify.NewWebhook(cfg.Webhook)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise the callback sink: %w", err)
	}

	// Redacted as the topology view redacts it: a webhook's secret is routinely its path, and a
	// log line outlives the configuration it came from (TODO-3 item 168).
	log.Infof("callbacks enabled, delivering to %s", hippocampus.RedactEndpoint(cfg.Webhook.URL))

	return hook, nil
}
