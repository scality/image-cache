// Package controller reconciles the local node: cache directories and
// sync-status node labels follow the ImageCache resources selecting it.
// See DESIGN.md at the module root for the model.
package controller

import (
	"context"
	"maps"
	"path/filepath"
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
	"github.com/scality/image-cache/agent/internal/fill"
	"github.com/scality/image-cache/agent/internal/puller"
)

// defaultCachePath is always scanned for garbage, see cache.DefaultPath.
const defaultCachePath = cache.DefaultPath

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
		scanPaths[cachePathOf(ic)] = true
		if matches(ic.Spec.NodeSelector, node.Labels) {
			desired = append(desired, ic)
		}
	}
	scanPaths = r.rememberPaths(scanPaths)

	// First label pass: expose pending state before the (slow) pulls, so
	// orchestration gating on the labels sees work in progress.
	want := map[string]string{}
	keep := map[string]map[string]bool{}
	// Complete directories another writer seeded, pending until the second
	// pass has checked they hold the image their resource names.
	seeded := map[string]cache.Record{}
	var errs []error
	for _, ic := range desired {
		path := cachePathOf(ic)
		if keep[path] == nil {
			keep[path] = map[string]bool{}
		}
		keep[path][ic.Name] = true
		want[ic.Name] = StatusPending
		state, err := r.Store.State(path, ic.Name)
		if err != nil {
			errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
			continue
		}
		if state != cache.Complete {
			continue
		}
		rec, err := r.Store.ReadRecord(path, ic.Name)
		switch {
		case err != nil:
			errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
		case rec.Foreign():
			seeded[ic.Name] = rec
		default:
			want[ic.Name] = StatusSynced
		}
	}
	if err := r.patchLabels(ctx, &node, want); err != nil {
		errs = append(errs, err)
	}

	for _, ic := range desired {
		if want[ic.Name] == StatusSynced {
			continue
		}
		if rec, ok := seeded[ic.Name]; ok {
			adopted, err := r.adopt(ctx, ic, rec)
			if err != nil {
				r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "AdoptFailed", "Adopt",
					"checking the directory seeded for %s on node %s: %v", ic.Spec.Source, r.NodeName, err)
				errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
				continue
			}
			if adopted {
				want[ic.Name] = StatusSynced
				continue
			}
			// It holds another image: replaced like any directory that does
			// not hold what its resource asks for.
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

// cachePathOf returns the resource's cache path in canonical form. The path
// is used as a map key and compared against the paths other resources
// declare, so spellings of the same directory have to collapse into one:
// `/var/lib/image-cache/` keyed apart from `/var/lib/image-cache` would give
// the garbage collector a scan set with no matching keep set, and it would
// delete what the same pass had just extracted.
func cachePathOf(ic *imagecachev1alpha1.ImageCache) string {
	return filepath.Clean(ic.Spec.CachePath)
}

// adopt takes over a directory another writer seeded when it holds the image
// the resource names, and reports whether it did. It compares the layers'
// diff IDs, see "Adopting a seeded directory" in agent/DESIGN.md. A different
// image is not an error: the caller replaces the content. A source that
// cannot be resolved is an error, and the directory is left as it is.
func (r *NodeReconciler) adopt(ctx context.Context, ic *imagecachev1alpha1.ImageCache, rec cache.Record) (bool, error) {
	log := logf.FromContext(ctx).WithValues("resource", ic.Name, "seededFrom", rec.Source)
	id, err := r.Puller.Resolve(ctx, ic.Spec.Source)
	if err != nil {
		return false, errors.Wrap(ErrSync, errors.CausedBy(err),
			errors.WithDetail("resolving the image a seeded directory is checked against"))
	}
	if len(rec.Layers) == 0 || !slices.Equal(rec.Layers, id.Layers) {
		log.Info("Seeded cache directory holds another image, replacing it",
			"recorded", rec.Layers, "wanted", id.Layers)
		return false, nil
	}
	if err := r.Store.Adopt(cachePathOf(ic), ic.Name); err != nil {
		return false, errors.Wrap(ErrSync, errors.CausedBy(err))
	}
	r.Recorder.Eventf(ic, nil, corev1.EventTypeNormal, "Adopted", "Adopt",
		"adopted the cache directory of %s, seeded by %s, on node %s", ic.Name, rec.Owner, r.NodeName)
	log.Info("Adopted a seeded cache directory", "owner", rec.Owner)
	return true, nil
}

// sync pulls the resource's image and extracts it into its cache directory.
// It refuses to run when the cache path itself is missing: that means the
// host mount does not cover it, and extracting would write into the
// container filesystem.
func (r *NodeReconciler) sync(ctx context.Context, ic *imagecachev1alpha1.ImageCache) error {
	path := cachePathOf(ic)
	if err := fill.CheckCachePath(path); err != nil {
		r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "CachePathUnavailable", "Sync",
			"cache path %s is not usable on node %s (is it mounted?)", path, r.NodeName)
		return errors.Wrap(ErrSync, errors.CausedBy(err))
	}
	return fill.Fill(ctx, r.Store, r.Puller, path, ic.Name, ic.Spec.Source, cache.OwnerAgent, func(cerr error) {
		logf.FromContext(ctx).Error(cerr, "closing image stream", "resource", ic.Name)
	})
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

// patchLabels brings the node's sync-status labels to want, updating node in
// place to match. Nothing is sent when they already agree, which is what makes
// the second call of a reconcile free once the first one converged.
//
// The write is a merge patch of those labels alone, never a full update: this
// agent is not the only writer of a Node object, and everything it did not
// touch must survive its writes.
func (r *NodeReconciler) patchLabels(ctx context.Context, node *corev1.Node, want map[string]string) error {
	labels, changed := applyStatusLabels(node.Labels, want)
	if !changed {
		return nil
	}

	// The patch is computed at Patch time as the difference between base and
	// node, so base has to be a copy of the node as read: taking it after the
	// assignment below, or aliasing node, would yield an empty patch.
	base := node.DeepCopy()
	node.Labels = labels
	if err := r.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		return errors.Wrap(ErrNode, errors.CausedBy(err),
			errors.WithDetail("patching the sync-status labels"),
			errors.WithProperty("node", r.NodeName))
	}
	return nil
}

// SetupWithManager wires every trigger to the single reconcile key, which is
// the node's name: nothing reads it back, but it is what the manager logs as
// the object being reconciled.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toNode := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: r.NodeName}}}
		})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("node").
		Watches(&imagecachev1alpha1.ImageCache{}, toNode)

	// Guarantee one pass at startup even when no ImageCache exists, so
	// stale labels and directories left from a previous life of the agent
	// are cleaned up without waiting for a resource event.
	b = b.WatchesRawSource(source.Func(
		func(_ context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Name: r.NodeName}})
			return nil
		}))

	if r.FS != nil {
		b = b.WatchesRawSource(source.Channel(r.FS.Events, toNode))
	}
	return b.Complete(r)
}
