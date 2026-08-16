package manifests

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Binding subject kinds and the unstructured field names used while rewriting
// RBAC binding objects (REQ-004).
const (
	subjectKindServiceAccount = "ServiceAccount"
	subjectKindUser           = "User"

	fieldKind      = "kind"
	fieldName      = "name"
	fieldNamespace = "namespace"
	fieldSubjects  = "subjects"
)

// RewriteRBACSubjects returns a deep copy of objs in which every
// ServiceAccount subject of a ClusterRoleBinding or RoleBinding is replaced
// by a User subject named by the manager CN mapped to the subject's namespace
// (REQ-004). Non-ServiceAccount subjects, roleRef, and every non-binding kind
// pass through unchanged. A ServiceAccount subject in a namespace without a
// mapped CN, or a RoleBinding without metadata.namespace, is an error.
func RewriteRBACSubjects(objs []unstructured.Unstructured, cnByNamespace map[string]string) ([]unstructured.Unstructured, error) {
	out := make([]unstructured.Unstructured, len(objs))
	for i := range objs {
		copied := objs[i].DeepCopy()
		switch copied.GetKind() {
		case kindClusterRoleBinding:
			if err := rewriteSubjects(copied, cnByNamespace); err != nil {
				return nil, fmt.Errorf("manifests: rewrite %s %q: %w", copied.GetKind(), copied.GetName(), err)
			}
		case kindRoleBinding:
			if copied.GetNamespace() == "" {
				return nil, fmt.Errorf("manifests: rewrite RoleBinding %q: no namespace", copied.GetName())
			}
			if err := rewriteSubjects(copied, cnByNamespace); err != nil {
				return nil, fmt.Errorf("manifests: rewrite %s %q: %w", copied.GetKind(), copied.GetName(), err)
			}
		}
		out[i] = *copied
	}
	return out, nil
}

// rewriteSubjects replaces each ServiceAccount subject of a binding with a
// User subject named by the manager CN mapped to the subject's namespace.
func rewriteSubjects(binding *unstructured.Unstructured, cnByNamespace map[string]string) error {
	raw, found := binding.Object[fieldSubjects]
	if !found {
		return nil
	}
	subjects, ok := raw.([]interface{})
	if !ok {
		return fmt.Errorf("subjects field is %T, want a list", raw)
	}
	for i := range subjects {
		subject, ok := subjects[i].(map[string]interface{})
		if !ok {
			continue
		}
		kind, ok := subject[fieldKind].(string)
		if !ok || kind != subjectKindServiceAccount {
			continue
		}
		ns, ok := subject[fieldNamespace].(string)
		if !ok {
			return errors.New("ServiceAccount subject has no namespace")
		}
		cn, ok := cnByNamespace[ns]
		if !ok {
			return fmt.Errorf("no manager CN for ServiceAccount subject in namespace %q", ns)
		}
		subjects[i] = map[string]interface{}{
			fieldKind: subjectKindUser,
			fieldName: cn,
		}
	}
	return nil
}

// AdminClusterRoleBinding returns the ClusterRoleBinding that binds the admin
// identity to cluster-admin: a User subject named adminCN (REQ-004).
func AdminClusterRoleBinding(adminCN string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       kindClusterRoleBinding,
		"metadata": map[string]interface{}{
			"name": "capishim-admin",
		},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     kindClusterRole,
			"name":     "cluster-admin",
		},
		"subjects": []interface{}{
			map[string]interface{}{
				fieldKind: subjectKindUser,
				fieldName: adminCN,
			},
		},
	}}
}
