// The setup subcommand performs the full management-cluster initialization:
// it ensures the certificate inventory, waits for the management apiserver,
// applies the vendored provider CRDs and waits for Established, rewrites and
// applies the provider RBAC, rewrites and applies the webhook configurations,
// and writes the manager and admin kubeconfigs. Every step is idempotent so a
// re-run converges to the same state (REQ-002..REQ-005, REQ-010).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/manifests"
	"github.com/moeryomenko/capishim/internal/pki"
	"github.com/moeryomenko/capishim/internal/webhookrewrite"
)

// Environment variables understood by the setup subcommand.
const (
	// EnvManifestsDir overrides the directory holding the vendored provider
	// manifest trees (templates/manifests/<provider>/provider.yaml). The
	// capishim image bakes the tree at the default below (REQ-011).
	EnvManifestsDir = "CAPISHIM_MANIFESTS_DIR"

	// EnvAPIServerTimeout overrides how long setup waits for the management
	// apiserver to answer /healthz before failing.
	EnvAPIServerTimeout = "CAPISHIM_APISERVER_TIMEOUT"
)

// defaultManifestsDir is the provider-manifest root baked into the capishim
// image; host runs point EnvManifestsDir at the repo's templates tree.
const defaultManifestsDir = "/templates/manifests"

// defaultAPIServerTimeout bounds the /healthz wait when EnvAPIServerTimeout
// is unset; the verification contract allows the pod five minutes to come up
// (VC-01).
const defaultAPIServerTimeout = 5 * time.Minute

// apiserverPollInterval is the delay between /healthz probes.
const apiserverPollInterval = 500 * time.Millisecond

// healthzProbeTimeout is the per-request timeout of a single /healthz probe.
const healthzProbeTimeout = 5 * time.Second

// crdWaitTimeout bounds the wait for every CRD to report Established after
// Apply (REQ-003).
const crdWaitTimeout = 2 * time.Minute

// adminCN is the client-certificate CN of the admin identity: it is bound to
// cluster-admin by the admin ClusterRoleBinding and authenticates the admin
// kubeconfig (REQ-004, REQ-010).
const adminCN = "capishim:admin"

// apiserverSchemeHost is the scheme and loopback host of every client-facing
// apiserver URL. The apiserver serving certificate always carries the
// 127.0.0.1 SAN (pki), and a wildcard bind host such as 0.0.0.0 is not a
// connectable address (REQ-010).
const apiserverSchemeHost = "https://127.0.0.1:"

// defaultAPIServerPort is the port used when the bind address cannot be
// parsed; config.Load always produces a valid address, so this is defensive.
const defaultAPIServerPort = "6443"

// kubeconfigContextName is the cluster, auth-info, and context name in the
// generated kubeconfigs.
const kubeconfigContextName = "default"

// providerManifestFile is the vendored provider manifest filename under each
// provider directory (assumption 4).
const providerManifestFile = "provider.yaml"

// Webhook-carrying kinds rewritten by WebhookObjects (REQ-005).
const (
	kindCustomResourceDefinition       = "CustomResourceDefinition"
	kindMutatingWebhookConfiguration   = "MutatingWebhookConfiguration"
	kindValidatingWebhookConfiguration = "ValidatingWebhookConfiguration"
)

// File and directory modes for generated artifacts.
const (
	kubeconfigFileMode os.FileMode = 0o600
	stateDirFileMode   os.FileMode = 0o755
)

