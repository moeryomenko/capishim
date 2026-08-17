// Package webhookrewrite_test exercises the webhook rewrite contract for
// internal/webhookrewrite with black-box tests (no access to implementation
// internals). The tests are written against the TASK-010 design contract and
// are red until TASK-011 implements the package.
//
// Contract summary (asserted design choices, documented for TASK-011):
//
//   - RewriteClientConfig converts a service-based clientConfig to
//     `url: https://localhost:<port><path>` preserving the path verbatim,
//     clears service, and sets caBundle to the raw pod CA PEM bytes.
//   - caBundle holds raw PEM bytes in memory (admissionregistration/v1 and
//     apiextensions/v1 semantics); base64 is only the wire encoding.
//   - An empty service.path yields `https://localhost:<port>` with no trailing
//     slash.
//   - An empty caPEM is an error on any clientConfig that would be rewritten;
//     configs with no webhooks are no-ops and return nil.
//   - A clientConfig that already uses url is left as-is except caBundle is
//     normalized to the pod CA (stale bundles are replaced).
//   - RewriteAll picks the port per webhook from a map keyed by the webhook
//     service namespace; an unknown service namespace or unsupported object
//     kind is an error; objects with no service refs need no port lookup.
package webhookrewrite_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apimachyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/moeryomenko/capishim/internal/webhookrewrite"
)

// podCA returns the pod CA bundle used by every rewrite test. The bytes are a
// stand-in for the PEM bundle the pki container produces.
func podCA(t *testing.T) []byte {
	t.Helper()
	// The PEM body is split across string chunks so each source line stays
	// under the lll limit; the assembled bytes are identical to the single
	// inline blob.
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

func serviceRef(namespace, name, path string) *admissionv1.ServiceReference {
	return &admissionv1.ServiceReference{
		Namespace: namespace,
		Name:      name,
		Path:      ptr.To(path),
	}
}

func wantURL(port int, path string) string {
	return "https://localhost:" + strconv.Itoa(port) + path
}

// --- Requirement 1: service -> URL rewrite preserving the path ---

func TestRewriteClientConfigServiceToURL(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	tests := []struct {
		name      string
		port      int
		namespace string
		service   string
		path      string
	}{
		{
			name:      "core",
			port:      9443,
			namespace: "capi-system",
			service:   "capi-webhook-service",
			path:      "/mutate-cluster-x-k8s-io-v1beta2-cluster",
		},
		{
			name:      "cabpk",
			port:      9444,
			namespace: "capi-kubeadm-bootstrap-system",
			service:   "capi-kubeadm-bootstrap-webhook-service",
			path:      "/validate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig",
		},
		{
			name:      "kcp",
			port:      9445,
			namespace: "capi-kubeadm-control-plane-system",
			service:   "capi-kubeadm-control-plane-webhook-service",
			path:      "/validate-scale-controlplane-cluster-x-k8s-io-v1beta2-kubeadmcontrolplane",
		},
		{
			name:      "capd",
			port:      9446,
			namespace: "capd-system",
			service:   "capd-webhook-service",
			path:      "/mutate-infrastructure-cluster-x-k8s-io-v1beta2-dockercluster",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cc := &admissionv1.WebhookClientConfig{
				Service: serviceRef(tt.namespace, tt.service, tt.path),
			}
			if err := webhookrewrite.RewriteClientConfig(cc, tt.port, ca); err != nil {
				t.Fatalf("RewriteClientConfig() error = %v", err)
			}
			if cc.URL == nil || *cc.URL != wantURL(tt.port, tt.path) {
				t.Errorf("URL = %v, want %s", cc.URL, wantURL(tt.port, tt.path))
			}
			if cc.Service != nil {
				t.Errorf("Service = %+v, want nil (cleared)", cc.Service)
			}
			if !bytes.Equal(cc.CABundle, ca) {
				t.Errorf("CABundle = %q, want pod CA", cc.CABundle)
			}
		})
	}
}

