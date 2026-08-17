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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	framework "sigs.k8s.io/cluster-api/test/framework"
)

// VC-06: After a pod restart, previously created clusters remain Provisioned
// and keep reconciling; the pre-restart admin kubeconfig still works (REQ-009).
var _ = Describe("VC-06 restart persistence keeps clusters Provisioned", Ordered, func() {
	var namespace *corev1.Namespace
	const clusterName = "vc-06-cluster"

	BeforeAll(func() {
		namespace = framework.CreateNamespace(suiteCtx, framework.CreateNamespaceInput{
			Creator:             mgmtClient,
			Name:                "vc-06-restart-persistence",
			IgnoreAlreadyExists: true,
		}, "2m", pollInterval)

		applyInMemoryClusterClass(suiteCtx, namespace.Name)
		applyInMemoryClusterTemplate(suiteCtx, namespace.Name, clusterName,
			defaultControlPlaneMachineCount, defaultWorkerMachineCount)
		waitForClusterProvisioned(suiteCtx, clusterName, namespace.Name)
	})

	It("keeps previously created clusters Provisioned and reconciling after a pod restart", func() {
		clusterProvider.Restart(suiteCtx)

		// The kubeconfig issued before the restart still authenticates.
		version, err := getServerVersion(kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "pre-restart admin kubeconfig must authenticate after the pod restart")
		Expect(version).NotTo(BeEmpty())

		// The cluster remains Provisioned: status persists in etcd and the
		// controllers re-reconcile the same objects after the restart.
		Eventually(func(g Gomega) {
			cluster := &clusterv1.Cluster{}
			g.Expect(mgmtClient.Get(suiteCtx,
				client.ObjectKey{Namespace: namespace.Name, Name: clusterName}, cluster)).To(Succeed())
			g.Expect(cluster.Status.Phase).To(Equal(string(clusterv1.ClusterPhaseProvisioned)),
				"cluster %s/%s must remain Provisioned", namespace.Name, clusterName)
		}, provisioningTimeout, pollInterval).Should(Succeed())

		expectKCPInitialized(suiteCtx, clusterName, namespace.Name, defaultControlPlaneMachineCount)
		expectMachinesReady(suiteCtx, clusterName, namespace.Name,
			defaultControlPlaneMachineCount+defaultWorkerMachineCount)
	})

	AfterAll(func() {
		cleanupClusterAndNamespace(suiteCtx, namespace.Name, clusterName)
	})
})