// runSetup executes the setup subcommand: it runs the full management-cluster
// initialization and reports readiness on success.
func runSetup(ctx context.Context, stdout, stderr io.Writer, env map[string]string) int {
	if err := setup(ctx, env); err != nil {
		fmt.Fprintf(stderr, "capishim setup: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, "capishim setup: ready")
	return exitOK
}

// setup performs the management-cluster initialization in the order the
// apiserver requires: CRDs (waiting for Established), RBAC with the manager
// identities, webhook configuration rewrite, and finally the kubeconfigs the
// manager containers consume. Every phase is idempotent: applies are
// create-or-update, rewrites are full replacements, and kubeconfig writes are
// atomic, so a repeated run is a no-op that converges to the same state
// (REQ-003, REQ-004, REQ-005, REQ-010).
func setup(ctx context.Context, env map[string]string) error {
	cfg, err := config.Load(env)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	manifestsRoot, err := ManifestsDir(env)
	if err != nil {
		return fmt.Errorf("manifests dir: %w", err)
	}
	apiserverTimeout, err := APIServerTimeout(env)
	if err != nil {
		return fmt.Errorf("apiserver timeout: %w", err)
	}
	inv, err := pki.Generate(ctx, pki.Config{StateDir: cfg.StateDir, BindAddress: cfg.BindAddress})
	if err != nil {
		return fmt.Errorf("generate certificates: %w", err)
	}
	server := APIServerURL(cfg.BindAddress)
	if err := WaitForAPIServer(ctx, server+"/healthz", inv.CA.CertPath, inv.Admin.CertPath, inv.Admin.KeyPath, apiserverTimeout); err != nil {
		return err
	}

	// The setup container acts with admin privileges: every client is built
	// from the admin client certificate and the pod CA (REQ-002, REQ-010).
	restCfg := &rest.Config{
		Host: server,
		TLSClientConfig: rest.TLSClientConfig{
			CAFile:   inv.CA.CertPath,
			CertFile: inv.Admin.CertPath,
			KeyFile:  inv.Admin.KeyPath,
		},
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}
	apiext, err := apiextclient.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build apiextensions client: %w", err)
	}

	// CRDs: apply the kept kinds from all four provider manifests and wait
	// until every CAPI CRD reports Established (REQ-003).
	loaded, err := manifests.Load(ProviderManifestFiles(manifestsRoot)...)
	if err != nil {
		return fmt.Errorf("load provider manifests: %w", err)
	}
	kept := keepObjects(loaded)
	if err := manifests.Apply(ctx, dyn, kept); err != nil {
		return fmt.Errorf("apply provider manifests: %w", err)
	}
	if err := manifests.WaitForCRDEstablished(ctx, apiext.ApiextensionsV1(), CRDNames(), crdWaitTimeout); err != nil {
		return fmt.Errorf("wait for CRD establishment: %w", err)
	}

	// RBAC: rewrite every ServiceAccount subject to the manager User bound
	// by the client-cert CN, bind the admin identity to cluster-admin, and
	// apply (REQ-004).
	rewritten, err := manifests.RewriteRBACSubjects(kept, ManagerCNByNamespace())
	if err != nil {
		return fmt.Errorf("rewrite RBAC subjects: %w", err)
	}
	rewritten = append(rewritten, *manifests.AdminClusterRoleBinding(adminCN))
	if err := manifests.Apply(ctx, dyn, rewritten); err != nil {
		return fmt.Errorf("apply RBAC: %w", err)
	}

	// Webhooks: rewrite every service-based clientConfig (admission webhook
	// configurations and CRD conversion webhooks) to a loopback URL with the
	// pod CA injected, then apply the rewritten objects (REQ-005).
	caPEM, err := os.ReadFile(inv.CA.CertPath)
	if err != nil {
		return fmt.Errorf("read pod CA: %w", err)
	}
	webhookObjects, err := WebhookObjects(kept)
	if err != nil {
		return err
	}
	if err := webhookrewrite.RewriteAll(webhookObjects, WebhookPortsByNamespace(), caPEM); err != nil {
		return fmt.Errorf("rewrite webhooks: %w", err)
	}
	rewrittenWebhooks, err := UnstructuredObjects(webhookObjects)
	if err != nil {
		return err
	}
	if err := manifests.Apply(ctx, dyn, rewrittenWebhooks); err != nil {
		return fmt.Errorf("apply rewritten webhooks: %w", err)
	}

	// Kubeconfigs: one per manager using its client certificate, plus the
	// admin kubeconfig, all against the loopback apiserver (REQ-010).
	for _, spec := range config.Components() {
		if spec.ManagerCN == "" {
			continue
		}
		artifact, ok := ManagerArtifact(spec.ID, inv)
		if !ok {
			return fmt.Errorf("no manager certificate for %s", spec.ID)
		}
		kubeconfigPath, ok := cfg.KubeconfigPath(spec.ID)
		if !ok {
			return fmt.Errorf("no kubeconfig path for %s", spec.ID)
		}
		kubeconfig := BuildKubeconfig(server, inv.CA.CertPath, artifact.CertPath, artifact.KeyPath)
		if err := WriteKubeconfigFile(kubeconfig, kubeconfigPath); err != nil {
			return fmt.Errorf("write manager kubeconfig for %s: %w", spec.ID, err)
		}
	}
	adminKubeconfig := BuildKubeconfig(server, inv.CA.CertPath, inv.Admin.CertPath, inv.Admin.KeyPath)
	if err := WriteKubeconfigFile(adminKubeconfig, cfg.AdminKubeconfigPath()); err != nil {
		return fmt.Errorf("write admin kubeconfig: %w", err)
	}
	return nil
}

// ManifestsDir resolves the directory holding the vendored provider manifest
// trees from the environment: EnvManifestsDir wins when set and non-empty,
// otherwise the container default /templates/manifests is used. A
// set-but-empty value is an error, mirroring config.Load.
func ManifestsDir(env map[string]string) (string, error) {
	raw, ok := env[EnvManifestsDir]
	if !ok {
		return defaultManifestsDir, nil
	}
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", fmt.Errorf("%s is set but empty", EnvManifestsDir)
	}
	return dir, nil
}

