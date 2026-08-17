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

package e2e

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	framework "sigs.k8s.io/cluster-api/test/framework"

	"github.com/moeryomenko/capishim/e2e/shim"
)

// VC-03: All CAPI CRDs report ESTABLISHED; manager kubeconfigs perform their
// controller operations with zero RBAC Forbidden errors in manager logs during
// a full provisioning run; an unbound identity gets Forbidden on list clusters
// (REQ-003, REQ-004).
var _ = Describe("VC-03 CRDs and RBAC are provisioned", Ordered, func() {
	var namespace *corev1.Namespace
	const clusterName = "vc-03-cluster"

	// Run a full provisioning pass first so the manager logs scanned below
	// cover a complete controller workload (the precondition the VC states).
	BeforeAll(func() {
		namespace = framework.CreateNamespace(suiteCtx, framework.CreateNamespaceInput{
			Creator:             mgmtClient,
			Name:                "vc-03-crds-rbac",
			IgnoreAlreadyExists: true,
		}, "2m", pollInterval)

		applyInMemoryClusterClass(suiteCtx, namespace.Name)
		applyInMemoryClusterTemplate(suiteCtx, namespace.Name, clusterName,
			defaultControlPlaneMachineCount, defaultWorkerMachineCount)
		waitForClusterProvisioned(suiteCtx, clusterName, namespace.Name)
	})

	It("has all REQ-003 CRDs Established", func() {
		for _, name := range expectedCRDs {
			waitForCRDEstablished(suiteCtx, name)
		}
	})

	It("has zero RBAC Forbidden errors in manager logs during the provisioning run", func() {
		for _, container := range managerContainers {
			logs, err := clusterProvider.ManagerLogs(suiteCtx, container)
			Expect(err).NotTo(HaveOccurred(), "failed to read logs of manager %s", container)
			Expect(strings.ToLower(logs)).NotTo(ContainSubstring("forbidden"),
				"manager %s logged RBAC Forbidden errors during provisioning", container)
		}
	})

	It("rejects an unbound identity with Forbidden on list clusters", func() {
		unboundKubeconfig, err := shim.MintUnboundClientCert(suiteCtx, kubeconfigPath, stateDir)
		Expect(err).NotTo(HaveOccurred(), "failed to mint an unbound client cert")
		Expect(unboundKubeconfig).To(BeAnExistingFile(), "unbound kubeconfig %s must exist", unboundKubeconfig)

		cfg, err := clientcmd.BuildConfigFromFlags("", unboundKubeconfig)
		Expect(err).NotTo(HaveOccurred())
		unboundClient, err := client.New(cfg, client.Options{Scheme: mgmtScheme})
		Expect(err).NotTo(HaveOccurred())

		err = unboundClient.List(suiteCtx, &clusterv1.ClusterList{})
		Expect(err).To(HaveOccurred(), "an unbound identity must not be able to list clusters")
		Expect(apierrors.IsForbidden(err)).To(BeTrue(),
			"unbound identity must receive Forbidden on list clusters, got %v", err)
	})

	AfterAll(func() {
		cleanupClusterAndNamespace(suiteCtx, namespace.Name, clusterName)
	})
})
