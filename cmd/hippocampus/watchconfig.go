package main

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
)

// setWatchDefaults applies the watch.* defaults.
//
// The change log is off by default - it is a row per write - and its bounds are defaulted anyway,
// on the forgotten log's reasoning: turning it on gets a log that is already bounded. A day of
// history and a million rows, whichever binds first, is a resume window that outlasts any ordinary
// outage of a client. watch.changeLog.maxBytes has no default, for the reason the other byte caps
// have none (setConsolidationDefaults); 0 there, or on either cap, removes that bound.
func setWatchDefaults() {
	viper.SetDefault("watch.changeLog.enabled", false)
	viper.SetDefault("watch.changeLog.maxRows", 1000000)
	viper.SetDefault("watch.changeLog.maxAgeHours", 24)
	viper.SetDefault("watch.changeLog.pollIntervalMillis", 500)
}

// watchProblems checks the watch.* keys. A negative bound is a typo; a poll interval that is not
// positive would spin the reader, which runs only while the log is on.
func watchProblems() []error {
	var problems []error

	bounds := []string{
		"watch.changeLog.maxRows",
		"watch.changeLog.maxBytes",
		"watch.changeLog.maxAgeHours",
	}

	for _, key := range bounds {
		if value := viper.GetInt64(key); value < 0 {
			problems = append(problems, fmt.Errorf("%s must not be negative, got %d", key, value))
		}
	}

	if !viper.GetBool("watch.changeLog.enabled") {

		return problems
	}

	if interval := viper.GetInt("watch.changeLog.pollIntervalMillis"); interval <= 0 {
		problems = append(problems, fmt.Errorf("watch.changeLog.pollIntervalMillis must be greater than 0, got %d", interval))
	}

	return problems
}

// changeLogPolicyFromViper reads the change log's policy, which the store records under.
func changeLogPolicyFromViper() db.ChangeLogPolicy {
	return db.ChangeLogPolicy{
		Enabled:  viper.GetBool("watch.changeLog.enabled"),
		MaxRows:  viper.GetInt64("watch.changeLog.maxRows"),
		MaxBytes: viper.GetInt64("watch.changeLog.maxBytes"),
		MaxAge:   hours("watch.changeLog.maxAgeHours"),
	}
}

// warnIfUnboundedChangeLog says so when the change log is on with none of its bounds.
func warnIfUnboundedChangeLog(policy db.ChangeLogPolicy) {
	if !policy.Enabled || policy.MaxRows > 0 || policy.MaxBytes > 0 || policy.MaxAge > 0 {

		return
	}

	log.Warn(
		"the change log is enabled with none of watch.changeLog.maxRows, maxBytes or maxAgeHours " +
			"set, so it will grow with every write; it is excluded from the capacity target but not " +
			"from the disk",
	)
}
