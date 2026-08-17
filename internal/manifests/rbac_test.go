// Red-phase tests for the RBAC half of the internal/manifests contract
// (REQ-004, plan assumption 5). Locked behaviors:
//
//   - RewriteRBACSubjects rewrites every ServiceAccount subject in
//     ClusterRoleBinding and RoleBinding objects to a User named by the
//     provider's manager CN (capishim:{core,cabpk,kcp,capd}-manager, shared
//     with the pki CN contract). The provider CN is looked up from the
//     subject's namespace (capi-system, capi-kubeadm-bootstrap-system,
//     capi-kubeadm-control-plane-system, capd-system).
//   - The manager ClusterRoleBinding roleRef is REDIRECTED from the
//     `<prefix>-aggregated-manager-role` aggregation shell to the concrete
//     `<prefix>-manager-role`. capishim runs no kube-controller-manager, so
//     the ClusterRole aggregation controller never executes and an
//     aggregation-rule binding grants nothing; only a direct binding to the
//     concrete role grants permissions (TASK-018 e2e finding). Bindings that
//     already point at the concrete role are left untouched.
//   - Role/ClusterRole objects pass through untouched, so the aggregation
//     label/rule structure of the core and kcp aggregated manager roles
//     survives verbatim (harmless; the redirect is what grants permissions).
//   - Namespaced RoleBindings stay in the provider namespace.
//   - A RoleBinding without metadata.namespace is an error: placement cannot
//     be determined, and guessing would silently misplace permissions.
//   - A ServiceAccount subject in an unmapped namespace is an error.
//   - Non-ServiceAccount subjects (User/Group) are preserved.
//   - AdminClusterRoleBinding returns the cluster-admin binding for the admin
//     User (named capishim-admin, subject kind User, roleRef cluster-admin).
package manifests_test

import (
	"reflect"
	"testing"

	"github.com/moeryomenko/capishim/internal/manifests"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// providerRBAC is the per-provider RBAC contract derived from the vendored
// manifests: kustomize-prefixed binding names, the provider namespace, the
// manager identity CN (REQ-002/pki contract), and the roleRefs of the manager
// ClusterRoleBinding before (crbOrigRoleRef, as rendered) and after
// (crbRoleRef) the rewrite. crbRoleRef is the CONCRETE manager role the
// binding must point at: for core and kcp the rendered binding references the
// aggregated-manager-role shell, which grants nothing without the ClusterRole
// aggregation controller (absent in capishim), so the rewrite redirects it to
// the concrete role.
type providerRBAC struct {
	dir            string
	namespace      string
	cn             string
	crbName        string
	crbRoleRef     string // roleRef after rewrite: concrete <prefix>-manager-role
	crbOrigRoleRef string // roleRef before rewrite, as rendered in the fixture
	rbName         string
	rbRole         string
}

// allProviderRBACs returns the per-provider RBAC contract derived from the
// vendored manifests: kustomize-prefixed binding names, the provider
// namespace, the manager identity CN (REQ-002/pki contract), and the manager
// ClusterRoleBinding roleRefs before/after the aggregation redirect.
func allProviderRBACs() []providerRBAC {
	return []providerRBAC{
		{
			dir: "core", namespace: "capi-system", cn: "capishim:core-manager",
			crbName: "capi-manager-rolebinding", crbRoleRef: "capi-manager-role",
			crbOrigRoleRef: "capi-aggregated-manager-role",
			rbName:         "capi-leader-election-rolebinding", rbRole: "capi-leader-election-role",
		},
		{
			dir: "cabpk", namespace: "capi-kubeadm-bootstrap-system", cn: "capishim:cabpk-manager",
			crbName: "capi-kubeadm-bootstrap-manager-rolebinding", crbRoleRef: "capi-kubeadm-bootstrap-manager-role",
			crbOrigRoleRef: "capi-kubeadm-bootstrap-manager-role",
			rbName:         "capi-kubeadm-bootstrap-leader-election-rolebinding", rbRole: "capi-kubeadm-bootstrap-leader-election-role",
		},
		{
			dir: "kcp", namespace: "capi-kubeadm-control-plane-system", cn: "capishim:kcp-manager",
			crbName: "capi-kubeadm-control-plane-manager-rolebinding", crbRoleRef: "capi-kubeadm-control-plane-manager-role",
			crbOrigRoleRef: "capi-kubeadm-control-plane-aggregated-manager-role",
			rbName:         "capi-kubeadm-control-plane-leader-election-rolebinding", rbRole: "capi-kubeadm-control-plane-leader-election-role",
		},
		{
			dir: "capd", namespace: "capd-system", cn: "capishim:capd-manager",
			crbName: "capd-manager-rolebinding", crbRoleRef: "capd-manager-role",
			crbOrigRoleRef: "capd-manager-role",
			rbName:         "capd-leader-election-rolebinding", rbRole: "capd-leader-election-role",
		},
	}
}

func providerCNByNamespace() map[string]string {
	m := make(map[string]string, len(allProviderRBACs()))
	for _, p := range allProviderRBACs() {
		m[p.namespace] = p.cn
	}
	return m
}

func findByKindName(t *testing.T, objs []unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i]
		}
	}
	t.Fatalf("object %s/%s not found among %d objects", kind, name, len(objs))
	return nil
}

