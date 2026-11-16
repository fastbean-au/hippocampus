package main

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/hippocampus"
)

// validateTopologyConfig checks the topology view's settings.
//
// The tier is validated here as well as in NewAuthoriser because the authoriser is only built when
// authentication is enabled: without this, a typo in topology.minimumTier on an unauthenticated
// instance would be accepted in silence and simply mean nothing, and would then start refusing
// callers the day authentication was turned on.
func validateTopologyConfig() error {
	if !viper.GetBool("topology.enabled") {
		return nil
	}

	if tier := strings.TrimSpace(viper.GetString("topology.minimumTier")); tier != "" {
		if !slices.Contains(auth.TierNames(), strings.ToLower(tier)) {
			return fmt.Errorf("topology.minimumTier must be one of %s, got %q", strings.Join(auth.TierNames(), ", "), tier)
		}
	}

	// A probe round runs its checks in sequence, so an interval shorter than one round would leave
	// the prober permanently behind its own schedule - ticks would be dropped, and the interval the
	// RPC reports (which a console paces its polling by) would be a number nothing observes.
	// A negative TTL would disable the cache rather than fail, which is a surprising way to read a
	// typo - the same reasoning as the probe intervals below.
	if seconds := viper.GetInt("listing.countCacheSeconds"); seconds < 0 {
		return fmt.Errorf("listing.countCacheSeconds must not be negative, got %d", seconds)
	}

	interval := viper.GetInt("topology.probeIntervalSeconds")
	timeout := viper.GetInt("topology.probeTimeoutSeconds")

	if interval < 0 {
		return fmt.Errorf("topology.probeIntervalSeconds must not be negative, got %d", interval)
	}

	if timeout < 0 {
		return fmt.Errorf("topology.probeTimeoutSeconds must not be negative, got %d", timeout)
	}

	if interval > 0 && timeout > interval {
		return fmt.Errorf("topology.probeTimeoutSeconds (%d) must not exceed topology.probeIntervalSeconds (%d)", timeout, interval)
	}

	// Zero is meaningful here rather than a fallback to the default: it turns the instance registry
	// off. Negative is not, and would otherwise be accepted as "off" while reading like a setting.
	if heartbeat := viper.GetInt("topology.heartbeatSeconds"); heartbeat < 0 {
		return fmt.Errorf("topology.heartbeatSeconds must not be negative, got %d", heartbeat)
	}

	return validateTopologyComponents()
}

// validateTopologyComponents checks the declared half of the deployment view.
//
// It is strict, and deliberately: a declared component that cannot be probed contributes nothing to
// the view but its own name, while looking on the diagram exactly like a live one. Every rejection
// here is a case that would otherwise render as a permanently red - or, worse, permanently
// plausible - box that nobody could explain.
func validateTopologyComponents() error {
	var components []hippocampus.TopologyComponent

	if err := viper.UnmarshalKey("topology.components", &components); err != nil {
		return fmt.Errorf("failed to read topology.components: %w", err)
	}

	if len(components) > hippocampus.MaxTopologyComponents {
		return fmt.Errorf("topology.components has %d entries, more than the %d allowed (each one is an outbound request every probe interval)",
			len(components), hippocampus.MaxTopologyComponents)
	}

	seen := make(map[string]bool, len(components))
	kinds := hippocampus.TopologyComponentKinds()

	for i, component := range components {
		name := strings.TrimSpace(component.Name)

		if name == "" {
			return fmt.Errorf("topology.components[%d] has no name", i)
		}

		// The name is the node id a client keys its per-node state by, so two components sharing one
		// would collapse into a single box that flickered between their two statuses.
		if seen[name] {
			return fmt.Errorf("topology.components[%d] repeats the name %q", i, name)
		}

		seen[name] = true

		if !slices.Contains(kinds, strings.ToLower(strings.TrimSpace(component.Kind))) {
			return fmt.Errorf("topology.components[%d] (%q) has kind %q, expected one of %s",
				i, name, component.Kind, strings.Join(kinds, ", "))
		}

		if err := validateHealthURL(component.HealthURL); err != nil {
			return fmt.Errorf("topology.components[%d] (%q): %w", i, name, err)
		}
	}

	return nil
}

// validateHealthURL rejects a health URL this process could not actually request. An address with no
// scheme is the one worth naming explicitly: "nats-bridge:8090" is what an operator naturally writes
// and what url.Parse accepts as a path, so without this it would be accepted at startup and then
// fail every probe for a reason the error message would never mention.
func validateHealthURL(raw string) error {
	address := strings.TrimSpace(raw)

	if address == "" {
		return fmt.Errorf("healthUrl must be set (a component that cannot be probed reports nothing the config file does not already say)")
	}

	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("healthUrl %q is not a valid URL: %w", address, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("healthUrl %q must start with http:// or https://", address)
	}

	if parsed.Host == "" {
		return fmt.Errorf("healthUrl %q names no host", address)
	}

	return nil
}

// setTopologyDefaults applies the topology.* defaults.
func setTopologyDefaults() {
	// The deployment topology view. On by default, unlike every other optional feature here,
	// because it neither stores anything nor reaches anything a running instance is not already
	// attached to - and because the failure it exists to make visible (a dependency that is quietly
	// unreachable) is one nobody thinks to switch a diagnostic on for beforehand.
	//
	// The tier defaults to reader rather than admin. That is safe only because no address in the
	// response is unredacted (see hippocampus/topology_probe.go's redactEndpoint), and it is
	// deliberate: what the view exposes is the deployment's shape rather than its contents, and the
	// people who need it most are rarely the ones holding an admin token. Deployments where the
	// shape is itself sensitive raise it here.
	viper.SetDefault("topology.enabled", true)
	viper.SetDefault("topology.minimumTier", "reader")
	viper.SetDefault("topology.probeIntervalSeconds", 30)
	viper.SetDefault("topology.probeTimeoutSeconds", 2)

	// The instance registry (see hippocampus/peers.go). On by default on the server drivers, where
	// it is the only way an instance can name its peers - and the only way "every instance came up
	// as a replica, so nothing is forgetting" is reported at all, that being a fault with no
	// component to attach itself to. Ignored on SQLite, which is single-instance by construction.
	// One small upsert and one indexed delete per instance per interval.
	viper.SetDefault("topology.heartbeatSeconds", 30)
}
