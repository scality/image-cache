package controller

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	imagecachev1alpha1 "github.com/scality/image-cache/agent/api/v1alpha1"
	"github.com/scality/image-cache/agent/internal/cache"
)

// Resource names, labels, and paths used by the tests added below.
const (
	// worker133ResourceName is an older coexisting version of
	// workerResourceName, used to prove that independent versions of the
	// same boot cache do not interfere with each other (the ticket's core
	// upgrade scenario).
	worker133ResourceName = "worker-133-0-0"
	// missingResourceName selects a cachePath that never exists on this
	// node, exercising the "unmounted cache path" refusal.
	missingResourceName = "missing-134-0-0"
	// nonexistentCachePath is never present on the test filesystem.
	nonexistentCachePath = "/nonexistent/image-cache-test"
	// nonexistentParentPath is nonexistentCachePath's parent: asserting it
	// was never created proves sync() never attempted to write under it.
	nonexistentParentPath = "/nonexistent"
	// cachePathUnavailableReason mirrors the Event reason NodeReconciler.sync
	// records when a resource's cache path is missing.
	cachePathUnavailableReason = "CachePathUnavailable"
	// etcdTarName is the flattened file name every fakePuller image
	// produces.
	etcdTarName = "etcd.tar"

	// fsNodeName, fsRepairLabelKey/Value, and fsResourceName back the
	// dedicated "filesystem repair" suite below, which runs its own
	// manager/node/reconciler outside the Ordered block.
	fsNodeName       = "fs-node"
	fsRepairLabelKey = "fsrepair"
	fsRepairLabelYes = "yes"
	fsResourceName   = "fsrepair-1-0-0"
)

// fakePuller is a mutable, race-safe puller.Puller: tests flip fail to
// exercise the sync failure and self-heal paths without touching a
// registry. On success it returns a forged image whose single layer file
// is images/etcd.tar, mirroring what mutate.Extract would flatten out of a
// real image.
type fakePuller struct{ fail atomic.Bool }

