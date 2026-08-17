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

	"github.com/moeryomenko/capishim/e2e/shim"
)

// VC-01: On a clean host, booting the pod results in all containers Running
// (six long-running + pki/setup oneshot exited 0) and the apiserver accepting
// authenticated requests within 5 minutes.
var _ = Describe("VC-01 quadlet pod boots a clean management stack", func() {
	It("runs the six long-running containers and completes pki and setup", func() {
		states, err := clusterProvider.ContainerStates(suiteCtx)
		Expect(err).NotTo(HaveOccurred(), "failed to inspect pod container states")

		byName := make(map[string]shim.ContainerState, len(states))
		for _, s := range states {
			byName[s.Name] = s
		}

		for _, name := range longRunningContainers {
			s, ok := byName[name]
			Expect(ok).To(BeTrue(), "expected container %s to exist in the pod", name)
			Expect(s.State).To(Equal("running"), "container %s should be running, got %q", name, s.State)
		}
		for _, name := range oneshotContainers {
			s, ok := byName[name]
			Expect(ok).To(BeTrue(), "expected container %s to exist in the pod", name)
			Expect(s.State).To(Equal("exited"), "container %s should be exited (oneshot), got %q", name, s.State)
			Expect(s.ExitCode).To(Equal(0), "container %s should have exited 0, got %d", name, s.ExitCode)
		}
	})

	It("accepts authenticated requests on the apiserver", func() {
		version, err := getServerVersion(kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "authenticated GET /version must succeed against the shim apiserver")
		Expect(version).NotTo(BeEmpty())
	})
})
