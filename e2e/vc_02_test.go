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
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// VC-02: Two consecutive pod restarts produce an unchanged CA and admin client
// cert; a kubeconfig issued before the first restart authenticates after the
// second. The pod CA lives on the host volume (stateDir/pki), so restarting the
// pod must not regenerate or invalidate it (REQ-002, REQ-009).
var _ = Describe("VC-02 setup idempotency and cert persistence across restarts", func() {
	var caPath string
	var adminCertPath string

	BeforeEach(func() {
		caPath = filepath.Join(stateDir, "pki", "ca.crt")
		adminCertPath = filepath.Join(stateDir, "pki", "admin.crt")
		Expect(caPath).To(BeAnExistingFile(), "pod CA must exist at %s", caPath)
		Expect(adminCertPath).To(BeAnExistingFile(), "admin client cert must exist at %s", adminCertPath)
	})

	It("keeps the CA and admin cert byte-identical and the pre-restart kubeconfig valid", func() {
		caBefore, err := os.ReadFile(caPath)
		Expect(err).NotTo(HaveOccurred())
		adminCertBefore, err := os.ReadFile(adminCertPath)
		Expect(err).NotTo(HaveOccurred())

		// The kubeconfig issued before the first restart must authenticate.
		version, err := getServerVersion(kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "pre-restart kubeconfig must authenticate before any restart")
		Expect(version).NotTo(BeEmpty())

		for restart := 1; restart <= 2; restart++ {
			By(fmt.Sprintf("Restarting the pod (restart %d)", restart))
			clusterProvider.Restart(suiteCtx)

			caAfter, err := os.ReadFile(caPath)
			Expect(err).NotTo(HaveOccurred())
			adminCertAfter, err := os.ReadFile(adminCertPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(caAfter).To(Equal(caBefore),
				"pod CA must be byte-identical after restart %d", restart)
			Expect(adminCertAfter).To(Equal(adminCertBefore),
				"admin client cert must be byte-identical after restart %d", restart)

			version, err := getServerVersion(kubeconfigPath)
			Expect(err).NotTo(HaveOccurred(),
				"pre-restart kubeconfig must authenticate after restart %d", restart)
			Expect(version).NotTo(BeEmpty())
		}
	})
})