func (f *fakePuller) Pull(context.Context, string) (io.ReadCloser, string, error) {
	if f.fail.Load() {
		return nil, "", errors.New("registry unreachable")
	}
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	_ = tw.WriteHeader(&tar.Header{Name: "images/etcd.tar", Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("etcd"))
	_ = tw.Close()
	return io.NopCloser(buf), "sha256:fake", nil
}

var _ = Describe("NodeReconciler", Ordered, func() {
	ctx := context.Background()

	nodeKey := types.NamespacedName{Name: testNodeName}

	It("becomes synced once the reconciler pulls and extracts it", func() {
		By("creating a matching ImageCache")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: workerResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{osLabelKey: osLabelLinux},
				Source:       "registry.example.com/boot-cache-worker:134.0.0",
				CachePath:    cacheDir,
			},
		}
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		By("waiting for the synced label")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, workerResourceName)
		}).Should(Equal(StatusSynced))

		By("checking the extracted content")
		Eventually(func() ([]byte, error) {
			return os.ReadFile(filepath.Join(cacheDir, workerResourceName, etcdTarName))
		}).Should(Equal([]byte("etcd")))

		By("checking the sentinel file")
		Eventually(func() error {
			_, err := os.Stat(filepath.Join(cacheDir, workerResourceName, ".image-cache-agent.json"))
			return err
		}).Should(Succeed())
	})

	It("does not label the node for a resource that does not select it", func() {
		By("creating a non-matching ImageCache")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: "other-134-0-0"},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{zoneLabelKey: "mars"},
				Source:       "registry.example.com/boot-cache-other:134.0.0",
				CachePath:    cacheDir,
			},
		}
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		By("checking the label never appears")
		Consistently(func() (bool, error) {
			return hasNodeLabel(ctx, nodeKey, "other-134-0-0")
		}, "2s").Should(BeFalse())

		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
	})

	It("removes the label and the directory once the resource is deleted", func() {
		By("deleting the resource created in the first case")
		ic := &imagecachev1alpha1.ImageCache{ObjectMeta: metav1.ObjectMeta{Name: workerResourceName}}
		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())

		By("waiting for the label to disappear")
		Eventually(func() (bool, error) {
			return hasNodeLabel(ctx, nodeKey, workerResourceName)
		}).Should(BeFalse())

		By("waiting for the directory to disappear")
		Eventually(func() bool {
			_, err := os.Stat(filepath.Join(cacheDir, workerResourceName))
			return os.IsNotExist(err)
		}).Should(BeTrue())
	})

	It("keeps a resource pending while its image cannot be pulled, then self-heals", func() {
		testPuller.fail.Store(true)

		By("creating a matching ImageCache while the puller fails")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: "broken-134-0-0"},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{osLabelKey: osLabelLinux},
				Source:       "registry.example.com/boot-cache-broken:134.0.0",
				CachePath:    cacheDir,
			},
		}
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		By("waiting for the pending label")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, "broken-134-0-0")
		}).Should(Equal(StatusPending))

		By("checking it never becomes synced while the puller keeps failing")
		Consistently(func() (string, error) {
			return nodeLabel(ctx, nodeKey, "broken-134-0-0")
		}, "2s").Should(Equal(StatusPending))

		By("letting the puller succeed")
		testPuller.fail.Store(false)

		By("waiting for the self-heal to synced")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, "broken-134-0-0")
		}).Should(Equal(StatusSynced))

		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
	})

	It("keeps coexisting versions independent (upgrade scenario)", func() {
		By("creating two coexisting versions of the same boot cache")
		older := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: worker133ResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{osLabelKey: osLabelLinux},
				Source:       "registry.example.com/boot-cache-worker:133.0.0",
				CachePath:    cacheDir,
			},
		}
		Expect(k8sClient.Create(ctx, older)).To(Succeed())
		newer := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: workerResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{osLabelKey: osLabelLinux},
				Source:       "registry.example.com/boot-cache-worker:134.0.0",
				CachePath:    cacheDir,
			},
		}
		Expect(k8sClient.Create(ctx, newer)).To(Succeed())

		By("waiting for both versions to become synced")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, worker133ResourceName)
		}).Should(Equal(StatusSynced))
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, workerResourceName)
		}).Should(Equal(StatusSynced))

		By("checking both cache directories were populated")
		Eventually(func() ([]byte, error) {
			return os.ReadFile(filepath.Join(cacheDir, worker133ResourceName, etcdTarName))
		}).Should(Equal([]byte("etcd")))
		Eventually(func() ([]byte, error) {
			return os.ReadFile(filepath.Join(cacheDir, workerResourceName, etcdTarName))
		}).Should(Equal([]byte("etcd")))

		By("deleting the older version")
		Expect(k8sClient.Delete(ctx, older)).To(Succeed())

		By("waiting for the older version's label and directory to disappear")
		Eventually(func() (bool, error) {
			return hasNodeLabel(ctx, nodeKey, worker133ResourceName)
		}).Should(BeFalse())
		Eventually(func() bool {
			_, err := os.Stat(filepath.Join(cacheDir, worker133ResourceName))
			return os.IsNotExist(err)
		}).Should(BeTrue())

		By("checking the newer version is left untouched")
		Consistently(func() (string, error) {
			return nodeLabel(ctx, nodeKey, workerResourceName)
		}, "2s").Should(Equal(StatusSynced))
		Consistently(func() error {
			_, err := os.Stat(filepath.Join(cacheDir, workerResourceName, etcdTarName))
			return err
		}, "2s").Should(Succeed())

		By("cleaning up the newer version to leave a clean state")
		Expect(k8sClient.Delete(ctx, newer)).To(Succeed())
		Eventually(func() (bool, error) {
			return hasNodeLabel(ctx, nodeKey, workerResourceName)
		}).Should(BeFalse())
	})

	It("keeps a resource pending when its cache path does not exist", func() {
		By("creating an ImageCache whose cache path is not mounted on this node")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: missingResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{osLabelKey: osLabelLinux},
				Source:       "registry.example.com/boot-cache-missing:134.0.0",
				CachePath:    nonexistentCachePath,
			},
		}
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		By("waiting for the pending label")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, missingResourceName)
		}).Should(Equal(StatusPending))

		By("checking it never becomes synced while the cache path is missing")
		Consistently(func() (string, error) {
			return nodeLabel(ctx, nodeKey, missingResourceName)
		}, "2s").Should(Equal(StatusPending))

		By("checking no directory was ever created for the unmounted path")
		_, err := os.Stat(nonexistentParentPath)
		Expect(os.IsNotExist(err)).To(BeTrue())

		By("checking a CachePathUnavailable event was recorded for the resource")
		Eventually(func() (bool, error) {
			var events corev1.EventList
			if err := k8sClient.List(ctx, &events); err != nil {
				return false, err
			}
			for _, e := range events.Items {
				if e.InvolvedObject.Name == missingResourceName && e.Reason == cachePathUnavailableReason {
					return true, nil
				}
			}
			return false, nil
		}).Should(BeTrue())

		By("cleaning up")
		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
		Eventually(func() (bool, error) {
			return hasNodeLabel(ctx, nodeKey, missingResourceName)
		}).Should(BeFalse())
	})
})