func TestRewriteMutatingConfigServiceToURL(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "capi-mutating-webhook-configuration",
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name: "default.cluster.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef("capi-system", "capi-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
				},
			},
			{
				Name: "default.clusterresourceset.addons.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef(
						"capi-system",
						"capi-webhook-service",
						"/mutate-addons-cluster-x-k8s-io-v1beta2-clusterresourceset",
					),
				},
			},
		},
	}
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("RewriteMutatingConfig() error = %v", err)
	}
	want := []string{
		"https://localhost:9443/mutate-cluster-x-k8s-io-v1beta2-cluster",
		"https://localhost:9443/mutate-addons-cluster-x-k8s-io-v1beta2-clusterresourceset",
	}
	for i, wh := range cfg.Webhooks {
		if wh.ClientConfig.URL == nil || *wh.ClientConfig.URL != want[i] {
			t.Errorf("webhook %q URL = %v, want %s", wh.Name, wh.ClientConfig.URL, want[i])
		}
		if wh.ClientConfig.Service != nil {
			t.Errorf("webhook %q Service = %+v, want nil", wh.Name, wh.ClientConfig.Service)
		}
		if !bytes.Equal(wh.ClientConfig.CABundle, ca) {
			t.Errorf("webhook %q CABundle = %q, want pod CA", wh.Name, wh.ClientConfig.CABundle)
		}
	}
}

func TestRewriteValidatingConfigServiceToURL(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "capi-validating-webhook-configuration",
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name: "validation.cluster.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef("capi-system", "capi-webhook-service", "/validate-cluster-x-k8s-io-v1beta2-cluster"),
				},
			},
		},
	}
	if err := webhookrewrite.RewriteValidatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("RewriteValidatingConfig() error = %v", err)
	}
	wh := cfg.Webhooks[0]
	want := "https://localhost:9443/validate-cluster-x-k8s-io-v1beta2-cluster"
	if wh.ClientConfig.URL == nil || *wh.ClientConfig.URL != want {
		t.Errorf("URL = %v, want %s", wh.ClientConfig.URL, want)
	}
	if wh.ClientConfig.Service != nil {
		t.Errorf("Service = %+v, want nil", wh.ClientConfig.Service)
	}
	if !bytes.Equal(wh.ClientConfig.CABundle, ca) {
		t.Errorf("CABundle = %q, want pod CA", wh.ClientConfig.CABundle)
	}
}

// --- Requirement 2: per-provider port mapping ---

