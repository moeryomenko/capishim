// Package main_test — TASK-006 red-phase tests for the setup-side seams of
// the URL-based webhook rewrite (REQ-003, REQ-006, REQ-007 wiring, VC-01
// webhook clauses).
//
// Pinned seams for the implementer (TASK-006), and why they exist:
//
//   - cmd/capishim gains a separate rewrite-path port accessor:
//
//     func RewriteWebhookPortsByNamespace() map[string]int
//
//     returning WebhookPortsByNamespace() plus "hypervisor-system": 9443.
//     A separate accessor is used because TASK-004's TestWebhookPortsByNamespace
//     pins the in-pod map at exactly four entries and must not be modified;
//     the hypervisor entry therefore joins the map only on the rewrite path.
//
//   - The configured host flows from internal/config (Config.HypervisorWebhookHost,
//     key CAPISHIM_HYPERVISOR_WEBHOOK_HOST) into webhookrewrite.RewriteAll via
//     webhookrewrite.WithExternalWebhookHost(host, 9443). The composition test
//     below drives the exact objects and accessors setup() uses, minus the
//     live apiserver.
//
// Until the seams exist this file fails to compile; that failure is the red
// phase evidence.
package main_test

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	capishim "github.com/moeryomenko/capishim/cmd/capishim"
	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/webhookrewrite"
)

// --- REQ-003/REQ-004 wiring: the rewrite-path port map carries hypervisor-system ---

func TestRewriteWebhookPortsByNamespaceIncludesHypervisorSystem(t *testing.T) {
	t.Parallel()
	got := capishim.RewriteWebhookPortsByNamespace()
	if got["hypervisor-system"] != 9443 {
		t.Errorf("RewriteWebhookPortsByNamespace()[%q] = %d, want 9443", "hypervisor-system", got["hypervisor-system"])
	}
	for namespace, port := range capishim.WebhookPortsByNamespace() {
		if got[namespace] != port {
			t.Errorf("RewriteWebhookPortsByNamespace()[%q] = %d, want %d (in-pod entries preserved)",
				namespace, got[namespace], port)
		}
	}
}

// --- REQ-006 flow: the configured host reaches the rewrite setup performs ---

func TestSetupRewritePathUsesConfiguredHypervisorHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "override host flows to rewritten URLs",
			env: map[string]string{
				"HOME":                          "/home/testuser",
				config.EnvHypervisorWebhookHost: "webhook.lab.local",
			},
			want: "webhook.lab.local",
		},
		{
			name: "default host when env unset",
			env:  map[string]string{"HOME": "/home/testuser"},
			want: "host.containers.internal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(tt.env)
			if err != nil {
				t.Fatalf("config.Load(%v) returned error: %v", tt.env, err)
			}

			// Synthetic hypervisor webhook configurations in the shape of the
			// vendored provider manifests: loopback URLs on port 9443 with an
			// empty caBundle. The TypeMeta mirrors manifests.Load output,
			// which always carries apiVersion/kind from the YAML documents;
			// without it WebhookObjects cannot recognize the kinds.
			mutating := &admissionv1.MutatingWebhookConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "admissionregistration.k8s.io/v1",
					Kind:       "MutatingWebhookConfiguration",
				},
				ObjectMeta: metav1.ObjectMeta{Name: "hypervisor-mutating-webhook-configuration"},
				Webhooks: []admissionv1.MutatingWebhook{{
					Name:          "default.hypervisorcluster.infrastructure.cluster.x-k8s.io",
					FailurePolicy: ptr.To(admissionv1.Fail),
					ClientConfig: admissionv1.WebhookClientConfig{
						URL: ptr.To("https://127.0.0.1:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster"),
					},
				}},
			}
			validating := &admissionv1.ValidatingWebhookConfiguration{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "admissionregistration.k8s.io/v1",
					Kind:       "ValidatingWebhookConfiguration",
				},
				ObjectMeta: metav1.ObjectMeta{Name: "hypervisor-validating-webhook-configuration"},
				Webhooks: []admissionv1.ValidatingWebhook{{
					Name:          "validate.hypervisorconfig.bootstrap.cluster.x-k8s.io",
					FailurePolicy: ptr.To(admissionv1.Fail),
					ClientConfig: admissionv1.WebhookClientConfig{
						URL: ptr.To("https://127.0.0.1:9443/validate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig"),
					},
				}},
			}

			// Drive the same pipeline stages setup() uses: unstructured kept
			// objects -> typed webhook objects -> RewriteAll with the
			// rewrite-path ports and the configured host.
			kept := toUnstructuredList(t, mutating, validating)
			webhookObjects, err := capishim.WebhookObjects(kept)
			if err != nil {
				t.Fatalf("WebhookObjects returned error: %v", err)
			}
			if len(webhookObjects) != 2 {
				t.Fatalf("WebhookObjects returned %d objects, want 2", len(webhookObjects))
			}
			ca := podCAForRewrite(t)
			if err := webhookrewrite.RewriteAll(
				webhookObjects,
				capishim.RewriteWebhookPortsByNamespace(),
				ca,
				webhookrewrite.WithExternalWebhookHost(cfg.HypervisorWebhookHost, 9443),
			); err != nil {
				t.Fatalf("RewriteAll returned error: %v", err)
			}

			mut, ok := webhookObjects[0].(*admissionv1.MutatingWebhookConfiguration)
			if !ok {
				t.Fatalf("webhookObjects[0] is %T, want *MutatingWebhookConfiguration", webhookObjects[0])
			}
			val, ok := webhookObjects[1].(*admissionv1.ValidatingWebhookConfiguration)
			if !ok {
				t.Fatalf("webhookObjects[1] is %T, want *ValidatingWebhookConfiguration", webhookObjects[1])
			}
			wantMut := "https://" + tt.want + ":9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster"
			if mut.Webhooks[0].ClientConfig.URL == nil || *mut.Webhooks[0].ClientConfig.URL != wantMut {
				t.Errorf("mutating URL = %v, want %s (configured host, not loopback)", mut.Webhooks[0].ClientConfig.URL, wantMut)
			}
			wantVal := "https://" + tt.want + ":9443/validate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig"
			if val.Webhooks[0].ClientConfig.URL == nil || *val.Webhooks[0].ClientConfig.URL != wantVal {
				t.Errorf("validating URL = %v, want %s (configured host, not loopback)", val.Webhooks[0].ClientConfig.URL, wantVal)
			}
		})
	}
}

