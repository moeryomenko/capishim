// Package hack hosts integration tests for the installer scripts in hack/.
// install_quadlet_test.go exercises hack/install-quadlet.sh end-to-end against
// an isolated HOME and CAPISHIM_STATE_DIR (REQ-001, REQ-004, REQ-009, REQ-011,
// VC-01).
//
// The script is run for real: it builds the renderer binary (go build
// ./cmd/capishim), renders and installs the quadlet units into the temporary
// HOME, and calls systemctl --user daemon-reload and loginctl enable-linger
// against the real user session (both are harmless and idempotent). The Go
// build/module caches of the invoking user are passed through so the script's
// go build reuses them instead of rebuilding the module graph into the
// temporary HOME.
//
// Background: the e2e driver (e2e/shim/cluster_provider.go) pre-creates the
// state subdirectories the quadlet containers bind-mount and writes the ABAC
// bootstrap policy itself, so the e2e path masked the installer defect: a
// host that only ran `make install-quadlet` (no e2e driver) booted a pod whose
// etcd container failed with "statfs <state>/etcd: no such file or directory"
// because install-quadlet.sh never created <state>/etcd, <state>/pki,
// <state>/kubeconfigs, <state>/abac, or the per-manager webhook cert dirs.
// These tests pin the installer to the same contract the e2e driver enforces.
package hack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wantPolicy is the exact bootstrap ABAC policy the e2e driver writes
// (e2e/shim/cluster_provider.go writeABACPolicy): the apiserver unit mounts
// --authorization-policy-file=<state>/abac/policy.json, so the installer must
// produce the file (with its trailing newline) before the stack boots
// (REQ-004, VC-01).
const wantPolicy = `{"apiVersion":"abac.authorization.kubernetes.io/v1beta1","kind":"Policy","spec":{"user":"capishim:admin","namespace":"*","resource":"*","apiGroup":"*","nonResourcePath":"*"}}` + "\n"

// wantStateDirs are the state subdirectories the quadlet containers bind-mount
// (REQ-009), mirroring e2e/shim/cluster_provider.go ensureStateDirs: etcd
// data, pki certs, manager kubeconfigs, the ABAC policy dir, and one webhook
// cert dir per provider manager.
var wantStateDirs = []string{
	"pki",
	"etcd",
	"kubeconfigs",
	"abac",
	filepath.Join("pki", "core-webhook"),
	filepath.Join("pki", "cabpk-webhook"),
	filepath.Join("pki", "kcp-webhook"),
	filepath.Join("pki", "capd-webhook"),
}

// repoRoot resolves the repository root from the package working directory
// (go test runs with the package dir as the working directory).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	root, err := filepath.Abs(filepath.Join(wd, ".."))
	if err != nil {
		t.Fatalf("resolve repository root from %s: %v", wd, err)
	}
	return root
}

// goEnv returns the value of one `go env` key from the invoking user's
// environment.
func goEnv(key string) (string, error) {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// installQuadletEnv builds the environment for one real run of
// hack/install-quadlet.sh with an isolated HOME and state dir. GOCACHE and
// GOMODCACHE from the invoking user are passed through so the script's
// `go build ./cmd/capishim` reuses the warm caches instead of rebuilding and
// re-downloading the module graph under the temporary HOME.
func installQuadletEnv(homeDir, stateDir string) ([]string, error) {
	gocache, err := goEnv("GOCACHE")
	if err != nil {
		return nil, err
	}
	gomodcache, err := goEnv("GOMODCACHE")
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "HOME="),
			strings.HasPrefix(kv, "CAPISHIM_STATE_DIR="),
			strings.HasPrefix(kv, "CAPISHIM_SYSTEM="):
			// HOME and the state override are set below; CAPISHIM_SYSTEM is
			// dropped so the test always exercises the user-mode install.
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+homeDir,
		"CAPISHIM_STATE_DIR="+stateDir,
		"GOCACHE="+gocache,
		"GOMODCACHE="+gomodcache,
	), nil
}

