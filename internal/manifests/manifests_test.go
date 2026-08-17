// Package manifests_test contains the red-phase tests for the internal/manifests
// API designed in TASK-008 (test-first). These tests lock the contract that
// TASK-009 must implement for REQ-003 (CRD installation + Established wait) and
// REQ-004 (RBAC provisioning with ServiceAccount->User subject rewrite):
//
//   - Parse/Load turn the vendored, kustomize-rendered provider.yaml multi-doc
//     YAML files (templates/manifests/<provider>/provider.yaml) into
//     unstructured objects (plan assumption 4: fixtures come from the vendored
//     manifests, never hand-authored).
//   - Keep filters to the eight kinds capishim applies: Namespace,
//     CustomResourceDefinition, ClusterRole, ClusterRoleBinding, Role,
//     RoleBinding, MutatingWebhookConfiguration, ValidatingWebhookConfiguration.
//     Workloads (Deployment/Service/ServiceAccount) and cert-manager objects
//     (Certificate/Issuer) are dropped.
//   - Apply is an idempotent create-or-update against a dynamic client. The
//     fake tracker cannot create via server-side apply (its Apply path does a
//     Get first), so the implementation must create missing objects explicitly.
//     Apply refuses duplicate identifiers (a cross-provider collision must not
//     be silently overwritten) and refuses kinds it cannot map to a GVR.
//   - WaitForCRDEstablished polls the apiextensions client until every CRD in
//     the REQ-003 list reports Established=True, and errors on timeout, a
//     missing CRD, or a CRD that never establishes.
//
// Until TASK-009 lands, the import below does not resolve and `go test
// ./internal/manifests` fails to compile. That failure is the red phase.
package manifests_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moeryomenko/capishim/internal/manifests"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// allVendoredProviders is the set of provider manifests vendored by TASK-004.
func allVendoredProviders() []string {
	return []string{"core", "cabpk", "kcp", "capd"}
}

// vendoredKindCounts returns the full per-kind document contract of the
// vendored provider.yaml fixtures, verified against templates/manifests at
// TASK-008 time. A deliberate vendor refresh (make vendor-templates) that
// changes these counts fails here until the table is updated, which is the
// point: the package contract is derived from the vendored fixtures.
func vendoredKindCounts() map[string]map[string]int {
	return map[string]map[string]int{
		"core": {
			"Namespace": 1, "CustomResourceDefinition": 13,
			"ClusterRole": 2, "ClusterRoleBinding": 1, "Role": 1, "RoleBinding": 1,
			"MutatingWebhookConfiguration": 1, "ValidatingWebhookConfiguration": 1,
			"ServiceAccount": 1, "Service": 1, "Deployment": 1,
			"Certificate": 1, "Issuer": 1,
		},
		"cabpk": {
			"Namespace": 1, "CustomResourceDefinition": 2,
			"ClusterRole": 1, "ClusterRoleBinding": 1, "Role": 1, "RoleBinding": 1,
			"MutatingWebhookConfiguration": 1, "ValidatingWebhookConfiguration": 1,
			"ServiceAccount": 1, "Service": 1, "Deployment": 1,
			"Certificate": 1, "Issuer": 1,
		},
		"kcp": {
			"Namespace": 1, "CustomResourceDefinition": 2,
			"ClusterRole": 2, "ClusterRoleBinding": 1, "Role": 1, "RoleBinding": 1,
			"MutatingWebhookConfiguration": 1, "ValidatingWebhookConfiguration": 1,
			"ServiceAccount": 1, "Service": 1, "Deployment": 1,
			"Certificate": 1, "Issuer": 1,
		},
		"capd": {
			"Namespace": 1, "CustomResourceDefinition": 12,
			"ClusterRole": 1, "ClusterRoleBinding": 1, "Role": 1, "RoleBinding": 1,
			"MutatingWebhookConfiguration": 1, "ValidatingWebhookConfiguration": 1,
			"ServiceAccount": 1, "Service": 1, "Deployment": 1,
			"Certificate": 1, "Issuer": 1,
		},
	}
}

