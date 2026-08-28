// Package manifests loads the vendored Cluster API provider manifests
// (templates/manifests/<provider>/provider.yaml), filters them to the eight
// kinds the capishim setup container applies, applies them to the management
// apiserver with an idempotent create-or-update, waits for every
// CustomResourceDefinition to report Established, and rewrites the provider
// RBAC bindings so each manager identity (a User named by its client-cert CN
// from the pki package) carries the permissions the stock manifests grant its
// ServiceAccount (REQ-003, REQ-004).
package manifests

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

// Applied kinds (REQ-003/REQ-004): the eight object kinds the setup container
// installs from the vendored provider manifests.
const (
	kindNamespace                      = "Namespace"
	kindCustomResourceDefinition       = "CustomResourceDefinition"
	kindClusterRole                    = "ClusterRole"
	kindClusterRoleBinding             = "ClusterRoleBinding"
	kindRole                           = "Role"
	kindRoleBinding                    = "RoleBinding"
	kindMutatingWebhookConfiguration   = "MutatingWebhookConfiguration"
	kindValidatingWebhookConfiguration = "ValidatingWebhookConfiguration"
)

// yamlBufferSize is the lookahead buffer used to guess whether a manifest
// stream is JSON or YAML.
const yamlBufferSize = 4096

// Parse decodes a multi-document YAML (or JSON) stream into unstructured
// objects. Empty documents and stray document separators are skipped, so an
// empty stream yields an empty slice without error; malformed YAML is an
// error. Documents without apiVersion/kind are returned as-is; callers decide
// whether they are applicable.
func Parse(r io.Reader) ([]unstructured.Unstructured, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(r, yamlBufferSize)
	var objs []unstructured.Unstructured
	for {
		var raw map[string]any
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifests: decode document: %w", err)
		}
		if raw == nil {
			continue
		}
		objs = append(objs, unstructured.Unstructured{Object: raw})
	}
	return objs, nil
}

// Load reads and parses every file in paths, in order, and returns all parsed
// objects concatenated. A missing or unreadable file is an error; no paths
// yields an empty slice.
func Load(paths ...string) ([]unstructured.Unstructured, error) {
	var objs []unstructured.Unstructured
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("manifests: read %s: %w", path, err)
		}
		parsed, err := Parse(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("manifests: parse %s: %w", path, err)
		}
		objs = append(objs, parsed...)
	}
	return objs, nil
}

// Keep reports whether obj is one of the eight kinds the setup container
// applies: Namespace, CustomResourceDefinition, the four RBAC kinds
// (ClusterRole, ClusterRoleBinding, Role, RoleBinding), and the two webhook
// configuration kinds. The apiVersion is part of the identity, so a legacy
// rbac.authorization.k8s.io/v1beta1 ClusterRole or an unrelated group's
// Namespace is not kept. A nil or empty object is never kept.
func Keep(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	switch gvk := obj.GroupVersionKind(); gvk {
	case corev1.SchemeGroupVersion.WithKind(kindNamespace),
		apiextv1.SchemeGroupVersion.WithKind(kindCustomResourceDefinition),
		rbacv1.SchemeGroupVersion.WithKind(kindClusterRole),
		rbacv1.SchemeGroupVersion.WithKind(kindClusterRoleBinding),
		rbacv1.SchemeGroupVersion.WithKind(kindRole),
		rbacv1.SchemeGroupVersion.WithKind(kindRoleBinding),
		admissionregistrationv1.SchemeGroupVersion.WithKind(kindMutatingWebhookConfiguration),
		admissionregistrationv1.SchemeGroupVersion.WithKind(kindValidatingWebhookConfiguration):
		return true
	}
	return false
}