// runInstallQuadlet executes the real installer script against the given HOME
// and state dir and returns its combined stdout/stderr.
func runInstallQuadlet(t *testing.T, homeDir, stateDir string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, "hack", "install-quadlet.sh")
	env, err := installQuadletEnv(homeDir, stateDir)
	if err != nil {
		t.Fatalf("build install-quadlet environment: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, script)
	cmd.Dir = root
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("install-quadlet timed out after 4 minutes")
	}
	return out.String(), err
}

// assertStateDirs checks that every state subdirectory the quadlet containers
// bind-mount exists (REQ-009).
func assertStateDirs(t *testing.T, stateDir string) {
	t.Helper()
	for _, rel := range wantStateDirs {
		dir := filepath.Join(stateDir, rel)
		fi, err := os.Stat(dir)
		if err != nil {
			t.Errorf("install-quadlet did not create state directory %s: %v (REQ-009)", dir, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("state path %s exists but is not a directory (REQ-009)", dir)
		}
	}
}

// assertABACPolicy checks that <state>/abac/policy.json exists with exactly the
// shim's bootstrap policy, trailing newline included (REQ-004).
func assertABACPolicy(t *testing.T, stateDir string) []byte {
	t.Helper()
	path := filepath.Join(stateDir, "abac", "policy.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ABAC policy %s: %v (REQ-004)", path, err)
	}
	if string(got) != wantPolicy {
		t.Errorf("abac/policy.json content mismatch (REQ-004)\nwant: %q\n got: %q", wantPolicy, string(got))
	}
	return got
}

// TestInstallQuadletPreparesStateDirs is the regression test for the installer
// defect: `make install-quadlet` must leave the host state directory ready for
// the quadlet pod (REQ-009) and the apiserver's ABAC policy file in place
// (REQ-004), so a clean host that runs `make images && make install-quadlet
// && systemctl --user daemon-reload && systemctl --user start capishim-pod`
// boots all containers (VC-01, REQ-011).
func TestInstallQuadletPreparesStateDirs(t *testing.T) {
	homeDir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "capishim-state")

	out, err := runInstallQuadlet(t, homeDir, stateDir)
	if err != nil {
		t.Fatalf("install-quadlet failed: %v\n%s", err, out)
	}

	assertStateDirs(t, stateDir)
	assertABACPolicy(t, stateDir)
}

// TestInstallQuadletIdempotent runs the installer twice against the same HOME
// and state dir: the second run must succeed and must not change the ABAC
// policy (REQ-004, VC-02-style idempotency for the installer path).
func TestInstallQuadletIdempotent(t *testing.T) {
	homeDir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "capishim-state")

	out1, err := runInstallQuadlet(t, homeDir, stateDir)
	if err != nil {
		t.Fatalf("first install-quadlet run failed: %v\n%s", err, out1)
	}
	policy1 := assertABACPolicy(t, stateDir)

	out2, err := runInstallQuadlet(t, homeDir, stateDir)
	if err != nil {
		t.Fatalf("second install-quadlet run failed: %v\n%s", err, out2)
	}
	policy2 := assertABACPolicy(t, stateDir)

	if !bytes.Equal(policy1, policy2) {
		t.Errorf("re-running install-quadlet changed abac/policy.json\nbefore: %q\nafter: %q", policy1, policy2)
	}
}

// TestInstallQuadletHonorsCustomStateDir proves CAPISHIM_STATE_DIR is honored
// and the script never falls back to the hardcoded default location under the
// temporary HOME (REQ-009).
func TestInstallQuadletHonorsCustomStateDir(t *testing.T) {
	homeDir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "custom", "state")

	out, err := runInstallQuadlet(t, homeDir, stateDir)
	if err != nil {
		t.Fatalf("install-quadlet failed: %v\n%s", err, out)
	}

	assertStateDirs(t, stateDir)

	defaultState := filepath.Join(homeDir, ".local", "share", "capishim")
	if _, err := os.Stat(defaultState); err == nil {
		t.Errorf("install-quadlet wrote to default state dir %s despite CAPISHIM_STATE_DIR=%s (REQ-009)", defaultState, stateDir)
	}
}
