/*
Copyright 2026 The capishim Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package e2e holds the ginkgo suite that exercises the capishim quadlet pod as
// a Cluster API management cluster (VC-01..VC-08, REQ-008, REQ-012).
//
// The suite is a red-phase skeleton until TASK-018 implements
// github.com/moeryomenko/capishim/e2e/shim: it drives podman directly to boot
// the pod, emits the admin kubeconfig, and exposes the helpers the specs below
// assert on.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	framework "sigs.k8s.io/cluster-api/test/framework"
	bootstrap "sigs.k8s.io/cluster-api/test/framework/bootstrap"
	capiyaml "sigs.k8s.io/cluster-api/util/yaml"

	"github.com/moeryomenko/capishim/e2e/shim"
)

// TestE2E is the ginkgo entry point: it runs the capishim e2e suite
// (VC-01..VC-08, REQ-012) with the shim bootstrap as the management cluster.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "capishim-e2e")
}

// Compile-time contract: the shim provider must implement the upstream
// bootstrap.ClusterProvider interface (Create / GetKubeconfigPath / Dispose).
var _ bootstrap.ClusterProvider = (*shim.ClusterProvider)(nil)

const (
	// kubernetesVersion is the Kubernetes version rendered into the vendored
	// in-memory templates (REQ-007 documents v1.36.1).
	kubernetesVersion = "v1.36.1"

	// defaultControlPlaneMachineCount is the control-plane replica count used by
	// the provisioning specs.
	defaultControlPlaneMachineCount = 1
	// defaultWorkerMachineCount is the worker replica count used by the
	// provisioning specs.
	defaultWorkerMachineCount = 1

	// Intervals used by the suite's Eventually blocks. Provisioning an
	// in-memory cluster involves the full CAPI state machine (VC-05), so the
	// provisioning timeout is deliberately generous.
	pollInterval            = 10 * time.Second
	provisioningTimeout     = 15 * time.Minute
	webhookOperationTimeout = 5 * time.Minute
)

// Container names in the capishim pod. They mirror the quadlet unit names
// (plan assumption 2/6) and are the names the shim's podman driver must use.
const (
	containerPKI       = "capishim-pki"
	containerEtcd      = "capishim-etcd"
	containerAPIServer = "capishim-apiserver"
	containerSetup     = "capishim-setup"
	containerCore      = "capishim-core"
	containerCABPK     = "capishim-cabpk"
	containerKCP       = "capishim-kcp"
	containerCAPD      = "capishim-capd"
)

// longRunningContainers are the six containers that must be in "running" state
// after boot (VC-01; pki and setup are oneshot and exit).
var longRunningContainers = []string{
	containerEtcd,
	containerAPIServer,
	containerCore,
	containerCABPK,
	containerKCP,
	containerCAPD,
}

// oneshotContainers are the two init containers that must be exited with code 0
// after boot (VC-01).
var oneshotContainers = []string{containerPKI, containerSetup}

// managerContainers are the four provider manager containers whose logs are
// scanned for RBAC Forbidden errors (VC-03).
var managerContainers = []string{containerCore, containerCABPK, containerKCP, containerCAPD}

// expectedCRDs is the unique set of CRDs REQ-003 requires to be Established
// (the spec text lists KubeadmConfigTemplate twice).
var expectedCRDs = []string{
	"clusters.cluster.x-k8s.io",
	"machines.cluster.x-k8s.io",
	"machinedeployments.cluster.x-k8s.io",
	"machinesets.cluster.x-k8s.io",
	"machinehealthchecks.cluster.x-k8s.io",
	"clusterclasses.cluster.x-k8s.io",
	"clusterresourcesets.addons.cluster.x-k8s.io",
	"kubeadmconfigs.bootstrap.cluster.x-k8s.io",
	"kubeadmconfigtemplates.bootstrap.cluster.x-k8s.io",
	"kubeadmcontrolplanes.controlplane.cluster.x-k8s.io",
	"devclusters.infrastructure.cluster.x-k8s.io",
	"devmachines.infrastructure.cluster.x-k8s.io",
	"devclustertemplates.infrastructure.cluster.x-k8s.io",
	"devmachinetemplates.infrastructure.cluster.x-k8s.io",
	"devmachinepooltemplates.infrastructure.cluster.x-k8s.io",
}

var (
	// suiteCtx is the suite-wide context, canceled in AfterSuite.
	suiteCtx    context.Context
	cancelSuite context.CancelFunc

	// clusterProvider drives the podman-created capishim pod (TASK-018).
	clusterProvider *shim.ClusterProvider

	// stateDir is the shared state directory backing the pod
	// (default CAPISHIM_STATE_DIR or a temp dir under os.TempDir()).
	stateDir string

	// kubeconfigPath is the management-cluster admin kubeconfig emitted by the
	// setup container (stateDir/kubeconfigs/admin.kubeconfig).
	kubeconfigPath string

	// mgmtClusterProxy wraps the management cluster client, scheme and REST
	// config (framework.NewClusterProxy over the emitted admin kubeconfig).
	mgmtClusterProxy framework.ClusterProxy
	// mgmtClient is the controller-runtime client used by specs and framework
	// helpers.
	mgmtClient client.Client
	// mgmtRESTConfig is the REST config used for dynamic-client operations
	// (VC-04 conversion round-trip).
	mgmtRESTConfig *rest.Config
	// mgmtScheme registers the CAPI, core and CRD types used by the suite.
	mgmtScheme *runtime.Scheme
)

var _ = BeforeSuite(func(_ SpecContext) {
	suiteCtx, cancelSuite = context.WithCancel(context.Background())

	// VC-08: the suite must not ride on a stock kind management cluster.
	assertNoKindCluster()

	stateDir = os.Getenv("CAPISHIM_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join(os.TempDir(), "capishim-e2e", "state")
	}

	clusterProvider = shim.NewClusterProvider(stateDir)
	clusterProvider.Create(suiteCtx)

	kubeconfigPath = clusterProvider.GetKubeconfigPath()
	Expect(kubeconfigPath).NotTo(BeEmpty(), "shim must emit the admin kubeconfig path")
	Expect(kubeconfigPath).To(BeAnExistingFile(), "admin kubeconfig %s must exist", kubeconfigPath)

	mgmtScheme = buildScheme()
	mgmtClusterProxy = framework.NewClusterProxy("capishim-mgmt", kubeconfigPath, mgmtScheme)
	mgmtClient = mgmtClusterProxy.GetClient()
	mgmtRESTConfig = mgmtClusterProxy.GetRESTConfig()

	// The management apiserver must accept authenticated requests before any
	// spec runs (VC-01 precondition).
	version, err := getServerVersion(kubeconfigPath)
	Expect(err).NotTo(HaveOccurred(), "management apiserver must accept authenticated requests")
	Expect(version).NotTo(BeEmpty())
}, NodeTimeout(15*time.Minute))

var _ = AfterSuite(func() {
	if clusterProvider != nil {
		clusterProvider.Dispose(suiteCtx)
	}
	if cancelSuite != nil {
		cancelSuite()
	}
})

// buildScheme returns the runtime scheme used by the management cluster client.
func buildScheme() *runtime.Scheme {
	sc := runtime.NewScheme()
	framework.TryAddDefaultSchemes(sc)
	return sc
}

// getServerVersion performs an authenticated GET /version against the apiserver
// reachable through kubeconfigPath. VC-01 uses it to prove the apiserver
// accepts authenticated requests; VC-02/VC-06 use it to prove a kubeconfig
// issued before a restart still authenticates afterwards.
func getServerVersion(kubeconfigPath string) (string, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return "", err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return version.GitVersion, nil
}

// assertNoKindCluster fails the suite if a kind management cluster exists
// (VC-08: the suite must run against the capishim quadlet pod, never kind).
func assertNoKindCluster() {
	out, err := exec.Command("kind", "get", "clusters").Output()
	if err != nil {
		// kind is not installed or not usable; the suite cannot be riding on a
		// kind management cluster.
		return
	}
	clusters := strings.Fields(string(out))
	Expect(clusters).To(BeEmpty(),
		"VC-08 requires the capishim quadlet pod as the management cluster; found kind clusters %v", clusters)
}

// templatesDir returns the directory holding the vendored in-memory templates.
// It honors CAPISHIM_TEMPLATES_DIR and otherwise walks up from the working
// directory (the suite usually runs from e2e/, so the repo root is one level up).
func templatesDir() string {
	if dir := os.Getenv("CAPISHIM_TEMPLATES_DIR"); dir != "" {
		return dir
	}
	dir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve the working directory")
	for {
		if _, err := os.Stat(filepath.Join(dir, "templates", "cluster-template-in-memory.yaml")); err == nil {
			return filepath.Join(dir, "templates")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	Fail("could not locate templates/cluster-template-in-memory.yaml; set CAPISHIM_TEMPLATES_DIR")
	return ""
}

var templateVarPattern = regexp.MustCompile(`\$\{([A-Z0-9_]+)(:=([^}]*))?\}`)

// renderTemplate substitutes ${VAR} and ${VAR:=default} placeholders. Values
// provided in vars win; otherwise the template default is emitted verbatim so
// YAML structure (quotes for strings, flow lists for CIDR blocks) is preserved.
func renderTemplate(data []byte, vars map[string]string) []byte {
	return templateVarPattern.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := templateVarPattern.FindSubmatch(m)
		name := string(sub[1])
		if value, ok := vars[name]; ok {
			return []byte(value)
		}
		if len(sub[2]) > 0 {
			return sub[3]
		}
		return m
	})
}

// applyInNamespace decodes multi-document YAML, forces every object into the
// given namespace, and creates each with retries (admission webhooks may be
// briefly unavailable right after a pod restart). Used for the vendored
// clusterclass-in-memory.yaml, whose objects carry no namespace.
func applyInNamespace(ctx context.Context, namespace string, data []byte) {
	objs, err := capiyaml.ToUnstructured(data)
	Expect(err).NotTo(HaveOccurred(), "failed to decode multi-document YAML")
	Expect(objs).NotTo(BeEmpty(), "expected at least one object in the YAML")
	for i := range objs {
		obj := &objs[i]
		obj.SetNamespace(namespace)
		Eventually(func(g Gomega) {
			g.Expect(mgmtClient.Create(ctx, obj)).To(Succeed(),
				"failed to create %s %s/%s", obj.GetKind(), namespace, obj.GetName())
		}, webhookOperationTimeout, pollInterval).Should(Succeed())
	}
}

// applyInMemoryClusterClass applies the vendored ClusterClass and its templates
// (DevClusterTemplate, KubeadmControlPlaneTemplate, DevMachineTemplate x2,
// KubeadmConfigTemplate, ClusterClass "in-memory") into namespace and waits for
// the ClusterClass to be readable.
func applyInMemoryClusterClass(ctx context.Context, namespace string) {
	data, err := os.ReadFile(filepath.Join(templatesDir(), "clusterclass-in-memory.yaml"))
	Expect(err).NotTo(HaveOccurred(), "failed to read clusterclass-in-memory.yaml")
	rendered := renderTemplate(data, map[string]string{"NAMESPACE": namespace})
	By(fmt.Sprintf("Applying in-memory ClusterClass objects to namespace %s", namespace))
	applyInNamespace(ctx, namespace, rendered)

	Eventually(func(g Gomega) {
		cc := &clusterv1.ClusterClass{}
		g.Expect(mgmtClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "in-memory"}, cc)).To(Succeed(),
			"ClusterClass in-memory must be readable in namespace %s", namespace)
		g.Expect(cc.Spec.Infrastructure.TemplateRef.Name).To(Equal("in-memory-cluster"))
		g.Expect(cc.Spec.ControlPlane.TemplateRef.Name).To(Equal("in-memory-control-plane"))
	}, webhookOperationTimeout, pollInterval).Should(Succeed())
}

// applyInMemoryClusterTemplate renders cluster-template-in-memory.yaml with the
// given variables and applies it into the target namespace.
func applyInMemoryClusterTemplate(
	ctx context.Context,
	namespace, clusterName string,
	controlPlaneCount, workerCount int,
) {
	data, err := os.ReadFile(filepath.Join(templatesDir(), "cluster-template-in-memory.yaml"))
	Expect(err).NotTo(HaveOccurred(), "failed to read cluster-template-in-memory.yaml")
	rendered := renderTemplate(data, map[string]string{
		"CLUSTER_NAME":                clusterName,
		"NAMESPACE":                   namespace,
		"KUBERNETES_VERSION":          kubernetesVersion,
		"CONTROL_PLANE_MACHINE_COUNT": strconv.Itoa(controlPlaneCount),
		"WORKER_MACHINE_COUNT":        strconv.Itoa(workerCount),
	})
	By(fmt.Sprintf("Applying in-memory cluster template for %s/%s (cp=%d workers=%d)",
		namespace, clusterName, controlPlaneCount, workerCount))
	// The Cluster object carries metadata.namespace from the rendered template;
	// create with polling to absorb transient admission-webhook unavailability.
	Expect(
		mgmtClusterProxy.Create(ctx, rendered, framework.CreateWithPolling(webhookOperationTimeout, pollInterval)),
	).To(Succeed())
}

// waitForClusterProvisioned waits for the full in-memory provisioning outcome
// (REQ-008 / VC-05) and returns the provisioned Cluster.
func waitForClusterProvisioned(ctx context.Context, clusterName, namespace string) *clusterv1.Cluster {
	By(fmt.Sprintf("Waiting for cluster %s/%s to reach Phase=Provisioned", namespace, clusterName))
	return framework.DiscoveryAndWaitForCluster(ctx, framework.DiscoveryAndWaitForClusterInput{
		Getter:    mgmtClient,
		Namespace: namespace,
		Name:      clusterName,
	}, provisioningTimeout, pollInterval)
}

// expectKCPInitialized asserts the KubeadmControlPlane of the cluster is
// Initialized with the expected number of available replicas (REQ-008).
func expectKCPInitialized(ctx context.Context, clusterName, namespace string, replicas int32) {
	By(fmt.Sprintf("Waiting for KCP of cluster %s/%s to be initialized", namespace, clusterName))
	Eventually(func(g Gomega) {
		kcpList := &controlplanev1.KubeadmControlPlaneList{}
		g.Expect(mgmtClient.List(ctx, kcpList,
			client.InNamespace(namespace),
			client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName})).To(Succeed())
		g.Expect(kcpList.Items).To(HaveLen(1),
			"expected exactly one KCP for cluster %s/%s", namespace, clusterName)
		kcp := kcpList.Items[0]
		g.Expect(ptr.Deref(kcp.Status.Initialization.ControlPlaneInitialized, false)).To(BeTrue(),
			"KCP %s must be Initialized", kcp.Name)
		g.Expect(ptr.Deref(kcp.Status.AvailableReplicas, 0)).To(Equal(replicas),
			"KCP %s must have %d available replicas", kcp.Name, replicas)
	}, provisioningTimeout, pollInterval).Should(Succeed())
}

// expectMachinesReady asserts the cluster has exactly expected Machines, all
// Ready with a NodeRef populated by the in-memory backend (REQ-008).
func expectMachinesReady(ctx context.Context, clusterName, namespace string, expected int) {
	By(fmt.Sprintf("Waiting for %d Ready machines with NodeRef for cluster %s/%s",
		expected, namespace, clusterName))
	Eventually(func(g Gomega) {
		machines := &clusterv1.MachineList{}
		g.Expect(mgmtClient.List(ctx, machines,
			client.InNamespace(namespace),
			client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName})).To(Succeed())
		g.Expect(machines.Items).To(HaveLen(expected),
			"expected %d machines, got %d", expected, len(machines.Items))
		for _, m := range machines.Items {
			g.Expect(m.Status.NodeRef.IsDefined()).To(BeTrue(), "Machine %s has no NodeRef", m.Name)
			ready := false
			for _, c := range m.Status.Conditions {
				if c.Type == clusterv1.ReadyCondition && c.Status == metav1.ConditionTrue {
					ready = true
				}
			}
			g.Expect(ready).To(BeTrue(), "Machine %s is not Ready", m.Name)
		}
	}, provisioningTimeout, pollInterval).Should(Succeed())
}

// expectClusterKubeconfigSecret asserts the <cluster>-kubeconfig Secret exists
// with a non-empty value (REQ-008 / VC-05).
func expectClusterKubeconfigSecret(ctx context.Context, clusterName, namespace string) {
	By(fmt.Sprintf("Waiting for %s-kubeconfig Secret in namespace %s", clusterName, namespace))
	Eventually(func(g Gomega) {
		secret := &corev1.Secret{}
		g.Expect(mgmtClient.Get(ctx,
			client.ObjectKey{Namespace: namespace, Name: clusterName + "-kubeconfig"}, secret)).To(Succeed())
		g.Expect(secret.Data).To(HaveKey("value"))
		g.Expect(secret.Data["value"]).NotTo(BeEmpty())
	}, webhookOperationTimeout, pollInterval).Should(Succeed())
}

// waitForCRDEstablished waits for a CRD to report the Established condition.
func waitForCRDEstablished(ctx context.Context, name string) {
	Eventually(func(g Gomega) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		g.Expect(mgmtClient.Get(ctx, client.ObjectKey{Name: name}, crd)).To(Succeed(),
			"CRD %s must be served", name)
		established := false
		for _, c := range crd.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		g.Expect(established).To(BeTrue(), "CRD %s is not Established: %+v", name, crd.Status.Conditions)
	}, webhookOperationTimeout, pollInterval).Should(Succeed())
}

// cleanupClusterAndNamespace deletes the cluster, waits for it (and the
// controller-driven cascade of its owned resources) to be gone, then deletes
// the namespace best-effort. Note: the shim has no namespace controller, so a
// Terminating namespace may linger until the pod is disposed; each spec uses a
// dedicated namespace so this does not affect later specs.
func cleanupClusterAndNamespace(ctx context.Context, namespace, clusterName string) {
	By(fmt.Sprintf("Cleaning up cluster %s/%s", namespace, clusterName))
	cluster := &clusterv1.Cluster{}
	if err := mgmtClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, cluster); err == nil {
		Expect(mgmtClient.Delete(ctx, cluster)).To(Succeed())
	} else {
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}

	Eventually(func(g Gomega) {
		err := mgmtClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, &clusterv1.Cluster{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"cluster %s/%s must be fully deleted", namespace, clusterName)
	}, provisioningTimeout, pollInterval).Should(Succeed())

	ns := &corev1.Namespace{}
	if err := mgmtClient.Get(ctx, client.ObjectKey{Name: namespace}, ns); err == nil {
		framework.DeleteNamespace(suiteCtx, framework.DeleteNamespaceInput{Deleter: mgmtClient, Name: namespace})
	}
}
