package controller

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	imagecachev1alpha1 "github.com/scality/image-cache/agent/api/v1alpha1"
	"github.com/scality/image-cache/agent/internal/cache"
	"github.com/scality/image-cache/agent/internal/cli"
	"github.com/scality/image-cache/agent/internal/puller"
)

// Resource names, labels, and paths used by the tests added below.
const (
	// worker133ResourceName is an older coexisting version of
	// workerResourceName, used to prove that independent versions of the
	// same boot cache do not interfere with each other (the ticket's core
	// upgrade scenario).
	worker133ResourceName = "worker-133-0-0"
	// missingNodeName, missingPathLabelKey/Yes, and missingResourceName back
	// the "missing cache path" suite below. Its reconciler is given a cache
	// path that never exists, exercising the "unmounted cache path" refusal.
	missingNodeName     = "missing-node"
	missingPathLabelKey = "missingpath"
	missingPathLabelYes = "yes"
	missingResourceName = "missing-134-0-0"
	// nonexistentDir is never present on the test filesystem.
	nonexistentDir = "/nonexistent/image-cache-test"
	// nonexistentParentDir is nonexistentDir's parent: asserting it was
	// never created proves sync() never attempted to write under it.
	nonexistentParentDir = "/nonexistent"
	// cachePathUnavailableReason mirrors the Event reason NodeReconciler.sync
	// records when the cache path is missing.
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

	// strayNodeName backs the "stray cache entries" suite below.
	strayNodeName = "stray-node"
)

// fakePuller is a mutable, race-safe puller.Puller: tests flip fail to
// exercise the sync failure and self-heal paths without touching a
// registry. On success it returns a forged stream whose single entry is
// images/etcd.tar, as a real puller passes on the entries of a one-layer
// image. It counts the pulls of each source, for the cases that must not
// pull at all.
type fakePuller struct {
	fail atomic.Bool

	mu    sync.Mutex
	pulls map[string]int
}

func (f *fakePuller) Pull(_ context.Context, ref string) (io.ReadCloser, puller.Image, error) {
	f.mu.Lock()
	if f.pulls == nil {
		f.pulls = map[string]int{}
	}
	f.pulls[ref]++
	f.mu.Unlock()
	if f.fail.Load() {
		return nil, puller.Image{}, errors.New("registry unreachable")
	}
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	_ = tw.WriteHeader(&tar.Header{Name: "images/etcd.tar", Mode: 0o644, Size: 4})
	_, _ = tw.Write([]byte("etcd"))
	_ = tw.Close()
	return io.NopCloser(buf), fakeImage, nil
}

// fakeImage is what every source resolves to in these tests.
var fakeImage = puller.Image{Digest: "sha256:fake", Layers: []string{"sha256:fakelayer"}}

// pullsOf reports how many times ref was pulled.
func (f *fakePuller) pullsOf(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[ref]
}