func TestRewriteAllProviderPortMapping(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	objs := []runtime.Object{
		&admissionv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "capi-mutating-webhook-configuration"},
			Webhooks: []admissionv1.MutatingWebhook{
				{Name: "default.cluster.cluster.x-k8s.io", ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef("capi-system", "capi-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
				}},
			},
		},
		&admissionv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "capi-kubeadm-bootstrap-validating-webhook-configuration"},
			Webhooks: []admissionv1.ValidatingWebhook{
				{Name: "validation.kubeadmconfig.bootstrap.cluster.x-k8s.io", ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef(
						"capi-kubeadm-bootstrap-system",
						"capi-kubeadm-bootstrap-webhook-service",
						"/validate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig",
					),
				}},
			},
		},
		&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "kubeadmcontrolplanes.controlplane.cluster.x-k8s.io"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "controlplane.cluster.x-k8s.io",
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "KubeadmControlPlane"},
				Scope: apiextensionsv1.NamespaceScoped,
				Conversion: &apiextensionsv1.CustomResourceConversion{
					Strategy: apiextensionsv1.WebhookConverter,
					Webhook: &apiextensionsv1.WebhookConversion{
						ClientConfig: &apiextensionsv1.WebhookClientConfig{
							Service: &apiextensionsv1.ServiceReference{
								Namespace: "capi-kubeadm-control-plane-system",
								Name:      "capi-kubeadm-control-plane-webhook-service",
								Path:      ptr.To("/convert"),
							},
						},
						ConversionReviewVersions: []string{"v1"},
					},
				},
			},
		},
		&admissionv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "capd-mutating-webhook-configuration"},
			Webhooks: []admissionv1.MutatingWebhook{
				{Name: "default.dockercluster.infrastructure.cluster.x-k8s.io", ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef(
						"capd-system",
						"capd-webhook-service",
						"/mutate-infrastructure-cluster-x-k8s-io-v1beta2-dockercluster",
					),
				}},
			},
		},
	}
	ports := map[string]int{
		"capi-system":                       9443,
		"capi-kubeadm-bootstrap-system":     9444,
		"capi-kubeadm-control-plane-system": 9445,
		"capd-system":                       9446,
	}
	if err := webhookrewrite.RewriteAll(objs, ports, ca); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}

	checkURL := func(desc string, url *string, want string) {
		t.Helper()
		if url == nil || *url != want {
			t.Errorf("%s URL = %v, want %s", desc, url, want)
		}
	}
	coreMut, ok := objs[0].(*admissionv1.MutatingWebhookConfiguration)
	if !ok {
		t.Fatalf("objs[0] is %T, want *MutatingWebhookConfiguration", objs[0])
	}
	cabpkVal, ok := objs[1].(*admissionv1.ValidatingWebhookConfiguration)
	if !ok {
		t.Fatalf("objs[1] is %T, want *ValidatingWebhookConfiguration", objs[1])
	}
	kcpCRD, ok := objs[2].(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		t.Fatalf("objs[2] is %T, want *CustomResourceDefinition", objs[2])
	}
	capdMut, ok := objs[3].(*admissionv1.MutatingWebhookConfiguration)
	if !ok {
		t.Fatalf("objs[3] is %T, want *MutatingWebhookConfiguration", objs[3])
	}
	checkURL("core mutating", coreMut.Webhooks[0].ClientConfig.URL,
		"https://localhost:9443/mutate-cluster-x-k8s-io-v1beta2-cluster")
	checkURL("cabpk validating", cabpkVal.Webhooks[0].ClientConfig.URL,
		"https://localhost:9444/validate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig")
	checkURL("kcp conversion", kcpCRD.Spec.Conversion.Webhook.ClientConfig.URL,
		"https://localhost:9445/convert")
	checkURL("capd mutating", capdMut.Webhooks[0].ClientConfig.URL,
		"https://localhost:9446/mutate-infrastructure-cluster-x-k8s-io-v1beta2-dockercluster")

	// No service reference may survive the batch rewrite.
	for _, obj := range objs {
		switch o := obj.(type) {
		case *admissionv1.MutatingWebhookConfiguration:
			for _, wh := range o.Webhooks {
				if wh.ClientConfig.Service != nil {
					t.Errorf("mutating %q Service = %+v, want nil", wh.Name, wh.ClientConfig.Service)
				}
			}
		case *admissionv1.ValidatingWebhookConfiguration:
			for _, wh := range o.Webhooks {
				if wh.ClientConfig.Service != nil {
					t.Errorf("validating %q Service = %+v, want nil", wh.Name, wh.ClientConfig.Service)
				}
			}
		case *apiextensionsv1.CustomResourceDefinition:
			if o.Spec.Conversion != nil && o.Spec.Conversion.Webhook != nil && o.Spec.Conversion.Webhook.ClientConfig != nil {
				if cc := o.Spec.Conversion.Webhook.ClientConfig; cc.Service != nil {
					t.Errorf("CRD %q conversion Service = %+v, want nil", o.Name, cc.Service)
				}
			}
		}
	}
}

// --- Requirement 3: caBundle injection (raw PEM in memory, base64 on the wire) ---

