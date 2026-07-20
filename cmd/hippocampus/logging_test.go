package main

import (
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestInitLogging verifies every recognised level string (including the short aliases) maps to
// the expected logrus level, that an unrecognised level falls back to Info, and that json toggles
// the formatter between JSONFormatter and logrus's default TextFormatter.
func TestInitLogging(t *testing.T) {
	// Save and restore the global logrus state so this test cannot leak into any other test in the
	// package (initLogging mutates package-level logrus globals).
	restoreLevel := log.GetLevel()
	restoreFormatter := log.StandardLogger().Formatter
	restoreOutput := log.StandardLogger().Out
	t.Cleanup(func() {
		log.SetLevel(restoreLevel)
		log.SetFormatter(restoreFormatter)
		log.SetOutput(restoreOutput)
	})

	cases := []struct {
		name  string
		level string
		want  log.Level
	}{
		{name: "trace short", level: "t", want: log.TraceLevel},
		{name: "trace long", level: "trace", want: log.TraceLevel},
		{name: "debug short", level: "d", want: log.DebugLevel},
		{name: "debug long", level: "debug", want: log.DebugLevel},
		{name: "verbose short", level: "v", want: log.DebugLevel},
		{name: "verbose long", level: "verbose", want: log.DebugLevel},
		{name: "info short", level: "i", want: log.InfoLevel},
		{name: "info long", level: "info", want: log.InfoLevel},
		{name: "information", level: "information", want: log.InfoLevel},
		{name: "warn short", level: "w", want: log.WarnLevel},
		{name: "warn long", level: "warn", want: log.WarnLevel},
		{name: "warning", level: "warning", want: log.WarnLevel},
		{name: "error short", level: "e", want: log.ErrorLevel},
		{name: "err", level: "err", want: log.ErrorLevel},
		{name: "error long", level: "error", want: log.ErrorLevel},
		{name: "fatal short", level: "f", want: log.FatalLevel},
		{name: "fatal long", level: "fatal", want: log.FatalLevel},
		{name: "panic short", level: "p", want: log.PanicLevel},
		{name: "panic long", level: "panic", want: log.PanicLevel},
		{name: "unrecognised falls back to info", level: "not-a-level", want: log.InfoLevel},
		{name: "empty falls back to info", level: "", want: log.InfoLevel},
		{name: "case-insensitive", level: "DEBUG", want: log.DebugLevel},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			initLogging(tc.level, false)

			if got := log.GetLevel(); got != tc.want {
				t.Errorf("initLogging(%q, false): level = %s, want %s", tc.level, got, tc.want)
			}
		})
	}

	// json=false leaves whatever formatter logrus already had in place - initLogging only ever sets
	// the JSON formatter, never a text one - which matches its actual call site (main() calls it
	// exactly once at startup with the configured logging.json).
	log.SetFormatter(&log.TextFormatter{})
	initLogging("info", false)

	if _, ok := log.StandardLogger().Formatter.(*log.JSONFormatter); ok {
		t.Error("expected initLogging(..., false) not to install the JSON formatter")
	}

	// json=true installs the JSON formatter.
	initLogging("info", true)

	if _, ok := log.StandardLogger().Formatter.(*log.JSONFormatter); !ok {
		t.Errorf("expected initLogging(..., true) to install the JSON formatter, got %T", log.StandardLogger().Formatter)
	}
}
