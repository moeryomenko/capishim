// Package webhookrewrite_test — TASK-006 red-phase tests for REQ-003: the
// batch rewrite learns to redirect URL-form clientConfigs. A clientConfig.url
// whose authority is 127.0.0.1:<P> or localhost:<P>, where <P> is an External
// component's webhook port, becomes https://<external-host>:<P><same-path>;
// every other URL is left untouched; service-based rewriting is unchanged;
// the transform is idempotent and injects the pod CA as caBundle.
//
// Pinned seam for the implementer (TASK-006): RewriteAll gains a variadic
// options parameter so existing three-argument callers keep compiling:
//
//	func RewriteAll(objs []runtime.Object, ports map[string]int, caPEM []byte, opts ...Option) error
//	func WithExternalWebhookHost(host string, ports ...int) Option
//
// Without the option the behavior is exactly today's service-based rewrite.
// Until the seam exists this file fails to compile; that failure is the red
// phase evidence.
package webhookrewrite_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/moeryomenko/capishim/internal/webhookrewrite"
)

// defaultExternalHost is the REQ-006 default webhook host every rewritten
// loopback URL points at when the environment carries no override.
const defaultExternalHost = "host.containers.internal"

// mutatingWithURL builds a one-webhook mutating configuration whose
// clientConfig carries the given URL, mirroring the shape of the vendored
// hypervisor provider manifests.
func mutatingWithURL(name, url string) *admissionv1.MutatingWebhookConfiguration {
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name:          "default.hypervisorcluster.infrastructure.cluster.x-k8s.io",
				FailurePolicy: ptr.To(admissionv1.Fail),
				ClientConfig:  admissionv1.WebhookClientConfig{URL: new(url)},
			},
		},
	}
}

// --- REQ-003: matching loopback URLs are redirected to the external host ---

func TestRewriteAllRewritesLoopbackURLsToExternalHost(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	tests := []struct {
		name    string
		giveURL string
		wantURL string
	}{
		{
			name:    "127.0.0.1 authority keeps the path",
			giveURL: "https://127.0.0.1:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster",
			wantURL: "https://" + defaultExternalHost +
				":9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster",
		},
		{
			name:    "localhost authority keeps the path",
			giveURL: "https://localhost:9443/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine",
			wantURL: "https://" + defaultExternalHost +
				":9443/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine",
		},
		{
			name:    "empty path yields no trailing slash",
			giveURL: "https://127.0.0.1:9443",
			wantURL: "https://" + defaultExternalHost + ":9443",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := mutatingWithURL("hypervisor-mutating-webhook-configuration", tt.giveURL)
			objs := []runtime.Object{cfg}
			err := webhookrewrite.RewriteAll(
				objs,
				map[string]int{"hypervisor-system": 9443},
				ca,
				webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
			)
			if err != nil {
				t.Fatalf("RewriteAll() error = %v", err)
			}
			got := cfg.Webhooks[0].ClientConfig
			if got.URL == nil || *got.URL != tt.wantURL {
				t.Errorf("URL = %v, want %s", got.URL, tt.wantURL)
			}
			if !bytes.Equal(got.CABundle, ca) {
				t.Errorf("CABundle = %q, want pod CA", got.CABundle)
			}
		})
	}
}

// --- REQ-003: non-matching URLs are untouched ---

func TestRewriteAllLeavesNonMatchingURLsUntouched(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	tests := []struct {
		name    string
		giveURL string
	}{
		{
			name:    "different host same port",
			giveURL: "https://example.com:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster",
		},
		{
			name:    "loopback host different port",
			giveURL: "https://127.0.0.1:9999/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster",
		},
		{
			name:    "localhost in-pod manager port",
			giveURL: "https://localhost:9444/validate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := mutatingWithURL("hypervisor-mutating-webhook-configuration", tt.giveURL)
			objs := []runtime.Object{cfg}
			if err := webhookrewrite.RewriteAll(
				objs,
				map[string]int{"hypervisor-system": 9443},
				ca,
				webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
			); err != nil {
				t.Fatalf("RewriteAll() error = %v", err)
			}
			if cfg.Webhooks[0].ClientConfig.URL == nil || *cfg.Webhooks[0].ClientConfig.URL != tt.giveURL {
				t.Errorf("URL = %v, want untouched %s", cfg.Webhooks[0].ClientConfig.URL, tt.giveURL)
			}
		})
	}
}

// --- REQ-006: the configured host, not a literal, drives the rewrite ---