func requireSubject(t *testing.T, binding *unstructured.Unstructured, idx int, kind, name string) {
	t.Helper()
	subjects, found, err := unstructured.NestedSlice(binding.Object, "subjects")
	if err != nil || !found {
		t.Fatalf("binding %s has no subjects: %v", binding.GetName(), err)
	}
	if len(subjects) <= idx {
		t.Fatalf("binding %s has %d subjects, want more than %d", binding.GetName(), len(subjects), idx)
	}
	s, ok := subjects[idx].(map[string]interface{})
	if !ok {
		t.Fatalf("binding %s subject %d is %T, want map", binding.GetName(), idx, subjects[idx])
	}
	if got := s["kind"]; got != kind {
		t.Errorf("binding %s subject %d kind = %v, want %q", binding.GetName(), idx, got, kind)
	}
	if got := s["name"]; got != name {
		t.Errorf("binding %s subject %d name = %v, want %q", binding.GetName(), idx, got, name)
	}
}

func requireRoleRef(t *testing.T, binding *unstructured.Unstructured, kind, name string) {
	t.Helper()
	gotName := roleRefName(t, binding)
	gotKind, found, err := unstructured.NestedString(binding.Object, "roleRef", "kind")
	if err != nil || !found {
		t.Fatalf("binding %s has no roleRef.kind: %v", binding.GetName(), err)
	}
	if gotKind != kind || gotName != name {
		t.Errorf("binding %s roleRef = %s/%s, want %s/%s", binding.GetName(), gotKind, gotName, kind, name)
	}
}

func roleRefName(t *testing.T, binding *unstructured.Unstructured) string {
	t.Helper()
	name, found, err := unstructured.NestedString(binding.Object, "roleRef", "name")
	if err != nil || !found {
		t.Fatalf("binding %s has no roleRef.name: %v", binding.GetName(), err)
	}
	return name
}

func firstAggregationMatchLabels(t *testing.T, obj *unstructured.Unstructured) map[string]string {
	t.Helper()
	selectors, found, err := unstructured.NestedSlice(obj.Object, "aggregationRule", "clusterRoleSelectors")
	if err != nil || !found || len(selectors) == 0 {
		t.Fatalf("ClusterRole %s has no aggregationRule.clusterRoleSelectors: %v", obj.GetName(), err)
	}
	sel, ok := selectors[0].(map[string]interface{})
	if !ok {
		t.Fatalf("ClusterRole %s selector 0 is %T, want map", obj.GetName(), selectors[0])
	}
	matchLabels, ok := sel["matchLabels"].(map[string]interface{})
	if !ok {
		t.Fatalf("ClusterRole %s selector 0 has no matchLabels", obj.GetName())
	}
	out := make(map[string]string, len(matchLabels))
	for k, v := range matchLabels {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("ClusterRole %s selector matchLabel %q value is %T, want string", obj.GetName(), k, v)
		}
		out[k] = s
	}
	return out
}

