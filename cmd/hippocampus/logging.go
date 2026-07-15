package main

import (
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
)

func initLogging(level string, json bool) {
	if json {
		log.SetFormatter(&log.JSONFormatter{})
	}

	log.SetOutput(os.Stdout)

	var l log.Level

	switch strings.ToLower(level) {
	case "t", "trace":
		l = log.TraceLevel
	case "d", "debug", "v", "verbose":
		l = log.DebugLevel
	case "i", "info", "information":
		l = log.InfoLevel
	case "w", "warn", "warning":
		l = log.WarnLevel
	case "e", "err", "error":
		l = log.ErrorLevel
	case "f", "fatal":
		l = log.FatalLevel
	case "p", "panic":
		l = log.PanicLevel
	default:
		l = log.InfoLevel
	}

	log.SetLevel(l)
}