func TestRewriteAllUsesConfiguredHostFromOption(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := mutatingWithURL(
		"hypervisor-mutating-webhook-configuration",
		"https://127.0.0.1:9443/mutate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig",
	)
	if err := webhookrewrite.RewriteAll(
		[]runtime.Object{cfg},
		map[string]int{"hypervisor-system": 9443},
		ca,
		webhookrewrite.WithExternalWebhookHost("webhook.lab.local", 9443),
	); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}
	want := "https://webhook.lab.local:9443/mutate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig"
	if cfg.Webhooks[0].ClientConfig.URL == nil || *cfg.Webhooks[0].ClientConfig.URL != want {
		t.Errorf("URL = %v, want %s", cfg.Webhooks[0].ClientConfig.URL, want)
	}
}

// The option carries the External ports set: every listed port matches, an
// unlisted loopback port does not, proving the matcher reads the argument
// rather than hardcoding 9443.
func TestRewriteAllMatchesEveryConfiguredExternalPort(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	matched := mutatingWithURL(
		"hypervisor-mutating-webhook-configuration",
		"https://127.0.0.1:9543/mutate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorcontrolplane",
	)
	unmatched := mutatingWithURL(
		"other-mutating-webhook-configuration",
		"https://127.0.0.1:9444/validate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig",
	)
	objs := []runtime.Object{matched, unmatched}
	if err := webhookrewrite.RewriteAll(
		objs,
		map[string]int{},
		ca,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443, 9543),
	); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}
	wantMatched := "https://" + defaultExternalHost + ":9543/mutate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorcontrolplane"
	if matched.Webhooks[0].ClientConfig.URL == nil || *matched.Webhooks[0].ClientConfig.URL != wantMatched {
		t.Errorf("port 9543 URL = %v, want %s", matched.Webhooks[0].ClientConfig.URL, wantMatched)
	}
	if unmatched.Webhooks[0].ClientConfig.URL == nil ||
		*unmatched.Webhooks[0].ClientConfig.URL != "https://127.0.0.1:9444/validate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig" {
		t.Errorf("port 9444 URL = %v, want untouched", unmatched.Webhooks[0].ClientConfig.URL)
	}
}

// --- REQ-003: service-based rewriting is unchanged when the option is active ---

func TestRewriteAllServiceRewriteUnchangedWithHostOption(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed-webhook-configuration"},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name: "default.kubeadmconfig.bootstrap.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: serviceRef(
						"capi-kubeadm-bootstrap-system",
						"capi-kubeadm-bootstrap-webhook-service",
						"/mutate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig",
					),
				},
			},
			{
				Name: "default.hypervisorcluster.infrastructure.cluster.x-k8s.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: new("https://127.0.0.1:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster"),
				},
			},
		},
	}
	ports := map[string]int{
		"capi-system":                   9443,
		"capi-kubeadm-bootstrap-system": 9444,
		"hypervisor-system":             9443,
	}
	if err := webhookrewrite.RewriteAll(
		[]runtime.Object{cfg},
		ports,
		ca,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
	); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}
	serviceURL := cfg.Webhooks[0].ClientConfig.URL
	if serviceURL == nil ||
		*serviceURL != "https://localhost:9444/mutate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig" {
		t.Errorf(
			"service-based URL = %v, want https://localhost:9444/mutate-bootstrap-cluster-x-k8s-io-v1beta2-kubeadmconfig",
			serviceURL,
		)
	}
	if cfg.Webhooks[0].ClientConfig.Service != nil {
		t.Errorf("service-based Service = %+v, want nil", cfg.Webhooks[0].ClientConfig.Service)
	}
	urlHost := cfg.Webhooks[1].ClientConfig.URL
	wantURLHost := "https://" + defaultExternalHost + ":9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster"
	if urlHost == nil || *urlHost != wantURLHost {
		t.Errorf("url-based URL = %v, want %s", urlHost, wantURLHost)
	}
}

// --- REQ-003: CRD conversion webhooks follow the same URL rule ---

func TestRewriteAllCRDConversionLoopbackURLRewritten(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "hypervisorclusters.infrastructure.cluster.x-k8s.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "infrastructure.cluster.x-k8s.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "HypervisorCluster"},
			Scope: apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						URL: new("https://127.0.0.1:9443/convert"),
					},
					ConversionReviewVersions: []string{"v1"},
				},
			},
		},
	}
	if err := webhookrewrite.RewriteAll(
		[]runtime.Object{crd},
		map[string]int{"hypervisor-system": 9443},
		ca,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
	); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}
	cc := crd.Spec.Conversion.Webhook.ClientConfig
	if cc.URL == nil || *cc.URL != "https://"+defaultExternalHost+":9443/convert" {
		t.Errorf("conversion URL = %v, want https://%s:9443/convert", cc.URL, defaultExternalHost)
	}
	if !bytes.Equal(cc.CABundle, ca) {
		t.Errorf("conversion CABundle = %q, want pod CA", cc.CABundle)
	}
}

