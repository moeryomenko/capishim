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
	"k8s.io/client-go/tools/clientcmd"
)

// VC-08: make test-e2e-shim passes in CI using the quadlet pod as the
// management cluster (no kind, no clusterctl init). The BeforeSuite-level kind
// guard lives in suite_test.go; this spec asserts the management cluster is
// the capishim apiserver behind the emitted admin kubeconfig.
var _ = Describe("VC-08 the suite is the green e2e run with the shim bootstrap", func() {
	It("uses the capishim quadlet pod as the management cluster (no kind, no clusterctl init)", func() {
		// The emitted admin kubeconfig must point at the capishim apiserver
		// publish address (REQ-010 default 127.0.0.1:6443), not at a kind
		// cluster endpoint.
		config, err := clientcmd.LoadFromFile(kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "failed to load the admin kubeconfig %s", kubeconfigPath)
		Expect(config.CurrentContext).NotTo(BeEmpty())
		currentContext := config.Contexts[config.CurrentContext]
		Expect(currentContext).NotTo(BeNil())
		clusterCfg := config.Clusters[currentContext.Cluster]
		Expect(clusterCfg).NotTo(BeNil())
		Expect(clusterCfg.Server).To(Equal("https://127.0.0.1:6443"),
			"admin kubeconfig must target the capishim apiserver publish address")

		// And the management stack answers through it.
		version, err := getServerVersion(kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "management apiserver must be reachable through the admin kubeconfig")
		Expect(version).NotTo(BeEmpty())
	})
})
