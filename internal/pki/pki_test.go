// Package pki_test exercises the certificate generation contract for
// internal/pki with black-box tests (no access to implementation internals).
package pki_test

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/moeryomenko/capishim/internal/pki"
)

const (
	defaultBind                   = "127.0.0.1:6443"
	keyPerm           fs.FileMode = 0o600
	certPerm          fs.FileMode = 0o644
	minValidityYears              = 9
	maxValidityYears              = 11
	expectedArtifacts             = 15
	expectedFileCount             = 32
)

// generate runs pki.Generate against a fresh state dir and fails the test on
// error. bindAddress is passed through verbatim so callers can exercise the
// SAN and validation contract.
func generate(t *testing.T, stateDir, bindAddress string) *pki.Inventory {
	t.Helper()
	inv, err := pki.Generate(t.Context(), pki.Config{
		StateDir:    stateDir,
		BindAddress: bindAddress,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return inv
}

func pkiDir(stateDir string) string {
	return filepath.Join(stateDir, "pki")
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// writeFile writes data with the private-key permission mask; every generated
// key is private and every fixture corruption write is likewise private.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, keyPerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func loadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	certs, err := parseCertsPEM(readFile(t, path))
	if err != nil {
		t.Fatalf("parse certificates from %s: %v", path, err)
	}
	return certs[0]
}

func parseCertsPEM(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	var block *pem.Block
	for {
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no CERTIFICATE PEM blocks found")
	}
	return certs, nil
}

func loadKey(t *testing.T, path string) crypto.Signer {
	t.Helper()
	data := readFile(t, path)
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("parse PKCS1 private key %s: %v", path, err)
		}
		return key
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("parse EC private key %s: %v", path, err)
		}
		return key
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("parse PKCS8 private key %s: %v", path, err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			t.Fatalf("PKCS8 key %s is %T, not a crypto.Signer", path, key)
		}
		return signer
	default:
		t.Fatalf("unsupported key PEM block type %q in %s", block.Type, path)
		return nil
	}
}

func publicKeysEqual(t *testing.T, a, b crypto.PublicKey) bool {
	t.Helper()
	ab, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	bb, err := x509.MarshalPKIXPublicKey(b)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return bytes.Equal(ab, bb)
}

// assertKeyMatchesCert asserts the private key at keyPath corresponds to the
// certificate at certPath.
func assertKeyMatchesCert(t *testing.T, certPath, keyPath string) {
	t.Helper()
	key := loadKey(t, keyPath)
	cert := loadCert(t, certPath)
	if !publicKeysEqual(t, key.Public(), cert.PublicKey) {
		t.Errorf("private key %s does not match certificate %s", keyPath, certPath)
	}
}

// verifyLeaf asserts certPath chains to the CA at caPath with the given EKU
// usage. Name checks are done separately by SAN assertions.
func verifyLeaf(t *testing.T, certPath, caPath string, usages []x509.ExtKeyUsage) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(loadCert(t, caPath))
	leaf := loadCert(t, certPath)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: usages}); err != nil {
		t.Errorf("certificate %s does not verify against CA %s: %v", certPath, caPath, err)
	}
}

func assertRSAOrECDSA(t *testing.T, key crypto.Signer) {
	t.Helper()
	switch key.(type) {
	case *rsa.PrivateKey, *ecdsa.PrivateKey:
	default:
		t.Errorf("key has type %T, want *rsa.PrivateKey or *ecdsa.PrivateKey", key)
	}
}

// assertValidity asserts the certificate lifetime is approximately ten years
// (between 9 and 11 years to tolerate exact duration choices).
func assertValidity(t *testing.T, cert *x509.Certificate) {
	t.Helper()
	if !cert.NotAfter.After(cert.NotBefore.AddDate(minValidityYears, 0, 0)) {
		t.Errorf("certificate valid %s..%s is shorter than %d years", cert.NotBefore, cert.NotAfter, minValidityYears)
	}
	if !cert.NotBefore.AddDate(maxValidityYears, 0, 0).After(cert.NotAfter) {
		t.Errorf("certificate valid %s..%s is longer than %d years", cert.NotBefore, cert.NotAfter, maxValidityYears)
	}
}

// snapshotDir captures every file under dir keyed by its path relative to dir.
func snapshotDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	snap := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		snap[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return snap
}

// assertSnapshotsMatch asserts the two snapshots are byte-identical except for
// the listed relative paths, which are skipped.
func assertSnapshotsMatch(t *testing.T, before, after map[string][]byte, excluded ...string) {
	t.Helper()
	skip := make(map[string]bool, len(excluded))
	for _, rel := range excluded {
		skip[rel] = true
	}
	for rel, want := range before {
		if skip[rel] {
			continue
		}
		got, ok := after[rel]
		if !ok {
			t.Errorf("file disappeared: %s", rel)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("file changed: %s", rel)
		}
	}
	for rel := range after {
		if skip[rel] {
			continue
		}
		if _, ok := before[rel]; !ok {
			t.Errorf("unexpected file appeared: %s", rel)
		}
	}
}