// toUnstructuredList converts typed runtime.Objects into the unstructured form
// WebhookObjects consumes, mirroring manifests.Load output.
func toUnstructuredList(t *testing.T, objs ...runtime.Object) []unstructured.Unstructured {
	t.Helper()
	out := make([]unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		objectMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			t.Fatalf("convert %T to unstructured: %v", obj, err)
		}
		out = append(out, unstructured.Unstructured{Object: objectMap})
	}
	return out
}

// podCAForRewrite returns the stand-in pod CA PEM for the rewrite composition
// test; named separately so this file stays independent of helpers that may
// move in other test files.
func podCAForRewrite(t *testing.T) []byte {
	t.Helper()
	return []byte("-----BEGIN CERTIFICATE-----\n" +
		"MIICzDCCAbSgAwIBAgIBADANBgkqhkiG9w0BAQsFADASMRAwDgYDVQQDEwdjYXBp\n" +
		"c2hpbTAgFw0yNjA4MTYwMDAwMDBaGA8yMDI2MDgxNzAwMDAwMFowEjEQMA4GA1UE\n" +
		"AxMHY2FwaXNoaW0wggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDCtT+W\n" +
		"cImXf3Xqxh5ZfUyTmH2wpvA5oJxH9Wm6R4Qh2g6nP9X7Vj8YzB9Gp1bGaLh/KlQ8\n" +
		"Z2y5s3Fqj0Kf3cUz5QpV4m2uRkS0RzVfZ0tM1LqN4W7y8UwqK1nLvXxK8yP9oDk2\n" +
		"Hj7c4wXa5jDpYyL3m2wBzNfF2eQpJX9oHq7gYcVf3bTp2kRfQ6LmX1vQ4nZc2aQ\n" +
		"h3jWqYrS5oPmE9nRz8dGkIuLb6pNQvXw4yCbHn2gTfS8tE3fJcWaKqO9vHjLdM\n" +
		"AgMBAAGjUzBRMA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MB0GA1Ud\n" +
		"DgQWBBSuZ2Y6wT3q1jRc4kZbY8tNlV0dHjANBgkqhkiG9w0BAQsFAAOCAQEAdJ+\n" +
		"k9LqzHcGjM9aBfQ2vW5k3XeY8TzFh0wRz1vQm4sXqJkH6bN7oP3zU1fQcGtL2yY\n" +
		"-----END CERTIFICATE-----\n")
}