func TestRewriteClientConfigCABundleInjection(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cc := &admissionv1.WebhookClientConfig{
		Service: serviceRef("capi-system", "capi-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
	}
	if err := webhookrewrite.RewriteClientConfig(cc, 9443, ca); err != nil {
		t.Fatalf("RewriteClientConfig() error = %v", err)
	}
	// The in-memory field must hold the raw PEM (the API encodes to base64 on
	// the wire); storing base64 here would double-encode.
	if !bytes.Equal(cc.CABundle, ca) {
		t.Errorf("CABundle = %q, want raw pod CA PEM", cc.CABundle)
	}

	// Wire form: marshaling a rewritten config must produce base64(caPEM) and
	// must not contain the raw PEM text (no double encoding).
	cfg := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "capi-mutating-webhook-configuration"},
		Webhooks: []admissionv1.MutatingWebhook{
			{Name: "default.cluster.cluster.x-k8s.io", ClientConfig: *cc},
		},
	}
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("RewriteMutatingConfig() error = %v", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantBase64 := base64.StdEncoding.EncodeToString(podCA(t))
	if !strings.Contains(string(out), wantBase64) {
		t.Errorf("marshaled config missing base64 caBundle %q\n%s", wantBase64, out)
	}
	if strings.Contains(string(out), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("marshaled config contains raw PEM; caBundle must be base64 on the wire\n%s", out)
	}
}

// --- Requirement 4: CRD conversion webhook rewrite ---

func TestRewriteCRDConversionServiceToURL(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeadmcontrolplanes.controlplane.cluster.x-k8s.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "controlplane.cluster.x-k8s.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "KubeadmControlPlane"},
			Scope: apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Namespace: "capi-kubeadm-control-plane-system",
							Name:      "capi-kubeadm-control-plane-webhook-service",
							Path:      ptr.To("/convert"),
						},
					},
					ConversionReviewVersions: []string{"v1"},
				},
			},
		},
	}
	if err := webhookrewrite.RewriteCRDConversion(crd, 9445, ca); err != nil {
		t.Fatalf("RewriteCRDConversion() error = %v", err)
	}
	cc := crd.Spec.Conversion.Webhook.ClientConfig
	if cc.URL == nil || *cc.URL != "https://localhost:9445/convert" {
		t.Errorf("URL = %v, want https://localhost:9445/convert", cc.URL)
	}
	if cc.Service != nil {
		t.Errorf("Service = %+v, want nil", cc.Service)
	}
	if !bytes.Equal(cc.CABundle, ca) {
		t.Errorf("CABundle = %q, want pod CA", cc.CABundle)
	}
	if want := []string{"v1"}; !reflect.DeepEqual(crd.Spec.Conversion.Webhook.ConversionReviewVersions, want) {
		t.Errorf("ConversionReviewVersions = %v, want %v", crd.Spec.Conversion.Webhook.ConversionReviewVersions, want)
	}
}

func TestRewriteCRDConversionUntouched(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	tests := []struct {
		name string
		conv *apiextensionsv1.CustomResourceConversion
	}{
		{name: "nil conversion", conv: nil},
		{name: "strategy none", conv: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}},
		{
			name: "strategy webhook nil webhook",
			conv: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.WebhookConverter},
		},
		{name: "strategy webhook nil clientConfig", conv: &apiextensionsv1.CustomResourceConversion{
			Strategy: apiextensionsv1.WebhookConverter,
			Webhook:  &apiextensionsv1.WebhookConversion{},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			crd := &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "clusters.cluster.x-k8s.io"},
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Group:      "cluster.x-k8s.io",
					Names:      apiextensionsv1.CustomResourceDefinitionNames{Kind: "Cluster"},
					Scope:      apiextensionsv1.NamespaceScoped,
					Conversion: tt.conv,
				},
			}
			before := crd.DeepCopy()
			if err := webhookrewrite.RewriteCRDConversion(crd, 9443, ca); err != nil {
				t.Fatalf("RewriteCRDConversion() error = %v", err)
			}
			if !reflect.DeepEqual(crd, before) {
				t.Errorf("CRD modified by rewrite, want untouched:\n%+v", crd)
			}
		})
	}
}

// --- Requirement 5: idempotent re-apply ---