// walkFiles returns every file under dir sorted by relative path.
func walkFiles(t *testing.T, dir string) []string {
	t.Helper()
	var rels []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(rels)
	return rels
}

// expectedFiles is the complete, sorted set of files the pki directory must
// contain after generation: ca, etcd server+client, apiserver serving+client,
// four manager client pairs plus the external hypervisor manager client pair,
// four webhook serving pairs under <comp>-webhook/ (tls.crt + tls.key),
// admin, and the SA signing keypair.
func expectedFiles() []string {
	names := []string{
		"ca", "etcd-server", "etcd-client", "apiserver", "apiserver-client",
		"core-manager", "cabpk-manager", "kcp-manager", "capd-manager",
		"hypervisor-manager",
		"core-webhook/tls", "cabpk-webhook/tls", "kcp-webhook/tls", "capd-webhook/tls",
		"admin",
	}
	files := []string{"sa.key", "sa.pub"}
	for _, name := range names {
		files = append(files, name+".crt", name+".key")
	}
	sort.Strings(files)
	return files
}

// TestGenerateFullInventory verifies that a full generation from an empty
// state dir produces every artifact at the documented paths with no extra
// files, and that the inventory exposes the exact naming scheme.
func TestGenerateFullInventory(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	if inv == nil {
		t.Fatal("Generate returned nil inventory")
	}

	pkiRoot := pkiDir(stateDir)
	info, err := os.Stat(pkiRoot)
	if err != nil {
		t.Fatalf("stat pki dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("pki path %s is not a directory", pkiRoot)
	}

	if got := len(inv.All()); got != expectedArtifacts {
		t.Errorf("Inventory.All() returned %d artifacts, want %d", got, expectedArtifacts)
	}

	relChecks := []struct {
		name        string
		certPath    string
		keyPath     string
		wantCertRel string
		wantKeyRel  string
	}{
		{"ca", inv.CA.CertPath, inv.CA.KeyPath, "ca.crt", "ca.key"},
		{"etcd-server", inv.EtcdServer.CertPath, inv.EtcdServer.KeyPath, "etcd-server.crt", "etcd-server.key"},
		{"etcd-client", inv.EtcdClient.CertPath, inv.EtcdClient.KeyPath, "etcd-client.crt", "etcd-client.key"},
		{"apiserver", inv.APIServer.CertPath, inv.APIServer.KeyPath, "apiserver.crt", "apiserver.key"},
		{
			"apiserver-client",
			inv.APIServerClient.CertPath,
			inv.APIServerClient.KeyPath,
			"apiserver-client.crt",
			"apiserver-client.key",
		},
		{"core-manager", inv.CoreManager.CertPath, inv.CoreManager.KeyPath, "core-manager.crt", "core-manager.key"},
		{"cabpk-manager", inv.CABPKManager.CertPath, inv.CABPKManager.KeyPath, "cabpk-manager.crt", "cabpk-manager.key"},
		{"kcp-manager", inv.KCPManager.CertPath, inv.KCPManager.KeyPath, "kcp-manager.crt", "kcp-manager.key"},
		{"capd-manager", inv.CAPDManager.CertPath, inv.CAPDManager.KeyPath, "capd-manager.crt", "capd-manager.key"},
		{"core-webhook", inv.CoreWebhook.CertPath, inv.CoreWebhook.KeyPath, "core-webhook/tls.crt", "core-webhook/tls.key"},
		{
			"cabpk-webhook",
			inv.CABPKWebhook.CertPath,
			inv.CABPKWebhook.KeyPath,
			"cabpk-webhook/tls.crt",
			"cabpk-webhook/tls.key",
		},
		{"kcp-webhook", inv.KCPWebhook.CertPath, inv.KCPWebhook.KeyPath, "kcp-webhook/tls.crt", "kcp-webhook/tls.key"},
		{"capd-webhook", inv.CAPDWebhook.CertPath, inv.CAPDWebhook.KeyPath, "capd-webhook/tls.crt", "capd-webhook/tls.key"},
		{"admin", inv.Admin.CertPath, inv.Admin.KeyPath, "admin.crt", "admin.key"},
	}
	for _, tt := range relChecks {
		if want := filepath.Join(pkiRoot, tt.wantCertRel); tt.certPath != want {
			t.Errorf("inventory %s cert path = %s, want %s", tt.name, tt.certPath, want)
		}
		if want := filepath.Join(pkiRoot, tt.wantKeyRel); tt.keyPath != want {
			t.Errorf("inventory %s key path = %s, want %s", tt.name, tt.keyPath, want)
		}
	}

	if inv.SAPubPath != filepath.Join(pkiRoot, "sa.pub") {
		t.Errorf("SAPubPath = %s, want %s", inv.SAPubPath, filepath.Join(pkiRoot, "sa.pub"))
	}
	if inv.SAKeyPath != filepath.Join(pkiRoot, "sa.key") {
		t.Errorf("SAKeyPath = %s, want %s", inv.SAKeyPath, filepath.Join(pkiRoot, "sa.key"))
	}

	for _, a := range inv.All() {
		for _, path := range []string{a.CertPath, a.KeyPath} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("artifact %s file %s: %v", a.Name, path, err)
			}
		}
	}
	for _, path := range []string{inv.SAPubPath, inv.SAKeyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("SA file %s: %v", path, err)
		}
	}

	got := walkFiles(t, pkiRoot)
	if want := expectedFiles(); !slices.Equal(got, want) {
		t.Errorf("pki file set has %d files, want %d: got %v, want %v", len(got), expectedFileCount, got, want)
	}
}

