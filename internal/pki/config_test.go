package pki_test

import (
	"net"
	"os"
	"strings"
	"testing"

	"github.com/moeryomenko/capishim/internal/pki"
)

// TestStateDirIsInjectable asserts every artifact path lives under
// <state-dir>/pki/ with no hardcoded home-directory default.
func TestStateDirIsInjectable(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	prefix := pkiDir(stateDir) + string(os.PathSeparator)
	for _, a := range inv.All() {
		for _, path := range []string{a.CertPath, a.KeyPath} {
			if !strings.HasPrefix(path, prefix) {
				t.Errorf("artifact %s path %q outside %q", a.Name, path, prefix)
			}
		}
	}
	for _, path := range []string{inv.SAPubPath, inv.SAKeyPath} {
		if !strings.HasPrefix(path, prefix) {
			t.Errorf("SA path %q outside %q", path, prefix)
		}
	}
}

// TestBindAddressEmptyDefaultsLoopback asserts an empty bind address is
// tolerated and produces the loopback SAN set (REQ-010 default 127.0.0.1).
func TestBindAddressEmptyDefaultsLoopback(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, "")

	cert := loadCert(t, inv.APIServer.CertPath)
	assertDNSName(t, cert, "localhost")
	assertIP(t, cert, net.ParseIP("127.0.0.1"))
}

// TestBindAddressMalformedErrors asserts an unparseable bind address (no port
// or an empty host) is rejected rather than silently producing wrong SANs.
func TestBindAddressMalformedErrors(t *testing.T) {
	t.Parallel()

	tests := []string{
		"127.0.0.1",
		"capishim.test",
		"6443",
		":6443",
	}
	for _, bind := range tests {
		t.Run(bind, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			_, err := pki.Generate(t.Context(), pki.Config{StateDir: stateDir, BindAddress: bind})
			if err == nil {
				t.Errorf("Generate with bind address %q succeeded; want error", bind)
			}
		})
	}
}
