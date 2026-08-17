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

// VC-07: kubectl get clusterclass succeeds; a ClusterClass-based cluster
// reconciles via the topology controllers; the MachinePool CRD is served
// (REQ-006: ClusterTopology gate on, MachinePool default on).
var _ = Describe("VC-07 ClusterClass and MachinePool are served", Ordered, func() {
	var namespace *corev1.Namespace
	const clusterName = "vc-07-cluster"

	BeforeAll(func() {
		namespace = framework.CreateNamespace(suiteCtx, framework.CreateNamespaceInput{
			Creator:             mgmtClient,
			Name:                "vc-07-clusterclass-machinepool",
			IgnoreAlreadyExists: true,
		}, "2m", pollInterval)
	})

	It("serves the in-memory ClusterClass", func() {
		applyInMemoryClusterClass(suiteCtx, namespace.Name)

		cc := &clusterv1.ClusterClass{}
		Eventually(func(g Gomega) {
			g.Expect(mgmtClient.Get(suiteCtx,
				client.ObjectKey{Namespace: namespace.Name, Name: "in-memory"}, cc)).To(Succeed(),
				"kubectl get clusterclass must succeed")
		}, webhookOperationTimeout, pollInterval).Should(Succeed())

		Expect(cc.Spec.Infrastructure.TemplateRef.Name).To(Equal("in-memory-cluster"))
		Expect(cc.Spec.ControlPlane.TemplateRef.Name).To(Equal("in-memory-control-plane"))
	})

	It("serves the MachinePool CRD", func() {
		waitForCRDEstablished(suiteCtx, "machinepools.cluster.x-k8s.io")
	})

	It("reconciles a ClusterClass-based cluster via the topology controllers", func() {
		applyInMemoryClusterTemplate(suiteCtx, namespace.Name, clusterName,
			defaultControlPlaneMachineCount, 0)

		cluster := waitForClusterProvisioned(suiteCtx, clusterName, namespace.Name)
		Expect(cluster.Spec.Topology.IsDefined()).To(BeTrue(),
			"Cluster must be created from a managed topology")
		Expect(cluster.Spec.Topology.ClassRef.Name).To(Equal("in-memory"))

		// Topology expansion produced and reconciled a KCP from the ClusterClass.
		expectKCPInitialized(suiteCtx, clusterName, namespace.Name, defaultControlPlaneMachineCount)
		expectMachinesReady(suiteCtx, clusterName, namespace.Name, defaultControlPlaneMachineCount)
	})

	AfterAll(func() {
		cleanupClusterAndNamespace(suiteCtx, namespace.Name, clusterName)
	})
})
