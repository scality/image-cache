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
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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
var nodeLabelKey = labelKey(imageCacheName)

// agentSelector selects the agent pods, one per node.
const agentSelector = "control-plane=controller-manager"

// syncedName is the ImageCache that pulls a real image from the in-cluster
// registry, so that the whole sync path runs: pull, extraction, sentinel,
// synced label, then garbage collection once it is deleted.
const syncedName = "imagecache-e2e-synced"

// tlsName is the ImageCache that pulls from the registry signed by a
// throwaway CA, to exercise --ca-file through the shipped patch.
const tlsName = "imagecache-e2e-tls"

// cachePath is the agent's default --cache-path, mounted by the manifests.
const cachePath = "/var/lib/image-cache"

// registryNamespace holds the two registries the agent pulls from, one plain
// HTTP and one TLS. Sources name them by their Service, a host name: without
// --plain-http the agent only tries HTTPS, as it would for a real registry.
const registryNamespace = "image-cache-e2e-registry"

// registryImage is pinned by digest, as it is pulled from Docker Hub on every
// run.
const registryImage = "docker.io/library/registry:3.1.2@sha256:" +
	"ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8"

// carrierFile is the archive the test image carries, and carrierBody its
// content. The agent extracts it under its base name.
const (
	carrierFile = "e2e.tar"
	carrierBody = "e2e"
)

// preexisting is what a node holds before the agent runs, written as root in
// mode 0700: a directory imagecachectl seeded, which stays, then what goes:
// the temporary of an interrupted extraction, a directory with a damaged
// sentinel, and two with no sentinel. The last one sorts after the seeded
// directory, so once it is gone the pass has walked past the seeded one.
var preexisting = []string{
	"/var/lib/image-cache/seeded-e2e",
	"/var/lib/image-cache/.seeded-e2e.tmp-1",
	"/var/lib/image-cache/damaged-e2e",
	"/var/lib/image-cache/nosentinel-e2e",
	"/var/lib/image-cache/zz-nosentinel-e2e",
}

var seedScript = "set -e\n" +
	"mkdir -p " + strings.Join(preexisting, " ") + "\n" +
	"chmod 0700 " + strings.Join(preexisting, " ") + "\n" +
	`echo '{"digest":"sha256:seeded","files":[],"owner":"imagecachectl"}' > ` +
	preexisting[0] + "/.image-cache-agent.json\n" +
	"echo half > " + preexisting[1] + "/half.tar\n" +
	"echo '{not json' > " + preexisting[2] + "/.image-cache-agent.json\n"

