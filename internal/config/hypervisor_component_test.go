// Hypervisor component-spec tests (TASK-008 red phase). REQ-004 models the
// external hypervisor provider manager as a ComponentHypervisor entry in the
// component table carrying a new External boolean field: identity material
// (manager client certificate, kubeconfig) and RBAC subject rewriting derive
// from the table entry alone, while the quadlet renderer and the e2e podman
// driver must skip it. Until TASK-009 adds the constant and the field this
// package fails to compile ("undefined: config.ComponentHypervisor",
// "spec.External undefined"); that build failure is the red phase.
//
// Image choice (pinned deliberately): the empty string. The hypervisor
// manager runs from CAPH's own quadlet unit (REQ-007), not from a
// capishim-built image, and the renderer skips External specs so no Image=
// line is ever derived from this spec. An empty value makes the "no container
// of our own" nature explicit at the type level.
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/moeryomenko/capishim/internal/config"
)

// TestComponentHypervisorSpec locks every REQ-004 field of the external
// hypervisor component entry.
func TestComponentHypervisorSpec(t *testing.T) {
	t.Parallel()
	spec, ok := config.Component(config.ComponentHypervisor)
	if !ok {
		t.Fatalf("Component(%q) not found: Components() must expose the external hypervisor manager entry (REQ-004)", config.ComponentHypervisor)
	}
	if spec.ID != config.ComponentHypervisor {
		t.Errorf("spec.ID = %q, want %q", spec.ID, config.ComponentHypervisor)
	}
	if !spec.External {
		t.Errorf("spec.External = false, want true (REQ-004: the hypervisor manager runs outside the pod)")
	}
	if spec.WebhookPort != 9443 {
		t.Errorf("spec.WebhookPort = %d, want 9443 (REQ-004)", spec.WebhookPort)
	}
	if spec.ProviderNamespace != "hypervisor-system" {
		t.Errorf("spec.ProviderNamespace = %q, want %q (REQ-004)", spec.ProviderNamespace, "hypervisor-system")
	}
	if spec.ManagerCN != "capishim:hypervisor-manager" {
		t.Errorf("spec.ManagerCN = %q, want %q (REQ-004)", spec.ManagerCN, "capishim:hypervisor-manager")
	}
	if spec.Kubeconfig != "kubeconfigs/hypervisor.kubeconfig" {
		t.Errorf("spec.Kubeconfig = %q, want %q (REQ-004)", spec.Kubeconfig, "kubeconfigs/hypervisor.kubeconfig")
	}
	if spec.Image != "" {
		t.Errorf("spec.Image = %q, want an empty reference: the external manager is booted by the CAPH quadlet (REQ-007), not by a capishim image", spec.Image)
	}
}

// TestComponentHypervisorIsSoleExternalSpec guards against accidental
// duplication: exactly one component in the table may carry External=true,
// and it must be the hypervisor manager.
func TestComponentHypervisorIsSoleExternalSpec(t *testing.T) {
	t.Parallel()
	var external []config.ComponentID
	for _, spec := range config.Components() {
		if spec.External {
			external = append(external, spec.ID)
		}
	}
	if len(external) != 1 || external[0] != config.ComponentHypervisor {
		t.Errorf("Components() external specs = %v, want exactly [%s]", external, config.ComponentHypervisor)
	}
}

// TestConfigKubeconfigPathHypervisor verifies the kubeconfig seam setup uses
// when writing <state>/kubeconfigs/hypervisor.kubeconfig (REQ-004 consequence,
// REQ-005 prerequisite): the entry must resolve like the four in-pod managers.
func TestConfigKubeconfigPathHypervisor(t *testing.T) {
	t.Parallel()
	cfg := config.Config{StateDir: "/srv/capishim"}
	got, ok := cfg.KubeconfigPath(config.ComponentHypervisor)
	want := filepath.Join("/srv/capishim", "kubeconfigs", "hypervisor.kubeconfig")
	if !ok {
		t.Fatalf("KubeconfigPath(%q) reported ok=false, want the hypervisor kubeconfig path %q", config.ComponentHypervisor, want)
	}
	if got != want {
		t.Errorf("KubeconfigPath(%q) = %q, want %q", config.ComponentHypervisor, got, want)
	}
}
