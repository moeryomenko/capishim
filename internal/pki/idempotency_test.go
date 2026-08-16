package pki_test

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/capishim/internal/pki"
)

// TestIdempotencyByteIdentical asserts two consecutive generations over the
// same state dir produce byte-identical artifacts: the CA is never regenerated
// and every existing leaf is untouched (REQ-002 pass precursor / VC-02).
func TestIdempotencyByteIdentical(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generate(t, stateDir, defaultBind)
	before := snapshotDir(t, pkiDir(stateDir))

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, pkiDir(stateDir))

	assertSnapshotsMatch(t, before, after)
}

// TestIdempotencyRegeneratesMissingLeafOnly asserts that when the CA exists
// but a leaf pair is missing, only that leaf is generated and every other
// file stays byte-identical.
func TestIdempotencyRegeneratesMissingLeafOnly(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	before := snapshotDir(t, pkiDir(stateDir))

	if err := os.Remove(inv.Admin.CertPath); err != nil {
		t.Fatalf("remove admin cert: %v", err)
	}
	if err := os.Remove(inv.Admin.KeyPath); err != nil {
		t.Fatalf("remove admin key: %v", err)
	}

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, pkiDir(stateDir))

	assertSnapshotsMatch(t, before, after, "admin.crt", "admin.key")
	assertKeyMatchesCert(t, inv.Admin.CertPath, inv.Admin.KeyPath)
	verifyLeaf(t, inv.Admin.CertPath, inv.CA.CertPath, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

// TestIdempotencyRegeneratesMissingWebhookPair asserts a deleted webhook
// serving pair (tls.crt + tls.key) is regenerated while the other webhook
// directories remain byte-identical.
func TestIdempotencyRegeneratesMissingWebhookPair(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	before := snapshotDir(t, pkiDir(stateDir))

	if err := os.Remove(inv.CAPDWebhook.CertPath); err != nil {
		t.Fatalf("remove capd webhook cert: %v", err)
	}
	if err := os.Remove(inv.CAPDWebhook.KeyPath); err != nil {
		t.Fatalf("remove capd webhook key: %v", err)
	}

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, pkiDir(stateDir))

	assertSnapshotsMatch(t, before, after, "capd-webhook/tls.crt", "capd-webhook/tls.key")
	assertKeyMatchesCert(t, inv.CAPDWebhook.CertPath, inv.CAPDWebhook.KeyPath)
	verifyLeaf(t, inv.CAPDWebhook.CertPath, inv.CA.CertPath, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
}

// TestIdempotencyRegeneratesAllLeavesWhenMissing asserts that from a state
// containing only the CA pair every leaf is regenerated and the CA stays
// byte-identical (partial state: CA exists, no leaves).
func TestIdempotencyRegeneratesAllLeavesWhenMissing(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generate(t, stateDir, defaultBind)
	pkiRoot := pkiDir(stateDir)
	before := snapshotDir(t, pkiRoot)

	for rel := range before {
		if rel == "ca.crt" || rel == "ca.key" {
			continue
		}
		if err := os.Remove(filepath.Join(pkiRoot, rel)); err != nil {
			t.Fatalf("remove %s: %v", rel, err)
		}
	}

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, pkiRoot)

	if !bytes.Equal(after["ca.crt"], before["ca.crt"]) {
		t.Error("CA certificate regenerated; must stay byte-identical")
	}
	if !bytes.Equal(after["ca.key"], before["ca.key"]) {
		t.Error("CA key regenerated; must stay byte-identical")
	}
	for _, rel := range expectedFiles() {
		if rel == "ca.crt" || rel == "ca.key" {
			continue
		}
		if _, ok := after[rel]; !ok {
			t.Errorf("leaf file %s not regenerated", rel)
		}
	}
}

// TestIdempotencyRegeneratesBrokenLeafPair asserts a leaf with a half-present
// pair (cert without key, or key without cert) is regenerated as a consistent
// pair, and that other files stay byte-identical.
func TestIdempotencyRegeneratesBrokenLeafPair(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		breakPair func(t *testing.T, inv *pki.Inventory)
	}{
		{"key missing", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			if err := os.Remove(inv.Admin.KeyPath); err != nil {
				t.Fatalf("remove admin key: %v", err)
			}
		}},
		{"cert missing", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			if err := os.Remove(inv.Admin.CertPath); err != nil {
				t.Fatalf("remove admin cert: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			inv := generate(t, stateDir, defaultBind)
			before := snapshotDir(t, pkiDir(stateDir))

			tt.breakPair(t, inv)
			generate(t, stateDir, defaultBind)
			after := snapshotDir(t, pkiDir(stateDir))

			assertSnapshotsMatch(t, before, after, "admin.crt", "admin.key")
			assertKeyMatchesCert(t, inv.Admin.CertPath, inv.Admin.KeyPath)
			verifyLeaf(t, inv.Admin.CertPath, inv.CA.CertPath, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
		})
	}
}

// TestCorruptedCAPairErrors asserts a corrupt or incomplete CA pair is never
// treated as valid: generation errors and leaves the tree byte-identical.
// Corrupted CA variants: zero-byte cert, zero-byte key, key that does not
// match the cert, and a missing half of the pair.
func TestCorruptedCAPairErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		corrupt func(t *testing.T, inv *pki.Inventory)
	}{
		{"zero-byte ca.crt", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			writeFile(t, inv.CA.CertPath, nil)
		}},
		{"zero-byte ca.key", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			writeFile(t, inv.CA.KeyPath, nil)
		}},
		{"ca.key does not match ca.crt", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			writeFile(t, inv.CA.KeyPath, readFile(t, inv.Admin.KeyPath))
		}},
		{"ca.key missing", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			if err := os.Remove(inv.CA.KeyPath); err != nil {
				t.Fatalf("remove ca key: %v", err)
			}
		}},
		{"ca.crt missing", func(t *testing.T, inv *pki.Inventory) {
			t.Helper()
			if err := os.Remove(inv.CA.CertPath); err != nil {
				t.Fatalf("remove ca cert: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			inv := generate(t, stateDir, defaultBind)
			tt.corrupt(t, inv)
			corrupted := snapshotDir(t, pkiDir(stateDir))

			_, err := pki.Generate(t.Context(), pki.Config{StateDir: stateDir, BindAddress: defaultBind})
			if err == nil {
				t.Fatal("Generate succeeded with corrupt CA; want error")
			}

			after := snapshotDir(t, pkiDir(stateDir))
			assertSnapshotsMatch(t, corrupted, after)
		})
	}
}

// TestCorruptedLeafRegenerates asserts a corrupt (unparseable) leaf cert is
// regenerated together with its key, while all other files stay
// byte-identical.
func TestCorruptedLeafRegenerates(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	before := snapshotDir(t, pkiDir(stateDir))

	writeFile(t, inv.Admin.CertPath, nil)

	generate(t, stateDir, defaultBind)
	after := snapshotDir(t, pkiDir(stateDir))

	assertSnapshotsMatch(t, before, after, "admin.crt", "admin.key")
	assertKeyMatchesCert(t, inv.Admin.CertPath, inv.Admin.KeyPath)
	verifyLeaf(t, inv.Admin.CertPath, inv.CA.CertPath, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}
