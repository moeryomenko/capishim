// Package main_test exercises the testable helpers behind the capishim setup
// subcommand: manifests-dir resolution, apiserver URL and timeout derivation,
// the kubeconfig writer, the first-apply RBAC binding filter, the
// RBAC/webhook namespace maps, the CRD name list, the unstructured-to-typed
// webhook conversion round trip against the vendored provider manifests, and
// the apiserver readiness probe.
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
		{
			name: "override trimmed",
			env:  map[string]string{capishim.EnvManifestsDir: " /srv/manifests "},
			want: "/srv/manifests",
		},
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
		filepath.Join("/m", "infrastructure-hypervisor", "provider.yaml"),
		filepath.Join("/m", "bootstrap-hypervisor", "provider.yaml"),
		filepath.Join("/m", "control-plane-hypervisor", "provider.yaml"),
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
		"hypervisor-system":                 "capishim:hypervisor-manager",
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

// TestWithoutRBACBindings covers the first-apply filter behind the RBAC
// double-apply fix (REQ-004, VC-01): the setup container's first apply must
// not create the ClusterRoleBinding and RoleBinding objects, because the
// second apply rewrites their roleRef and the apiserver rejects changing a
// binding's roleRef once it exists ("cannot change roleRef"). The filter
// returns only the non-binding kept objects, in input order, with unchanged
// content, and must not mutate its input.
func TestWithoutRBACBindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		give []unstructured.Unstructured
		want []unstructured.Unstructured
	}{
		{
			name: "mixed kept kinds drop only the bindings",
			give: []unstructured.Unstructured{
				bindingTestObj("ClusterRoleBinding", "capi-manager-rolebinding", "", "capi-aggregated-manager-role"),
				testObj("v1", "Namespace", "capi-system", ""),
				bindingTestObj("RoleBinding", "capi-leader-election-rolebinding", "capi-system", "capi-leader-election-role"),
				testObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "clusters.cluster.x-k8s.io", ""),
				testObj("rbac.authorization.k8s.io/v1", "ClusterRole", "capi-manager-role", ""),
				testObj("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "capi-webhook", ""),
				testObj("rbac.authorization.k8s.io/v1", "Role", "capi-manager-role", "capi-system"),
				testObj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "capi-webhook", ""),
			},
			want: []unstructured.Unstructured{
				testObj("v1", "Namespace", "capi-system", ""),
				testObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "clusters.cluster.x-k8s.io", ""),
				testObj("rbac.authorization.k8s.io/v1", "ClusterRole", "capi-manager-role", ""),
				testObj("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "capi-webhook", ""),
				testObj("rbac.authorization.k8s.io/v1", "Role", "capi-manager-role", "capi-system"),
				testObj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "capi-webhook", ""),
			},
		},
		{name: "empty input", give: []unstructured.Unstructured{}, want: nil},
		{name: "nil input", give: nil, want: nil},
		{
			name: "only bindings",
			give: []unstructured.Unstructured{
				bindingTestObj("ClusterRoleBinding", "capi-manager-rolebinding", "", "capi-aggregated-manager-role"),
				bindingTestObj("RoleBinding", "capi-leader-election-rolebinding", "capi-system", "capi-leader-election-role"),
			},
			want: nil,
		},
		{
			name: "no bindings passes everything through",
			give: []unstructured.Unstructured{
				testObj("v1", "Namespace", "capi-system", ""),
				testObj("rbac.authorization.k8s.io/v1", "ClusterRole", "capi-manager-role", ""),
				testObj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "capi-webhook", ""),
			},
			want: []unstructured.Unstructured{
				testObj("v1", "Namespace", "capi-system", ""),
				testObj("rbac.authorization.k8s.io/v1", "ClusterRole", "capi-manager-role", ""),
				testObj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "capi-webhook", ""),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Snapshot the input so the call is checked for mutation: the
			// filter selects objects, it must not rewrite or reorder them.
			snapshot, err := json.Marshal(tt.give)
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			got := capishim.WithoutRBACBindings(tt.give)
			after, err := json.Marshal(tt.give)
			if err != nil {
				t.Fatalf("marshal input after call: %v", err)
			}
			if !bytes.Equal(snapshot, after) {
				t.Errorf("WithoutRBACBindings mutated its input:\nbefore: %s\nafter:  %s", snapshot, after)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("WithoutRBACBindings returned %d objects, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				gotData, err := json.Marshal(got[i].Object)
				if err != nil {
					t.Fatalf("marshal result[%d]: %v", i, err)
				}
				wantData, err := json.Marshal(tt.want[i].Object)
				if err != nil {
					t.Fatalf("marshal want[%d]: %v", i, err)
				}
				if !bytes.Equal(gotData, wantData) {
					t.Errorf("WithoutRBACBindings[%d] = %s, want %s", i, gotData, wantData)
				}
			}
		})
	}
}

