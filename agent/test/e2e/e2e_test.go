//go:build e2e
// +build e2e

/*
Copyright 2026.

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
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/scality/image-cache/agent/test/utils"
)

// namespace where the project is deployed in
const namespace = "image-cache-agent-system"

// daemonSetName is the name of the per-node agent DaemonSet.
const daemonSetName = "image-cache-agent-controller-manager"

// imageCacheName is the ImageCache resource used to exercise the node
// labelling contract: nodeSelector/source/status label, end to end, without
// any registry infrastructure.
const imageCacheName = "imagecache-e2e-smoke"

// nodeLabelKey is the sync-status label the agent sets on a selected node for
// imageCacheName. See api/v1alpha1.ImageCache's doc comment for the contract.
const nodeLabelKey = "image-cache.scality.com/" + imageCacheName

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		// The agent mounts a hostPath volume for the cache directory, and its
		// chown-cache init container needs root plus the CHOWN capability to
		// hand that directory to the agent UID (fsGroup does not apply to
		// hostPath). hostPath volumes are already disallowed starting at the
		// "baseline" level, so this DaemonSet needs "privileged".
		By("labeling the namespace to enforce the privileged security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=privileged")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with privileged policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("deleting the smoke-test ImageCache, in case an earlier step left it behind")
		cmd := exec.Command("kubectl", "delete", "imagecache", imageCacheName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should have the DaemonSet ready on every scheduled node", func() {
			By("waiting for the DaemonSet to report every scheduled pod ready")
			verifyDaemonSetReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset", daemonSetName, "-n", namespace,
					"-o", "jsonpath={.status.desiredNumberScheduled}")
				desired, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(desired).NotTo(Equal("0"), "no node has been scheduled yet")

				cmd = exec.Command("kubectl", "get", "daemonset", daemonSetName, "-n", namespace,
					"-o", "jsonpath={.status.numberReady}")
				ready, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ready).To(Equal(desired), "not every scheduled pod is ready yet")
			}
			Eventually(verifyDaemonSetReady).Should(Succeed())

			By("getting the name of the controller-manager pod")
			cmd := exec.Command("kubectl", "get",
				"pods", "-l", "control-plane=controller-manager",
				"-o", "go-template={{ range .items }}"+
					"{{ if not .metadata.deletionTimestamp }}"+
					"{{ .metadata.name }}"+
					"{{ \"\\n\" }}{{ end }}{{ end }}",
				"-n", namespace,
			)
			podOutput, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
			podNames := utils.GetNonEmptyLines(podOutput)
			Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
			controllerPodName = podNames[0]
			Expect(controllerPodName).To(ContainSubstring("controller-manager"))
		})

		// This is the smoke assertion for the label contract: watch, node
		// matching, per-resource failure isolation, labelling, and cleanup,
		// exercised without any registry infrastructure by pointing source at
		// an address nothing listens on.
		It("should label the node pending for an ImageCache with an unreachable source, then clear it on deletion", func() {
			By("getting the name of the (single) kind node")
			cmd := exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
			nodeName, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeName).NotTo(BeEmpty())

			By("creating an ImageCache selecting this node with an unreachable source")
			imageCacheYAML := fmt.Sprintf(`apiVersion: image-cache.scality.com/v1alpha1
kind: ImageCache
metadata:
  name: %s
spec:
  nodeSelector:
    kubernetes.io/os: linux
  source: 127.0.0.1:1/nope:1
`, imageCacheName)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(imageCacheYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the ImageCache")

			By("waiting for the node to be labelled pending")
			verifyPending := func(g Gomega) {
				labels, err := nodeLabels(nodeName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(labels).To(HaveKeyWithValue(nodeLabelKey, "pending"))
			}
			Eventually(verifyPending, 2*time.Minute).Should(Succeed())

			By("deleting the ImageCache")
			cmd = exec.Command("kubectl", "delete", "imagecache", imageCacheName)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the ImageCache")

			By("waiting for the label to be removed from the node")
			verifyLabelGone := func(g Gomega) {
				labels, err := nodeLabels(nodeName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(labels).NotTo(HaveKey(nodeLabelKey))
			}
			Eventually(verifyLabelGone, 2*time.Minute).Should(Succeed())
		})
	})
})

// nodeLabels returns the labels currently set on the named node.
func nodeLabels(name string) (map[string]string, error) {
	cmd := exec.Command("kubectl", "get", "node", name, "-o", "jsonpath={.metadata.labels}")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, err
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(output), &labels); err != nil {
		return nil, fmt.Errorf("parsing labels of node %s: %w", name, err)
	}
	return labels, nil
}