// APIServerTimeout resolves how long setup waits for the management apiserver
// to answer /healthz: EnvAPIServerTimeout (a Go duration string) when set,
// otherwise five minutes. An invalid value is an error.
func APIServerTimeout(env map[string]string) (time.Duration, error) {
	raw, ok := env[EnvAPIServerTimeout]
	if !ok {
		return defaultAPIServerTimeout, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", EnvAPIServerTimeout, raw, err)
	}
	return d, nil
}

// APIServerURL derives the client-facing apiserver URL from the bind address:
// the loopback host (the serving certificate always carries the 127.0.0.1
// SAN, pki) with the configured port. A wildcard bind host is not a
// connectable address. An unparseable address falls back to the default port;
// config.Load always produces a valid address, so the fallback is defensive.
func APIServerURL(bindAddress string) string {
	if _, port, err := net.SplitHostPort(bindAddress); err == nil {
		return apiserverSchemeHost + port
	}
	return apiserverSchemeHost + defaultAPIServerPort
}

// ProviderManifestFiles returns the four vendored provider manifest files
// under the manifests root dir: templates/manifests/<provider>/provider.yaml
// (assumption 4).
func ProviderManifestFiles(dir string) []string {
	return []string{
		filepath.Join(dir, "core", providerManifestFile),
		filepath.Join(dir, "cabpk", providerManifestFile),
		filepath.Join(dir, "kcp", providerManifestFile),
		filepath.Join(dir, "capd", providerManifestFile),
	}
}

// ManagerCNByNamespace maps each provider namespace to the manager client-cert
// CN that carries that provider's RBAC permissions (REQ-004), derived from
// the component spec table.
func ManagerCNByNamespace() map[string]string {
	cnByNamespace := make(map[string]string)
	for _, spec := range config.Components() {
		if spec.ProviderNamespace != "" && spec.ManagerCN != "" {
			cnByNamespace[spec.ProviderNamespace] = spec.ManagerCN
		}
	}
	return cnByNamespace
}

// WebhookPortsByNamespace maps each provider namespace to the manager webhook
// listener port (REQ-005, REQ-006), derived from the component spec table.
func WebhookPortsByNamespace() map[string]int {
	portsByNamespace := make(map[string]int)
	for _, spec := range config.Components() {
		if spec.ProviderNamespace != "" && spec.WebhookPort != 0 {
			portsByNamespace[spec.ProviderNamespace] = spec.WebhookPort
		}
	}
	return portsByNamespace
}

// ManagerArtifact returns the manager client certificate artifact for the
// given provider component. The bool is false for non-provider components.
func ManagerArtifact(id config.ComponentID, inv *pki.Inventory) (pki.Artifact, bool) {
	switch id {
	case config.ComponentCore:
		return inv.CoreManager, true
	case config.ComponentCABPK:
		return inv.CABPKManager, true
	case config.ComponentKCP:
		return inv.KCPManager, true
	case config.ComponentCAPD:
		return inv.CAPDManager, true
	case config.ComponentPKI, config.ComponentEtcd, config.ComponentAPIServer, config.ComponentSetup:
		return pki.Artifact{}, false
	default:
		return pki.Artifact{}, false
	}
}

// BuildKubeconfig assembles a kubeconfig for server that trusts the pod CA
// and authenticates with the given client certificate pair (REQ-010).
func BuildKubeconfig(server, caPath, certPath, keyPath string) *clientcmdapi.Config {
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters[kubeconfigContextName] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthority:     caPath,
		CertificateAuthorityData: nil,
	}
	kubeconfig.AuthInfos[kubeconfigContextName] = &clientcmdapi.AuthInfo{
		ClientCertificate: certPath,
		ClientKey:         keyPath,
	}
	kubeconfig.Contexts[kubeconfigContextName] = &clientcmdapi.Context{
		Cluster:  kubeconfigContextName,
		AuthInfo: kubeconfigContextName,
	}
	kubeconfig.CurrentContext = kubeconfigContextName
	return kubeconfig
}

// WriteKubeconfigFile serializes kubeconfig with clientcmd and writes it to
// path atomically with 0600 permissions, so a re-run converges
// byte-identically and a crash never leaves a truncated kubeconfig behind
// (REQ-010).
func WriteKubeconfigFile(kubeconfig *clientcmdapi.Config, path string) error {
	data, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return fmt.Errorf("serialize kubeconfig: %w", err)
	}
	if err := WriteFileAtomic(path, data, kubeconfigFileMode); err != nil {
		return fmt.Errorf("write kubeconfig %s: %w", path, err)
	}
	return nil
}