// TestWithoutRBACBindingsOnVendoredManifests runs the first-apply filter over
// the real kept provider objects and asserts the field-failure precondition:
// no ClusterRoleBinding or RoleBinding may survive, so the second apply
// creates every binding instead of updating it and the apiserver never sees a
// roleRef change on a clean boot (REQ-004, VC-01).
func TestWithoutRBACBindingsOnVendoredManifests(t *testing.T) {
	t.Parallel()
	kept := loadKept(t)
	bindings := 0
	for i := range kept {
		switch kept[i].GetKind() {
		case "ClusterRoleBinding", "RoleBinding":
			bindings++
		}
	}
	if bindings == 0 {
		t.Fatal("vendored provider manifests contain no RBAC bindings; test would be vacuous")
	}
	got := capishim.WithoutRBACBindings(kept)
	if len(got) != len(kept)-bindings {
		t.Fatalf("WithoutRBACBindings kept %d of %d objects, want %d", len(got), len(kept), len(kept)-bindings)
	}
	i := 0
	for j := range kept {
		switch kept[j].GetKind() {
		case "ClusterRoleBinding", "RoleBinding":
			continue
		}
		gotData, err := json.Marshal(got[i].Object)
		if err != nil {
			t.Fatalf("marshal result[%d]: %v", i, err)
		}
		keptData, err := json.Marshal(kept[j].Object)
		if err != nil {
			t.Fatalf("marshal kept[%d]: %v", j, err)
		}
		if !bytes.Equal(gotData, keptData) {
			t.Errorf(
				"WithoutRBACBindings changed %s %q (order and content must be preserved)",
				kept[j].GetKind(),
				kept[j].GetName(),
			)
		}
		i++
	}
}

// testObj builds a minimal unstructured object with the given identity for
// the first-apply filter tests.
func testObj(apiVersion, kind, name, namespace string) unstructured.Unstructured {
	meta := map[string]interface{}{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   meta,
	}}
}

