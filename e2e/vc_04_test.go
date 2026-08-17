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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	framework "sigs.k8s.io/cluster-api/test/framework"
)

// VC-04: After the webhook rewrite, an invalid Cluster is rejected by the
// admission webhook with a CAPI-specific error, and a v1beta1→v1beta2
// conversion of a Cluster object succeeds via the conversion webhook
// (REQ-005).
var _ = Describe("VC-04 admission and conversion webhooks are functional after the rewrite", func() {
	var namespace *corev1.Namespace

	BeforeEach(func() {
		namespace = framework.CreateNamespace(suiteCtx, framework.CreateNamespaceInput{
			Creator:             mgmtClient,
			Name:                "vc-04-webhooks",
			IgnoreAlreadyExists: true,
		}, "2m", pollInterval)
	})

	It("rejects an invalid Cluster with a CAPI-specific validation error", func() {
		invalid := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-cluster",
				Namespace: namespace.Name,
			},
			// No topology, no controlPlaneRef, no infrastructureRef: this
			// violates the CAPI Cluster webhook rule. The spec must still be
			// non-empty (the v1beta2 CRD schema enforces
			// spec.minProperties=1), otherwise the structural schema rejects
			// the object before the admission webhook runs and the
			// CAPI-specific message never appears.
			Spec: clusterv1.ClusterSpec{
				ClusterNetwork: clusterv1.ClusterNetwork{
					Pods: clusterv1.NetworkRanges{CIDRBlocks: []string{"10.0.0.0/16"}},
				},
			},
		}
		err := mgmtClient.Create(suiteCtx, invalid)
		Expect(err).To(HaveOccurred(), "invalid Cluster must be rejected by the admission webhook")
		Expect(err.Error()).To(ContainSubstring(
			"one of spec.controlPlaneRef, spec.infrastructureRef or spec.topology must be set"),
			"rejection must carry the CAPI-specific validation message, got %v", err)
	})

	It("round-trips a Cluster through the v1beta1->v1beta2 conversion webhook", func() {
		dyn, err := dynamic.NewForConfig(mgmtRESTConfig)
		Expect(err).NotTo(HaveOccurred())

		const name = "conversion-cluster"
		v1beta1GVR := schema.GroupVersionResource{
			Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters",
		}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "cluster.x-k8s.io/v1beta1",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace.Name,
			},
			"spec": map[string]any{
				"clusterNetwork": map[string]any{
					"pods":          map[string]any{"cidrBlocks": []any{"192.168.0.0/16"}},
					"services":      map[string]any{"cidrBlocks": []any{"10.128.0.0/12"}},
					"serviceDomain": "cluster.local",
				},
				"infrastructureRef": map[string]any{
					"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
					"kind":       "DevCluster",
					"name":       name + "-infra",
				},
			},
		}}

		// Create with the v1beta1 GVK; the conversion webhook converts the
		// object to the v1beta2 storage version on write.
		_, err = dyn.Resource(v1beta1GVR).Namespace(namespace.Name).Create(suiteCtx, obj, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "v1beta1 Cluster must be created via the conversion webhook")

		// Read the same object back with the v1beta2 GVK and verify the
		// round-trip preserved the spec.
		v1beta2GVR := schema.GroupVersionResource{
			Group: "cluster.x-k8s.io", Version: "v1beta2", Resource: "clusters",
		}
		got, err := dyn.Resource(v1beta2GVR).Namespace(namespace.Name).Get(suiteCtx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Cluster must be readable as v1beta2")
		Expect(got.GetAPIVersion()).To(Equal("cluster.x-k8s.io/v1beta2"))

		serviceDomain, found, err := unstructured.NestedString(got.Object, "spec", "clusterNetwork", "serviceDomain")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "converted v1beta2 Cluster must preserve spec.clusterNetwork.serviceDomain")
		Expect(serviceDomain).To(Equal("cluster.local"))

		Expect(dyn.Resource(v1beta2GVR).Namespace(namespace.Name).
			Delete(suiteCtx, name, metav1.DeleteOptions{})).To(Succeed())
	})
})
