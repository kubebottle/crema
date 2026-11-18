/*
Copyright 2026 kubebottle.

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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kubebottle/crema/test/utils"
)

var _ = Describe("GPUWorkspace Lifecycle", Ordered, func() {
	const (
		wsName = "e2e-workspace"
		wsNS   = "default"
	)

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "gpuworkspace", wsName, "-n", wsNS, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)
	})

	It("persists rootfs mutations and whiteouts across GPU scale and suspend/resume", func() {
		By("creating GPUWorkspace with 0 GPUs")
		manifest := fmt.Sprintf(`
apiVersion: crema.kubebottle.io/v1alpha1
kind: GPUWorkspace
metadata:
  name: %s
  namespace: %s
spec:
  suspend: false
  template:
    spec:
      containers:
        - name: workspace
          image: alpine:3.20
          command: ["sh", "-c", "touch /etc/crema-to-delete && sleep infinity"]
          resources:
            limits:
              nvidia.com/gpu: "0"
`, wsName, wsNS)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for GPUWorkspace to reach Running")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "gws", wsName, "-n", wsNS, "-o", "jsonpath={.status.phase}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("Running"))
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		podName := wsName + "-ws"

		By("mutating rootfs (install jq, write dotfile, delete /etc/crema-to-delete for whiteout)")
		_, err = utils.Run(exec.Command("kubectl", "exec", podName, "-n", wsNS, "--",
			"sh", "-c", "apk add --no-cache jq && echo 'export CREMA_OK=1' > /root/.profile && rm -f /etc/crema-to-delete"))
		Expect(err).NotTo(HaveOccurred())

		By("suspending GPUWorkspace before changing GPU allocation (Strategy B)")
		_, err = utils.Run(exec.Command("kubectl", "patch", "gws", wsName, "-n", wsNS, "--type=merge",
			"-p", `{"spec":{"suspend":true}}`))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "gws", wsName, "-n", wsNS, "-o", "jsonpath={.status.phase}:{.status.snapshotSeq}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("Suspended:1"))
		}, 5*time.Minute, 3*time.Second).Should(Succeed())

		By("scaling nvidia.com/gpu to 1 and resuming GPUWorkspace")
		_, err = utils.Run(exec.Command("kubectl", "patch", "gws", wsName, "-n", wsNS, "--type=merge",
			"-p", `{"spec":{"suspend":false,"template":{"spec":{"containers":[{"name":"workspace","resources":{"limits":{"nvidia.com/gpu":"1"}}}]}}}}`))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for resumed Pod with GPU=1 to reach Running")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "gws", wsName, "-n", wsNS, "-o", "jsonpath={.status.phase}:{.status.snapshotSeq}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("Running:1"))
		}, 5*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying jq, dotfile, and whiteout deletion survived suspend/resize/resume")
		out, err := utils.Run(exec.Command("kubectl", "exec", podName, "-n", wsNS, "--",
			"sh", "-c", "jq --version && cat /root/.profile && test ! -e /etc/crema-to-delete && echo WHITEOUT_OK"))
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("WHITEOUT_OK"))
	})
})