// capiCRDNames returns the REQ-003 PASS-condition list of CAPI CRDs that must
// report Established=True after setup, as fully qualified CRD names.
func capiCRDNames() []string {
	return []string{
		"clusterclasses.cluster.x-k8s.io",
		"clusterresourcesets.addons.cluster.x-k8s.io",
		"clusters.cluster.x-k8s.io",
		"devclusters.infrastructure.cluster.x-k8s.io",
		"devclustertemplates.infrastructure.cluster.x-k8s.io",
		"devmachines.infrastructure.cluster.x-k8s.io",
		"devmachinetemplates.infrastructure.cluster.x-k8s.io",
		"devmachinepooltemplates.infrastructure.cluster.x-k8s.io",
		"kubeadmconfigs.bootstrap.cluster.x-k8s.io",
		"kubeadmconfigtemplates.bootstrap.cluster.x-k8s.io",
		"kubeadmcontrolplanes.controlplane.cluster.x-k8s.io",
		"machinedeployments.cluster.x-k8s.io",
		"machinehealthchecks.cluster.x-k8s.io",
		"machines.cluster.x-k8s.io",
		"machinesets.cluster.x-k8s.io",
	}
}

func vendoredPath(provider string) string {
	return filepath.Join("..", "..", "templates", "manifests", provider, "provider.yaml")
}

func mustLoadVendored(t *testing.T, providers ...string) []unstructured.Unstructured {
	t.Helper()
	if len(providers) == 0 {
		providers = allVendoredProviders()
	}
	paths := make([]string, 0, len(providers))
	for _, p := range providers {
		paths = append(paths, vendoredPath(p))
	}
	objs, err := manifests.Load(paths...)
	if err != nil {
		t.Fatalf("Load(%v): %v", paths, err)
	}
	return objs
}

func keepOnly(t *testing.T, objs []unstructured.Unstructured) []unstructured.Unstructured {
	t.Helper()
	kept := make([]unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		if manifests.Keep(&obj) {
			kept = append(kept, obj)
		}
	}
	return kept
}

func kindCounts(objs []unstructured.Unstructured) map[string]int {
	counts := make(map[string]int, len(objs))
	for i := range objs {
		counts[objs[i].GetKind()]++
	}
	return counts
}

func newDynamicFake(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		rbacv1.AddToScheme,
		admissionregistrationv1.AddToScheme,
		apiextv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	return dynamicfake.NewSimpleDynamicClient(scheme)
}

func listCount(t *testing.T, client dynamic.Interface, gvr schema.GroupVersionResource) int {
	t.Helper()
	list, err := client.Resource(gvr).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List(%s): %v", gvr.Resource, err)
	}
	return len(list.Items)
}

// keptGVRCounts is the expected applied-object count per kept kind when all
// four providers' rendered manifests are filtered and applied. The totals are
// derived from vendoredKindCounts: 4 namespaces, 29 CRDs (13+2+2+12), 6
// ClusterRoles (2+1+2+1), 4 ClusterRoleBindings, 4 Roles, 4 RoleBindings, and
// 8 webhook configurations (2 per provider).
func keptGVRCounts() map[schema.GroupVersionResource]int {
	return map[schema.GroupVersionResource]int{
		corev1.SchemeGroupVersion.WithResource("namespaces"):                                       4,
		apiextv1.SchemeGroupVersion.WithResource("customresourcedefinitions"):                      29,
		rbacv1.SchemeGroupVersion.WithResource("clusterroles"):                                     6,
		rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings"):                              4,
		rbacv1.SchemeGroupVersion.WithResource("roles"):                                            4,
		rbacv1.SchemeGroupVersion.WithResource("rolebindings"):                                     4,
		admissionregistrationv1.SchemeGroupVersion.WithResource("mutatingwebhookconfigurations"):   4,
		admissionregistrationv1.SchemeGroupVersion.WithResource("validatingwebhookconfigurations"): 4,
	}
}

func byName(t *testing.T, items []unstructured.Unstructured, name string) *unstructured.Unstructured {
	t.Helper()
	for i := range items {
		if items[i].GetName() == name {
			return &items[i]
		}
	}
	t.Fatalf("object %q not found among %d items", name, len(items))
	return nil
}