var _ = Describe("Manager", Ordered, func() {
	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		// The agent mounts a hostPath volume for the cache directory, which
		// is already disallowed starting at the "baseline" level, so this
		// DaemonSet needs "privileged".
		By("labeling the namespace to enforce the privileged security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=privileged")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with privileged policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("seeding the cache directory as root, as imagecachectl would")
		_, err = utils.OnKindNode(seedScript)
		Expect(err).NotTo(HaveOccurred(), "Failed to seed the cache directory")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("removing the preexisting directories")
		_, _ = utils.OnKindNode("rm -rf " + strings.Join(preexisting, " "))

		By("deleting the e2e ImageCaches, in case an earlier step left them behind")
		cmd := exec.Command("kubectl", "delete", "imagecache", imageCacheName, syncedName, tlsName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing the registry")
		cmd = exec.Command("kubectl", "delete", "ns", registryNamespace, "--ignore-not-found")
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
			By("Fetching the logs of every agent pod")
			cmd := exec.Command("kubectl", "logs", "-l", agentSelector, "-n", namespace, "--prefix", "--tail=-1")
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
			cmd = exec.Command("kubectl", "describe", "pods", "-l", agentSelector, "-n", namespace)
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

			By("checking that one agent runs on each of the two nodes")
			cmd := exec.Command("kubectl", "get", "pods", "-l", agentSelector, "-n", namespace,
				"-o", "jsonpath={range .items[*]}{.spec.nodeName}{\"\\n\"}{end}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to list the agent pods")
			Expect(utils.GetNonEmptyLines(out)).To(ConsistOf(utils.ControlPlaneNode(), utils.WorkerNode()))
		})

		It("should collect what no resource keeps, and keep what imagecachectl seeded", func() {
			// They are root's, in mode 0700, and some hold a file: removing
			// them takes DAC_OVERRIDE.
			for _, gone := range preexisting[1:] {
				Eventually(func() (bool, error) {
					return pathExists(utils.ControlPlaneNode(), gone)
				}).Should(BeFalse(), gone)
			}

			out, err := utils.OnKindNode("stat -c '%u %n' " + preexisting[0] +
				" && grep -c '\"owner\":\"imagecachectl\"' " + preexisting[0] + "/.image-cache-agent.json")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("0 " + preexisting[0] + "\n1\n"))
		})

		// This is the smoke assertion for the label contract: watch, node
		// matching, per-resource failure isolation, labelling, and cleanup,
		// exercised without any registry infrastructure by pointing source at
		// an address nothing listens on.
		It("should label the node pending for an ImageCache with an unreachable source, then clear it on deletion", func() {
			// The ImageCache selects both nodes; one is enough to check the
			// contract, node selection has a spec of its own.
			nodeName := utils.ControlPlaneNode()

			By("creating an ImageCache selecting every node with an unreachable source")
			imageCacheYAML := fmt.Sprintf(`apiVersion: image-cache.scality.com/v1alpha1
kind: ImageCache
metadata:
  name: %s
spec:
  nodeSelector:
    kubernetes.io/os: linux
  source: 127.0.0.1:1/nope:1
`, imageCacheName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(imageCacheYAML)
			_, err := utils.Run(cmd)
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

		It("should pull over plain HTTP onto the selected node only, then collect it on deletion", func() {
			deployRegistry("registry", "")
			digest := pushCarrier("registry", nil)

			By("restarting the agent with --plain-http")
			patchAgent("--type=json",
				`-p=[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--plain-http"}]`)

			By("creating an ImageCache that selects the worker only")
			worker, controlPlane := utils.WorkerNode(), utils.ControlPlaneNode()
			applyImageCache(syncedName, worker, "registry")
			syncedKey := labelKey(syncedName)
			dir := cachePath + "/" + syncedName

			By("waiting for the worker to be labelled synced")
			Eventually(func(g Gomega) {
				labels, err := nodeLabels(worker)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(labels).To(HaveKeyWithValue(syncedKey, "synced"))
			}).Should(Succeed())
			expectExtracted(worker, dir, digest)

			By("checking that the control plane, not selected, got neither label nor directory")
			labels, err := nodeLabels(controlPlane)
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).NotTo(HaveKey(syncedKey))
			Expect(pathExists(controlPlane, dir)).To(BeFalse())

			By("deleting the ImageCache")
			cmd := exec.Command("kubectl", "delete", "imagecache", syncedName)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the ImageCache")

			By("waiting for the directory and the label to go")
			Eventually(func() (bool, error) {
				return pathExists(worker, dir)
			}).Should(BeFalse())
			Eventually(func(g Gomega) {
				labels, err := nodeLabels(worker)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(labels).NotTo(HaveKey(syncedKey))
			}).Should(Succeed())
		})

		It("should refuse a registry signed by an unknown CA, then pull once the registry CA patch is applied", func() {
			ca, cert, key := newRegistryCerts("registry-tls." + registryNamespace + ".svc.cluster.local")
			deployRegistry("registry-tls", tlsSecret("registry-tls", cert, key))
			digest := pushCarrier("registry-tls", ca)

			By("creating an ImageCache for the TLS registry, the agent trusting the system CAs only")
			controlPlane := utils.ControlPlaneNode()
			applyImageCache(tlsName, controlPlane, "registry-tls")

			By("waiting for the agent to report the unknown authority")
			Eventually(func() (string, error) {
				cmd := exec.Command("kubectl", "logs", "-l", agentSelector, "-n", namespace, "--tail=-1")
				return utils.Run(cmd)
			}).Should(ContainSubstring("certificate signed by unknown authority"))
			labels, err := nodeLabels(controlPlane)
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).To(HaveKeyWithValue(labelKey(tlsName), "pending"))

			By("creating the registry-ca ConfigMap and applying the shipped CA patch")
			cmd := exec.Command("kubectl", "create", "configmap", "registry-ca", "-n", namespace,
				"--from-literal=ca.crt="+string(ca))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the registry-ca ConfigMap")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "configmap", "registry-ca", "-n", namespace))
			})
			patchAgent("--type=json", "--patch-file=config/default/manager_registry_ca_patch.yaml")

			By("waiting for the control plane to be labelled synced")
			Eventually(func(g Gomega) {
				labels, err := nodeLabels(controlPlane)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(labels).To(HaveKeyWithValue(labelKey(tlsName), "synced"))
			}).Should(Succeed())
			expectExtracted(controlPlane, cachePath+"/"+tlsName, digest)

			By("deleting the ImageCache, so no agent-owned state outlives the run")
			cmd = exec.Command("kubectl", "delete", "imagecache", tlsName)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the ImageCache")
			Eventually(func() (bool, error) {
				return pathExists(controlPlane, cachePath+"/"+tlsName)
			}).Should(BeFalse())
			Eventually(func(g Gomega) {
				labels, err := nodeLabels(controlPlane)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(labels).NotTo(HaveKey(labelKey(tlsName)))
			}).Should(Succeed())
		})
	})
})