func TestRewriteIdempotent(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "capi-mutating-webhook-configuration",
			Labels: map[string]string{"cluster.x-k8s.io/provider": "cluster-api"},
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name: "default.cluster.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef("capi-system", "capi-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
				},
			},
			{
				Name: "default.clusterresourceset.addons.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef(
						"capi-system",
						"capi-webhook-service",
						"/mutate-addons-cluster-x-k8s-io-v1beta2-clusterresourceset",
					),
				},
			},
		},
	}
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("first rewrite error = %v", err)
	}
	first, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal first result: %v", err)
	}

	// Second application must be a no-op: byte-stable output.
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("second rewrite error = %v", err)
	}
	second, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal second result: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("re-apply produced drift:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	for i, wh := range cfg.Webhooks {
		want := []string{
			"https://localhost:9443/mutate-cluster-x-k8s-io-v1beta2-cluster",
			"https://localhost:9443/mutate-addons-cluster-x-k8s-io-v1beta2-clusterresourceset",
		}[i]
		if wh.ClientConfig.URL == nil || *wh.ClientConfig.URL != want {
			t.Errorf("webhook %q URL = %v, want %s", wh.Name, wh.ClientConfig.URL, want)
		}
		if !bytes.Equal(wh.ClientConfig.CABundle, ca) {
			t.Errorf("webhook %q CABundle = %q, want pod CA", wh.Name, wh.ClientConfig.CABundle)
		}
	}
}

// --- Requirement 6: untouched fields preserved verbatim ---

func TestRewriteUntouchedFieldsPreserved(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "capi-validating-webhook-configuration",
			Namespace:   "",
			Labels:      map[string]string{"cluster.x-k8s.io/provider": "cluster-api"},
			Annotations: map[string]string{"cert-manager.io/inject-ca-from": "capi-system/capi-serving-cert"},
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name: "validation.cluster.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef("capi-system", "capi-webhook-service", "/validate-cluster-x-k8s-io-v1beta2-cluster"),
				},
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				FailurePolicy:           ptr.To(admissionv1.Fail),
				MatchPolicy:             ptr.To(admissionv1.Equivalent),
				SideEffects:             ptr.To(admissionv1.SideEffectClassNone),
				Rules: []admissionv1.RuleWithOperations{
					{
						Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
						Rule: admissionv1.Rule{
							APIGroups:   []string{"cluster.x-k8s.io"},
							APIVersions: []string{"v1beta2"},
							Resources:   []string{"clusters"},
						},
					},
				},
			},
		},
	}
	before := cfg.DeepCopy()
	if err := webhookrewrite.RewriteValidatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("RewriteValidatingConfig() error = %v", err)
	}

	// Only clientConfig is permitted to change: service -> url + caBundle.
	before.Webhooks[0].ClientConfig = admissionv1.WebhookClientConfig{
		URL:      cfg.Webhooks[0].ClientConfig.URL,
		CABundle: ca,
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Errorf("fields other than clientConfig changed:\n got %+v\nwant %+v", cfg, before)
	}
}

// --- Requirement 7: URL-based clientConfig left as-is except caBundle ---

func TestRewriteClientConfigURLBasedNormalized(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	tests := []struct {
		name     string
		caBundle []byte
	}{
		{name: "no existing bundle"},
		{name: "stale bundle replaced", caBundle: []byte("stale-ca")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url := "https://localhost:9443/mutate-cluster-x-k8s-io-v1beta2-cluster"
			cc := &admissionv1.WebhookClientConfig{URL: &url, CABundle: tt.caBundle}
			if err := webhookrewrite.RewriteClientConfig(cc, 9443, ca); err != nil {
				t.Fatalf("RewriteClientConfig() error = %v", err)
			}
			if cc.URL == nil || *cc.URL != url {
				t.Errorf("URL = %v, want %s (untouched)", cc.URL, url)
			}
			if cc.Service != nil {
				t.Errorf("Service = %+v, want nil", cc.Service)
			}
			if !bytes.Equal(cc.CABundle, ca) {
				t.Errorf("CABundle = %q, want pod CA (normalized)", cc.CABundle)
			}
		})
	}
}

