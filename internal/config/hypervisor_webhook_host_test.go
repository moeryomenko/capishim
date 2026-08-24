// Package config_test — TASK-006 red-phase tests for REQ-006: the
// CAPISHIM_HYPERVISOR_WEBHOOK_HOST environment key selects the hostname the
// rewritten hypervisor webhook URLs use. Unset and empty values select the
// default host.containers.internal; a whitespace-only value is an error at
// Load; any other value is trimmed and carried verbatim.
//
// Pinned seam for the implementer (TASK-006): internal/config exposes
//
//	const EnvHypervisorWebhookHost = "CAPISHIM_HYPERVISOR_WEBHOOK_HOST"
//
// and Load populates a new Config field:
//
//	type Config struct {
//		StateDir              string
//		BindAddress           string
//		HypervisorWebhookHost string // default host.containers.internal
//	}
//
// Until the seam exists this file fails to compile; that failure is the red
// phase evidence.
package config_test

import (
	"strings"
	"testing"

	"github.com/moeryomenko/capishim/internal/config"
)

// defaultHypervisorWebhookHost is the REQ-006 default, pinned by spec decision
// D3 and VC-01.
const defaultHypervisorWebhookHost = "host.containers.internal"

func TestLoadHypervisorWebhookHostDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "key unset", env: map[string]string{"HOME": "/home/testuser"}},
		{name: "key set to empty string selects default", env: map[string]string{
			"HOME":                          "/home/testuser",
			config.EnvHypervisorWebhookHost: "",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(tt.env)
			if err != nil {
				t.Fatalf("Load(%v) returned error: %v", tt.env, err)
			}
			if cfg.HypervisorWebhookHost != defaultHypervisorWebhookHost {
				t.Errorf("HypervisorWebhookHost = %q, want default %q",
					cfg.HypervisorWebhookHost, defaultHypervisorWebhookHost)
			}
		})
	}
}

func TestLoadHypervisorWebhookHostOverride(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		give string
		want string
	}{
		{name: "plain value carried through", give: "webhook.lab.local", want: "webhook.lab.local"},
		{
			// Assumption (documented in the test manifest): surrounding
			// whitespace is trimmed, mirroring every other env resolution in
			// this package; only an all-whitespace value is an error.
			name: "surrounding whitespace trimmed",
			give: "  webhook.lab.local  ",
			want: "webhook.lab.local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(map[string]string{
				"HOME":                          "/home/testuser",
				config.EnvHypervisorWebhookHost: tt.give,
			})
			if err != nil {
				t.Fatalf("Load with %s=%q returned error: %v", config.EnvHypervisorWebhookHost, tt.give, err)
			}
			if cfg.HypervisorWebhookHost != tt.want {
				t.Errorf("HypervisorWebhookHost = %q, want %q", cfg.HypervisorWebhookHost, tt.want)
			}
		})
	}
}

func TestLoadHypervisorWebhookHostWhitespaceOnlyError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		give string
	}{
		{name: "spaces", give: "   "},
		{name: "tabs and newlines", give: "\t\n  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(map[string]string{
				"HOME":                          "/home/testuser",
				config.EnvHypervisorWebhookHost: tt.give,
			})
			if err == nil {
				t.Errorf("Load with whitespace-only %s returned no error, want error", config.EnvHypervisorWebhookHost)
			} else if !strings.Contains(err.Error(), config.EnvHypervisorWebhookHost) {
				t.Errorf("error = %v, want it to name %s", err, config.EnvHypervisorWebhookHost)
			}
		})
	}
}