// patchAgent patches the agent DaemonSet, waits for the new pods, and rolls
// the change back when the spec ends: --plain-http and --ca-file exclude each
// other, so each spec starts from the shipped manifests.
func patchAgent(args ...string) {
	cmd := exec.Command("kubectl", append([]string{"patch", "daemonset", daemonSetName, "-n", namespace}, args...)...)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to patch the agent")
	// Registered before the wait, so a rollout that times out is undone too.
	DeferCleanup(func() {
		// After the last spec, AfterAll has already removed the namespace
		// when this runs, and there is nothing left to roll back.
		if _, err := utils.Run(exec.Command("kubectl", "get", "ns", namespace)); err != nil {
			return
		}
		cmd := exec.Command("kubectl", "rollout", "undo", "daemonset/"+daemonSetName, "-n", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to roll the agent back")
		waitAgentRollout()
	})
	waitAgentRollout()
}

func waitAgentRollout() {
	cmd := exec.Command("kubectl", "rollout", "status", "daemonset/"+daemonSetName, "-n", namespace, "--timeout=2m")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "The agent did not roll out")
}

// applyImageCache creates an ImageCache that selects one node and pulls the
// carrier image from the named in-cluster registry.
func applyImageCache(name, node, registry string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: image-cache.scality.com/v1alpha1
kind: ImageCache
metadata:
  name: %s
spec:
  nodeSelector:
    kubernetes.io/hostname: %s
  source: %s.%s.svc.cluster.local:5000/e2e/carrier:1
`, name, node, registry, registryNamespace))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the ImageCache")
}

// expectExtracted checks the archive and the sentinel the agent wrote on node.
func expectExtracted(node, dir string, digest v1.Hash) {
	By("checking the extracted archive and its sentinel on " + node)
	out, err := utils.OnNode(node, "cat "+dir+"/"+carrierFile)
	Expect(err).NotTo(HaveOccurred())
	Expect(out).To(Equal(carrierBody))
	out, err = utils.OnNode(node, "cat "+dir+"/.image-cache-agent.json")
	Expect(err).NotTo(HaveOccurred())
	var sentinel struct {
		Digest string   `json:"digest"`
		Owner  string   `json:"owner"`
		Files  []string `json:"files"`
	}
	Expect(json.Unmarshal([]byte(out), &sentinel)).To(Succeed())
	Expect(sentinel.Owner).To(Equal("image-cache-agent"))
	Expect(sentinel.Digest).To(Equal(digest.String()))
	Expect(sentinel.Files).To(ConsistOf(carrierFile))
}

// deployRegistry deploys a single-replica registry and its Service, both
// called name, and waits for it. Given a TLS Secret manifest from tlsSecret,
// it applies it and the registry serves HTTPS with that certificate.
func deployRegistry(name, secret string) {
	By("deploying the " + name + " registry in the cluster")
	tlsEnv, tlsMount, tlsVolume, scheme := "", "", "", "HTTP"
	if secret != "" {
		tlsEnv = `
          env:
            - name: REGISTRY_HTTP_TLS_CERTIFICATE
              value: /certs/tls.crt
            - name: REGISTRY_HTTP_TLS_KEY
              value: /certs/tls.key`
		tlsMount = `
          volumeMounts:
            - name: certs
              mountPath: /certs`
		tlsVolume = `
      volumes:
        - name: certs
          secret:
            secretName: ` + name
		scheme = "HTTPS"
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---%[3]s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[2]s
  template:
    metadata:
      labels:
        app: %[2]s
    spec:
      containers:
        - name: registry
          image: %[4]s
          ports:
            - containerPort: 5000%[5]s%[6]s
          readinessProbe:
            httpGet:
              path: /v2/
              port: 5000
              scheme: %[7]s%[8]s
---
apiVersion: v1
kind: Service
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  selector:
    app: %[2]s
  ports:
    - port: 5000
`, registryNamespace, name, secret, registryImage, tlsEnv, tlsMount, scheme, tlsVolume)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the registry")
	cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", registryNamespace, "--timeout=2m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "The registry did not become ready")
}