func (f *fakePuller) Resolve(context.Context, string) (puller.Image, error) {
	if f.fail.Load() {
		return puller.Image{}, errors.New("registry unreachable")
	}
	return fakeImage, nil
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
			},
		}
		Expect(k8sClient.Create(ctx, older)).To(Succeed())
		newer := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: workerResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{osLabelKey: osLabelLinux},
				Source:       "registry.example.com/boot-cache-worker:134.0.0",
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

	// At install the agent can land before the resources that name what the
	// command seeded. Its garbage collection runs on every pass, and has to
	// leave a directory another writer owns until a resource claims it. A
	// directory the agent wrote itself, with nothing claiming it, is the
	// witness that the collection did run meanwhile.
	It("leaves a directory the command seeded alone until a resource claims it", func() {
		seeded := filepath.Join(cacheDir, "seeded-134-0-0")
		orphan := filepath.Join(cacheDir, "orphan-134-0-0")
		for dir, owner := range map[string]string{seeded: cli.Owner, orphan: cache.OwnerAgent} {
			placeDirectory(dir, "etcd", `{"digest":"d","files":["`+etcdTarName+`"],"owner":"`+owner+`"}`)
		}

		By("triggering a pass over the cache path with a resource that selects nothing here")
		trigger := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: "trigger-134-0-0"},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{zoneLabelKey: "mars"},
				Source:       "registry.example.com/boot-cache-other:134.0.0",
			},
		}
		Expect(k8sClient.Create(ctx, trigger)).To(Succeed())

		By("waiting for the agent's own orphan to be collected")
		Eventually(func() bool {
			_, err := os.Stat(orphan)
			return os.IsNotExist(err)
		}).Should(BeTrue())

		By("checking the seeded directory is still whole")
		Consistently(func() ([]byte, error) {
			return os.ReadFile(filepath.Join(seeded, etcdTarName))
		}, "3s").Should(Equal([]byte("etcd")))

		Expect(k8sClient.Delete(ctx, trigger)).To(Succeed())
		Expect(os.RemoveAll(seeded)).To(Succeed())
	})

	// A resource that claims a directory the command seeded takes it over when
	// it holds the resource's image, which the layers' diff IDs decide
	// whatever the command read it from, and costs no pull.
	It("adopts a seeded directory that holds the resource's image, without pulling", func() {
		const name = "adopted-134-0-0"
		source := "registry.example.com/boot-cache-adopted:134.0.0"
		dir := seedDirectory(name, fakeImage.Layers[0])

		ic := matchingResource(name, source)
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, name)
		}).Should(Equal(StatusSynced))
		Expect(ownerOf(dir)).To(Equal(cache.OwnerAgent))
		Expect(os.ReadFile(filepath.Join(dir, etcdTarName))).To(Equal([]byte(seededContent)))
		Expect(testPuller.pullsOf(source)).To(BeZero())

		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
		Eventually(func() bool {
			_, err := os.Stat(dir)
			return os.IsNotExist(err)
		}).Should(BeTrue(), "an adopted directory is the agent's to collect")
	})

	It("replaces a seeded directory that holds another image", func() {
		const name = "reseeded-134-0-0"
		source := "registry.example.com/boot-cache-reseeded:134.0.0"
		dir := seedDirectory(name, "sha256:anotherlayer")

		ic := matchingResource(name, source)
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		Eventually(func() ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, etcdTarName))
		}).Should(Equal([]byte("etcd")))
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, name)
		}).Should(Equal(StatusSynced))
		Expect(ownerOf(dir)).To(Equal(cache.OwnerAgent))
		Expect(testPuller.pullsOf(source)).To(BeNumerically(">=", 1))

		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
	})

	It("leaves a seeded directory untouched while its source cannot be resolved", func() {
		const name = "unresolved-134-0-0"
		source := "registry.example.com/boot-cache-unresolved:134.0.0"
		dir := seedDirectory(name, fakeImage.Layers[0])
		testPuller.fail.Store(true)
		DeferCleanup(func() { testPuller.fail.Store(false) })

		ic := matchingResource(name, source)
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())

		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, name)
		}).Should(Equal(StatusPending))
		Consistently(func() ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, etcdTarName))
		}, "2s").Should(Equal([]byte(seededContent)))
		Expect(ownerOf(dir)).To(Equal(cli.Owner))
		Expect(testPuller.pullsOf(source)).To(BeZero())

		By("letting the registry answer again")
		testPuller.fail.Store(false)
		Eventually(func() (string, error) {
			return nodeLabel(ctx, nodeKey, name)
		}).Should(Equal(StatusSynced))
		Expect(ownerOf(dir)).To(Equal(cache.OwnerAgent))
		Expect(testPuller.pullsOf(source)).To(BeZero())

		Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
	})
})

// startOwnReconciler runs r under a second manager, next to the suite's one,
// until the spec ends. It fills in the client and the recorder, and runs r.FS
// when there is one.
func startOwnReconciler(r *NodeReconciler) {
	GinkgoHelper()
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

	if r.FS != nil {
		Expect(mgr.Add(r.FS)).To(Succeed())
	}
	r.Client = mgr.GetClient()
	r.Recorder = mgr.GetEventRecorder("image-cache-agent-" + r.NodeName)
	Expect(r.SetupWithManager(mgr)).To(Succeed())

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
}

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
		startOwnReconciler(&NodeReconciler{
			NodeName:  staleNodeName,
			CachePath: GinkgoT().TempDir(),
			Store:     cache.Store{},
			Puller:    &fakePuller{},
			Resync:    0,
		})

		By("waiting for the startup pass to remove the stale label with zero matching resources")
		Eventually(func() (bool, error) {
			return hasNodeLabel(ctx, staleNodeKey, "gone-1-0-0")
		}, 10*time.Second).Should(BeFalse())
	})
})

// An entry no resource keeps goes when it is not a seeded directory: no
// sentinel, a damaged one, a link.
var _ = Describe("stray cache entries", func() {
	It("removes them", func() {
		ctx := context.Background()
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: strayNodeName}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), node)).To(Succeed())
		})
		dir := GinkgoT().TempDir()
		stray := filepath.Join(dir, "toto")
		Expect(os.MkdirAll(stray, 0o755)).To(Succeed())

		r := &NodeReconciler{
			Client:    k8sClient,
			Recorder:  events.NewFakeRecorder(10),
			NodeName:  strayNodeName,
			CachePath: dir,
			Puller:    &fakePuller{},
			Resync:    time.Hour,
		}
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: strayNodeName}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(time.Hour))
		Expect(stray).NotTo(BeAnExistingFile())
	})

	It("fails the pass when it cannot tell whether a directory is seeded", func() {
		ctx := context.Background()
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: strayNodeName}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), node)).To(Succeed())
		})
		dir := GinkgoT().TempDir()
		// A sentinel that is a directory cannot be read, even by root.
		unreadable := filepath.Join(dir, "toto")
		Expect(os.MkdirAll(filepath.Join(unreadable, ".image-cache-agent.json"), 0o755)).To(Succeed())

		r := &NodeReconciler{
			Client:    k8sClient,
			Recorder:  events.NewFakeRecorder(10),
			NodeName:  strayNodeName,
			CachePath: dir,
			Puller:    &fakePuller{},
			Resync:    time.Hour,
		}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: strayNodeName}})
		Expect(err).To(MatchError(cache.ErrGC))
		Expect(unreadable).To(BeADirectory())
	})
})

