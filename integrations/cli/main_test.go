package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestExecuteSuccess(t *testing.T) {
	var out bytes.Buffer

	if code := execute([]string{"--version"}, &out, &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if strings.TrimSpace(out.String()) != version {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestExecuteError(t *testing.T) {
	var out bytes.Buffer

	if code := execute([]string{"frobnicate"}, &out, &out); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	if !strings.Contains(out.String(), "hippo: unknown command") {
		t.Fatalf("stderr = %q", out.String())
	}
}

func TestResolveCommandPrefersTwoWord(t *testing.T) {
	key, rest, _, ok := resolveCommand([]string{"memory", "store", "--body", "x"})
	if !ok || key != "memory store" {
		t.Fatalf("key = %q ok = %t, want 'memory store'", key, ok)
	}

	if strings.Join(rest, " ") != "--body x" {
		t.Fatalf("rest = %v", rest)
	}
}

func TestResolveCommandOneWord(t *testing.T) {
	key, _, _, ok := resolveCommand([]string{"whoami"})
	if !ok || key != "whoami" {
		t.Fatalf("key = %q ok = %t, want 'whoami'", key, ok)
	}
}

func TestResolveCommandUnknown(t *testing.T) {
	if _, _, _, ok := resolveCommand([]string{"frobnicate", "widget"}); ok {
		t.Fatal("expected no match")
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	var out bytes.Buffer

	if err := run(context.Background(), []string{"--version"}, &out, &out); err != nil {
		t.Fatalf("version: %v", err)
	}

	if strings.TrimSpace(out.String()) != version {
		t.Fatalf("version output = %q, want %q", out.String(), version)
	}

	out.Reset()

	if err := run(context.Background(), []string{"help"}, &out, &out); err != nil {
		t.Fatalf("help: %v", err)
	}

	if !strings.Contains(out.String(), "memory store") {
		t.Fatalf("help should list commands, got %q", out.String())
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out bytes.Buffer

	if err := run(context.Background(), nil, &out, &out); err != nil {
		t.Fatalf("no args: %v", err)
	}

	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage, got %q", out.String())
	}
}

func TestRunUnknownCommandErrors(t *testing.T) {
	var out bytes.Buffer

	err := run(context.Background(), []string{"frobnicate"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunBadLogLevel(t *testing.T) {
	var out bytes.Buffer

	err := run(context.Background(), []string{"whoami", "--log-level", "loud"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "invalid --log-level") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigFromFlagsDefaults(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	registerGlobalFlags(fs)

	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	cfg, err := configFromFlags(fs)
	if err != nil {
		t.Fatalf("configFromFlags: %v", err)
	}

	if cfg.Transport != "grpc" || cfg.Address != "localhost:50051" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestConfigFromFlagsHTTPDefaultAddress(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	registerGlobalFlags(fs)

	if err := fs.Parse([]string{"--transport", "http"}); err != nil {
		t.Fatal(err)
	}

	cfg, err := configFromFlags(fs)
	if err != nil {
		t.Fatalf("configFromFlags: %v", err)
	}

	if cfg.Address != "localhost:8080" {
		t.Fatalf("http default address = %q, want localhost:8080", cfg.Address)
	}
}

func TestConfigFromFlagsEnvOverride(t *testing.T) {
	t.Setenv("HIPPOCAMPUS_TOKEN", "env-token")
	t.Setenv("HIPPOCAMPUS_ADDRESS", "example:9000")

	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	registerGlobalFlags(fs)

	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	cfg, err := configFromFlags(fs)
	if err != nil {
		t.Fatalf("configFromFlags: %v", err)
	}

	if cfg.Token != "env-token" {
		t.Fatalf("token = %q, want env-token", cfg.Token)
	}

	if cfg.Address != "example:9000" {
		t.Fatalf("address = %q, want example:9000", cfg.Address)
	}
}

func TestNewClientUnknownTransport(t *testing.T) {
	if _, _, err := newClient(Config{Transport: "carrier-pigeon"}); err == nil {
		t.Fatal("expected an error for an unknown transport")
	}
}

func TestNewHTTPClientSchemePrefix(t *testing.T) {
	client, _, err := newHTTPClient(Config{Transport: "http", Address: "localhost:8080"})
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	if got := client.(*httpClient).baseURL; got != "http://localhost:8080" {
		t.Fatalf("baseURL = %q, want http://localhost:8080", got)
	}

	tlsClient, _, err := newHTTPClient(Config{Transport: "http", Address: "localhost:8443", TLS: TLSConfig{Enabled: true}})
	if err != nil {
		t.Fatalf("newHTTPClient tls: %v", err)
	}

	if got := tlsClient.(*httpClient).baseURL; got != "https://localhost:8443" {
		t.Fatalf("tls baseURL = %q, want https://localhost:8443", got)
	}
}

func TestNewGRPCClientBuilds(t *testing.T) {
	client, closeClient, err := newGRPCClient(Config{Transport: "grpc", Address: "localhost:50051"})
	if err != nil {
		t.Fatalf("newGRPCClient: %v", err)
	}

	if client == nil {
		t.Fatal("client is nil")
	}

	if err := closeClient(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestTLSMutualRequiresBothCertAndKey(t *testing.T) {
	_, err := tlsClientConfig(TLSConfig{Enabled: true, Cert: "only-cert.pem"})
	if err == nil || !strings.Contains(err.Error(), "mutual TLS requires both") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyLogLevel(t *testing.T) {
	if err := applyLogLevel("debug"); err != nil {
		t.Fatalf("applyLogLevel(debug): %v", err)
	}

	if err := applyLogLevel("nonsense"); err == nil {
		t.Fatal("expected an error for a bad level")
	}
}