func TestRewriteRBACSubjectsVendored(t *testing.T) {
	t.Parallel()
	cnByNS := providerCNByNamespace()
	for _, p := range allProviderRBACs() {
		t.Run(p.dir, func(t *testing.T) {
			t.Parallel()
			objs := mustLoadVendored(t, p.dir)
			rewritten, err := manifests.RewriteRBACSubjects(objs, cnByNS)
			if err != nil {
				t.Fatalf("RewriteRBACSubjects(%s): %v", p.dir, err)
			}
			if len(rewritten) != len(objs) {
				t.Errorf("RewriteRBACSubjects(%s) changed object count: %d -> %d", p.dir, len(objs), len(rewritten))
			}

			// The manager ClusterRoleBinding: subject ServiceAccount -> User CN,
			// roleRef redirected from the aggregation shell (if any) to the
			// concrete manager role.
			inCRB := findByKindName(t, objs, "ClusterRoleBinding", p.crbName)
			requireRoleRef(t, inCRB, "ClusterRole", p.crbOrigRoleRef)
			crb := findByKindName(t, rewritten, "ClusterRoleBinding", p.crbName)
			requireSubject(t, crb, 0, "User", p.cn)
			requireRoleRef(t, crb, "ClusterRole", p.crbRoleRef)

			// The leader-election RoleBinding: namespaced in the provider
			// namespace, subject rewritten, roleRef preserved.
			rb := findByKindName(t, rewritten, "RoleBinding", p.rbName)
			if got := rb.GetNamespace(); got != p.namespace {
				t.Errorf("RoleBinding %s namespace = %q, want %q", p.rbName, got, p.namespace)
			}
			requireSubject(t, rb, 0, "User", p.cn)
			requireRoleRef(t, rb, "Role", p.rbRole)

			// Roles are not rewritten: the concrete manager role (and the
			// aggregated shell where one exists) survive byte-for-byte.
			inRole := findByKindName(t, objs, "ClusterRole", p.crbRoleRef)
			outRole := findByKindName(t, rewritten, "ClusterRole", p.crbRoleRef)
			if !reflect.DeepEqual(outRole.Object, inRole.Object) {
				t.Errorf("RewriteRBACSubjects modified ClusterRole %s; roles must pass through untouched", p.crbRoleRef)
			}
			if p.crbOrigRoleRef != p.crbRoleRef {
				inAgg := findByKindName(t, objs, "ClusterRole", p.crbOrigRoleRef)
				outAgg := findByKindName(t, rewritten, "ClusterRole", p.crbOrigRoleRef)
				if !reflect.DeepEqual(outAgg.Object, inAgg.Object) {
					t.Errorf("RewriteRBACSubjects modified ClusterRole %s; roles must pass through untouched", p.crbOrigRoleRef)
				}
			}
		})
	}
}

func TestRewriteRBACSubjectsRedirectsAggregatedRoleRef(t *testing.T) {
	t.Parallel()
	cnByNS := providerCNByNamespace()
	for _, p := range allProviderRBACs() {
		t.Run(p.dir, func(t *testing.T) {
			t.Parallel()
			objs := mustLoadVendored(t, p.dir)
			rewritten, err := manifests.RewriteRBACSubjects(objs, cnByNS)
			if err != nil {
				t.Fatalf("RewriteRBACSubjects(%s): %v", p.dir, err)
			}

			inCRB := findByKindName(t, objs, "ClusterRoleBinding", p.crbName)
			outCRB := findByKindName(t, rewritten, "ClusterRoleBinding", p.crbName)

			// The fixture binding must carry the recorded original roleRef.
			requireRoleRef(t, inCRB, "ClusterRole", p.crbOrigRoleRef)
			// The rewritten binding must point at the concrete manager role.
			requireRoleRef(t, outCRB, "ClusterRole", p.crbRoleRef)
			// The redirect target must be present as an applied ClusterRole.
			findByKindName(t, rewritten, "ClusterRole", p.crbRoleRef)

			if p.crbOrigRoleRef != p.crbRoleRef {
				// core/kcp: aggregated shell -> concrete role. Without the
				// ClusterRole aggregation controller (no kube-controller-
				// manager in capishim) an aggregation-rule binding grants
				// nothing, so the redirect is mandatory.
				gotOrig := roleRefName(t, inCRB)
				gotNew := roleRefName(t, outCRB)
				if gotOrig == gotNew {
					t.Errorf("binding %s roleRef %q unchanged; want redirect to %q", p.crbName, gotNew, p.crbRoleRef)
				}
			} else {
				// cabpk/capd: the rendered binding already targets the concrete
				// role; it must pass through untouched.
				gotOrig := roleRefName(t, inCRB)
				gotNew := roleRefName(t, outCRB)
				if gotOrig != gotNew {
					t.Errorf("binding %s roleRef changed from %q to %q; want untouched", p.crbName, gotOrig, gotNew)
				}
			}
		})
	}
}