// TestGeneratePermissions verifies private keys are written with 0600 and
// certificates with 0644, per the documented naming and permission scheme.
func TestGeneratePermissions(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	generate(t, stateDir, defaultBind)

	for _, rel := range expectedFiles() {
		info, err := os.Stat(filepath.Join(pkiDir(stateDir), rel))
		if err != nil {
			t.Errorf("stat %s: %v", rel, err)
			continue
		}
		want := certPerm
		if strings.HasSuffix(rel, ".key") {
			want = keyPerm
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %o, want %o", rel, got, want)
		}
	}
}

// TestGenerateVerifiesAgainstCA asserts every certificate chains to the pod
// CA, is issued directly by it, does not outlive it, and that its private key
// matches the certificate.
func TestGenerateVerifiesAgainstCA(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	ca := loadCert(t, inv.CA.CertPath)
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	serverAuth := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	clientAuth := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	tests := []struct {
		name     string
		certPath string
		keyPath  string
		usages   []x509.ExtKeyUsage
	}{
		{"etcd-server", inv.EtcdServer.CertPath, inv.EtcdServer.KeyPath, serverAuth},
		{"etcd-client", inv.EtcdClient.CertPath, inv.EtcdClient.KeyPath, clientAuth},
		{"apiserver", inv.APIServer.CertPath, inv.APIServer.KeyPath, serverAuth},
		{"apiserver-client", inv.APIServerClient.CertPath, inv.APIServerClient.KeyPath, clientAuth},
		{"core-manager", inv.CoreManager.CertPath, inv.CoreManager.KeyPath, clientAuth},
		{"cabpk-manager", inv.CABPKManager.CertPath, inv.CABPKManager.KeyPath, clientAuth},
		{"kcp-manager", inv.KCPManager.CertPath, inv.KCPManager.KeyPath, clientAuth},
		{"capd-manager", inv.CAPDManager.CertPath, inv.CAPDManager.KeyPath, clientAuth},
		{"core-webhook", inv.CoreWebhook.CertPath, inv.CoreWebhook.KeyPath, serverAuth},
		{"cabpk-webhook", inv.CABPKWebhook.CertPath, inv.CABPKWebhook.KeyPath, serverAuth},
		{"kcp-webhook", inv.KCPWebhook.CertPath, inv.KCPWebhook.KeyPath, serverAuth},
		{"capd-webhook", inv.CAPDWebhook.CertPath, inv.CAPDWebhook.KeyPath, serverAuth},
		{"admin", inv.Admin.CertPath, inv.Admin.KeyPath, clientAuth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			leaf := loadCert(t, tt.certPath)
			if !bytes.Equal(leaf.RawIssuer, ca.RawSubject) {
				t.Errorf("certificate %s issuer does not match CA subject", tt.certPath)
			}
			if leaf.NotAfter.After(ca.NotAfter) {
				t.Errorf("certificate %s outlives CA (leaf %s, ca %s)", tt.certPath, leaf.NotAfter, ca.NotAfter)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: tt.usages}); err != nil {
				t.Errorf("verify %s against CA: %v", tt.certPath, err)
			}
			assertKeyMatchesCert(t, tt.certPath, tt.keyPath)
		})
	}
}

// TestCAProperties asserts the CA is a self-signed, cert-signing root with
// approximately ten years of validity and an RSA or ECDSA key.
func TestCAProperties(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	ca := loadCert(t, inv.CA.CertPath)

	if !ca.IsCA {
		t.Error("CA IsCA = false, want true")
	}
	if !ca.BasicConstraintsValid {
		t.Error("CA BasicConstraintsValid = false, want true")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA KeyUsage = %v, want KeyUsageCertSign", ca.KeyUsage)
	}
	if !bytes.Equal(ca.RawSubject, ca.RawIssuer) {
		t.Error("CA is not self-signed: subject != issuer")
	}
	assertValidity(t, ca)
	assertRSAOrECDSA(t, loadKey(t, inv.CA.KeyPath))
}
