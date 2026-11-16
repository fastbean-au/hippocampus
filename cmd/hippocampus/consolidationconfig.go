package main

import (
	"fmt"
	"math"

	"github.com/spf13/viper"
)

// setConsolidationDefaults applies the consolidation.* and sleep.* defaults.
func setConsolidationDefaults() {
	// Three of the four keys validateConfig refuses at zero; storage.directory, the fourth, is in
	// setStoreDefaults. They are defaulted so that a run with no configuration file at all - the
	// first thing anyone does after cloning - starts on the same decay curve the documentation
	// describes, rather than failing on four keys in turn. This is
	// deliberately not a relaxation of item 19.1's guarantee: viper falls back to a default only
	// for a key the configuration does not set, so an explicitly configured 0 still fails
	// validation, which is the case that silently deleted everything. The values match the repo's
	// own config.json, so reading that file changes nothing.
	viper.SetDefault("consolidation.method", 1)
	viper.SetDefault("consolidation.aggressiveness", 1.0)
	viper.SetDefault("consolidation.unitsOfAgeInDays", 1.0)

	// The three keys that made the defaults VALID but not FUNCTIONAL, which is the sharper form of
	// the same mistake. None of them was refused at zero, so all three read as their zero value and
	// nothing failed: a zero deletionThreshold makes shouldConsolidate's comparison `value < 0`,
	// which no positive value satisfies, so consolidation became a total no-op; a zero
	// sleep.periodSeconds disabled the timed cycle that would have applied it; and
	// consolidation.enabled was defaulted in run() rather than here, so an instance built from the
	// defaults alone rejected its own Sleep RPC. A memory service that starts, validates, serves,
	// and forgets nothing is a worse failure than one that refuses to start, because only the
	// second is noticed. Values match the repo's own config.json.
	viper.SetDefault("consolidation.deletionThreshold", 10.0)
	viper.SetDefault("sleep.periodSeconds", 3600)
	viper.SetDefault("consolidation.enabled", true)

	// How long a significance registry level survives having nothing left that carries it, before
	// the sleep cycle reaps it. The registry is the one table that otherwise grows with the store's
	// HISTORY rather than its contents - a row per distinct significance value ever written, never
	// removed - so unlike every other bound here this one defaults to being IN FORCE.
	//
	// A week rather than a month, which is the window the forgotten log takes, because the two are
	// bounding different things. The log is a record and its window is how much history an operator
	// wants to be able to read; the registry is a SCALE, and what the window protects is a client
	// that positions against a value it is not carrying at this instant. Seven days spans the gap
	// between any two sessions of a client that is actually using the registry that way, while a
	// producer writing arbitrary values - the shape that grew one to 29,001 rows - never reads it at
	// all. 0 keeps every value ever seen, which is what the service did before.
	viper.SetDefault("consolidation.significanceLevels.unusedRetentionInDays", 7)

	// The forgotten log (see docs/operations.md). Off by default - it costs a row per forgotten
	// memory - but its bounds are defaulted anyway, so that turning it on gets a log that is
	// already bounded rather than one that grows until somebody notices. An explicit 0 on either
	// disables that bound.
	viper.SetDefault("consolidation.tombstones.maxRows", 100000)
	viper.SetDefault("consolidation.tombstones.maxAgeInDays", 30)

	// consolidation.tombstones.maxBytes has deliberately NO default, and neither do the byte caps
	// on the two queues (callbacks.maxBytes and opensearch.outbox.maxBytes). A tombstone is a
	// fixed-width row, so the row cap above already IS a byte cap - roughly 19 MB at the default -
	// and a second defaulted number would be the same
	// bound said twice, in a unit that would then disagree with it the day the per-row allowance
	// moved. What the key buys is being able to state the bound in the unit a disk is sized in.
	// Zero means unbounded on all three; see docs/operations.md on sizing.
}

