// Package main_test exercises the testable helpers behind the capishim setup
// subcommand: manifests-dir resolution, apiserver URL and timeout derivation,
// the kubeconfig writer, the RBAC/webhook namespace maps, the CRD name list,
// the unstructured-to-typed webhook conversion round trip against the
// vendored provider manifests, and the apiserver readiness probe.
package main_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"

	capishim "github.com/moeryomenko/capishim/cmd/capishim"
	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/manifests"
	"github.com/moeryomenko/capishim/internal/pki"
)

// vendoredManifestsRoot is the repo-relative path to the vendored provider
// manifests used by the black-box helpers below.
const vendoredManifestsRoot = "../../templates/manifests"

func TestManifestsDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "default when unset", env: nil, want: "/templates/manifests"},
		{name: "default when other keys set", env: map[string]string{"HOME": "/x"}, want: "/templates/manifests"},
		{name: "override", env: map[string]string{capishim.EnvManifestsDir: "/srv/manifests"}, want: "/srv/manifests"},
		{name: "override trimmed", env: map[string]string{capishim.EnvManifestsDir: " /srv/manifests "}, want: "/srv/manifests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := capishim.ManifestsDir(tt.env)
			if err != nil {
				t.Fatalf("ManifestsDir(%v) returned error: %v", tt.env, err)
			}
			if got != tt.want {
				t.Errorf("ManifestsDir(%v) = %q, want %q", tt.env, got, tt.want)
			}
		})
	}
}

func TestManifestsDirEmptyOverride(t *testing.T) {
	t.Parallel()
	if _, err := capishim.ManifestsDir(map[string]string{capishim.EnvManifestsDir: ""}); err == nil {
		t.Error("ManifestsDir with set-but-empty override returned no error, want error")
	}
}

func TestAPIServerURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		bind string
		want string
	}{
		{name: "default loopback", bind: "127.0.0.1:6443", want: "https://127.0.0.1:6443"},
		{name: "custom port", bind: "127.0.0.1:7443", want: "https://127.0.0.1:7443"},
		{name: "wildcard host", bind: "0.0.0.0:6443", want: "https://127.0.0.1:6443"},
		{name: "unparseable falls back", bind: "nonsense", want: "https://127.0.0.1:6443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := capishim.APIServerURL(tt.bind); got != tt.want {
				t.Errorf("APIServerURL(%q) = %q, want %q", tt.bind, got, tt.want)
			}
		})
	}
}

func TestAPIServerTimeout(t *testing.T) {
	t.Parallel()
	got, err := capishim.APIServerTimeout(nil)
	if err != nil {
		t.Fatalf("APIServerTimeout(nil) returned error: %v", err)
	}
	if want := 5 * time.Minute; got != want {
		t.Errorf("APIServerTimeout(nil) = %v, want %v", got, want)
	}
	got, err = capishim.APIServerTimeout(map[string]string{capishim.EnvAPIServerTimeout: "30s"})
	if err != nil {
		t.Fatalf("APIServerTimeout(30s) returned error: %v", err)
	}
	if want := 30 * time.Second; got != want {
		t.Errorf("APIServerTimeout(30s) = %v, want %v", got, want)
	}
	if _, err := capishim.APIServerTimeout(map[string]string{capishim.EnvAPIServerTimeout: "banana"}); err == nil {
		t.Error("APIServerTimeout(invalid) returned no error, want error")
	}
}