// tlsSecret returns the manifest of the Secret deployRegistry mounts for the
// registry called name, followed by the document separator it expects.
func tlsSecret(name string, cert, key []byte) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
---`, name, registryNamespace, base64.StdEncoding.EncodeToString(cert), base64.StdEncoding.EncodeToString(key))
}

// pushCarrier pushes the carrier image to the named registry through a
// port-forward and returns its digest. With ca, the push trusts that CA,
// which also signed the certificate for localhost.
func pushCarrier(registry string, ca []byte) v1.Hash {
	By("pushing the carrier image to " + registry + " through a port-forward")
	local := portForward(registryNamespace, "svc/"+registry, 5000)
	img, err := crane.Image(map[string][]byte{"images/" + carrierFile: []byte(carrierBody)})
	Expect(err).NotTo(HaveOccurred())
	opts := []crane.Option{crane.Insecure}
	if ca != nil {
		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(ca)).To(BeTrue())
		tr := remote.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		opts = []crane.Option{crane.WithTransport(tr)}
	}
	Expect(crane.Push(img, local+"/e2e/carrier:1", opts...)).To(Succeed())
	digest, err := img.Digest()
	Expect(err).NotTo(HaveOccurred())
	return digest
}

// newRegistryCerts returns a throwaway CA, and a certificate it signed for
// host and for localhost, the port-forward the test pushes through. All PEM.
func newRegistryCerts(host string) (ca, cert, key []byte) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "image-cache e2e CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	Expect(err).NotTo(HaveOccurred())

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// forwardingFrom matches the line kubectl prints once a port-forward listens.
var forwardingFrom = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// portForward forwards a random local port to the target's port and returns
// "localhost:<port>". The forward stops when the spec ends.
func portForward(ns, target string, port int) string {
	cmd := exec.Command("kubectl", "port-forward", "-n", ns, target, fmt.Sprintf(":%d", port))
	stdout, err := cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	Expect(cmd.Start()).To(Succeed())

	// One goroutine reads stdout to the end: it reports the port, then keeps
	// draining so kubectl never blocks on a full pipe. Wait must only run
	// once it is done, as os/exec requires.
	found := make(chan string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			if m := forwardingFrom.FindStringSubmatch(lines.Text()); m != nil {
				found <- m[1]
			}
		}
	}()
	DeferCleanup(func() {
		_ = cmd.Process.Kill()
		<-drained
		_ = cmd.Wait()
	})

	select {
	case p := <-found:
		return "localhost:" + p
	case <-drained:
		Fail("kubectl port-forward exited before listening: " + stderr.String())
	case <-time.After(30 * time.Second):
		Fail("kubectl port-forward did not listen within 30s: " + stderr.String())
	}
	return ""
}

// pathExists reports whether path exists on the named kind node.
func pathExists(node, path string) (bool, error) {
	out, err := utils.OnNode(node, "test -e "+path+" && echo present || echo gone")
	return out == "present\n", err
}

// labelKey returns the sync-status label the agent sets for an ImageCache.
// See api/v1alpha1.ImageCache's doc comment for the contract.
func labelKey(name string) string {
	return "image-cache.scality.com/" + name
}

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
