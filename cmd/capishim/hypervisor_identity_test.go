// External hypervisor identity tests (TASK-008 red phase). REQ-004 folds the
// TASK-005 overlay constants into the component table as the single source of
// truth: ManagerCNByNamespace must carry hypervisor-system ->
// capishim:hypervisor-manager because the table entry exists, and setup's
// kubeconfig loop must find a manager client certificate for the hypervisor
// component so <state>/kubeconfigs/hypervisor.kubeconfig authenticates as CN
// capishim:hypervisor-manager against the loopback apiserver URL (same
// BuildKubeconfig path as the four in-pod managers).
//
// These tests compile against today's tree and fail at runtime until TASK-009
// lands the ComponentHypervisor entry and its ManagerArtifact mapping; that
// runtime failure is the red phase.
package main_test

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	capishim "github.com/moeryomenko/capishim/cmd/capishim"
	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/pki"
)

// TestManagerCNByNamespaceDerivedFromComponentTable asserts the RBAC rewrite
// map covers hypervisor-system from the component table entry itself. The
// failing condition is the absent Components() entry: while only the TASK-005
// overlay satisfies the mapping the single-source-of-truth requirement
// (REQ-004) is unmet, even though the map value would already be correct.
func TestManagerCNByNamespaceDerivedFromComponentTable(t *testing.T) {
	t.Parallel()
	spec, ok := config.Component(config.ComponentID("hypervisor"))
	if !ok {
		t.Fatalf("config.Components() has no hypervisor entry: ManagerCNByNamespace() must derive hypervisor-system from the table (REQ-004 single source of truth), not from the pinned overlay constants alone")
	}

	got := capishim.ManagerCNByNamespace()
	const wantCN = "capishim:hypervisor-manager"
	if got["hypervisor-system"] != wantCN {
		t.Errorf("ManagerCNByNamespace()[%q] = %q, want %q", "hypervisor-system", got["hypervisor-system"], wantCN)
	}
	if got[spec.ProviderNamespace] != spec.ManagerCN {
		t.Errorf("ManagerCNByNamespace()[%q] = %q, want the table value %q", spec.ProviderNamespace, got[spec.ProviderNamespace], spec.ManagerCN)
	}
}

// TestManagerArtifactHypervisorManager verifies setup can resolve the client
// certificate for the hypervisor component the same way it does for the four
// in-pod managers: ManagerArtifact returns the pair minted under the shared
// <id>-manager naming, and that certificate carries the CN the kubeconfig and
// the RBAC rewrite both rely on (REQ-004, REQ-005 prerequisite).
func TestManagerArtifactHypervisorManager(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv, err := pki.Generate(t.Context(), pki.Config{StateDir: stateDir, BindAddress: "127.0.0.1:6443"})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	artifact, ok := capishim.ManagerArtifact(config.ComponentID("hypervisor"), inv)
	if !ok {
		t.Fatalf("ManagerArtifact(%q) reported not found: setup cannot write the hypervisor manager kubeconfig without its client certificate (REQ-004)", "hypervisor")
	}

	wantCert := filepath.Join(stateDir, "pki", "hypervisor-manager.crt")
	wantKey := filepath.Join(stateDir, "pki", "hypervisor-manager.key")
	if artifact.CertPath != wantCert {
		t.Errorf("ManagerArtifact(hypervisor).CertPath = %q, want %q", artifact.CertPath, wantCert)
	}
	if artifact.KeyPath != wantKey {
		t.Errorf("ManagerArtifact(hypervisor).KeyPath = %q, want %q", artifact.KeyPath, wantKey)
	}
	for _, path := range []string{artifact.CertPath, artifact.KeyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("artifact file %s: %v", path, err)
		}
	}

	data, err := os.ReadFile(artifact.CertPath)
	if err != nil {
		t.Fatalf("read %s: %v", artifact.CertPath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM block in %s", artifact.CertPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate from %s: %v", artifact.CertPath, err)
	}
	if got := cert.Subject.CommonName; got != "capishim:hypervisor-manager" {
		t.Errorf("manager certificate CN = %q, want %q", got, "capishim:hypervisor-manager")
	}
}