func TestParseVendoredProviders(t *testing.T) {
	t.Parallel()
	for _, p := range allVendoredProviders() {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			f, err := os.Open(vendoredPath(p))
			if err != nil {
				t.Fatalf("open fixture %s: %v", vendoredPath(p), err)
			}
			defer f.Close()

			objs, err := manifests.Parse(f)
			if err != nil {
				t.Fatalf("Parse(%s): %v", p, err)
			}

			for i := range objs {
				if objs[i].GetAPIVersion() == "" || objs[i].GetKind() == "" {
					t.Errorf("Parse(%s) doc %d missing apiVersion/kind", p, i)
				}
			}

			got := kindCounts(objs)
			want := vendoredKindCounts()[p]
			for kind, n := range want {
				if got[kind] != n {
					t.Errorf("Parse(%s) kind %s count = %d, want %d", p, kind, got[kind], n)
				}
			}
			for kind, n := range got {
				if _, ok := want[kind]; !ok {
					t.Errorf("Parse(%s) produced unexpected kind %s (%d docs)", p, kind, n)
				}
			}

			total := 0
			for _, n := range want {
				total += n
			}
			if len(objs) != total {
				t.Errorf("Parse(%s) total docs = %d, want %d", p, len(objs), total)
			}
		})
	}
}

func TestParseMultiDoc(t *testing.T) {
	t.Parallel()
	doc := `apiVersion: v1
kind: Namespace
metadata:
  name: capi-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: capi-manager-role
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: capi-controller-manager
  namespace: capi-system
`
	objs, err := manifests.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("Parse produced %d docs, want 3", len(objs))
	}
	wantKinds := []string{"Namespace", "ClusterRole", "Deployment"}
	for i, want := range wantKinds {
		if got := objs[i].GetKind(); got != want {
			t.Errorf("doc %d kind = %q, want %q", i, got, want)
		}
	}
	if got := objs[0].GetName(); got != "capi-system" {
		t.Errorf("doc 0 name = %q, want %q", got, "capi-system")
	}
	if got := objs[2].GetNamespace(); got != "capi-system" {
		t.Errorf("doc 2 namespace = %q, want %q", got, "capi-system")
	}
}

func TestParseLeadingTrailingSeparators(t *testing.T) {
	t.Parallel()
	doc := "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: capi-system\n---\n"
	objs, err := manifests.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 1 {
		t.Errorf("Parse produced %d docs, want 1 (stray separators ignored)", len(objs))
	}
}

func TestParseEmpty(t *testing.T) {
	t.Parallel()
	for _, r := range []io.Reader{
		strings.NewReader(""),
		strings.NewReader("---\n---\n"),
	} {
		objs, err := manifests.Parse(r)
		if err != nil {
			t.Errorf("Parse(empty) returned error: %v", err)
		}
		if len(objs) != 0 {
			t.Errorf("Parse(empty) produced %d docs, want 0", len(objs))
		}
	}
}

func TestParseInvalidYAML(t *testing.T) {
	t.Parallel()
	doc := "apiVersion: v1\nkind: Namespace\nmetadata: [unclosed\n"
	if _, err := manifests.Parse(strings.NewReader(doc)); err == nil {
		t.Error("Parse(malformed YAML) returned no error")
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := manifests.Load("does-not-exist.yaml"); err == nil {
		t.Error("Load of a missing file returned no error")
	}
}

func TestLoadNoPaths(t *testing.T) {
	t.Parallel()
	objs, err := manifests.Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("Load() produced %d objects, want 0", len(objs))
	}
}

