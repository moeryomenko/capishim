package manifests

import (
	"context"
	"fmt"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclientv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// crdPollInterval is the delay between CustomResourceDefinition status polls.
const crdPollInterval = 50 * time.Millisecond

// resourceRef describes how one applied kind maps onto the dynamic client:
// the group-version-resource plus whether the kind is namespaced.
type resourceRef struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

// resourceFor returns the dynamic-client resource for one of the eight kept
// kinds. The bool is false for kinds the setup container never applies.
func resourceFor(kind string) (resourceRef, bool) {
	switch kind {
	case kindNamespace:
		return resourceRef{gvr: corev1.SchemeGroupVersion.WithResource("namespaces")}, true
	case kindCustomResourceDefinition:
		return resourceRef{gvr: apiextv1.SchemeGroupVersion.WithResource("customresourcedefinitions")}, true
	case kindClusterRole:
		return resourceRef{gvr: rbacv1.SchemeGroupVersion.WithResource("clusterroles")}, true
	case kindClusterRoleBinding:
		return resourceRef{gvr: rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings")}, true
	case kindRole:
		return resourceRef{gvr: rbacv1.SchemeGroupVersion.WithResource("roles"), namespaced: true}, true
	case kindRoleBinding:
		return resourceRef{gvr: rbacv1.SchemeGroupVersion.WithResource("rolebindings"), namespaced: true}, true
	case kindMutatingWebhookConfiguration:
		return resourceRef{
			gvr: admissionregistrationv1.SchemeGroupVersion.WithResource("mutatingwebhookconfigurations"),
		}, true
	case kindValidatingWebhookConfiguration:
		return resourceRef{
			gvr: admissionregistrationv1.SchemeGroupVersion.WithResource("validatingwebhookconfigurations"),
		}, true
	}
	return resourceRef{}, false
}

// Apply creates every object in objs against client, or updates it in place
// when it already exists, so a repeated Apply is a no-op (REQ-003, REQ-004).
// Apply refuses objects whose kind is not one of the eight applied kinds, and
// refuses two objects in the same call that resolve to the same
// namespace/kind/name identifier (an empty namespace for cluster-scoped
// kinds), which would otherwise silently overwrite each other.
func Apply(ctx context.Context, client dynamic.Interface, objs []unstructured.Unstructured) error {
	seen := make(map[string]string, len(objs))
	for i := range objs {
		obj := &objs[i]
		ref, ok := resourceFor(obj.GetKind())
		if !ok {
			return fmt.Errorf("manifests: apply %s %q: kind is not applied", obj.GetKind(), obj.GetName())
		}
		key := applyKey(obj, ref)
		if first, ok := seen[key]; ok {
			return fmt.Errorf(
				"manifests: apply %s %q: duplicate identifier, first applied as %s",
				obj.GetKind(),
				obj.GetName(),
				first,
			)
		}
		seen[key] = obj.GetKind() + "/" + obj.GetName()
	}

	for i := range objs {
		// Work on a copy so the create-or-update bookkeeping (notably the
		// resourceVersion adopted for updates) never leaks back into the
		// caller's objects: a leaked resourceVersion would poison a later
		// Create with the same object.
		obj := objs[i].DeepCopy()
		ref, _ := resourceFor(obj.GetKind())
		resource := client.Resource(ref.gvr)
		var ri dynamic.ResourceInterface = resource
		if ref.namespaced {
			ri = resource.Namespace(obj.GetNamespace())
		}
		_, err := ri.Create(ctx, obj, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			existing, getErr := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("manifests: apply %s %q: read existing for update: %w", obj.GetKind(), obj.GetName(), getErr)
			}
			// A real apiserver rejects updates without the current
			// resourceVersion; adopt the existing object's so a repeated
			// Apply converges instead of failing (REQ-003, REQ-004).
			obj.SetResourceVersion(existing.GetResourceVersion())
			_, err = ri.Update(ctx, obj, metav1.UpdateOptions{})
		}
		if err != nil {
			return fmt.Errorf("manifests: apply %s %q: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// applyKey is the uniqueness key of one applied object: namespace (empty for
// cluster-scoped kinds), kind, and name.
func applyKey(obj *unstructured.Unstructured, ref resourceRef) string {
	ns := obj.GetNamespace()
	if !ref.namespaced {
		ns = ""
	}
	return ns + "\x00" + obj.GetKind() + "\x00" + obj.GetName()
}

// WaitForCRDEstablished polls client until every CRD in names reports the
// Established condition True, or timeout elapses. A CRD that cannot be read
// (including a missing CRD) or never establishes within the timeout is an
// error; an empty name list returns immediately (REQ-003).
func WaitForCRDEstablished(
	ctx context.Context,
	client apiextclientv1.ApiextensionsV1Interface,
	names []string,
	timeout time.Duration,
) error {
	if len(names) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		unestablished, err := firstUnestablishedCRD(ctx, client, names)
		if err != nil {
			return err
		}
		if unestablished == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("manifests: CRD %q not established within %s", unestablished, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("manifests: wait for CRD establishment: %w", ctx.Err())
		case <-time.After(crdPollInterval):
		}
	}
}

// firstUnestablishedCRD returns the first name in names whose CRD does not
// yet report Established, or "" when all are established. A CRD that cannot
// be read is an error.
func firstUnestablishedCRD(
	ctx context.Context,
	client apiextclientv1.ApiextensionsV1Interface,
	names []string,
) (string, error) {
	for _, name := range names {
		crd, err := client.CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("manifests: get CRD %q: %w", name, err)
		}
		if !crdEstablished(crd) {
			return name, nil
		}
	}
	return "", nil
}

// crdEstablished reports whether crd's status carries the Established
// condition with status True.
func crdEstablished(crd *apiextv1.CustomResourceDefinition) bool {
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextv1.Established && cond.Status == apiextv1.ConditionTrue {
			return true
		}
	}
	return false
}