func TestProviderManifestFiles(t *testing.T) {
	t.Parallel()
	got := capishim.ProviderManifestFiles("/m")
	want := []string{
		filepath.Join("/m", "core", "provider.yaml"),
		filepath.Join("/m", "cabpk", "provider.yaml"),
		filepath.Join("/m", "kcp", "provider.yaml"),
		filepath.Join("/m", "capd", "provider.yaml"),
	}
	if len(got) != len(want) {
		t.Fatalf("ProviderManifestFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ProviderManifestFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManagerCNByNamespace(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"capi-system":                       "capishim:core-manager",
		"capi-kubeadm-bootstrap-system":     "capishim:cabpk-manager",
		"capi-kubeadm-control-plane-system": "capishim:kcp-manager",
		"capd-system":                       "capishim:capd-manager",
	}
	got := capishim.ManagerCNByNamespace()
	if len(got) != len(want) {
		t.Errorf("ManagerCNByNamespace() = %v, want %v", got, want)
	}
	for namespace, cn := range want {
		if got[namespace] != cn {
			t.Errorf("ManagerCNByNamespace()[%q] = %q, want %q", namespace, got[namespace], cn)
		}
	}
}

func TestWebhookPortsByNamespace(t *testing.T) {
	t.Parallel()
	want := map[string]int{
		"capi-system":                       9443,
		"capi-kubeadm-bootstrap-system":     9444,
		"capi-kubeadm-control-plane-system": 9445,
		"capd-system":                       9446,
	}
	got := capishim.WebhookPortsByNamespace()
	if len(got) != len(want) {
		t.Errorf("WebhookPortsByNamespace() = %v, want %v", got, want)
	}
	for namespace, port := range want {
		if got[namespace] != port {
			t.Errorf("WebhookPortsByNamespace()[%q] = %d, want %d", namespace, got[namespace], port)
		}
	}
}

func TestManagerArtifact(t *testing.T) {
	t.Parallel()
	inv := &pki.Inventory{
		CoreManager:  pki.Artifact{Name: "core-manager", CertPath: "/c.crt", KeyPath: "/c.key"},
		CABPKManager: pki.Artifact{Name: "cabpk-manager", CertPath: "/b.crt", KeyPath: "/b.key"},
		KCPManager:   pki.Artifact{Name: "kcp-manager", CertPath: "/k.crt", KeyPath: "/k.key"},
		CAPDManager:  pki.Artifact{Name: "capd-manager", CertPath: "/d.crt", KeyPath: "/d.key"},
	}
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "core", id: "core", want: "core-manager"},
		{name: "cabpk", id: "cabpk", want: "cabpk-manager"},
		{name: "kcp", id: "kcp", want: "kcp-manager"},
		{name: "capd", id: "capd", want: "capd-manager"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := capishim.ManagerArtifact(config.ComponentID(tt.id), inv)
			if !ok {
				t.Fatalf("ManagerArtifact(%q) not found", tt.id)
			}
			if got.Name != tt.want {
				t.Errorf("ManagerArtifact(%q).Name = %q, want %q", tt.id, got.Name, tt.want)
			}
		})
	}
	if _, ok := capishim.ManagerArtifact(config.ComponentEtcd, inv); ok {
		t.Error("ManagerArtifact(etcd) reported ok, want not found")
	}
}

func TestWriteKubeconfigFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	for _, p := range []string{caPath, certPath, keyPath} {
		if err := os.WriteFile(p, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", p, err)
		}
	}
	kubeconfig := capishim.BuildKubeconfig("https://127.0.0.1:6443", caPath, certPath, keyPath)
	out := filepath.Join(dir, "kubeconfigs", "admin.kubeconfig")
	if err := capishim.WriteKubeconfigFile(kubeconfig, out); err != nil {
		t.Fatalf("WriteKubeconfigFile returned error: %v", err)
	}
	loaded, err := clientcmd.LoadFromFile(out)
	if err != nil {
		t.Fatalf("load written kubeconfig: %v", err)
	}
	if got, want := loaded.CurrentContext, "default"; got != want {
		t.Errorf("CurrentContext = %q, want %q", got, want)
	}
	cluster := loaded.Clusters["default"]
	if cluster == nil {
		t.Fatal("kubeconfig has no cluster named default")
	}
	if got, want := cluster.Server, "https://127.0.0.1:6443"; got != want {
		t.Errorf("cluster server = %q, want %q", got, want)
	}
	if got, want := cluster.CertificateAuthority, caPath; got != want {
		t.Errorf("cluster certificate-authority = %q, want %q", got, want)
	}
	auth := loaded.AuthInfos["default"]
	if auth == nil {
		t.Fatal("kubeconfig has no auth info named default")
	}
	if got, want := auth.ClientCertificate, certPath; got != want {
		t.Errorf("auth client-certificate = %q, want %q", got, want)
	}
	if got, want := auth.ClientKey, keyPath; got != want {
		t.Errorf("auth client-key = %q, want %q", got, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat %s: %v", out, err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("kubeconfig mode = %v, want %v", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatalf("read kubeconfig dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".capishim-") {
			t.Errorf("leftover temporary file %s after successful write", entry.Name())
		}
	}
}

func TestWriteKubeconfigFileConverges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kubeconfig := capishim.BuildKubeconfig(
		"https://127.0.0.1:6443",
		filepath.Join(dir, "ca.crt"),
		filepath.Join(dir, "client.crt"),
		filepath.Join(dir, "client.key"),
	)
	out := filepath.Join(dir, "kc")
	if err := capishim.WriteKubeconfigFile(kubeconfig, out); err != nil {
		t.Fatalf("first write returned error: %v", err)
	}
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read first kubeconfig: %v", err)
	}
	if err := capishim.WriteKubeconfigFile(kubeconfig, out); err != nil {
		t.Fatalf("second write returned error: %v", err)
	}
	second, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read second kubeconfig: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("re-writing the same kubeconfig changed its bytes; setup must converge")
	}
}