func TestApplyAllProviders(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := newDynamicFake(t)

	objs := keepOnly(t, mustLoadVendored(t))
	if err := manifests.Apply(ctx, client, objs); err != nil {
		t.Fatalf("Apply(all four providers) returned error: %v", err)
	}

	// The applied result contains exactly the kept kinds, in the counts
	// implied by the vendored fixtures.
	for gvr, want := range keptGVRCounts() {
		if got := listCount(t, client, gvr); got != want {
			t.Errorf("applied %s count = %d, want %d", gvr.Resource, got, want)
		}
	}

	// Dropped kinds must never be written, even if a fixture were refreshed
	// with new workload/cert-manager objects (they are filtered before Apply).
	dropped := map[string]bool{
		"deployments": true, "services": true, "serviceaccounts": true,
		"certificates": true, "issuers": true,
	}
	for _, a := range client.Actions() {
		// The concrete *ActionImpl structs, not the Action interfaces: the
		// Create/Update interfaces are structurally identical and gocritic
		// flags any ordering of them in a type switch.
		switch a.(type) {
		case clienttesting.CreateActionImpl, clienttesting.PatchActionImpl, clienttesting.UpdateActionImpl:
			if dropped[a.GetResource().Resource] {
				t.Errorf("Apply wrote dropped kind %s", a.GetResource().Resource)
			}
		}
	}

	// Kustomize-prefixed names and provider namespaces survive (assumption 4).
	wantNamespaces := map[string]bool{
		"capi-system": true, "capi-kubeadm-bootstrap-system": true,
		"capi-kubeadm-control-plane-system": true, "capd-system": true,
	}
	nsList, err := client.Resource(corev1.SchemeGroupVersion.WithResource("namespaces")).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List(namespaces): %v", err)
	}
	for _, ns := range nsList.Items {
		if !wantNamespaces[ns.GetName()] {
			t.Errorf("unexpected applied Namespace %q", ns.GetName())
		}
	}

	wantCRBs := map[string]bool{
		"capi-manager-rolebinding": true, "capi-kubeadm-bootstrap-manager-rolebinding": true,
		"capi-kubeadm-control-plane-manager-rolebinding": true, "capd-manager-rolebinding": true,
	}
	crbList, err := client.Resource(rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings")).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List(clusterrolebindings): %v", err)
	}
	for _, crb := range crbList.Items {
		if !wantCRBs[crb.GetName()] {
			t.Errorf("unexpected applied ClusterRoleBinding %q", crb.GetName())
		}
	}
}

func TestApplyIdempotent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := newDynamicFake(t)
	objs := keepOnly(t, mustLoadVendored(t))

	if err := manifests.Apply(ctx, client, objs); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := manifests.Apply(ctx, client, objs); err != nil {
		t.Fatalf("second Apply returned error, want idempotent no-op: %v", err)
	}

	for gvr, want := range keptGVRCounts() {
		if got := listCount(t, client, gvr); got != want {
			t.Errorf("after second Apply, %s count = %d, want %d (nothing duplicated)", gvr.Resource, got, want)
		}
	}
}

func TestApplyEmpty(t *testing.T) {
	t.Parallel()
	client := newDynamicFake(t)
	if err := manifests.Apply(t.Context(), client, nil); err != nil {
		t.Errorf("Apply(nil) returned error: %v", err)
	}
	if err := manifests.Apply(t.Context(), client, []unstructured.Unstructured{}); err != nil {
		t.Errorf("Apply(empty) returned error: %v", err)
	}
}

func TestApplyDuplicateIdentifiers(t *testing.T) {
	t.Parallel()
	client := newDynamicFake(t)
	obj := func(name string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": name},
		}}
	}

	// Two cluster-scoped Namespace objects with the same name would silently
	// overwrite each other (a cross-provider collision); Apply must refuse.
	dup := []unstructured.Unstructured{obj("same"), obj("same")}
	if err := manifests.Apply(t.Context(), client, dup); err == nil {
		t.Error("Apply of two objects with the same kind/name returned no error, want collision error")
	}
}

func TestApplySameNameDifferentNamespaces(t *testing.T) {
	t.Parallel()
	client := newDynamicFake(t)
	rb := func(ns string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata": map[string]interface{}{
				"name":      "shared",
				"namespace": ns,
			},
		}}
	}

	objs := []unstructured.Unstructured{rb("ns-a"), rb("ns-b")}
	if err := manifests.Apply(t.Context(), client, objs); err != nil {
		t.Fatalf("Apply of same-named RoleBindings in different namespaces returned error: %v", err)
	}
	if got := listCount(t, client, rbacv1.SchemeGroupVersion.WithResource("rolebindings")); got != 2 {
		t.Errorf("applied rolebindings count = %d, want 2 (namespaces disambiguate)", got)
	}
}

func TestApplyRejectsUnmappedKind(t *testing.T) {
	t.Parallel()
	client := newDynamicFake(t)
	dep := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "nope"},
	}}
	if err := manifests.Apply(t.Context(), client, []unstructured.Unstructured{dep}); err == nil {
		t.Error("Apply of a Deployment (unmapped kind) returned no error, want error")
	}
}

