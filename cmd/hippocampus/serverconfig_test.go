package main

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/hippocampus"
)

// serverConfigRead matches a key serverConfig reads, with the accessor that reads it.
var serverConfigRead = regexp.MustCompile(`(viper\.Get[A-Za-z0-9]*|seconds|hours|days)\("([A-Za-z0-9.]+)"\)`)

// TestServerConfigFillsEveryField holds serverConfig to the Config it builds (TODO-3 item 177).
// It is a long list of hand-written assignments, and the failure it can have is silent: a field
// nobody assigns reads as its zero value, which for most of them means "off", so the service starts
// and quietly ignores a setting. So every key serverConfig reads is given a non-zero value - found
// by scanning its source, so a new read is covered without anyone extending a list - and every leaf
// field of the result must then be non-zero.
func TestServerConfigFillsEveryField(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	source, err := os.ReadFile("serverconfig.go")
	if err != nil {
		t.Fatalf("reading serverconfig.go: %s", err)
	}

	for i, match := range serverConfigRead.FindAllStringSubmatch(string(source), -1) {
		accessor, key := match[1], match[2]

		switch accessor {

		case "viper.GetBool":
			viper.Set(key, true)

		case "viper.GetString":
			viper.Set(key, "value-"+key)

		case "viper.GetStringSlice":
			viper.Set(key, []string{"value-" + key})

		case "viper.GetFloat64":
			viper.Set(key, 0.5+float64(i))

		case "viper.Get":
			// transfer.tls, read raw to accept the legacy scalar; the block form is set below.

		default:
			viper.Set(key, i+1)

		}
	}

	// The handful whose value is constrained rather than free.
	viper.Set("storage.driver", "postgres")
	viper.Set("callbacks.backlogPolicy", "retain")
	viper.Set("transfer.tls.enabled", true)
	viper.Set("topology.components", []map[string]any{{"name": "bridge", "kind": "bridge", "healthUrl": "http://bridge:8090"}})

	cfg := serverConfig("idp")

	var zero []string

	walkLeaves(reflect.ValueOf(cfg), "Config", func(path string, v reflect.Value) {
		if v.IsZero() {
			zero = append(zero, path)
		}
	})

	for _, path := range zero {
		t.Errorf("%s is zero although every key serverConfig reads was set: is it assigned?", path)
	}

	// Spot checks on the units, which the walk cannot see: a seconds key read as hours is non-zero
	// either way.
	if want := time.Duration(viper.GetInt("sleep.periodSeconds")) * time.Second; cfg.SleepPeriod != want {
		t.Errorf("SleepPeriod = %s, want %s (sleep.periodSeconds in seconds)", cfg.SleepPeriod, want)
	}

	if want := time.Duration(viper.GetInt("callbacks.maxAgeHours")) * time.Hour; cfg.Callbacks.Bounds.MaxAge != want {
		t.Errorf("Callbacks.Bounds.MaxAge = %s, want %s (callbacks.maxAgeHours in hours)", cfg.Callbacks.Bounds.MaxAge, want)
	}

	if cfg.Deployment.StorageDSN != "value-storage.postgres.dsn" {
		t.Errorf("StorageDSN = %q, want the postgres DSN for the postgres driver", cfg.Deployment.StorageDSN)
	}
}

// walkLeaves calls fn for every non-struct field reachable from v, naming it by its path.
func walkLeaves(v reflect.Value, path string, fn func(string, reflect.Value)) {
	if v.Kind() != reflect.Struct {
		fn(path, v)

		return
	}

	for i := range v.NumField() {
		field := v.Type().Field(i)

		if !field.IsExported() {
			continue
		}

		walkLeaves(v.Field(i), path+"."+field.Name, fn)
	}
}

// TestResolveAuthMethod pins the deprecated boolean's resolution, which moved here from the
// topology view when the hippocampus package stopped reading configuration. auth.enabled true with
// no auth.method is hmac; reporting "none" there would say the instance is open when it is not.
func TestResolveAuthMethod(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	if got := resolveAuthMethod(); got != "none" {
		t.Errorf("with nothing set, resolveAuthMethod = %q, want none", got)
	}

	viper.Set("auth.enabled", true)

	if got := resolveAuthMethod(); got != "hmac" {
		t.Errorf("the deprecated auth.enabled resolves to %q, want hmac", got)
	}

	viper.Set("auth.method", "idp")

	if got := resolveAuthMethod(); got != "idp" {
		t.Errorf("auth.method must win over auth.enabled, got %q", got)
	}
}

// TestServerConfigSurvivesAMalformedComponentList covers the declared-components read.
// configProblems rejects a malformed list at startup, so reaching here means the two disagree - and
// losing the declared half of a diagnostic view is not a reason to refuse to build the server that
// serves the store.
func TestServerConfigSurvivesAMalformedComponentList(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("topology.components", "not a list at all")

	if components := serverConfig("none").Topology.Components; len(components) != 0 {
		t.Errorf("a malformed list produced %d components", len(components))
	}
}

// TestHippocampusReadsNoConfiguration keeps the boundary this file exists for: every key is read in
// package main, and the service package takes a Config. A viper import there would bring back the
// global state that kept its tests from running in parallel, and a key read there would escape
// TestServerConfigFillsEveryField.
func TestHippocampusReadsNoConfiguration(t *testing.T) {
	files, err := os.ReadDir("../../hippocampus")
	if err != nil {
		t.Fatalf("reading the hippocampus package: %s", err)
	}

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".go") {
			continue
		}

		source, err := os.ReadFile("../../hippocampus/" + file.Name())
		if err != nil {
			t.Fatalf("reading %s: %s", file.Name(), err)
		}

		if strings.Contains(string(source), `"github.com/spf13/viper"`) {
			t.Errorf("hippocampus/%s imports viper: read the key in cmd/hippocampus/serverconfig.go and pass it in Config", file.Name())
		}
	}

	_ = hippocampus.Config{}
}