// WriteFileAtomic writes data to path via a temporary file in the same
// directory followed by a rename, so readers never observe a partial write
// and an existing file is replaced only on success. The temporary file is
// removed on every path that does not end in a successful rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, stateDirFileMode); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".capishim-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := tmp.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			fmt.Fprintf(os.Stderr, "capishim: close temporary file %s: %v\n", tmpPath, err)
		}
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "capishim: remove temporary file %s: %v\n", tmpPath, err)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temporary file to %s: %w", path, err)
	}
	return nil
}

// CRDNames returns the fifteen Cluster API CRDs setup waits to become
// Established after applying the vendored provider manifests (REQ-003): the
// kinds REQ-003 lists (Cluster, Machine, MachineDeployment, MachineSet,
// MachineHealthCheck, ClusterClass, ClusterResourceSet, KubeadmConfig,
// KubeadmConfigTemplate, KubeadmControlPlane, DevCluster, DevMachine,
// DevClusterTemplate, DevMachineTemplate, DevMachinePoolTemplate) with the
// Dev* kinds renamed to their infrastructure group resources.
func CRDNames() []string {
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

// WaitForAPIServer polls the apiserver /healthz endpoint until it returns any
// HTTP response, timeout elapses, or ctx is canceled. Any HTTP status counts
// as ready: a non-200 healthz (for example 401 when anonymous access is
// disabled) still proves the apiserver is serving, and the clients built
// afterwards surface the real readiness state. TLS and connection failures
// are tolerated as "not ready yet" so a slowly starting apiserver is retried;
// a probe that never answers within timeout is an error.
func WaitForAPIServer(ctx context.Context, healthzURL, caPath, certPath, keyPath string, timeout time.Duration) error {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read pod CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("pod CA contains no parseable certificates")
	}
	clientCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load probe client certificate: %w", err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{clientCert},
				MinVersion:   tls.VersionTLS12,
			},
		},
		Timeout: healthzProbeTimeout,
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthzURL, nil)
		if err != nil {
			return fmt.Errorf("build healthz probe: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if time.Now().After(deadline) {
				return fmt.Errorf("apiserver not ready within %s: %w", timeout, lastErr)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for apiserver: %w", ctx.Err())
			case <-time.After(apiserverPollInterval):
			}
			continue
		}
		if err := resp.Body.Close(); err != nil {
			return fmt.Errorf("close healthz response: %w", err)
		}
		return nil
	}
}

// keepObjects returns the subset of objs the setup container applies
// (manifests.Keep), preserving order.
func keepObjects(objs []unstructured.Unstructured) []unstructured.Unstructured {
	kept := make([]unstructured.Unstructured, 0, len(objs))
	for i := range objs {
		if manifests.Keep(&objs[i]) {
			kept = append(kept, objs[i])
		}
	}
	return kept
}

// WebhookObjects converts the kept objects that carry webhooks — mutating and
// validating webhook configurations and CRDs (whose conversion webhooks are
// rewritten too) — into their typed runtime.Object forms for
// webhookrewrite.RewriteAll. Other kept kinds are skipped, mirroring the
// manifests.Keep filter (REQ-005).
func WebhookObjects(kept []unstructured.Unstructured) ([]runtime.Object, error) {
	objects := make([]runtime.Object, 0, len(kept))
	for i := range kept {
		obj := &kept[i]
		switch obj.GetKind() {
		case kindMutatingWebhookConfiguration:
			typed := &admissionv1.MutatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, typed); err != nil {
				return nil, fmt.Errorf("convert %s %q: %w", obj.GetKind(), obj.GetName(), err)
			}
			objects = append(objects, typed)
		case kindValidatingWebhookConfiguration:
			typed := &admissionv1.ValidatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, typed); err != nil {
				return nil, fmt.Errorf("convert %s %q: %w", obj.GetKind(), obj.GetName(), err)
			}
			objects = append(objects, typed)
		case kindCustomResourceDefinition:
			typed := &apiextv1.CustomResourceDefinition{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, typed); err != nil {
				return nil, fmt.Errorf("convert %s %q: %w", obj.GetKind(), obj.GetName(), err)
			}
			objects = append(objects, typed)
		}
	}
	return objects, nil
}

// UnstructuredObjects converts typed runtime.Objects back into unstructured
// form for manifests.Apply.
func UnstructuredObjects(objects []runtime.Object) ([]unstructured.Unstructured, error) {
	out := make([]unstructured.Unstructured, 0, len(objects))
	for _, obj := range objects {
		objectMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, fmt.Errorf("convert %T: %w", obj, err)
		}
		out = append(out, unstructured.Unstructured{Object: objectMap})
	}
	return out, nil
}
