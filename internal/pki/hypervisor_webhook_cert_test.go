// External hypervisor webhook certificate tests (TASK-008 red phase). REQ-005
// requires the pki flow to mint a TLS serving certificate and key for the
// external provider webhook server at <state>/webhook-certs/hypervisor/
// (outside the pki directory), signed by the pod CA, with SANs covering the
// loopback addresses plus the configured override host (REQ-006 value), a
// 0600 key, and the same reuse-on-restart semantics as every other artifact.
// REQ-004 additionally has the pki container mint the external manager's
// client certificate under the usual <id>-manager naming.
//
// The override-host cases thread config.Config.HypervisorWebhookHost through
// a new pki.Config.HypervisorWebhookHost seam. Until TASK-009 adds that field
// this package fails to compile ("unknown field HypervisorWebhookHost in
// struct literal of type pki.Config"); that build failure is the red phase,
// and it also blocks the runtime-red default-host and reuse cases below.
package pki_test

import (
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/moeryomenko/capishim/internal/pki"
)

// defaultWebhookDNS and defaultWebhookIPs are the REQ-005 baseline SANs: the
// names a webhook client can use to reach the provider from inside the podman
// network namespace.
var (
	defaultWebhookDNS = []string{"localhost", "host.containers.internal"}
	defaultWebhookIPs = []net.IP{net.ParseIP("127.0.0.1")}
)

// generateWithHost runs pki.Generate with an explicit hypervisor webhook host
// (the REQ-006 override threaded through pki.Config).
func generateWithHost(t *testing.T, stateDir, bindAddress, webhookHost string) *pki.Inventory {
	t.Helper()
	inv, err := pki.Generate(t.Context(), pki.Config{
		StateDir:              stateDir,
		BindAddress:           bindAddress,
		HypervisorWebhookHost: webhookHost,
	})
	if err != nil {
		t.Fatalf("Generate(HypervisorWebhookHost=%q) error = %v", webhookHost, err)
	}
	return inv
}

// hypervisorWebhookCertsDir is the state-relative directory holding the
// external provider's serving pair (REQ-005).
func hypervisorWebhookCertsDir(stateDir string) string {
	return filepath.Join(stateDir, "webhook-certs", "hypervisor")
}

// assertSANIncludesDNS and assertSANIncludesIP fail unless the certificate
// carries the given DNS name / IP subject alternative name.
func assertSANIncludesDNS(t *testing.T, cert *x509.Certificate, want string) {
	t.Helper()
	if !slices.Contains(cert.DNSNames, want) {
		t.Errorf("certificate DNS SANs = %v, want them to include %q", cert.DNSNames, want)
	}
}

func assertSANIncludesIP(t *testing.T, cert *x509.Certificate, want net.IP) {
	t.Helper()
	for _, got := range cert.IPAddresses {
		if got.Equal(want) {
			return
		}
	}
	t.Errorf("certificate IP SANs = %v, want them to include %s", cert.IPAddresses, want)
}

// TestGenerateHypervisorWebhookCertDefaultSANs verifies the serving pair at
// <state>/webhook-certs/hypervisor/{tls.crt,tls.key}: created if absent,
// signed by the pod CA for server auth, carrying the default SAN set
// (localhost, host.containers.internal, 127.0.0.1), with the key at mode
// 0600 (REQ-005). With no override configured the SANs are exactly the
// defaults.
func TestGenerateHypervisorWebhookCertDefaultSANs(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	certPath := filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.crt")
	keyPath := filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.key")
	for _, path := range []string{certPath, keyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("external webhook serving material %s missing: %v (REQ-005)", path, err)
		}
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat %s: %v", keyPath, err)
	}
	if got := keyInfo.Mode().Perm(); got != keyPerm {
		t.Errorf("mode of %s = %o, want %o (REQ-005)", keyPath, got, keyPerm)
	}

	verifyLeaf(t, certPath, inv.CA.CertPath, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	cert := loadCert(t, certPath)
	for _, dns := range defaultWebhookDNS {
		assertSANIncludesDNS(t, cert, dns)
	}
	for _, ip := range defaultWebhookIPs {
		assertSANIncludesIP(t, cert, ip)
	}
	if len(cert.DNSNames) != len(defaultWebhookDNS) {
		t.Errorf("certificate DNS SANs = %v, want exactly %v when no override host is configured", cert.DNSNames, defaultWebhookDNS)
	}
	if len(cert.IPAddresses) != len(defaultWebhookIPs) {
		t.Errorf("certificate IP SANs = %v, want exactly %v when no override host is configured", cert.IPAddresses, defaultWebhookIPs)
	}
	assertKeyMatchesCert(t, certPath, keyPath)
}

