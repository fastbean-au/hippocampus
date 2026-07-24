// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateDefaultConfig_Valid(t *testing.T) {
	cfg, ok := createDefaultConfig().(*Config)
	require.True(t, ok)

	assert.Equal(t, defaultEndpoint, cfg.Endpoint)
	assert.True(t, cfg.CreateEvents)
	assert.Equal(t, []string{"service.name"}, cfg.EventKeyFrom)
	assert.NoError(t, cfg.Validate())
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{
			name:   "default ok",
			mutate: func(*Config) {},
		},
		{
			name:    "empty endpoint",
			mutate:  func(c *Config) { c.Endpoint = "" },
			wantErr: "endpoint must be set",
		},
		{
			name:    "bad bucket",
			mutate:  func(c *Config) { c.EventBucket = "week" },
			wantErr: "event_bucket must be one of",
		},
		{
			name: "no event key with events",
			mutate: func(c *Config) {
				c.CreateEvents = true
				c.EventKeyFrom = nil
			},
			wantErr: "event_key_from must name at least one",
		},
		{
			name: "no event key ok when events off",
			mutate: func(c *Config) {
				c.CreateEvents = false
				c.EventKeyFrom = nil
			},
		},
		{
			name:    "min above max",
			mutate:  func(c *Config) { c.Significance.Min = 100; c.Significance.Max = 10 },
			wantErr: "must not exceed significance.max",
		},
		{
			name:    "negative jitter",
			mutate:  func(c *Config) { c.Significance.Jitter = -1 },
			wantErr: "significance.jitter must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := createDefaultConfig().(*Config)
			tt.mutate(cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