func TestWaitForCRDEstablished(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := apiextensionsfake.NewSimpleClientset(crdsFor(capiCRDNames(), true)...)
	if err := manifests.WaitForCRDEstablished(ctx, client.ApiextensionsV1(), capiCRDNames(), 5*time.Second); err != nil {
		t.Fatalf("WaitForCRDEstablished with all Established=True returned error: %v", err)
	}
}

func TestWaitForCRDOneNeverEstablishes(t *testing.T) {
	t.Parallel()
	client := apiextensionsfake.NewSimpleClientset(
		append(crdsFor([]string{"clusters.cluster.x-k8s.io"}, true),
			crdsFor([]string{"machines.cluster.x-k8s.io"}, false)...)...,
	)
	names := []string{"clusters.cluster.x-k8s.io", "machines.cluster.x-k8s.io"}
	if err := manifests.WaitForCRDEstablished(t.Context(), client.ApiextensionsV1(), names, 120*time.Millisecond); err == nil {
		t.Error("WaitForCRDEstablished with one CRD never Established returned no error, want timeout/error")
	}
}

func TestWaitForCRDMissingCRD(t *testing.T) {
	t.Parallel()
	client := apiextensionsfake.NewSimpleClientset(crdsFor([]string{"clusters.cluster.x-k8s.io"}, true)...)
	names := []string{"clusters.cluster.x-k8s.io", "machines.cluster.x-k8s.io"}
	if err := manifests.WaitForCRDEstablished(t.Context(), client.ApiextensionsV1(), names, 120*time.Millisecond); err == nil {
		t.Error("WaitForCRDEstablished with a missing CRD returned no error, want timeout/error")
	}
}

func TestWaitForCRDEmptyNames(t *testing.T) {
	t.Parallel()
	client := apiextensionsfake.NewSimpleClientset()
	if err := manifests.WaitForCRDEstablished(t.Context(), client.ApiextensionsV1(), nil, 5*time.Second); err != nil {
		t.Errorf("WaitForCRDEstablished with no names returned error: %v", err)
	}
}

func TestWaitForCRDEstablishesOverTime(t *testing.T) {
	t.Parallel()
	cr := crd("clusters.cluster.x-k8s.io", false)
	client := apiextensionsfake.NewSimpleClientset(cr)

	var gets int
	client.PrependReactor("get", "customresourcedefinitions", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 3 {
			established := cr.DeepCopy()
			established.Status.Conditions = []apiextv1.CustomResourceDefinitionCondition{
				{Type: apiextv1.Established, Status: apiextv1.ConditionTrue},
			}
			gvr := apiextv1.SchemeGroupVersion.WithResource("customresourcedefinitions")
			if err := client.Tracker().Update(gvr, established, ""); err != nil {
				return false, nil, fmt.Errorf("update tracker: %w", err)
			}
		}
		return false, nil, nil
	})

	if err := manifests.WaitForCRDEstablished(t.Context(), client.ApiextensionsV1(),
		[]string{"clusters.cluster.x-k8s.io"}, 5*time.Second); err != nil {
		t.Fatalf("Wait did not observe the CRD establishing over time: %v", err)
	}
	if gets < 3 {
		t.Errorf("wait performed %d Gets, want at least 3 (must poll until Established)", gets)
	}
}

func TestWaitForCRDWithoutConversionWebhook(t *testing.T) {
	t.Parallel()
	cr := crd("clusters.cluster.x-k8s.io", true)
	cr.Spec.Conversion = &apiextv1.CustomResourceConversion{Strategy: apiextv1.NoneConverter}
	client := apiextensionsfake.NewSimpleClientset(cr)
	if err := manifests.WaitForCRDEstablished(t.Context(), client.ApiextensionsV1(),
		[]string{"clusters.cluster.x-k8s.io"}, 5*time.Second); err != nil {
		t.Errorf("WaitForCRDEstablished for a CRD with conversion strategy None returned error: %v", err)
	}
}

func crdsFor(names []string, established bool) []runtime.Object {
	out := make([]runtime.Object, 0, len(names))
	for _, n := range names {
		out = append(out, crd(n, established))
	}
	return out
}

func crd(name string, established bool) *apiextv1.CustomResourceDefinition {
	cr := &apiextv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if established {
		cr.Status.Conditions = []apiextv1.CustomResourceDefinitionCondition{
			{Type: apiextv1.Established, Status: apiextv1.ConditionTrue},
		}
	}
	return cr
}