// --- REQ-003: idempotency over the URL form ---

func TestRewriteAllURLFormIdempotent(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	build := func() []runtime.Object {
		return []runtime.Object{
			mutatingWithURL(
				"hypervisor-mutating-webhook-configuration",
				"https://127.0.0.1:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster",
			),
			&admissionv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "hypervisor-validating-webhook-configuration"},
				Webhooks: []admissionv1.ValidatingWebhook{
					{
						Name: "validate.infrastructure.cluster.x-k8s.io",
						ClientConfig: admissionv1.WebhookClientConfig{
							URL: new("https://localhost:9443/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine"),
						},
					},
				},
			},
			&apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "hypervisorconfigs.bootstrap.cluster.x-k8s.io"},
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Group: "bootstrap.cluster.x-k8s.io",
					Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "HypervisorConfig"},
					Scope: apiextensionsv1.NamespaceScoped,
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: apiextensionsv1.WebhookConverter,
						Webhook: &apiextensionsv1.WebhookConversion{
							ClientConfig: &apiextensionsv1.WebhookClientConfig{URL: new("https://127.0.0.1:9443/convert")},
						},
					},
				},
			},
		}
	}
	marshalAll := func(t *testing.T, objs []runtime.Object) []byte {
		t.Helper()
		var buf bytes.Buffer
		for _, obj := range objs {
			data, err := yaml.Marshal(obj)
			if err != nil {
				t.Fatalf("marshal %T: %v", obj, err)
			}
			buf.Write(data)
		}
		return buf.Bytes()
	}

	objs := build()
	if err := webhookrewrite.RewriteAll(
		objs,
		map[string]int{"hypervisor-system": 9443},
		ca,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
	); err != nil {
		t.Fatalf("first RewriteAll() error = %v", err)
	}
	first := marshalAll(t, objs)

	// Re-running setup over its own output must be a no-op: byte-stable.
	if err := webhookrewrite.RewriteAll(
		objs,
		map[string]int{"hypervisor-system": 9443},
		ca,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
	); err != nil {
		t.Fatalf("second RewriteAll() error = %v", err)
	}
	second := marshalAll(t, objs)
	if !bytes.Equal(first, second) {
		t.Errorf("re-apply produced drift:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(first), "https://"+defaultExternalHost+":9443/") {
		t.Errorf("rewritten output lost the external host:\n%s", first)
	}
	if strings.Contains(string(first), "127.0.0.1:9443") || strings.Contains(string(first), "localhost:9443") {
		t.Errorf("rewritten output still references loopback:\n%s", first)
	}
}

// --- REQ-003/REQ-005: caBundle injection into rewritten URL configs ---

func TestRewriteAllURLCABundleBase64OnWire(t *testing.T) {
	t.Parallel()
	ca := podCA(t)
	cfg := mutatingWithURL(
		"hypervisor-mutating-webhook-configuration",
		"https://127.0.0.1:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachinetemplate",
	)
	if err := webhookrewrite.RewriteAll(
		[]runtime.Object{cfg},
		map[string]int{"hypervisor-system": 9443},
		ca,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
	); err != nil {
		t.Fatalf("RewriteAll() error = %v", err)
	}
	// In memory the bundle holds the raw PEM; the wire encoding is base64.
	if !bytes.Equal(cfg.Webhooks[0].ClientConfig.CABundle, ca) {
		t.Errorf("CABundle = %q, want raw pod CA PEM", cfg.Webhooks[0].ClientConfig.CABundle)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal rewritten config: %v", err)
	}
	wantBase64 := base64.StdEncoding.EncodeToString(ca)
	if !strings.Contains(string(out), wantBase64) {
		t.Errorf("marshaled config missing base64 caBundle %q\n%s", wantBase64, out)
	}
	if strings.Contains(string(out), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("marshaled config contains raw PEM; caBundle must be base64 on the wire\n%s", out)
	}
}

// --- Contract continuity: an empty CA still blocks a would-be rewrite ---

func TestRewriteAllLoopbackURLEmptyCAPEMErrors(t *testing.T) {
	t.Parallel()
	cfg := mutatingWithURL(
		"hypervisor-mutating-webhook-configuration",
		"https://127.0.0.1:9443/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster",
	)
	if err := webhookrewrite.RewriteAll(
		[]runtime.Object{cfg},
		map[string]int{"hypervisor-system": 9443},
		nil,
		webhookrewrite.WithExternalWebhookHost(defaultExternalHost, 9443),
	); err == nil {
		t.Error("RewriteAll() error = nil, want error for empty caPEM on a matching loopback URL")
	}
}