// Separate top-level container: the suite's reconciler uses a cache path that
// exists, so this one runs its own manager, node and reconciler, whose cache
// path is missing, as when the host mount does not cover it.
var _ = Describe("missing cache path", func() {
	It("keeps a resource pending when the cache path does not exist", func() {
		ctx := context.Background()
		missingNodeKey := types.NamespacedName{Name: missingNodeName}

		By("creating a node dedicated to this suite")
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   missingNodeName,
				Labels: map[string]string{missingPathLabelKey: missingPathLabelYes},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), node)).To(Succeed())
		})

		By("starting a reconciler whose cache path is not mounted on its node")
		startOwnReconciler(&NodeReconciler{
			NodeName:  missingNodeName,
			CachePath: nonexistentDir,
			Store:     cache.Store{},
			Puller:    &fakePuller{},
			Resync:    0,
		})

		By("creating a matching ImageCache")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: missingResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{missingPathLabelKey: missingPathLabelYes},
				Source:       "registry.example.com/boot-cache-missing:134.0.0",
			},
		}
		Expect(k8sClient.Create(ctx, ic)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), ic)).To(Succeed())
		})

		By("waiting for the pending label")
		Eventually(func() (string, error) {
			return nodeLabel(ctx, missingNodeKey, missingResourceName)
		}).Should(Equal(StatusPending))

		By("checking it never becomes synced while the cache path is missing")
		Consistently(func() (string, error) {
			return nodeLabel(ctx, missingNodeKey, missingResourceName)
		}, "2s").Should(Equal(StatusPending))

		By("checking no directory was ever created for the unmounted path")
		_, err := os.Stat(nonexistentParentDir)
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
		startOwnReconciler(&NodeReconciler{
			NodeName:  fsNodeName,
			CachePath: fsCacheDir,
			Store:     cache.Store{},
			Puller:    &fakePuller{},
			FS:        fw,
			Resync:    0,
		})

		By("creating a matching ImageCache")
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: fsResourceName},
			Spec: imagecachev1alpha1.ImageCacheSpec{
				NodeSelector: map[string]string{fsRepairLabelKey: fsRepairLabelYes},
				Source:       "registry.example.com/boot-cache-fsrepair:1.0.0",
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
		// SetPaths therefore watches the cache path and its resource
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

// seededContent is what seedDirectory writes, distinct from what fakePuller
// extracts, so a case can tell a kept directory from a replaced one.
const seededContent = "seeded"

// seedDirectory writes, under the suite's cache directory, what the command
// leaves for the named resource: a complete directory it owns, holding the
// image whose only layer has the diff ID layer.
func seedDirectory(name, layer string) string {
	GinkgoHelper()
	dir := filepath.Join(cacheDir, name)
	placeDirectory(dir, seededContent, `{"digest":"sha256:seeded","files":["`+etcdTarName+`"],"owner":"`+cli.Owner+
		`","source":"/mnt/iso/boot-cache.tar","layers":["`+layer+`"]}`)
	return dir
}

// placeDirectory writes a complete resource directory at dir in one rename,
// as the command does: the agent removes a directory with no sentinel yet.
func placeDirectory(dir, content, sentinel string) {
	GinkgoHelper()
	tmp, err := os.MkdirTemp(filepath.Dir(cacheDir), "seed-")
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(tmp, etcdTarName), []byte(content), 0o644)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(tmp, ".image-cache-agent.json"), []byte(sentinel), 0o644)).To(Succeed())
	Expect(os.Rename(tmp, dir)).To(Succeed())
}

// matchingResource is a resource that selects the suite's node.
func matchingResource(name, source string) *imagecachev1alpha1.ImageCache {
	return &imagecachev1alpha1.ImageCache{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: imagecachev1alpha1.ImageCacheSpec{
			NodeSelector: map[string]string{osLabelKey: osLabelLinux},
			Source:       source,
		},
	}
}

// ownerOf reads who the sentinel of dir says wrote it.
func ownerOf(dir string) string {
	GinkgoHelper()
	rec, err := cache.Store{}.ReadRecord(filepath.Dir(dir), filepath.Base(dir))
	Expect(err).NotTo(HaveOccurred())
	return rec.Owner
}

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