// consolidationProblems checks the consolidation.* keys.
func consolidationProblems() []error {
	var problems []error

	if unitsOfAgeInDays := viper.GetFloat64("consolidation.unitsOfAgeInDays"); unitsOfAgeInDays <= 0 {
		problems = append(problems, fmt.Errorf("consolidation.unitsOfAgeInDays must be greater than 0, got %v", unitsOfAgeInDays))
	}

	if method := viper.GetInt("consolidation.method"); method < 1 || method > 6 {
		problems = append(problems, fmt.Errorf("consolidation.method must be between 1 and 6, got %d", method))
	}

	if aggressiveness := viper.GetFloat64("consolidation.aggressiveness"); aggressiveness <= 0 {
		problems = append(problems, fmt.Errorf("consolidation.aggressiveness must be greater than 0, got %v", aggressiveness))
	}

	// A standby waits for the shared store's single-consolidator lock (TODO-3 item 169). SQLite has
	// no shared lock to wait for, and consolidation.enabled true takes the lock at startup - so the
	// two together would be a standby that either never promotes or never waits.
	if viper.GetBool("consolidation.standby") {
		switch driver := viper.GetString("storage.driver"); driver {

		case "postgres", "mysql":

		default:
			problems = append(problems, fmt.Errorf("consolidation.standby needs a shared store (storage.driver postgres or mysql), got %q: there is no lock for a standby to wait on", driver))

		}

		if viper.GetBool("consolidation.enabled") {
			problems = append(problems, fmt.Errorf("consolidation.standby is set with consolidation.enabled true: a standby starts as a replica (consolidation.enabled false) and takes over when the lock is free"))
		}
	}

	if poll := viper.GetInt("consolidation.standbyPollSeconds"); poll < 0 {
		problems = append(problems, fmt.Errorf("consolidation.standbyPollSeconds must not be negative, got %d", poll))
	}

	// Method 3's decay factor is 1 + ln(aggressiveness), which goes non-positive for any
	// aggressiveness at or below 1/e (~0.368). calculateValue treats a non-positive factor as
	// "never delete" (it returns MaxFloat64 to keep NaN out of eviction's sort), so such a config
	// silently disables value-based consolidation entirely while the generic aggressiveness > 0
	// check above passes. Reject it here so the mistake surfaces at startup rather than as a store
	// that quietly never forgets anything.
	if method := viper.GetInt("consolidation.method"); method == 3 {
		if aggressiveness := viper.GetFloat64("consolidation.aggressiveness"); aggressiveness <= math.Exp(-1) {
			problems = append(problems, fmt.Errorf("consolidation.aggressiveness must be greater than 1/e (~0.368) for consolidation.method 3, got %v", aggressiveness))
		}
	}

	// The deletion threshold is the value every memory is compared against, so a non-positive one
	// makes `value < threshold` false for every memory and disables value-based consolidation
	// entirely - the same silent failure the method-3 aggressiveness check above exists to catch,
	// reached by a much more obvious route. An unset key falls back to setStartupDefaults' value, so
	// this refuses the mistake without making the key mandatory.
	//
	// It is refused only when nothing else would forget anything either, because switching value-
	// based consolidation off is a real configuration and not always a mistake: a store with a byte
	// capacity forgets on the capacity target alone (eviction in ascending value order), which keeps
	// a memory for as long as the store has room for it rather than for a lifetime derived from its
	// significance. That is the closed-loop end of the trade docs/consolidation.md's Forgetting modes
	// section names, and neither of the two idioms this check was first written against is a
	// substitute for it - consolidation.enabled and a non-positive sleep.periodSeconds both disable
	// EVICTION as well, which is the mechanism the mode runs on.
	//
	// capacityMemories deliberately does not rescue it. A row capacity only scales the pressure that
	// scales the threshold, and scaling a non-positive threshold leaves it non-positive; nothing
	// evicts on the row count, so that pairing really does forget nothing.
	//
	// consolidation.capacityExternalBytes rescues it on exactly the same terms, and for the same
	// reason: it is a capacity target eviction runs against, so a store carrying one forgets on the
	// capacity target alone whether or not it also bounds its own disk. A retention controller over
	// storage held elsewhere is precisely that configuration.
	if threshold := viper.GetFloat64("consolidation.deletionThreshold"); threshold <= 0 {
		capacityBytes := viper.GetInt64("consolidation.capacityBytes")
		capacityExternalBytes := viper.GetInt64("consolidation.capacityExternalBytes")

		if capacityBytes <= 0 && capacityExternalBytes <= 0 {
			problems = append(problems, fmt.Errorf(
				"consolidation.deletionThreshold must be greater than 0 (or set consolidation.capacityBytes or consolidation.capacityExternalBytes to forget on a capacity target alone), got %v",
				threshold,
			))
		}
	}

	// The external capacity axis's two keys. A negative is meaningless on both (0 disables), and
	// on the target it is worth catching rather than clamping: a mis-signed capacity would leave the
	// axis inert while the configuration says it is bounded, which reads as an axis keeping up.
	if capacityExternalBytes := viper.GetInt64("consolidation.capacityExternalBytes"); capacityExternalBytes < 0 {
		problems = append(problems, fmt.Errorf("consolidation.capacityExternalBytes must not be negative, got %d", capacityExternalBytes))
	}

	if floor := viper.GetInt64("consolidation.capacityExternalBytesFloor"); floor < 0 {
		problems = append(problems, fmt.Errorf("consolidation.capacityExternalBytesFloor must not be negative, got %d", floor))
	}

	// A negative retention window is meaningless (0 disables the floor). Catch it at startup rather
	// than let a mis-signed value silently disable the guarantee that overrides the capacity target.
	if retention := viper.GetInt("consolidation.minimumRetentionInDays"); retention < 0 {
		problems = append(problems, fmt.Errorf("consolidation.minimumRetentionInDays must not be negative, got %d", retention))
	}

	// The maximum is a ceiling (TODO-3 item 157): 0 disables it, a negative value is a mis-signed one,
	// and a ceiling at or below the minimum floor contradicts itself - every memory would be both
	// protected and past its limit for the whole of the window between them.
	maximum := viper.GetInt("consolidation.maximumRetentionInDays")
	minimum := viper.GetInt("consolidation.minimumRetentionInDays")

	switch {

	case maximum < 0:
		problems = append(problems, fmt.Errorf("consolidation.maximumRetentionInDays must not be negative, got %d", maximum))

	case maximum > 0 && minimum > 0 && maximum <= minimum:
		problems = append(problems, fmt.Errorf(
			"consolidation.maximumRetentionInDays (%d) must be greater than consolidation.minimumRetentionInDays (%d)",
			maximum,
			minimum,
		))

	}

	// sleep.periodSeconds is deliberately not validated: a non-positive value disables automatic
	// timed sleep cycles (a supported mode - e.g. an import-only instance, or one driven purely by
	// the manual Sleep RPC or the WAL trigger). autoSleep treats it as "no timed sleep".

	// The forgotten log's bounds. A negative value is meaningless (0 disables that bound), and
	// getting the sign wrong on the one thing standing between an optional log and unbounded growth
	// should not be discoverable only by watching the store fill.
	if maxRows := viper.GetInt("consolidation.tombstones.maxRows"); maxRows < 0 {
		problems = append(problems, fmt.Errorf("consolidation.tombstones.maxRows must not be negative, got %d", maxRows))
	}

	if maxAge := viper.GetInt("consolidation.tombstones.maxAgeInDays"); maxAge < 0 {
		problems = append(problems, fmt.Errorf("consolidation.tombstones.maxAgeInDays must not be negative, got %d", maxAge))
	}

	if maxBytes := viper.GetInt64("consolidation.tombstones.maxBytes"); maxBytes < 0 {
		problems = append(problems, fmt.Errorf("consolidation.tombstones.maxBytes must not be negative, got %d", maxBytes))
	}

	return problems
}