// --- Edge cases ---

func TestRewriteClientConfigEmptyPath(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	tests := []struct {
		name string
		path *string
	}{
		{name: "nil path"},
		{name: "empty string path", path: ptr.To("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cc := &admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Namespace: "capi-system",
					Name:      "capi-webhook-service",
					Path:      tt.path,
				},
			}
			if err := webhookrewrite.RewriteClientConfig(cc, 9443, ca); err != nil {
				t.Fatalf("RewriteClientConfig() error = %v", err)
			}
			// Asserted choice: empty path -> no trailing slash.
			if cc.URL == nil || *cc.URL != "https://localhost:9443" {
				t.Errorf("URL = %v, want https://localhost:9443", cc.URL)
			}
			if cc.Service != nil {
				t.Errorf("Service = %+v, want nil", cc.Service)
			}
			if !bytes.Equal(cc.CABundle, ca) {
				t.Errorf("CABundle = %q, want pod CA", cc.CABundle)
			}
		})
	}
}

func TestRewriteClientConfigEmptyCAPEM(t *testing.T) {
	t.Parallel()
	cc := &admissionv1.WebhookClientConfig{
		Service: serviceRef("capi-system", "capi-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
	}
	// Asserted choice: an empty pod CA is an error rather than leaving the
	// webhook unauthenticated.
	if err := webhookrewrite.RewriteClientConfig(cc, 9443, nil); err == nil {
		t.Error("RewriteClientConfig() error = nil, want error for empty caPEM")
	}

	cfg := &admissionv1.MutatingWebhookConfiguration{
		Webhooks: []admissionv1.MutatingWebhook{
			{Name: "default.cluster.cluster.x-k8s.io", ClientConfig: admissionv1.WebhookClientConfig{
				Service: serviceRef("capi-system", "capi-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
			}},
		},
	}
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, nil); err == nil {
		t.Error("RewriteMutatingConfig() error = nil, want error for empty caPEM")
	}
}

func TestRewriteMutatingConfigNilWebhooks(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "capi-mutating-webhook-configuration"},
		Webhooks:   nil,
	}
	before := cfg.DeepCopy()
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, ca); err != nil {
		t.Fatalf("RewriteMutatingConfig() with nil webhooks error = %v", err)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Errorf("config with nil webhooks modified, want untouched:\n%+v", cfg)
	}

	// A config with no webhooks is a no-op even when caPEM is empty.
	if err := webhookrewrite.RewriteMutatingConfig(cfg, 9443, nil); err != nil {
		t.Errorf("RewriteMutatingConfig() with nil webhooks and empty caPEM error = %v, want nil", err)
	}
}

func TestRewriteAllUnknownKind(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	objs := []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated"},
			Data:       map[string]string{"k": "v"},
		},
	}
	if err := webhookrewrite.RewriteAll(objs, map[string]int{"capi-system": 9443}, ca); err == nil {
		t.Error("RewriteAll() error = nil, want error for unsupported object kind")
	}
}

func TestRewriteAllUnknownNamespace(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	objs := []runtime.Object{
		&admissionv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "mystery-mutating-webhook-configuration"},
			Webhooks: []admissionv1.MutatingWebhook{
				{Name: "default.cluster.cluster.x-k8s.io", ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef("unknown-system", "unknown-webhook-service", "/mutate-cluster-x-k8s-io-v1beta2-cluster"),
				}},
			},
		},
	}
	if err := webhookrewrite.RewriteAll(objs, map[string]int{"capi-system": 9443}, ca); err == nil {
		t.Error("RewriteAll() error = nil, want error for unknown service namespace")
	}
}

