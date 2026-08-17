// Package webhookrewrite rewrites Kubernetes webhook clientConfig service
// references to loopback URLs so the capishim provider managers can serve
// their admission and conversion webhooks without in-cluster Service DNS
// (REQ-005). The rewrite is applied after CRD install and before manager
// readiness, and is idempotent so re-runs on restart are no-ops.
//
// Contract (locked by the black-box tests in this package):
//
//   - A service-based clientConfig becomes url https://localhost:<port><path>;
//     the service path is preserved verbatim and an empty path yields no
//     trailing slash. The service reference is cleared.
//   - caBundle holds the raw pod CA PEM bytes in memory; base64 is only the
//     wire encoding (admissionregistration/v1 and apiextensions/v1 semantics).
//   - An empty caPEM is an error whenever a clientConfig would be modified;
//     configurations with no webhooks are no-ops and return nil.
//   - A URL-based clientConfig keeps its URL; only caBundle is normalized.
//   - RewriteAll resolves the port per webhook from a map keyed by the webhook
//     service namespace and returns an error for unknown namespaces and
//     unsupported object kinds.
package webhookrewrite

import (
	"errors"
	"fmt"
	"strconv"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// webhookURLPrefix is the scheme and host every rewritten clientConfig points
// at; the webhook servers listen on loopback localhost (REQ-006).
const webhookURLPrefix = "https://localhost:"

// RewriteClientConfig rewrites a single admission webhook clientConfig: a
// service-based reference becomes url https://localhost:<port><path> (path
// preserved verbatim; empty path yields no trailing slash), the service
// reference is cleared, and caBundle is set to the raw caPEM bytes. A
// URL-based clientConfig keeps its URL; only caBundle is normalized to caPEM.
// An empty caPEM is an error whenever the clientConfig would be modified,
// because the webhook would otherwise be left unauthenticated.
func RewriteClientConfig(cc *admissionv1.WebhookClientConfig, port int, caPEM []byte) error {
	if cc.Service != nil {
		if len(caPEM) == 0 {
			return fmt.Errorf(
				"webhookrewrite: cannot rewrite service %s/%s: pod CA is empty",
				cc.Service.Namespace,
				cc.Service.Name,
			)
		}
		u := webhookURLPrefix + strconv.Itoa(port)
		if cc.Service.Path != nil && *cc.Service.Path != "" {
			u += *cc.Service.Path
		}
		cc.URL = &u
		cc.Service = nil
		cc.CABundle = caPEM
		return nil
	}
	if cc.URL == nil {
		return errors.New("webhookrewrite: clientConfig has neither service nor url")
	}
	if len(caPEM) == 0 {
		return fmt.Errorf("webhookrewrite: cannot normalize caBundle for url %q: pod CA is empty", *cc.URL)
	}
	cc.CABundle = caPEM
	return nil
}

// RewriteMutatingConfig rewrites every webhook clientConfig in a mutating
// webhook configuration via RewriteClientConfig, preserving all other fields
// (rules, failurePolicy, matchPolicy, admissionReviewVersions, sideEffects,
// name, namespace). A configuration with no webhooks is a no-op and returns
// nil even when caPEM is empty.
func RewriteMutatingConfig(cfg *admissionv1.MutatingWebhookConfiguration, port int, caPEM []byte) error {
	for i := range cfg.Webhooks {
		if err := RewriteClientConfig(&cfg.Webhooks[i].ClientConfig, port, caPEM); err != nil {
			return fmt.Errorf("webhookrewrite: mutating webhook %q: %w", cfg.Webhooks[i].Name, err)
		}
	}
	return nil
}

// RewriteValidatingConfig rewrites every webhook clientConfig in a validating
// webhook configuration via RewriteClientConfig, preserving all other fields.
// A configuration with no webhooks is a no-op and returns nil even when caPEM
// is empty.
func RewriteValidatingConfig(cfg *admissionv1.ValidatingWebhookConfiguration, port int, caPEM []byte) error {
	for i := range cfg.Webhooks {
		if err := RewriteClientConfig(&cfg.Webhooks[i].ClientConfig, port, caPEM); err != nil {
			return fmt.Errorf("webhookrewrite: validating webhook %q: %w", cfg.Webhooks[i].Name, err)
		}
	}
	return nil
}

// RewriteCRDConversion rewrites the spec.conversion.webhook.clientConfig of a
// CRD via the same service-to-URL contract as admission webhooks. CRDs with a
// nil conversion, nil webhook, or nil clientConfig are untouched and return
// nil.
func RewriteCRDConversion(crd *apiextensionsv1.CustomResourceDefinition, port int, caPEM []byte) error {
	conv := crd.Spec.Conversion
	if conv == nil || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		return nil
	}
	cc := conv.Webhook.ClientConfig
	// The admissionregistration/v1 and apiextensions/v1 clientConfig types
	// expose the same fields; rewriting through the admission type keeps the
	// rewrite contract in one place.
	admissionCC := admissionv1.WebhookClientConfig{
		URL:      cc.URL,
		CABundle: cc.CABundle,
	}
	if cc.Service != nil {
		admissionCC.Service = &admissionv1.ServiceReference{
			Namespace: cc.Service.Namespace,
			Name:      cc.Service.Name,
			Path:      cc.Service.Path,
		}
	}
	if err := RewriteClientConfig(&admissionCC, port, caPEM); err != nil {
		return err
	}
	cc.URL = admissionCC.URL
	cc.CABundle = admissionCC.CABundle
	cc.Service = nil
	return nil
}