// TestGenerateHypervisorWebhookCertOverrideHostDNS verifies that a DNS-name
// override host joins the SAN set alongside the defaults (REQ-005: "plus
// DNS/IP of the configured override host").
func TestGenerateHypervisorWebhookCertOverrideHostDNS(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generateWithHost(t, stateDir, defaultBind, "webhook.lab.local")

	cert := loadCert(t, filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.crt"))
	assertSANIncludesDNS(t, cert, "webhook.lab.local")
	for _, dns := range defaultWebhookDNS {
		assertSANIncludesDNS(t, cert, dns)
	}
	for _, ip := range defaultWebhookIPs {
		assertSANIncludesIP(t, cert, ip)
	}
}

// TestGenerateHypervisorWebhookCertOverrideHostIP verifies that an IP-literal
// override host becomes an IP SAN alongside the defaults (REQ-005).
func TestGenerateHypervisorWebhookCertOverrideHostIP(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generateWithHost(t, stateDir, defaultBind, "192.0.2.10")

	cert := loadCert(t, filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.crt"))
	assertSANIncludesIP(t, cert, net.ParseIP("192.0.2.10"))
	for _, dns := range defaultWebhookDNS {
		assertSANIncludesDNS(t, cert, dns)
	}
	for _, ip := range defaultWebhookIPs {
		assertSANIncludesIP(t, cert, ip)
	}
}

// TestGenerateHypervisorWebhookCertOverrideHostEqualsDefault verifies the
// boundary where the override equals the default: the SAN set stays exactly
// the defaults with no duplicated entries.
func TestGenerateHypervisorWebhookCertOverrideHostEqualsDefault(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generateWithHost(t, stateDir, defaultBind, "host.containers.internal")

	cert := loadCert(t, filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.crt"))
	for _, dns := range defaultWebhookDNS {
		assertSANIncludesDNS(t, cert, dns)
		count := 0
		for _, got := range cert.DNSNames {
			if got == dns {
				count++
			}
		}
		if count != 1 {
			t.Errorf("certificate DNS SANs = %v, want exactly one %q entry", cert.DNSNames, dns)
		}
	}
	if len(cert.DNSNames) != len(defaultWebhookDNS) {
		t.Errorf("certificate DNS SANs = %v, want exactly %v when the override equals the default", cert.DNSNames, defaultWebhookDNS)
	}
	if len(cert.IPAddresses) != len(defaultWebhookIPs) {
		t.Errorf("certificate IP SANs = %v, want exactly %v when the override equals the default", cert.IPAddresses, defaultWebhookIPs)
	}
}

// TestGenerateHypervisorWebhookCertReuseOnRestart mirrors the pki reuse rule:
// a second generation over the same state dir leaves the existing valid
// serving pair byte-identical (REQ-005).
func TestGenerateHypervisorWebhookCertReuseOnRestart(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generate(t, stateDir, defaultBind)
	certsRoot := filepath.Join(stateDir, "webhook-certs")
	before := snapshotDir(t, certsRoot)

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, certsRoot)

	assertSnapshotsMatch(t, before, after)
}

// TestGenerateHypervisorWebhookCertRegeneratesBrokenPair mirrors the leaf
// repair rule: a half-present serving pair (key deleted) is regenerated as a
// consistent pair while every other file under webhook-certs stays
// byte-identical (REQ-005 reuse semantics).
func TestGenerateHypervisorWebhookCertRegeneratesBrokenPair(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generate(t, stateDir, defaultBind)
	certsRoot := filepath.Join(stateDir, "webhook-certs")
	before := snapshotDir(t, certsRoot)

	keyPath := filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove external webhook key: %v", err)
	}

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, certsRoot)

	assertSnapshotsMatch(t, before, after, filepath.Join("hypervisor", "tls.crt"), filepath.Join("hypervisor", "tls.key"))
	certPath := filepath.Join(hypervisorWebhookCertsDir(stateDir), "tls.crt")
	assertKeyMatchesCert(t, certPath, keyPath)
}

// TestGenerateHypervisorManagerClientCert verifies REQ-004's consequence that
// the pki container mints the external manager's client certificate under the
// shared <id>-manager naming, with CN capishim:hypervisor-manager so the
// kubeconfig written by setup authenticates as the identity the RBAC rewrite
// binds (REQ-005 prerequisite).
func TestGenerateHypervisorManagerClientCert(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	certPath := filepath.Join(pkiDir(stateDir), "hypervisor-manager.crt")
	keyPath := filepath.Join(pkiDir(stateDir), "hypervisor-manager.key")
	for _, path := range []string{certPath, keyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("hypervisor manager client material %s missing: %v (REQ-004)", path, err)
		}
	}

	cert := loadCert(t, certPath)
	if got := cert.Subject.CommonName; got != "capishim:hypervisor-manager" {
		t.Errorf("manager certificate CN = %q, want %q", got, "capishim:hypervisor-manager")
	}
	verifyLeaf(t, certPath, inv.CA.CertPath, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	assertKeyMatchesCert(t, certPath, keyPath)
}