// bindingTestObj builds a minimal unstructured ClusterRoleBinding or
// RoleBinding with a roleRef and a ServiceAccount subject, the shape the
// vendored provider manifests carry.
func bindingTestObj(kind, name, namespace, roleName string) unstructured.Unstructured {
	obj := testObj("rbac.authorization.k8s.io/v1", kind, name, namespace)
	roleKind := "ClusterRole"
	if kind == "RoleBinding" {
		roleKind = "Role"
	}
	subjectNamespace := "capi-system"
	if namespace != "" {
		subjectNamespace = namespace
	}
	obj.Object["roleRef"] = map[string]interface{}{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     roleKind,
		"name":     roleName,
	}
	obj.Object["subjects"] = []interface{}{
		map[string]interface{}{
			"apiGroup":  "rbac.authorization.k8s.io",
			"kind":      "ServiceAccount",
			"name":      "capi-manager",
			"namespace": subjectNamespace,
		},
	}
	return obj
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

// The tests below cover the hypervisor integration of the setup pipeline
// (REQ-001, REQ-002, VC-01 CRD/RBAC clauses): the vendored load set gains the
// three hypervisor trees, the keep filter admits their CRDs and RBAC while
// dropping workload kinds, the CRD Established wait list is derived from the
// loaded manifests, and the hypervisor-system ServiceAccount bindings rewrite
// to the hypervisor manager CN User.

// hypervisorInfraDocs is the infrastructure-hypervisor provider.yaml fixture:
// the three infrastructure-group hypervisor CRDs plus a Deployment that the
// keep filter must drop (the manager runs outside the pod, REQ-002).
const hypervisorInfraDocs = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: hypervisorclusters.infrastructure.cluster.x-k8s.io
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: hypervisormachines.infrastructure.cluster.x-k8s.io
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: hypervisormachinetemplates.infrastructure.cluster.x-k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hypervisor-controller-manager
  namespace: hypervisor-system
`

// hypervisorBootstrapDocs is the bootstrap-hypervisor provider.yaml fixture:
// the bootstrap-group hypervisor CRD plus a RoleBinding whose ServiceAccount
// subject lives in hypervisor-system (REQ-002, REQ-004).
const hypervisorBootstrapDocs = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: hypervisorconfigs.bootstrap.cluster.x-k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: hypervisor-leader-election-rolebinding
  namespace: hypervisor-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: hypervisor-leader-election-role
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: ServiceAccount
  name: hypervisor-controller-manager
  namespace: hypervisor-system
`

// hypervisorControlPlaneDocs is the control-plane-hypervisor provider.yaml
// fixture: the controlplane-group hypervisor CRD plus the two webhook
// configuration kinds the setup container rewrites (REQ-002).
const hypervisorControlPlaneDocs = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: hypervisorcontrolplanes.controlplane.cluster.x-k8s.io
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: hypervisor-mutating-webhook-configuration
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: hypervisor-validating-webhook-configuration
`

// hypervisorCRDNames lists the five hypervisor CRDs REQ-002 names.
var hypervisorCRDNames = []string{
	"hypervisorclusters.infrastructure.cluster.x-k8s.io",
	"hypervisormachines.infrastructure.cluster.x-k8s.io",
	"hypervisormachinetemplates.infrastructure.cluster.x-k8s.io",
	"hypervisorconfigs.bootstrap.cluster.x-k8s.io",
	"hypervisorcontrolplanes.controlplane.cluster.x-k8s.io",
}

// TestProviderManifestFilesIncludesHypervisorTrees verifies the setup load
// set covers the three vendored hypervisor trees alongside the four existing
// ones (REQ-001, REQ-002): against a fixture root holding all seven
// provider.yaml files, ProviderManifestFiles must return exactly those seven
// paths, each pointing at an existing file.
func TestProviderManifestFilesIncludesHypervisorTrees(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProviderTree(t, root, map[string]string{
		"core":                      "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-system\n",
		"cabpk":                     "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-kubeadm-bootstrap-system\n",
		"kcp":                       "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-kubeadm-control-plane-system\n",
		"capd":                      "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capd-system\n",
		"infrastructure-hypervisor": hypervisorInfraDocs,
		"bootstrap-hypervisor":      hypervisorBootstrapDocs,
		"control-plane-hypervisor":  hypervisorControlPlaneDocs,
	})
	want := map[string]bool{}
	for _, dir := range []string{
		"core", "cabpk", "kcp", "capd",
		"infrastructure-hypervisor", "bootstrap-hypervisor", "control-plane-hypervisor",
	} {
		want[filepath.Join(root, dir, "provider.yaml")] = true
	}
	got := capishim.ProviderManifestFiles(root)
	if len(got) != len(want) {
		t.Errorf("ProviderManifestFiles returned %d paths, want %d: %v", len(got), len(want), got)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("ProviderManifestFiles returned unexpected path %q", path)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("ProviderManifestFiles path %q does not exist in the fixture tree: %v", path, err)
		}
	}
	for path := range want {
		found := false
		for _, gotPath := range got {
			if gotPath == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProviderManifestFiles is missing %q (REQ-001)", path)
		}
	}
}

// TestKeepObjectsHypervisorMultiDocManifest verifies the keep transform over
// a synthetic multi-document hypervisor manifest (REQ-002, VC-01): exactly
// the five hypervisor CRDs, the four RBAC kinds, the two webhook
// configuration kinds, and the Namespace survive, and the manager Deployment
// is dropped.
func TestKeepObjectsHypervisorMultiDocManifest(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "provider.yaml")
	docs := hypervisorInfraDocs + "---\n" + hypervisorBootstrapDocs + "---\n" +
		hypervisorControlPlaneDocs + "---\n" +
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: hypervisor-system\n---\n" +
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: hypervisor-manager-role\n---\n" +
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: hypervisor-manager-rolebinding\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: hypervisor-manager-role\nsubjects:\n- apiGroup: rbac.authorization.k8s.io\n  kind: ServiceAccount\n  name: hypervisor-controller-manager\n  namespace: hypervisor-system\n---\n" +
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: hypervisor-leader-election-role\n  namespace: hypervisor-system\n"
	if err := os.WriteFile(path, []byte(docs), 0o644); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
	loaded, err := manifests.Load(path)
	if err != nil {
		t.Fatalf("load fixture manifest: %v", err)
	}
	if len(loaded) != 13 {
		t.Fatalf("fixture parsed as %d objects, want 13 (5 CRDs, Deployment, 4 RBAC, 2 webhooks, Namespace)", len(loaded))
	}
	var kept []unstructured.Unstructured
	for i := range loaded {
		if manifests.Keep(&loaded[i]) {
			kept = append(kept, loaded[i])
		}
	}
	if len(kept) != 12 {
		t.Errorf("keep filter kept %d of %d objects, want 12 (Deployment dropped)", len(kept), len(loaded))
	}
	crds := make(map[string]bool)
	for i := range kept {
		switch {
		case kept[i].GetKind() == "CustomResourceDefinition":
			crds[kept[i].GetName()] = true
		case kept[i].GetKind() == "Deployment":
			t.Errorf("Deployment %q survived the keep filter; the manager runs outside the pod (REQ-002)", kept[i].GetName())
		}
	}
	for _, name := range hypervisorCRDNames {
		if !crds[name] {
			t.Errorf("hypervisor CRD %q was dropped by the keep filter (REQ-002)", name)
		}
	}
}

// TestSetupPipelineKeepsHypervisorObjectsFromLoadSet runs the pipeline
// composition setup uses — ProviderManifestFiles -> manifests.Load ->
// manifests.Keep — over a full seven-tree fixture root and asserts the five
// hypervisor CRDs reach the kept set while the manager Deployment does not
// (REQ-002, VC-01).
func TestSetupPipelineKeepsHypervisorObjectsFromLoadSet(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProviderTree(t, root, map[string]string{
		"core":                      "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-system\n",
		"cabpk":                     "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-kubeadm-bootstrap-system\n",
		"kcp":                       "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-kubeadm-control-plane-system\n",
		"capd":                      "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: capd-system\n",
		"infrastructure-hypervisor": hypervisorInfraDocs,
		"bootstrap-hypervisor":      hypervisorBootstrapDocs,
		"control-plane-hypervisor":  hypervisorControlPlaneDocs,
	})
	loaded, err := manifests.Load(capishim.ProviderManifestFiles(root)...)
	if err != nil {
		t.Fatalf("load provider manifests: %v", err)
	}
	deployments := 0
	var kept []unstructured.Unstructured
	for i := range loaded {
		if loaded[i].GetKind() == "Deployment" {
			deployments++
		}
		if manifests.Keep(&loaded[i]) {
			kept = append(kept, loaded[i])
		}
	}
	if deployments != 1 {
		t.Fatalf("fixture load set carries %d Deployments, want 1", deployments)
	}
	crds := make(map[string]bool)
	for i := range kept {
		if kept[i].GetKind() == "CustomResourceDefinition" {
			crds[kept[i].GetName()] = true
		}
	}
	for _, name := range hypervisorCRDNames {
		if !crds[name] {
			t.Errorf("hypervisor CRD %q missing from the kept set; the setup load set does not include its tree (REQ-002)", name)
		}
	}
	if len(kept) != len(loaded)-deployments {
		t.Errorf(
			"kept %d of %d objects after dropping %d Deployments; the filter dropped more than the Deployment",
			len(kept),
			len(loaded),
			deployments,
		)
	}
}

// TestCRDNamesDerivedFromLoadedManifests pins REQ-002's derivation clause: the
// Established wait list must come from the loaded manifests, so a CRD absent
// from the hardcoded CRDNames() literal is still waited on. The test targets
// the to-be-extracted seam crdNamesFrom(objs []unstructured.Unstructured)
// []string in package main of cmd/capishim. NOTE FOR THE IMPLEMENTER: this
// file is package main_test, so the seam must be reachable from the external
// test package — either export it or move this one test into an internal
// package-main test file. Until the seam exists the whole test package fails
// to compile; that compile failure is the expected red evidence.
func TestCRDNamesDerivedFromLoadedManifests(t *testing.T) {
	t.Parallel()
	const synthetic = "newskind.example.com"
	objs := []unstructured.Unstructured{
		testObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", synthetic, ""),
		testObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "clusters.cluster.x-k8s.io", ""),
		testObj("apps/v1", "Deployment", "hypervisor-controller-manager", "hypervisor-system"),
	}
	got := capishim.CrdNamesFrom(objs)
	found := false
	for _, name := range got {
		if name == synthetic {
			found = true
		}
	}
	if !found {
		t.Errorf(
			"crdNamesFrom omitted %q; a vendored CRD outside the hardcoded literal would never be waited on (REQ-002)",
			synthetic,
		)
	}
	found = false
	for _, name := range got {
		if name == "clusters.cluster.x-k8s.io" {
			found = true
		}
	}
	if !found {
		t.Errorf("crdNamesFrom omitted clusters.cluster.x-k8s.io from the derived wait list")
	}
	for _, name := range got {
		if name == "hypervisor-controller-manager" {
			t.Errorf("crdNamesFrom admitted non-CRD object %q", name)
		}
	}
	for _, name := range capishim.CRDNames() {
		if name == synthetic {
			t.Errorf(
				"%q is present in the hardcoded CRDNames literal; the fixture no longer proves derivation (REQ-002)",
				synthetic,
			)
		}
	}
	empty := capishim.CrdNamesFrom(nil)
	if len(empty) != 0 {
		t.Errorf("crdNamesFrom(nil) = %v, want empty", empty)
	}
}

// TestManagerCNByNamespaceCoversHypervisorSystem verifies the namespace-to-CN
// map carries the hypervisor entry REQ-004 requires. Forward dependency: the
// entry derives from config.Components(), so landing this assertion green
// requires the ComponentHypervisor spec entry (TASK-009); until then the map
// lookup misses and the test stays red.
func TestManagerCNByNamespaceCoversHypervisorSystem(t *testing.T) {
	t.Parallel()
	got := capishim.ManagerCNByNamespace()
	if got["hypervisor-system"] != "capishim:hypervisor-manager" {
		t.Errorf(
			"ManagerCNByNamespace()[%q] = %q, want %q (REQ-004)",
			"hypervisor-system",
			got["hypervisor-system"],
			"capishim:hypervisor-manager",
		)
	}
}

// TestRewriteRBACSubjectsHypervisorSystem verifies the RBAC rewrite end to end
// for the hypervisor namespace (REQ-002, REQ-004, VC-01): a RoleBinding whose
// ServiceAccount subject lives in hypervisor-system becomes a User subject
// named by the hypervisor manager CN, a mixed-namespace binding maps each
// subject through its own namespace's CN, and the input binding is not
// mutated.
func TestRewriteRBACSubjectsHypervisorSystem(t *testing.T) {
	t.Parallel()
	binding := bindingTestObj(
		"RoleBinding",
		"hypervisor-leader-election-rolebinding",
		"hypervisor-system",
		"hypervisor-leader-election-role",
	)
	snapshot, err := json.Marshal(binding.Object)
	if err != nil {
		t.Fatalf("marshal input binding: %v", err)
	}
	rewritten, err := manifests.RewriteRBACSubjects([]unstructured.Unstructured{binding}, capishim.ManagerCNByNamespace())
	if err != nil {
		t.Fatalf("RewriteRBACSubjects returned error: %v", err)
	}
	subjects, ok := rewritten[0].Object["subjects"].([]interface{})
	if !ok || len(subjects) != 1 {
		t.Fatalf(
			"rewritten binding carries %T subjects (%v), want one rewritten subject",
			rewritten[0].Object["subjects"],
			rewritten[0].Object["subjects"],
		)
	}
	subject, ok := subjects[0].(map[string]interface{})
	if !ok {
		t.Fatalf("subject is %T, want an object", subjects[0])
	}
	if subject["kind"] != "User" || subject["name"] != "capishim:hypervisor-manager" {
		t.Errorf("rewritten subject = %v, want User capishim:hypervisor-manager (REQ-004)", subject)
	}
	after, err := json.Marshal(binding.Object)
	if err != nil {
		t.Fatalf("marshal input binding after call: %v", err)
	}
	if !bytes.Equal(snapshot, after) {
		t.Errorf("RewriteRBACSubjects mutated its input:\nbefore: %s\nafter:  %s", snapshot, after)
	}

	mixed := testObj("rbac.authorization.k8s.io/v1", "RoleBinding", "hypervisor-mixed-rolebinding", "hypervisor-system")
	mixed.Object["roleRef"] = map[string]interface{}{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "Role",
		"name":     "hypervisor-manager-role",
	}
	mixed.Object["subjects"] = []interface{}{
		map[string]interface{}{
			"apiGroup":  "rbac.authorization.k8s.io",
			"kind":      "ServiceAccount",
			"name":      "hypervisor-controller-manager",
			"namespace": "hypervisor-system",
		},
		map[string]interface{}{
			"apiGroup":  "rbac.authorization.k8s.io",
			"kind":      "ServiceAccount",
			"name":      "capi-manager",
			"namespace": "capi-system",
		},
	}
	rewritten, err = manifests.RewriteRBACSubjects([]unstructured.Unstructured{mixed}, capishim.ManagerCNByNamespace())
	if err != nil {
		t.Fatalf("RewriteRBACSubjects(mixed) returned error: %v", err)
	}
	subjects, ok = rewritten[0].Object["subjects"].([]interface{})
	if !ok || len(subjects) != 2 {
		t.Fatalf("rewritten mixed binding carries %v subjects, want two", rewritten[0].Object["subjects"])
	}
	first, _ := subjects[0].(map[string]interface{})
	second, _ := subjects[1].(map[string]interface{})
	if first["name"] != "capishim:hypervisor-manager" {
		t.Errorf("mixed subject[0] = %v, want User capishim:hypervisor-manager", first)
	}
	if second["name"] != "capishim:core-manager" {
		t.Errorf("mixed subject[1] = %v, want User capishim:core-manager", second)
	}
}

// writeProviderTree materializes a fixture manifests root: one provider.yaml
// per entry of trees under root/<dir>/provider.yaml.
func writeProviderTree(t *testing.T, root string, trees map[string]string) {
	t.Helper()
	for dir, content := range trees {
		dirPath := filepath.Join(root, dir)
		if err := os.MkdirAll(dirPath, 0o755); err != nil {
			t.Fatalf("create fixture dir %s: %v", dirPath, err)
		}
		if err := os.WriteFile(filepath.Join(dirPath, "provider.yaml"), []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", filepath.Join(dirPath, "provider.yaml"), err)
		}
	}
}