func TestRewriteAllNoServiceRefs(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	url := "https://localhost:9443/validate-cluster-x-k8s-io-v1beta2-cluster"
	objs := []runtime.Object{
		&admissionv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "capi-validating-webhook-configuration"},
			Webhooks: []admissionv1.ValidatingWebhook{
				{Name: "validation.cluster.cluster.x-k8s.io", ClientConfig: admissionv1.WebhookClientConfig{
					URL: &url,
				}},
			},
		},
		&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "clusters.cluster.x-k8s.io"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group:      "cluster.x-k8s.io",
				Names:      apiextensionsv1.CustomResourceDefinitionNames{Kind: "Cluster"},
				Scope:      apiextensionsv1.NamespaceScoped,
				Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
			},
		},
	}
	// No service refs means no port lookup is needed; the batch must succeed
	// with an empty port map.
	if err := webhookrewrite.RewriteAll(objs, map[string]int{}, ca); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}
	valCfg, ok := objs[0].(*admissionv1.ValidatingWebhookConfiguration)
	if !ok {
		t.Fatalf("objs[0] is %T, want *ValidatingWebhookConfiguration", objs[0])
	}
	wh := valCfg.Webhooks[0]
	if wh.ClientConfig.URL == nil || *wh.ClientConfig.URL != url {
		t.Errorf("URL = %v, want %s (untouched)", wh.ClientConfig.URL, url)
	}
	if !bytes.Equal(wh.ClientConfig.CABundle, ca) {
		t.Errorf("URL-based webhook CABundle = %q, want pod CA (normalized)", wh.ClientConfig.CABundle)
	}
}

// --- Real fixture: vendored core provider manifests ---

func TestRewriteCoreFixture(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	objs := mustParseProvider(t, "core")

	// Record every service-based clientConfig path before the rewrite.
	wantURLByPath := map[string]string{}
	for _, cc := range collectAdmissionClientConfigs(objs) {
		if cc.Service != nil && cc.Service.Path != nil {
			wantURLByPath[*cc.Service.Path] = "https://localhost:9443" + *cc.Service.Path
		}
	}
	conversionPaths := map[string]string{}
	for _, cc := range collectConversionClientConfigs(objs) {
		if cc.Service != nil && cc.Service.Path != nil {
			conversionPaths[*cc.Service.Path] = "https://localhost:9443" + *cc.Service.Path
		}
	}
	if len(wantURLByPath) == 0 {
		t.Fatal("fixture has no service-based admission webhooks; rewrite contract not exercised")
	}
	if len(conversionPaths) == 0 {
		t.Fatal("fixture has no service-based CRD conversion webhooks; rewrite contract not exercised")
	}

	ports := map[string]int{"capi-system": 9443}
	if err := webhookrewrite.RewriteAll(objs, ports, ca); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}

	// After the rewrite no clientConfig may reference a service, every
	// rewritten URL must match the preserved path, and caBundle must be the
	// pod CA everywhere.
	for _, cc := range collectAdmissionClientConfigs(objs) {
		if cc.Service != nil {
			t.Errorf("admission clientConfig still has Service %+v, want nil", cc.Service)
		}
		if cc.URL == nil {
			t.Errorf("admission clientConfig has no URL")
			continue
		}
		if want, ok := wantURLByPath[strings.TrimPrefix(*cc.URL, "https://localhost:9443")]; ok {
			if *cc.URL != want {
				t.Errorf("admission URL = %s, want %s", *cc.URL, want)
			}
		}
		if !bytes.Equal(cc.CABundle, ca) {
			t.Errorf("admission URL %s CABundle = %q, want pod CA", *cc.URL, cc.CABundle)
		}
	}
	for _, cc := range collectConversionClientConfigs(objs) {
		if cc.Service != nil {
			t.Errorf("conversion clientConfig still has Service %+v, want nil", cc.Service)
		}
		if cc.URL == nil {
			t.Errorf("conversion clientConfig has no URL")
			continue
		}
		if want, ok := conversionPaths[strings.TrimPrefix(*cc.URL, "https://localhost:9443")]; ok {
			if *cc.URL != want {
				t.Errorf("conversion URL = %s, want %s", *cc.URL, want)
			}
		}
		if !bytes.Equal(cc.CABundle, ca) {
			t.Errorf("conversion URL %s CABundle = %q, want pod CA", *cc.URL, cc.CABundle)
		}
	}

	// The fixture must exercise all three rewrite targets.
	var sawMutating, sawValidating bool
	for _, obj := range objs {
		switch o := obj.(type) {
		case *admissionv1.MutatingWebhookConfiguration:
			sawMutating = true
			for _, wh := range o.Webhooks {
				if !strings.Contains(*wh.ClientConfig.URL, "/mutate-") {
					t.Errorf("mutating webhook URL %s missing /mutate- prefix", *wh.ClientConfig.URL)
				}
			}
		case *admissionv1.ValidatingWebhookConfiguration:
			sawValidating = true
			for _, wh := range o.Webhooks {
				if !strings.Contains(*wh.ClientConfig.URL, "/validate-") {
					t.Errorf("validating webhook URL %s missing /validate- prefix", *wh.ClientConfig.URL)
				}
			}
		}
	}
	if !sawMutating || !sawValidating {
		t.Errorf("fixture coverage incomplete: mutating=%v validating=%v", sawMutating, sawValidating)
	}
	if _, ok := conversionPaths["/convert"]; !ok {
		t.Errorf("fixture CRD conversion path /convert not found, got %v", conversionPaths)
	}
}

