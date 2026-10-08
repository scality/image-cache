// Package controller reconciles the local node: cache directories and
// sync-status node labels follow the ImageCache resources selecting it.
// See DESIGN.md at the module root for the model.
package controller

import (
	"context"
	"slices"
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

// NodeReconciler converges the local node: cache directories and sync-status
// node labels follow the ImageCache resources selecting this node.
type NodeReconciler struct {
	client.Client
	Recorder events.EventRecorder
	NodeName string
	// CachePath is the host directory the agent fills, cleaned by filepath.Clean.
	CachePath string
	Store     cache.Store
	Puller    puller.Puller
	FS        *FSWatcher
	Resync    time.Duration
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

	var desired []*imagecachev1alpha1.ImageCache
	for i := range list.Items {
		if ic := &list.Items[i]; matches(ic.Spec.NodeSelector, node.Labels) {
			desired = append(desired, ic)
		}
	}

	// First label pass: expose pending state before the (slow) pulls, so
	// orchestration gating on the labels sees work in progress.
	want := map[string]string{}
	keep := map[string]bool{}
	// Complete directories another writer seeded. They stay pending until the
	// loop below has checked they hold the image their resource names.
	seeded := map[string]cache.Record{}
	// Complete directories whose sentinel could not be read: they may be
	// seeded, so this pass leaves them alone.
	unread := map[string]bool{}
	var errs []error
	for _, ic := range desired {
		keep[ic.Name] = true
		want[ic.Name] = StatusPending
		state, err := r.Store.State(r.CachePath, ic.Name)
		if err != nil {
			errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
			continue
		}
		if state != cache.Complete {
			continue
		}
		rec, err := r.Store.ReadRecord(r.CachePath, ic.Name)
		switch {
		case err != nil:
			unread[ic.Name] = true
			errs = append(errs, errors.Wrap(err, errors.WithProperty("resource", ic.Name)))
		case rec.Owner != cache.OwnerAgent:
			seeded[ic.Name] = rec
		default:
			want[ic.Name] = StatusSynced
		}
	}
	if err := r.patchLabels(ctx, &node, want); err != nil {
		errs = append(errs, err)
	}

	for _, ic := range desired {
		if want[ic.Name] == StatusSynced || unread[ic.Name] {
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

	removed, err := r.Store.GC(r.CachePath, keep)
	if err != nil {
		errs = append(errs, errors.Wrap(err, errors.WithProperty("cachePath", r.CachePath)))
	}
	if len(removed) > 0 {
		log.Info("Removed cache entries no resource keeps", "cachePath", r.CachePath, "entries", removed)
	}
	if err := r.patchLabels(ctx, &node, want); err != nil {
		errs = append(errs, err)
	}
	if r.FS != nil {
		r.FS.SetPaths([]string{r.CachePath})
	}

	if err := utilerrors.NewAggregate(errs); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("node converged", "resources", len(desired))
	return ctrl.Result{RequeueAfter: r.Resync}, nil
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
	if err := r.Store.Adopt(r.CachePath, ic.Name); err != nil {
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
	if err := fill.CheckCachePath(r.CachePath); err != nil {
		r.Recorder.Eventf(ic, nil, corev1.EventTypeWarning, "CachePathUnavailable", "Sync",
			"cache path %s is not usable on node %s (is it mounted?)", r.CachePath, r.NodeName)
		return errors.Wrap(ErrSync, errors.CausedBy(err))
	}
	return fill.Fill(ctx, r.Store, r.Puller, r.CachePath, ic.Name, ic.Spec.Source, cache.OwnerAgent, func(cerr error) {
		logf.FromContext(ctx).Error(cerr, "Failed to close the cache image stream", "resource", ic.Name)
	})
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
