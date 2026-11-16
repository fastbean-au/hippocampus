package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
)

// TestWatchProblems: a negative cap is a typo, and a poll interval that is not positive is refused
// only while the log is on, since nothing reads it otherwise.
//
// Not parallel: it sets global viper state.
func TestWatchProblems(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	setWatchDefaults()

	if problems := watchProblems(); len(problems) != 0 {
		t.Fatalf("the defaults have problems: %v", problems)
	}

	viper.Set("watch.changeLog.maxRows", -1)
	viper.Set("watch.changeLog.pollIntervalMillis", 0)

	problems := watchProblems()

	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "watch.changeLog.maxRows") {
		t.Errorf("with the log off, problems = %v; want the negative cap alone", problems)
	}

	viper.Set("watch.changeLog.enabled", true)

	if problems := watchProblems(); len(problems) != 2 {
		t.Errorf("with the log on, problems = %v; want the cap and the poll interval", problems)
	}
}

// TestChangeLogPolicyFromViper reads the caps in their units.
//
// Not parallel: it sets global viper state.
func TestChangeLogPolicyFromViper(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	setWatchDefaults()
	viper.Set("watch.changeLog.enabled", true)
	viper.Set("watch.changeLog.maxBytes", 2048)

	want := db.ChangeLogPolicy{Enabled: true, MaxRows: 1000000, MaxBytes: 2048, MaxAge: 24 * time.Hour}

	if got := changeLogPolicyFromViper(); got != want {
		t.Errorf("changeLogPolicyFromViper() = %+v, want %+v", got, want)
	}

	// Says so, and must not panic, for both shapes.
	warnIfUnboundedChangeLog(db.ChangeLogPolicy{Enabled: true})
	warnIfUnboundedChangeLog(want)
}