// --- Fixture helpers ---

func mustParseProvider(t *testing.T, provider string) []runtime.Object {
	t.Helper()
	path := filepath.Join("..", "..", "templates", "manifests", provider, "provider.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}

	var objs []runtime.Object
	reader := apimachyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("read fixture document: %v", err)
		}
		var tm metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil {
			t.Fatalf("decode fixture document kind: %v", err)
		}
		switch tm.Kind {
		case "MutatingWebhookConfiguration":
			var obj admissionv1.MutatingWebhookConfiguration
			if err := yaml.Unmarshal(doc, &obj); err != nil {
				t.Fatalf("decode mutating webhook configuration: %v", err)
			}
			objs = append(objs, &obj)
		case "ValidatingWebhookConfiguration":
			var obj admissionv1.ValidatingWebhookConfiguration
			if err := yaml.Unmarshal(doc, &obj); err != nil {
				t.Fatalf("decode validating webhook configuration: %v", err)
			}
			objs = append(objs, &obj)
		case "CustomResourceDefinition":
			var obj apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(doc, &obj); err != nil {
				t.Fatalf("decode CRD: %v", err)
			}
			objs = append(objs, &obj)
		default:
			// Provider fixtures carry Namespace, ServiceAccount, Role,
			// ClusterRole, Service, Deployment and cert-manager objects that
			// are filtered out before the rewrite step; they are not part of
			// the webhook rewrite contract.
			continue
		}
	}
	if len(objs) == 0 {
		t.Fatalf("no supported objects parsed from %s", path)
	}
	return objs
}

func collectAdmissionClientConfigs(objs []runtime.Object) []*admissionv1.WebhookClientConfig {
	var out []*admissionv1.WebhookClientConfig
	for _, obj := range objs {
		switch o := obj.(type) {
		case *admissionv1.MutatingWebhookConfiguration:
			for i := range o.Webhooks {
				out = append(out, &o.Webhooks[i].ClientConfig)
			}
		case *admissionv1.ValidatingWebhookConfiguration:
			for i := range o.Webhooks {
				out = append(out, &o.Webhooks[i].ClientConfig)
			}
		}
	}
	return out
}

func collectConversionClientConfigs(objs []runtime.Object) []*apiextensionsv1.WebhookClientConfig {
	var out []*apiextensionsv1.WebhookClientConfig
	for _, obj := range objs {
		crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
		if !ok {
			continue
		}
		if crd.Spec.Conversion != nil && crd.Spec.Conversion.Webhook != nil &&
			crd.Spec.Conversion.Webhook.ClientConfig != nil {
			out = append(out, crd.Spec.Conversion.Webhook.ClientConfig)
		}
	}
	return out
}
