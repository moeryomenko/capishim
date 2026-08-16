// Package config_test contains the red-phase tests for the internal/config API
// designed in TASK-002 (test-first). These tests lock the contract that
// TASK-003 must implement:
//
//   - Defaults from REQ-009/REQ-010: state dir ~/.local/share/capishim (via
//     HOME), bind address 127.0.0.1:6443.
//   - Environment overrides CAPISHIM_STATE_DIR and CAPISHIM_BIND_ADDRESS, with
//     invalid values rejected.
//   - Load takes an explicit env map (no os.Getenv, no global state) so tests
//     are parallel-safe and unit-testable without a filesystem or network.
//
// Until TASK-003 lands, the import below does not resolve and `go test
// ./internal/config` fails to compile. That failure is the red phase.
package config_test

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/capishim/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(map[string]string{"HOME": "/home/testuser"})
	if err != nil {
		t.Fatalf("Load with HOME and no overrides returned error: %v", err)
	}
	if got, want := cfg.StateDir, filepath.Join("/home/testuser", ".local", "share", "capishim"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
	if got, want := cfg.BindAddress, "127.0.0.1:6443"; got != want {
		t.Errorf("BindAddress = %q, want %q", got, want)
	}
}

func TestLoadHomeUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "nil env", env: nil},
		{name: "HOME key absent", env: map[string]string{config.EnvBindAddress: "0.0.0.0:6443"}},
		{name: "HOME empty", env: map[string]string{"HOME": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Load(tt.env); err == nil {
				t.Errorf("Load(%v) returned no error, want error when HOME is unavailable and CAPISHIM_STATE_DIR is unset", tt.env)
			}
		})
	}
}

func TestLoadStateDirOverride(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "absolute override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvStateDir: "/tmp/custom"},
			want: "/tmp/custom",
		},
		{
			name: "override wins over HOME-derived default",
			env:  map[string]string{"HOME": "/home/other", config.EnvStateDir: "/srv/state"},
			want: "/srv/state",
		},
		{
			name: "override works without HOME",
			env:  map[string]string{config.EnvStateDir: "/tmp/nohome"},
			want: "/tmp/nohome",
		},
		{
			name: "relative value preserved as-is",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvStateDir: "var/lib/capishim"},
			want: "var/lib/capishim",
		},
		{
			name: "surrounding whitespace trimmed",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvStateDir: "  /tmp/ws  "},
			want: "/tmp/ws",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(tt.env)
			if err != nil {
				t.Fatalf("Load(%v) returned error: %v", tt.env, err)
			}
			if cfg.StateDir != tt.want {
				t.Errorf("StateDir = %q, want %q", cfg.StateDir, tt.want)
			}
		})
	}
}

func TestLoadStateDirInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "empty override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvStateDir: ""},
		},
		{
			name: "whitespace-only override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvStateDir: "   \t  "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Load(tt.env); err == nil {
				t.Errorf("Load(%v) returned no error, want error for invalid %s", tt.env, config.EnvStateDir)
			}
		})
	}
}

func TestLoadBindAddressValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "IPv4 override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "0.0.0.0:6443"},
			want: "0.0.0.0:6443",
		},
		{
			name: "loopback IPv6 override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "[::1]:6443"},
			want: "[::1]:6443",
		},
		{
			name: "wildcard IPv6 override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "[::]:6443"},
			want: "[::]:6443",
		},
		{
			name: "hostname override",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "localhost:6443"},
			want: "localhost:6443",
		},
		{
			name: "minimum port boundary",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:1"},
			want: "127.0.0.1:1",
		},
		{
			name: "maximum port boundary",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:65535"},
			want: "127.0.0.1:65535",
		},
		{
			name: "surrounding whitespace trimmed",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "  127.0.0.1:6443  "},
			want: "127.0.0.1:6443",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(tt.env)
			if err != nil {
				t.Fatalf("Load(%v) returned error: %v", tt.env, err)
			}
			if cfg.BindAddress != tt.want {
				t.Errorf("BindAddress = %q, want %q", cfg.BindAddress, tt.want)
			}
			host, port, err := net.SplitHostPort(cfg.BindAddress)
			if err != nil {
				t.Fatalf("stored BindAddress %q does not parse as host:port: %v", cfg.BindAddress, err)
			}
			if host == "" {
				t.Errorf("stored BindAddress %q has empty host", cfg.BindAddress)
			}
			if port == "0" {
				t.Errorf("stored BindAddress %q has port 0", cfg.BindAddress)
			}
		})
	}
}

func TestLoadBindAddressInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "empty value",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: ""},
		},
		{
			name: "whitespace only",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "   "},
		},
		{
			name: "missing port",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1"},
		},
		{
			name: "non-numeric port",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:abc"},
		},
		{
			name: "port out of range high",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:65536"},
		},
		{
			name: "port zero",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:0"},
		},
		{
			name: "negative port",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:-1"},
		},
		{
			name: "missing host",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: ":6443"},
		},
		{
			name: "unbracketed IPv6",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "::1:6443"},
		},
		{
			name: "too many colons",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1:6443:80"},
		},
		{
			name: "whitespace inside value",
			env:  map[string]string{"HOME": "/home/testuser", config.EnvBindAddress: "127.0.0.1 :6443"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Load(tt.env); err == nil {
				t.Errorf("Load(%v) returned no error, want error for invalid %s", tt.env, config.EnvBindAddress)
			}
		})
	}
}
