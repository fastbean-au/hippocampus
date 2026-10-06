package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// TestRun_TheCallbackURLIsLoggedRedacted: a webhook's secret is routinely its path, and the startup
// line naming the receiver printed it whole (TODO-3 item 168).
func TestRun_TheCallbackURLIsLoggedRedacted(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	viper.Set("callbacks.enabled", true)
	viper.Set("callbacks.url", "https://hooks.example.com/services/T000/B000/s3cretPathToken")

	var logged bytes.Buffer

	previous := log.StandardLogger().Out
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()
	<-done

	if strings.Contains(logged.String(), "s3cretPathToken") {
		t.Error("the callback URL's secret path reached the log")
	}

	if !strings.Contains(logged.String(), "delivering to https://hooks.example.com") {
		t.Error("the startup line no longer names the receiver's host")
	}
}
