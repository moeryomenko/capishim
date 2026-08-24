// Package config defines the static configuration shared by the capishim
// setup container, the quadlet renderer, and the e2e podman driver: the state
// directory, the apiserver bind address, and the per-component spec table.
//
// Load reads configuration from an explicit environment map so callers and
// tests can supply any environment without consulting process-global state,
// which keeps Load safe for parallel use.
package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment variable keys understood by Load and the defaults applied when
// they are unset.
const (
	// EnvStateDir overrides the default state directory.
	EnvStateDir = "CAPISHIM_STATE_DIR"

	// EnvBindAddress overrides the default apiserver bind address.
	EnvBindAddress = "CAPISHIM_BIND_ADDRESS"

	// EnvHypervisorWebhookHost overrides the hostname the rewritten hypervisor
	// provider webhook URLs point at (REQ-006).
	EnvHypervisorWebhookHost = "CAPISHIM_HYPERVISOR_WEBHOOK_HOST"

	// defaultBindAddress is the loopback address the management apiserver
	// publishes on (REQ-010).
	defaultBindAddress = "127.0.0.1:6443"

	// defaultHypervisorWebhookHost is the hostname used when
	// EnvHypervisorWebhookHost is unset or empty (REQ-006, decision D3).
	defaultHypervisorWebhookHost = "host.containers.internal"

	// stateDirSuffix is the path appended below $HOME (REQ-009).
	stateDirSuffix = ".local/share/capishim"

	// kubeconfigsDir is the state-directory subdirectory holding the manager
	// and admin kubeconfigs (REQ-009).
	kubeconfigsDir = "kubeconfigs"

	// minPort and maxPort bound the apiserver bind port.
	minPort = 1
	maxPort = 65535
)

// Config is the runtime configuration for the shim stack.
type Config struct {
	// StateDir holds etcd data, pki material, and kubeconfigs.
	StateDir string
	// BindAddress is the host:port the management apiserver listens on.
	BindAddress string
	// HypervisorWebhookHost is the hostname the rewritten hypervisor provider
	// webhook URLs point at (REQ-006).
	HypervisorWebhookHost string
}

// Load builds a Config from an explicit environment map. The state directory
// defaults to $HOME/.local/share/capishim and is overridable via EnvStateDir;
// the bind address defaults to 127.0.0.1:6443 and is overridable via
// EnvBindAddress; the hypervisor webhook host defaults to
// host.containers.internal and is overridable via EnvHypervisorWebhookHost.
// Load returns an error when HOME is unavailable and EnvStateDir is unset,
// when the bind address is not a valid host:port with a port in [1, 65535],
// or when EnvHypervisorWebhookHost is whitespace-only.
func Load(env map[string]string) (Config, error) {
	stateDir, err := stateDirFromEnv(env)
	if err != nil {
		return Config{}, err
	}
	bind, err := bindAddressFromEnv(env)
	if err != nil {
		return Config{}, err
	}
	host, err := hypervisorWebhookHostFromEnv(env)
	if err != nil {
		return Config{}, err
	}
	return Config{StateDir: stateDir, BindAddress: bind, HypervisorWebhookHost: host}, nil
}

// stateDirFromEnv resolves the state directory from the environment map:
// EnvStateDir wins when set and non-empty, otherwise $HOME is consulted.
func stateDirFromEnv(env map[string]string) (string, error) {
	if raw, ok := env[EnvStateDir]; ok {
		dir := strings.TrimSpace(raw)
		if dir == "" {
			return "", fmt.Errorf("config: %s is set but empty", EnvStateDir)
		}
		return dir, nil
	}
	home := strings.TrimSpace(env["HOME"])
	if home == "" {
		return "", fmt.Errorf("config: HOME is unavailable and %s is unset", EnvStateDir)
	}
	return filepath.Join(home, stateDirSuffix), nil
}

// bindAddressFromEnv resolves the bind address from the environment map:
// EnvBindAddress wins when set and non-empty, otherwise the default is used.
// A non-default value must parse as a valid host:port pair.
func bindAddressFromEnv(env map[string]string) (string, error) {
	bind := defaultBindAddress
	if raw, ok := env[EnvBindAddress]; ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return "", fmt.Errorf("config: %s is set but empty", EnvBindAddress)
		}
		bind = trimmed
		if err := validateBindAddress(bind); err != nil {
			return "", fmt.Errorf("config: invalid %s %q: %w", EnvBindAddress, bind, err)
		}
	}
	return bind, nil
}

// hypervisorWebhookHostFromEnv resolves the hypervisor webhook host from the
// environment map: EnvHypervisorWebhookHost wins when set to a non-empty
// value, with surrounding whitespace trimmed; unset and empty values select
// the default host.containers.internal (REQ-006). A whitespace-only value is
// an error.
func hypervisorWebhookHostFromEnv(env map[string]string) (string, error) {
	raw, ok := env[EnvHypervisorWebhookHost]
	if !ok || raw == "" {
		return defaultHypervisorWebhookHost, nil
	}
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("config: %s is set but whitespace-only", EnvHypervisorWebhookHost)
	}
	return host, nil
}

// validateBindAddress checks that addr is a host:port pair with a non-empty
// host and a port in [1, 65535].
func validateBindAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("split host:port: %w", err)
	}
	if host == "" {
		return errors.New("missing host")
	}
	if strings.ContainsAny(host, " \t") {
		return fmt.Errorf("invalid host %q", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("invalid port %q", port)
	}
	if n < minPort || n > maxPort {
		return fmt.Errorf("port %d out of range [%d, %d]", n, minPort, maxPort)
	}
	return nil
}

// KubeconfigPath returns the absolute path to the manager kubeconfig for the
// component with the given ID. Only the four provider managers have
// kubeconfigs; the bool is false for any other ID.
func (c Config) KubeconfigPath(id ComponentID) (string, bool) {
	spec, ok := Component(id)
	if !ok || spec.Kubeconfig == "" {
		return "", false
	}
	return filepath.Join(c.StateDir, spec.Kubeconfig), true
}

// AdminKubeconfigPath returns the path to the admin kubeconfig written by the
// setup container.
func (c Config) AdminKubeconfigPath() string {
	return filepath.Join(c.StateDir, kubeconfigsDir, "admin.kubeconfig")
}
