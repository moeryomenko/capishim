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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	framework "sigs.k8s.io/cluster-api/test/framework"
)

// VC-05: Applying the in-memory template yields Cluster Phase=Provisioned, KCP
// Initialized, all control-plane and worker Machines Ready with NodeRef, and a
// <name>-kubeconfig Secret. REQ-012 additionally requires the suite to cover
// scale-up of control plane and workers and cluster deletion, so this container
// also scales the topology and removes the cluster afterwards.
var _ = Describe("VC-05 in-memory ClusterClass-based cluster reaches Provisioned", Ordered, func() {
	var namespace *corev1.Namespace
	const clusterName = "vc-05-cluster"

	BeforeAll(func() {
		namespace = framework.CreateNamespace(suiteCtx, framework.CreateNamespaceInput{
			Creator:             mgmtClient,
			Name:                "vc-05-provisioning",
			IgnoreAlreadyExists: true,
		}, "2m", pollInterval)

		applyInMemoryClusterClass(suiteCtx, namespace.Name)
		applyInMemoryClusterTemplate(suiteCtx, namespace.Name, clusterName,
			defaultControlPlaneMachineCount, defaultWorkerMachineCount)
	})

	It("reaches Cluster Phase=Provisioned with KCP initialized and Ready machines", func() {
		cluster := waitForClusterProvisioned(suiteCtx, clusterName, namespace.Name)
		Expect(cluster.Status.Phase).To(Equal(string(clusterv1.ClusterPhaseProvisioned)),
			"Cluster %s/%s must be Provisioned", namespace.Name, clusterName)

		expectKCPInitialized(suiteCtx, clusterName, namespace.Name, defaultControlPlaneMachineCount)
		expectMachinesReady(suiteCtx, clusterName, namespace.Name,
			defaultControlPlaneMachineCount+defaultWorkerMachineCount)
		expectClusterKubeconfigSecret(suiteCtx, clusterName, namespace.Name)
	})

	It("scales up the control plane and worker replicas (REQ-012)", func() {
		// Patch the Cluster topology: control plane 1->3, worker md-0 1->2.
		// Note: the KubeadmControlPlane webhook forbids even control-plane
		// replica counts with stacked etcd ("cannot be an even number when
		// etcd is stacked"), so the scale-up target must be odd.
		Eventually(func(g Gomega) {
			cur := &clusterv1.Cluster{}
			g.Expect(mgmtClient.Get(suiteCtx,
				client.ObjectKey{Namespace: namespace.Name, Name: clusterName}, cur)).To(Succeed())
			g.Expect(cur.Spec.Topology.IsDefined()).To(BeTrue())
			g.Expect(cur.Spec.Topology.Workers.MachineDeployments).To(HaveLen(1))
			cur.Spec.Topology.ControlPlane.Replicas = new(int32(3))
			cur.Spec.Topology.Workers.MachineDeployments[0].Replicas = new(int32(2))
			g.Expect(mgmtClient.Update(suiteCtx, cur)).To(Succeed())
		}, webhookOperationTimeout, pollInterval).Should(Succeed())

		expectKCPInitialized(suiteCtx, clusterName, namespace.Name, 3)
		expectMachinesReady(suiteCtx, clusterName, namespace.Name, 5)

		// The MachineDeployment reconciles to the desired worker count (REQ-008).
		Eventually(func(g Gomega) {
			mdList := &clusterv1.MachineDeploymentList{}
			g.Expect(mgmtClient.List(suiteCtx, mdList,
				client.InNamespace(namespace.Name),
				client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName})).To(Succeed())
			g.Expect(mdList.Items).To(HaveLen(1),
				"expected one MachineDeployment for cluster %s", clusterName)
			g.Expect(ptr.Deref(mdList.Items[0].Spec.Replicas, 0)).To(Equal(int32(2)),
				"MachineDeployment must reconcile to 2 workers")
		}, provisioningTimeout, pollInterval).Should(Succeed())
	})

	AfterAll(func() {
		cleanupClusterAndNamespace(suiteCtx, namespace.Name, clusterName)
	})
})