func TestAggregationStructurePreserved(t *testing.T) {
	t.Parallel()
	cnByNS := providerCNByNamespace()
	tests := []struct {
		dir         string
		aggRole     string // aggregated ClusterRole carrying aggregationRule
		managerRole string // ClusterRole carrying the aggregate-to-manager label
		labelKey    string
	}{
		{"core", "capi-aggregated-manager-role", "capi-manager-role", "cluster.x-k8s.io/aggregate-to-manager"},
		{
			dir:         "kcp",
			aggRole:     "capi-kubeadm-control-plane-aggregated-manager-role",
			managerRole: "capi-kubeadm-control-plane-manager-role",
			labelKey:    "kubeadm.controlplane.cluster.x-k8s.io/aggregate-to-manager",
		},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			t.Parallel()
			objs := mustLoadVendored(t, tt.dir)
			rewritten, err := manifests.RewriteRBACSubjects(objs, cnByNS)
			if err != nil {
				t.Fatalf("RewriteRBACSubjects(%s): %v", tt.dir, err)
			}

			agg := findByKindName(t, rewritten, "ClusterRole", tt.aggRole)
			labels := firstAggregationMatchLabels(t, agg)
			if got := labels[tt.labelKey]; got != "true" {
				t.Errorf("aggregationRule selector for %s = %q, want %q", tt.aggRole, got, "true")
			}

			manager := findByKindName(t, rewritten, "ClusterRole", tt.managerRole)
			objLabels, found, err := unstructured.NestedStringMap(manager.Object, "metadata", "labels")
			if err != nil || !found {
				t.Fatalf("ClusterRole %s has no metadata.labels: %v", tt.managerRole, err)
			}
			if got := objLabels[tt.labelKey]; got != "true" {
				t.Errorf("manager role %s label %s = %q, want %q", tt.managerRole, tt.labelKey, got, "true")
			}
		})
	}
}

func TestRewriteRBACSubjectsPreservesOtherKinds(t *testing.T) {
	t.Parallel()
	objs := mustLoadVendored(t, "core")
	rewritten, err := manifests.RewriteRBACSubjects(objs, providerCNByNamespace())
	if err != nil {
		t.Fatalf("RewriteRBACSubjects: %v", err)
	}

	// Non-binding kinds pass through untouched.
	nsIn := findByKindName(t, objs, "Namespace", "capi-system")
	nsOut := findByKindName(t, rewritten, "Namespace", "capi-system")
	if !reflect.DeepEqual(nsOut.Object, nsIn.Object) {
		t.Error("RewriteRBACSubjects modified the Namespace; non-binding kinds must pass through untouched")
	}
	mwhIn := findByKindName(t, objs, "MutatingWebhookConfiguration", "capi-mutating-webhook-configuration")
	mwhOut := findByKindName(t, rewritten, "MutatingWebhookConfiguration", "capi-mutating-webhook-configuration")
	if !reflect.DeepEqual(mwhOut.Object, mwhIn.Object) {
		t.Error(
			"RewriteRBACSubjects modified the MutatingWebhookConfiguration; non-binding kinds must pass through untouched",
		)
	}
}

func TestRewriteRBACSubjectsPreservesNonServiceAccountSubjects(t *testing.T) {
	t.Parallel()
	crb := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]interface{}{"name": "mixed"},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     "some-role",
		},
		"subjects": []interface{}{
			map[string]interface{}{"kind": "ServiceAccount", "name": "capi-manager", "namespace": "capi-system"},
			map[string]interface{}{"kind": "User", "name": "human", "apiGroup": "rbac.authorization.k8s.io"},
			map[string]interface{}{"kind": "Group", "name": "developers", "apiGroup": "rbac.authorization.k8s.io"},
		},
	}}

	out, err := manifests.RewriteRBACSubjects([]unstructured.Unstructured{*crb}, providerCNByNamespace())
	if err != nil {
		t.Fatalf("RewriteRBACSubjects: %v", err)
	}
	// SA rewritten to the manager User; User/Group subjects preserved.
	requireSubject(t, &out[0], 0, "User", "capishim:core-manager")
	requireSubject(t, &out[0], 1, "User", "human")
	requireSubject(t, &out[0], 2, "Group", "developers")
}

func TestRewriteRBACSubjectsUnmappedSubjectNamespace(t *testing.T) {
	t.Parallel()
	crb := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]interface{}{"name": "orphan"},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     "some-role",
		},
		"subjects": []interface{}{
			map[string]interface{}{"kind": "ServiceAccount", "name": "manager", "namespace": "unknown-system"},
		},
	}}
	if _, err := manifests.RewriteRBACSubjects([]unstructured.Unstructured{*crb}, providerCNByNamespace()); err == nil {
		t.Error("RewriteRBACSubjects accepted a ServiceAccount subject in an unmapped namespace, want error")
	}
}

