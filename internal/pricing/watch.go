package pricing

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

// Watcher keeps a Store in step with the price-table ConfigMap.
//
// It is an informer (an API watch), never a mounted volume: a mounted ConfigMap refreshes
// on the kubelet's sync period, so a new price could take a minute to show; a watch
// delivers it in about a second. The informer lists and watches exactly one object, by a
// metadata.name field selector, which is also what lets RBAC grant list/watch on that one
// name (resourceNames) and nothing else.
//
// A table that fails to parse, or a deleted ConfigMap, leaves the last good table in
// place: the controller being down or wrong must not make priced routes free.
//
// It is built from the core/v1 client and a plain ListWatch, not the clientset and its
// informer factory: those import every API group's client and informer, which took the
// filter binary from 14 to 58 MiB to watch one ConfigMap.
type Watcher struct {
	Client    corev1client.ConfigMapsGetter
	Namespace string
	Name      string
	Networks  []radius.Profile // networks a route may charge on; see Parse
	Store     *Store
	Log       *slog.Logger

	notify func(accepted bool) // tests: called after each event is handled
}

// Run watches until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	cms := w.Client.ConfigMaps(w.Namespace)
	// Wrapped as the generated informers do: the reflector then streams the initial list
	// (watch-list) from a real API server, and skips that for a client that says it
	// cannot, such as a test fake.
	lw := cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			w.selector(&o)
			return cms.List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			w.selector(&o)
			return cms.Watch(ctx, o)
		},
	}, w.Client)
	inf := cache.NewSharedIndexInformer(lw, &corev1.ConfigMap{}, 0, cache.Indexers{}) // 0: no periodic resync, events only
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.apply(obj) },
		UpdateFunc: func(_, obj any) { w.apply(obj) },
		DeleteFunc: func(any) {
			w.Log.Warn("price table ConfigMap deleted; serving the last good table", "configmap", w.Namespace+"/"+w.Name, "loaded", w.Store.Load() != nil)
			w.done(false)
		},
	}); err != nil {
		return err
	}
	inf.RunWithContext(ctx) // returns when ctx is done
	return nil
}

func (w *Watcher) selector(o *metav1.ListOptions) {
	o.FieldSelector = fields.OneTermEqualSelector("metadata.name", w.Name).String()
}

func (w *Watcher) apply(obj any) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm.Name != w.Name { // the selector already ensures this; belt and braces
		return
	}
	log := w.Log.With("configmap", cm.Namespace+"/"+cm.Name, "resourceVersion", cm.ResourceVersion)
	data, ok := cm.Data[ConfigMapKey]
	if !ok {
		log.Error("price table rejected; serving the last good table", "err", "no "+ConfigMapKey+" key", "loaded", w.Store.Load() != nil)
		w.done(false)
		return
	}
	p, err := Parse([]byte(data), w.Networks...)
	if err != nil {
		log.Error("price table rejected; serving the last good table", "err", err, "loaded", w.Store.Load() != nil)
		w.done(false)
		return
	}
	w.Store.Set(p)
	log.Info("price table loaded", "routes", p.Len())
	w.done(true)
}

func (w *Watcher) done(accepted bool) {
	if w.notify != nil {
		w.notify(accepted)
	}
}