// Separate top-level container: it runs its own manager against a node that
// no suite CR selects, so it does not share Ordered-block state above.
var _ = Describe("startup pass", func() {
	const staleNodeName = "stale-node"

	It("cleans up stale labels left by a previous life of the agent even when no ImageCache exists", func() {
		ctx := context.Background()
		staleNodeKey := types.NamespacedName{Name: staleNodeName}

		By("creating a node carrying a stale synced label from a resource that no longer exists")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: staleNodeName,
				// Deliberately does not match any suite CR selector (those
				// use kubernetes.io/os): this node must stay untouched by
				// every other test's ImageCache resources.
				Labels: map[string]string{zoneLabelKey: "stale", LabelPrefix + "gone-1-0-0": StatusSynced},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), node)).To(Succeed())
		})

		By("starting a second manager whose reconciler converges stale-node")
		// controller-runtime validates controller names against a
		// process-global set (pkg/controller/name.go), not a per-manager
		// one: the suite's manager already registered "node", so this
		// second manager must opt out via its own (manager-scoped)
		// SkipNameValidation. This does not touch the production
		// controller's name or SetupWithManager.
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{
				SkipNameValidation: &skipNameValidation,
			},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&NodeReconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorder("image-cache-agent-startup-test"),
			NodeName: staleNodeName,
			Store:    cache.Store{},
			Puller:   &fakePuller{},
			FS:       nil,
			Resync:   0,
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			mgrCancel()
			<-done
		})

		By("waiting for the startup pass to remove the stale label with zero matching resources")
		Eventually(func() (bool, error) {
			return hasNodeLabel(ctx, staleNodeKey, "gone-1-0-0")
		}, 10*time.Second).Should(BeFalse())
	})
})

// Separate top-level container: it runs its own manager, node, and cache
// directory to exercise a real (non-mocked) FSWatcher end to end, so it does
// not share Ordered-block state with "NodeReconciler" above.
var _ = Describe("filesystem repair", func() {
	It("repairs a tampered cache directory via the fsnotify trigger alone", func() {
		ctx := context.Background()
		fsNodeKey := types.NamespacedName{Name: fsNodeName}

		By("creating a node dedicated to this suite")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   fsNodeName,
				Labels: map[string]string{fsRepairLabelKey: fsRepairLabelYes},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), node)).To(Succeed())
		})

		By("creating this test's own cache directory")
		fsCacheDir, err := os.MkdirTemp("", "image-cache-fsrepair-test-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(os.RemoveAll(fsCacheDir)).To(Succeed())
		})

		By("starting a real FSWatcher and a second manager/reconciler using it")
		fw, err := NewFSWatcher(fsNodeName)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(fw.Close()).To(Succeed())
		})

		// See the "startup pass" Describe above for why SkipNameValidation
		// is required for this second, suite-local manager.
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{
				SkipNameValidation: &skipNameValidation,
			},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(mgr.Add(fw)).To(Succeed())

		Expect((&NodeReconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorder("image-cache-agent-fsrepair-test"),
			NodeName: fsNodeName,
			Store:    cache.Store{},
			Puller:   &fakePuller{},
			FS:       fw,
			Resync:   0,
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			mgrCancel()
			<-done
		})

		By("creating a matching ImageCache")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: fsResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{fsRepairLabelKey: fsRepairLabelYes},
				Source:       "registry.example.com/boot-cache-fsrepair:1.0.0",
				CachePath:    fsCacheDir,
			},
		}
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), ic)).To(Succeed())
		})

		By("waiting for the initial sync")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, fsNodeKey, fsResourceName)
		}).Should(Equal(StatusSynced))
		tarPath := filepath.Join(fsCacheDir, fsResourceName, etcdTarName)
		Eventually(func() ([]byte, error) {
			return os.ReadFile(tarPath)
		}).Should(Equal([]byte("etcd")))

		// fsnotify's inotify backend is not recursive (verified against the
		// exact pinned version, github.com/fsnotify/fsnotify v1.10.1): a
		// watch on fsCacheDir reports changes to its direct children only.
		// SetPaths therefore watches each cachePath and its resource
		// directories, which is what makes the tamper below observable:
		// deleting a single tarball, the way an operator reclaiming disk
		// space would. No ImageCache event occurs anywhere in this flow,
		// only the fsnotify trigger does.
		By("deleting a single tarball from under the agent")
		Expect(os.Remove(tarPath)).To(Succeed())

		By("waiting for the fsnotify-triggered pass to repair it")
		Eventually(func() ([]byte, error) {
			return os.ReadFile(tarPath)
		}, 10*time.Second).Should(Equal([]byte("etcd")))
	})
})

// nodeLabel returns the value of the image-cache.scality.com/<name> label
// on the named node.
func nodeLabel(ctx context.Context, key types.NamespacedName, name string) (string, error) {
	var node corev1.Node
	if err := k8sClient.Get(ctx, key, &node); err != nil {
		return "", err
	}
	return node.Labels[LabelPrefix+name], nil
}

// hasNodeLabel reports whether the image-cache.scality.com/<name> label is
// currently set on the named node.
func hasNodeLabel(ctx context.Context, key types.NamespacedName, name string) (bool, error) {
	var node corev1.Node
	if err := k8sClient.Get(ctx, key, &node); err != nil {
		return false, err
	}
	_, ok := node.Labels[LabelPrefix+name]
	return ok, nil
}