func TestRewriteRBACSubjectsRoleBindingWithoutNamespace(t *testing.T) {
	t.Parallel()
	rb := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata":   map[string]interface{}{"name": "homeless"},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     "leader-election",
		},
		"subjects": []interface{}{
			map[string]interface{}{"kind": "ServiceAccount", "name": "capi-manager", "namespace": "capi-system"},
		},
	}}
	// Design decision: a namespaced RoleBinding with no namespace cannot be
	// placed, and guessing would silently misplace permissions; rewrite fails.
	if _, err := manifests.RewriteRBACSubjects([]unstructured.Unstructured{*rb}, providerCNByNamespace()); err == nil {
		t.Error("RewriteRBACSubjects accepted a RoleBinding without metadata.namespace, want error")
	}
}

func TestRewriteRBACSubjectsOnlyRoles(t *testing.T) {
	t.Parallel()
	objs := mustLoadVendored(t, "core")
	roles := make([]unstructured.Unstructured, 0, 3)
	for _, obj := range objs {
		if obj.GetKind() == "ClusterRole" || obj.GetKind() == "Role" {
			roles = append(roles, obj)
		}
	}
	rewritten, err := manifests.RewriteRBACSubjects(roles, providerCNByNamespace())
	if err != nil {
		t.Fatalf("RewriteRBACSubjects(roles only): %v", err)
	}
	if len(rewritten) != len(roles) {
		t.Fatalf("RewriteRBACSubjects(roles only) changed count: %d -> %d", len(roles), len(rewritten))
	}
	for i := range roles {
		if !reflect.DeepEqual(rewritten[i].Object, roles[i].Object) {
			t.Errorf("RewriteRBACSubjects(roles only) modified object %d", i)
		}
	}
}

func TestAdminClusterRoleBinding(t *testing.T) {
	t.Parallel()
	b := manifests.AdminClusterRoleBinding("capishim:admin")

	if got := b.GetKind(); got != "ClusterRoleBinding" {
		t.Errorf("kind = %q, want %q", got, "ClusterRoleBinding")
	}
	if got := b.GetName(); got != "capishim-admin" {
		t.Errorf("name = %q, want %q", got, "capishim-admin")
	}
	requireRoleRef(t, b, "ClusterRole", "cluster-admin")
	requireSubject(t, b, 0, "User", "capishim:admin")
}

func TestRBACPipelineAllProviders(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := newDynamicFake(t)

	objs := keepOnly(t, mustLoadVendored(t))
	rewritten, err := manifests.RewriteRBACSubjects(objs, providerCNByNamespace())
	if err != nil {
		t.Fatalf("RewriteRBACSubjects: %v", err)
	}
	if err := manifests.Apply(ctx, client, rewritten); err != nil {
		t.Fatalf("Apply(rewritten): %v", err)
	}

	gvr := func(resource string) schema.GroupVersionResource {
		return rbacv1.SchemeGroupVersion.WithResource(resource)
	}

	crbList, err := client.Resource(gvr("clusterrolebindings")).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List(clusterrolebindings): %v", err)
	}
	rbList, err := client.Resource(gvr("rolebindings")).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List(rolebindings): %v", err)
	}

	for _, p := range allProviderRBACs() {
		crb := byName(t, crbList.Items, p.crbName)
		requireSubject(t, crb, 0, "User", p.cn)
		// The applied binding points at the concrete manager role (the
		// aggregation redirect survives the pipeline); the aggregated shell
		// is applied too but grants nothing without the aggregation controller.
		requireRoleRef(t, crb, "ClusterRole", p.crbRoleRef)

		rb := byName(t, rbList.Items, p.rbName)
		if got := rb.GetNamespace(); got != p.namespace {
			t.Errorf("RoleBinding %s namespace = %q, want %q", p.rbName, got, p.namespace)
		}
		requireSubject(t, rb, 0, "User", p.cn)
		requireRoleRef(t, rb, "Role", p.rbRole)
	}

	// The aggregated manager role survives the full pipeline for core.
	crList, err := client.Resource(gvr("clusterroles")).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List(clusterroles): %v", err)
	}
	agg := byName(t, crList.Items, "capi-aggregated-manager-role")
	if len(firstAggregationMatchLabels(t, agg)) == 0 {
		t.Error("aggregated manager role lost its aggregationRule through the pipeline")
	}
}
