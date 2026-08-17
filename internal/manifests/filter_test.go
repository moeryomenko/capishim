// Red-phase tests for the kind filter of the internal/manifests contract
// (REQ-003/REQ-004, plan assumption 4): exactly the eight applied kinds are
// kept, workloads and cert-manager objects are dropped, and a manifest set
// with zero kept objects degrades to an empty apply.
package manifests_test

import (
	"strings"
	"testing"

	"github.com/moeryomenko/capishim/internal/manifests"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func contains(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func TestKeep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		want       bool
	}{
		{name: "namespace", apiVersion: "v1", kind: "Namespace", want: true},
		{
			name:       "customresourcedefinition",
			apiVersion: "apiextensions.k8s.io/v1",
			kind:       "CustomResourceDefinition",
			want:       true,
		},
		{name: "clusterrole", apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", want: true},
		{name: "clusterrolebinding", apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", want: true},
		{name: "role", apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", want: true},
		{name: "rolebinding", apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", want: true},
		{
			name:       "mutatingwebhookconfiguration",
			apiVersion: "admissionregistration.k8s.io/v1",
			kind:       "MutatingWebhookConfiguration",
			want:       true,
		},
		{
			name:       "validatingwebhookconfiguration",
			apiVersion: "admissionregistration.k8s.io/v1",
			kind:       "ValidatingWebhookConfiguration",
			want:       true,
		},

		{name: "deployment", apiVersion: "apps/v1", kind: "Deployment", want: false},
		{name: "service", apiVersion: "v1", kind: "Service", want: false},
		{name: "serviceaccount", apiVersion: "v1", kind: "ServiceAccount", want: false},
		{name: "certificate", apiVersion: "cert-manager.io/v1", kind: "Certificate", want: false},
		{name: "issuer", apiVersion: "cert-manager.io/v1", kind: "Issuer", want: false},
		{name: "secret", apiVersion: "v1", kind: "Secret", want: false},
		{name: "configmap", apiVersion: "v1", kind: "ConfigMap", want: false},
		{name: "unknown-kind", apiVersion: "example.io/v1", kind: "Example", want: false},

		// Keep must check the group/version, not only the kind: a legacy
		// rbac v1beta1 ClusterRole is not the applied v1 object.
		{
			name:       "clusterrole-wrong-version",
			apiVersion: "rbac.authorization.k8s.io/v1beta1",
			kind:       "ClusterRole",
			want:       false,
		},
		{name: "namespace-wrong-group", apiVersion: "other.io/v1", kind: "Namespace", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": tt.apiVersion,
				"kind":       tt.kind,
				"metadata":   map[string]interface{}{"name": "obj"},
			}}
			if got := manifests.Keep(obj); got != tt.want {
				t.Errorf("Keep(%s/%s) = %v, want %v", tt.apiVersion, tt.kind, got, tt.want)
			}
		})
	}
}

func TestKeepNilAndEmpty(t *testing.T) {
	t.Parallel()
	if manifests.Keep(nil) {
		t.Error("Keep(nil) = true, want false")
	}
	var empty unstructured.Unstructured
	if manifests.Keep(&empty) {
		t.Error("Keep(empty) = true, want false")
	}
}

func TestFilterVendoredProviders(t *testing.T) {
	t.Parallel()
	keptKinds := []string{
		"Namespace", "CustomResourceDefinition",
		"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding",
		"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration",
	}
	for _, p := range allVendoredProviders() {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			objs := mustLoadVendored(t, p)
			kept := keepOnly(t, objs)

			for kind, n := range vendoredKindCounts()[p] {
				got := kindCounts(kept)[kind]
				if contains(keptKinds, kind) {
					if got != n {
						t.Errorf("kept kind %s count = %d, want %d", kind, got, n)
					}
				} else if got != 0 {
					t.Errorf("filter kept dropped kind %s (%d objects)", kind, got)
				}
			}
		})
	}
}

func TestFilterProviderWithZeroKeptObjects(t *testing.T) {
	t.Parallel()
	doc := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nope
spec:
  replicas: 1
---
apiVersion: v1
kind: Service
metadata:
  name: nope
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nope
`
	objs, err := manifests.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	kept := keepOnly(t, objs)
	if len(kept) != 0 {
		t.Errorf("filtered %d objects, want 0 for a manifest with no kept kinds", len(kept))
	}

	client := newDynamicFake(t)
	if err := manifests.Apply(t.Context(), client, kept); err != nil {
		t.Errorf("Apply of an empty kept set returned error: %v", err)
	}
}
