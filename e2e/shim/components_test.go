// e2e podman driver tests for the external hypervisor component (TASK-008 red
// phase). REQ-004 requires the driver to stay in-pod-only: components() must
// not grow a hypervisor container (create() builds exactly the components()
// set, and boot() starts a fixed subset of those ids), while ensureStateDirs
// must pre-create <state>/webhook-certs/hypervisor so the CAPH quadlet's
// --webhook-cert-dir mount source exists before first boot (REQ-005).
//
// TestComponentsOmitHypervisorContainer is a green-by-construction guard
// today: it pins the exclusion invariant B3 must preserve rather than proving
// new behavior. TestEnsureStateDirsCreatesHypervisorWebhookCertsDir is the
// runtime-red case: the webhook-certs directory is not created yet.
package shim

import (
	"os"
	"path/filepath"
	"testing"
)

// TestComponentsOmitHypervisorContainer pins the container-set contract: the
// driver creates exactly the eight in-pod containers and never a hypervisor
// container. The external manager is booted by its own CAPH quadlet unit
// (REQ-007); a hypervisor entry here would make create() build and boot() a
// second, broken provider container inside the pod.
func TestComponentsOmitHypervisorContainer(t *testing.T) {
	t.Parallel()

	want := []string{"pki", "etcd", "apiserver", "setup", "core", "cabpk", "kcp", "capd"}
	got := components("/srv/capishim")

	if len(got) != len(want) {
		t.Fatalf("components(stateDir) returned %d entries (%v), want %d: %v", len(got), idsOf(got), len(want), want)
	}
	seen := make(map[string]bool, len(got))
	for i, c := range got {
		if c.id != want[i] {
			t.Errorf("components(stateDir)[%d].id = %q, want %q (boot order is locked)", i, c.id, want[i])
		}
		seen[c.id] = true
	}
	if seen["hypervisor"] {
		t.Error(
			"components(stateDir) includes a hypervisor container; the external manager must not run inside the capishim pod (REQ-004)",
		)
	}
}

// TestEnsureStateDirsCreatesHypervisorWebhookCertsDir verifies the state-dir
// seam the CAPH quadlet depends on: ensureStateDirs pre-creates
// <state>/webhook-certs/hypervisor (the --webhook-cert-dir mount source,
// REQ-005) alongside every directory it already created.
func TestEnsureStateDirsCreatesHypervisorWebhookCertsDir(t *testing.T) {
	t.Parallel()

	p := &ClusterProvider{stateDir: t.TempDir()}
	if err := p.ensureStateDirs(); err != nil {
		t.Fatalf("ensureStateDirs() error = %v", err)
	}

	hypervisorDir := filepath.Join(p.stateDir, "webhook-certs", "hypervisor")
	info, err := os.Stat(hypervisorDir)
	if err != nil {
		t.Fatalf("ensureStateDirs() did not create %s: %v (REQ-005)", hypervisorDir, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", hypervisorDir)
	}

	// Existing mount sources must keep being created.
	for _, rel := range []string{
		"pki",
		"etcd",
		"kubeconfigs",
		"abac",
		filepath.Join("pki", "core-webhook"),
		filepath.Join("pki", "capd-webhook"),
	} {
		if _, err := os.Stat(filepath.Join(p.stateDir, rel)); err != nil {
			t.Errorf("ensureStateDirs() dropped existing state directory %s: %v", rel, err)
		}
	}
}

// TestPKIAndSetupMountWebhookCertsDir pins the REQ-005 mount contract for the
// two containers that run pki.Generate: both the pki and setup components
// bind-mount <state>/webhook-certs at the same path so the hypervisor webhook
// serving pair (<state>/webhook-certs/hypervisor/{tls.crt,tls.key}) lands on
// the host instead of the container's ephemeral overlay, and survives reboots
// for reuse-on-restart.
func TestPKIAndSetupMountWebhookCertsDir(t *testing.T) {
	t.Parallel()

	const stateDir = "/srv/capishim"
	want := stateDir + "/webhook-certs"
	for _, id := range []string{"pki", "setup"} {
		var mounted, readOnly bool
		for _, c := range components(stateDir) {
			if c.id != id {
				continue
			}
			for _, v := range c.volumes {
				if v.hostPath == want {
					mounted = true
					readOnly = v.readOnly
				}
			}
		}
		if !mounted {
			t.Errorf(
				"%s component does not mount %s; the hypervisor webhook serving pair would be written to the ephemeral overlay (REQ-005)",
				id,
				want,
			)
			continue
		}
		if readOnly {
			t.Errorf("%s mounts %s read-only; pki.Generate must create the serving pair when absent", id, want)
		}
	}
}

// idsOf returns the component ids in order, for failure messages.
func idsOf(cs []component) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.id)
	}
	return out
}