func TestCRDNamesPresentInVendoredTemplates(t *testing.T) {
	t.Parallel()
	kept := loadKept(t)
	present := make(map[string]bool)
	for i := range kept {
		if kept[i].GetKind() == "CustomResourceDefinition" {
			present[kept[i].GetName()] = true
		}
	}
	for _, name := range capishim.CRDNames() {
		if !present[name] {
			t.Errorf("CRD %q from CRDNames is missing in the vendored provider manifests", name)
		}
	}
	if got, want := len(capishim.CRDNames()), 15; got != want {
		t.Errorf("CRDNames has %d entries, want %d (REQ-003)", got, want)
	}
}

func TestWebhookRoundTripPreservesVendoredObjects(t *testing.T) {
	t.Parallel()
	kept := loadKept(t)
	objects, err := capishim.WebhookObjects(kept)
	if err != nil {
		t.Fatalf("WebhookObjects returned error: %v", err)
	}
	if len(objects) == 0 {
		t.Fatal("no webhook objects parsed from vendored provider manifests")
	}
	roundTripped, err := capishim.UnstructuredObjects(objects)
	if err != nil {
		t.Fatalf("UnstructuredObjects returned error: %v", err)
	}
	if len(roundTripped) != len(objects) {
		t.Fatalf("round trip changed object count: %d -> %d", len(objects), len(roundTripped))
	}
	original := make(map[string]string)
	for i := range kept {
		obj := &kept[i]
		switch obj.GetKind() {
		case "CustomResourceDefinition", "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
			normalized := obj.DeepCopy()
			// The typed round trip below emits an empty status object on
			// CRDs; the apiserver's CRD update strategy replaces any
			// incoming status with the live one (PrepareForUpdate), so the
			// status field is not part of the rewrite contract. Strip it on
			// both sides before comparing.
			unstructured.RemoveNestedField(normalized.Object, "status")
			data, err := json.Marshal(normalized.Object)
			if err != nil {
				t.Fatalf("marshal original %s %q: %v", obj.GetKind(), obj.GetName(), err)
			}
			original[obj.GetKind()+"/"+obj.GetName()] = string(data)
		}
	}
	for i := range roundTripped {
		key := roundTripped[i].GetKind() + "/" + roundTripped[i].GetName()
		want, ok := original[key]
		if !ok {
			t.Errorf("round trip produced unexpected object %s", key)
			continue
		}
		normalized := roundTripped[i].DeepCopy()
		unstructured.RemoveNestedField(normalized.Object, "status")
		data, err := json.Marshal(normalized.Object)
		if err != nil {
			t.Fatalf("marshal round-tripped %s: %v", key, err)
		}
		if string(data) != want {
			t.Errorf("round trip changed %s; rewriting the webhook rewrite would corrupt the object", key)
		}
	}
}

func TestWaitForAPIServerReady(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	caPath := writeCertFile(t, server.Certificate())
	_, certPath, keyPath := writeClientKeyPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := capishim.WaitForAPIServer(ctx, server.URL+"/healthz", caPath, certPath, keyPath, 5*time.Second); err != nil {
		t.Fatalf("WaitForAPIServer against healthy server returned error: %v", err)
	}
}

func TestWaitForAPIServerTimeout(t *testing.T) {
	t.Parallel()
	// Bind an ephemeral port and close it so every probe fails to connect.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on ephemeral port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	cert, certPath, keyPath := writeClientKeyPair(t)
	caPath := writeCertFile(t, cert)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err = capishim.WaitForAPIServer(ctx, "https://"+addr+"/healthz", caPath, certPath, keyPath, 200*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForAPIServer against a closed port returned no error")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error = %v, want 'not ready'", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout probe took %v, want well under 5s", elapsed)
	}
}

// loadKept loads the four vendored provider manifests and returns the kept
// subset (manifests.Keep).
func loadKept(t *testing.T) []unstructured.Unstructured {
	t.Helper()
	loaded, err := manifests.Load(
		filepath.Join(vendoredManifestsRoot, "core", "provider.yaml"),
		filepath.Join(vendoredManifestsRoot, "cabpk", "provider.yaml"),
		filepath.Join(vendoredManifestsRoot, "kcp", "provider.yaml"),
		filepath.Join(vendoredManifestsRoot, "capd", "provider.yaml"),
	)
	if err != nil {
		t.Fatalf("load vendored provider manifests: %v", err)
	}
	var kept []unstructured.Unstructured
	for i := range loaded {
		if manifests.Keep(&loaded[i]) {
			kept = append(kept, loaded[i])
		}
	}
	return kept
}

// writeCertFile writes cert as a PEM certificate file and returns its path.
func writeCertFile(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write certificate file: %v", err)
	}
	return path
}

// writeClientKeyPair generates a self-signed ECDSA client certificate pair
// and returns the certificate plus the paths to the PEM files.
func writeClientKeyPair(t *testing.T) (_ *x509.Certificate, _, _ string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("generate client serial: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse client certificate: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write client certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	return parsed, certPath, keyPath
}
