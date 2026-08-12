// Package controller reconciles the local node: cache directories and
// sync-status node labels follow the ImageCache resources selecting it.
// See DESIGN.md at the module root for the model.
package controller

import (
	"context"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/scality/go-errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	imagecachev1alpha1 "github.com/scality/image-cache/agent/api/v1alpha1"
	"github.com/scality/image-cache/agent/internal/cache"
	"github.com/scality/image-cache/agent/internal/puller"
)

// defaultCachePath mirrors the CRD default; it is always scanned for garbage
// even when no resource references it anymore.
const defaultCachePath = "/var/lib/image-cache"

// NodeReconciler converges the local node: cache directories and sync-status
// node labels follow the ImageCache resources selecting this node.
type NodeReconciler struct {
	client.Client
	Recorder events.EventRecorder
	NodeName string
	Store    cache.Store
	Puller   puller.Puller
	FS       *FSWatcher
	Resync   time.Duration

	// mu guards knownPaths. The controller runs a single worker, so this is
	// belt-and-braces rather than a real contention risk.
	mu sync.Mutex
	// knownPaths accumulates every cachePath this process has ever scanned.
	// Once no ImageCache references a path anymore, the CR-derived scan set
	// would drop it and orphan its directories forever; remembering paths
	// keeps them GC'd for the rest of the process's lifetime, matching
	// DESIGN.md's promise that a deletion is repaired by the next pass.
	knownPaths map[string]bool
}

// +kubebuilder:rbac:groups=image-cache.scality.com,resources=imagecaches,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile runs one full convergence pass; see DESIGN.md for the model.
func (r *NodeReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: r.NodeName}, &node); err != nil {
		return ctrl.Result{}, errors.Wrap(ErrNode, errors.CausedBy(err),
			errors.WithDetail("getting the node"), errors.WithProperty("node", r.NodeName))
	}
	var list imagecachev1alpha1.ImageCacheList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, errors.Wrap(ErrResources, errors.CausedBy(err))
	}

	// Desired resources, plus every cache path any resource mentions: paths
	// are scanned for garbage even once nothing desires them anymore.
	var desired []*imagecachev1alpha1.ImageCache
	scanPaths := map[string]bool{defaultCachePath: true}
	for i := range list.Items {
		ic := &list.Items[i]
		scanPaths[ic.Spec.CachePath] = true
		if matches(ic.Spec.NodeSelector, node.Labels) {
			desired = append(desired, ic)
		}
	}
	scanPaths = r.rememberPaths(scanPaths)

	// First label pass: expose pending state before the (slow) pulls, so
	// orchestration gating on the labels sees work in progress.
	want := map[string]string{}
	keep := map[string]map[string]bool{}
	var errs []error
	for _, ic := range desired {
		if keep[ic.Spec.CachePath] == nil {
			keep[ic.Spec.CachePath] = map[string]bool{}
		}
		keep[ic.Spec.CachePath][ic.Name] = true
		state, err := r.Store.State(ic.Spec.CachePath, ic.Name)
		switch {
		case err != nil:
			errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
			want[ic.Name] = StatusPending
		case state == cache.Complete:
			want[ic.Name] = StatusSynced
		default:
			want[ic.Name] = StatusPending
		}
	}
	if err := r.patchLabels(ctx, &node, want); err != nil {
		errs = append(errs, err)
	}

	for _, ic := range desired {
		if want[ic.Name] == StatusSynced {
			continue
		}
		if err := r.sync(ctx, ic); err != nil {
			r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "SyncFailed", "Sync",
				"syncing %s on node %s: %v", ic.Spec.Source, r.NodeName, err)
			errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
			continue
		}
		want[ic.Name] = StatusSynced
	}

	for path := range scanPaths {
		if _, err := r.Store.GC(path, keep[path]); err != nil {
			errs = append(errs, errors.Wrap(err, errors.WithProperty("cachePath", path)))
		}
	}
	if err := r.patchLabels(ctx, &node, want); err != nil {
		errs = append(errs, err)
	}
	if r.FS != nil {
		r.FS.SetPaths(slices.Collect(maps.Keys(scanPaths)))
	}

	if err := utilerrors.NewAggregate(errs); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("node converged", "resources", len(desired))
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

// sync pulls the resource's image and extracts it into its cache directory.
// It refuses to run when the cache path itself is missing: that means the
// host mount does not cover it, and extracting would write into the
// container filesystem.
func (r *NodeReconciler) sync(ctx context.Context, ic *imagecachev1alpha1.ImageCache) error {
	if _, err := os.Stat(ic.Spec.CachePath); err != nil {
		r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "CachePathUnavailable", "Sync",
			"cache path %s does not exist on node %s (is it mounted?)", ic.Spec.CachePath, r.NodeName)
		return errors.Wrap(ErrSync, errors.CausedBy(err),
			errors.WithDetail("the cache path is missing: is it mounted?"),
			errors.WithProperty("cachePath", ic.Spec.CachePath))
	}
	content, digest, err := r.Puller.Pull(ctx, ic.Spec.Source)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := content.Close(); cerr != nil {
			logf.FromContext(ctx).Error(cerr, "closing image stream", "resource", ic.Name)
		}
	}()
	return r.Store.Extract(ctx, ic.Spec.CachePath, ic.Name, digest, content)
}

// rememberPaths merges paths into the reconciler's lifetime set of known
// cache paths and returns the union: every path ever seen keeps getting
// GC'd even after the last resource referencing it is deleted.
func (r *NodeReconciler) rememberPaths(paths map[string]bool) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.knownPaths == nil {
		r.knownPaths = map[string]bool{}
	}
	maps.Copy(r.knownPaths, paths)
	return maps.Clone(r.knownPaths)
}

func (r *NodeReconciler) patchLabels(ctx context.Context, node *corev1.Node, want map[string]string) error {
	labels, changed := applyStatusLabels(node.Labels, want)
	if !changed {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Labels = labels
	if err := r.Patch(ctx, node, patch); err != nil {
		return errors.Wrap(ErrNode, errors.CausedBy(err),
			errors.WithDetail("patching the sync-status labels"),
			errors.WithProperty("node", r.NodeName))
	}
	return nil
}

// SetupWithManager wires every trigger to the single node key.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toNode := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nodeKey}}}
		})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("node").
		Watches(&imagecachev1alpha1.ImageCache{}, toNode)

	// Guarantee one pass at startup even when no ImageCache exists, so
	// stale labels and directories left from a previous life of the agent
	// are cleaned up without waiting for a resource event.
	b = b.WatchesRawSource(source.Func(
		func(_ context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeKey}})
			return nil
		}))

	if r.FS != nil {
		b = b.WatchesRawSource(source.Channel(r.FS.Events, toNode))
	}
	return b.Complete(r)
}