// RewriteAll applies the rewrite to every supported object in objs: mutating
// and validating webhook configurations and CRD conversion webhooks. The port
// for each webhook is resolved from ports by the webhook's service namespace;
// URL-based clientConfigs need no port lookup. An unsupported object kind or
// a service namespace absent from ports is an error.
func RewriteAll(objs []runtime.Object, ports map[string]int, caPEM []byte) error {
	for _, obj := range objs {
		switch o := obj.(type) {
		case *admissionv1.MutatingWebhookConfiguration:
			if err := rewriteMutatingByNamespace(o, ports, caPEM); err != nil {
				return err
			}
		case *admissionv1.ValidatingWebhookConfiguration:
			if err := rewriteValidatingByNamespace(o, ports, caPEM); err != nil {
				return err
			}
		case *apiextensionsv1.CustomResourceDefinition:
			if err := rewriteCRDByNamespace(o, ports, caPEM); err != nil {
				return err
			}
		default:
			return fmt.Errorf("webhookrewrite: unsupported object kind %T", obj)
		}
	}
	return nil
}

// rewriteMutatingByNamespace rewrites a mutating webhook configuration,
// resolving the port per webhook from its service namespace.
func rewriteMutatingByNamespace(
	cfg *admissionv1.MutatingWebhookConfiguration,
	ports map[string]int,
	caPEM []byte,
) error {
	for i := range cfg.Webhooks {
		cc := &cfg.Webhooks[i].ClientConfig
		port, err := portForClientConfig(cc.Service, ports)
		if err != nil {
			return fmt.Errorf("webhookrewrite: mutating webhook %q: %w", cfg.Webhooks[i].Name, err)
		}
		if err := RewriteClientConfig(cc, port, caPEM); err != nil {
			return fmt.Errorf("webhookrewrite: mutating webhook %q: %w", cfg.Webhooks[i].Name, err)
		}
	}
	return nil
}

// rewriteValidatingByNamespace rewrites a validating webhook configuration,
// resolving the port per webhook from its service namespace.
func rewriteValidatingByNamespace(
	cfg *admissionv1.ValidatingWebhookConfiguration,
	ports map[string]int,
	caPEM []byte,
) error {
	for i := range cfg.Webhooks {
		cc := &cfg.Webhooks[i].ClientConfig
		port, err := portForClientConfig(cc.Service, ports)
		if err != nil {
			return fmt.Errorf("webhookrewrite: validating webhook %q: %w", cfg.Webhooks[i].Name, err)
		}
		if err := RewriteClientConfig(cc, port, caPEM); err != nil {
			return fmt.Errorf("webhookrewrite: validating webhook %q: %w", cfg.Webhooks[i].Name, err)
		}
	}
	return nil
}

// rewriteCRDByNamespace rewrites the conversion webhook of a CRD, resolving
// the port from the conversion service namespace.
func rewriteCRDByNamespace(crd *apiextensionsv1.CustomResourceDefinition, ports map[string]int, caPEM []byte) error {
	conv := crd.Spec.Conversion
	if conv == nil || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		return nil
	}
	port, err := portForConversionClientConfig(conv.Webhook.ClientConfig.Service, ports)
	if err != nil {
		return fmt.Errorf("webhookrewrite: CRD %q conversion: %w", crd.Name, err)
	}
	return RewriteCRDConversion(crd, port, caPEM)
}

// portForClientConfig resolves the webhook port for an admission clientConfig:
// a service-based config looks up its service namespace in ports; a URL-based
// config needs no port and returns 0, which the URL rewrite path ignores.
func portForClientConfig(service *admissionv1.ServiceReference, ports map[string]int) (int, error) {
	if service == nil {
		return 0, nil
	}
	port, ok := ports[service.Namespace]
	if !ok {
		return 0, fmt.Errorf("no webhook port for service namespace %q", service.Namespace)
	}
	return port, nil
}

// portForConversionClientConfig resolves the webhook port for an
// apiextensions conversion clientConfig, with the same semantics as
// portForClientConfig.
func portForConversionClientConfig(service *apiextensionsv1.ServiceReference, ports map[string]int) (int, error) {
	if service == nil {
		return 0, nil
	}
	port, ok := ports[service.Namespace]
	if !ok {
		return 0, fmt.Errorf("no webhook port for service namespace %q", service.Namespace)
	}
	return port, nil
}
