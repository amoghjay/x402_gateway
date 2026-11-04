package pricing

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	clienttesting "k8s.io/client-go/testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

const ns, name = "x402-system", "x402-prices"

// noWatchList is the fake's CoreV1 client, marked as not supporting watch-list (the
// streamed initial list a real API server sends). Only the fake clientset carries that
// mark, not its typed clients.
type noWatchList struct{ corev1client.ConfigMapsGetter }

func (noWatchList) IsWatchListSemanticsUnSupported() bool { return true }

func configMap(cmName string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: cmName}, Data: data}
}

// TestWatcher walks the table through its life: first load, a bad edit, a good edit with
// a second route, and a delete. Only the good versions ever reach the store.
func TestWatcher(t *testing.T) {
	good := string(example(t))
	var tbl map[string]any
	_ = json.Unmarshal(example(t), &tbl)
	routes := tbl["routes"].(map[string]any)
	routes["httproute/demo/second"] = routes["httproute/demo/httpbin"]
	b, _ := json.Marshal(tbl)
	twoRoutes := string(b)

	// The fake's watch does not replay history, so an object created between the
	// informer's list and its watch would be lost. Signal when the watch is open and
	// create nothing before (client-go's fake-client example does the same). The reactor
	// also checks that the watch carries the name selector RBAC needs.
	fc := fake.NewClientset()
	watching := make(chan struct{})
	var once sync.Once
	fc.PrependWatchReactor("configmaps", func(a clienttesting.Action) (bool, watch.Interface, error) {
		if got := a.(clienttesting.WatchAction).GetWatchRestrictions().Fields.String(); got != "metadata.name="+name {
			t.Errorf("watch field selector %q", got)
		}
		wi, err := fc.Tracker().Watch(a.GetResource(), a.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		once.Do(func() { close(watching) }) // an informer may re-watch
		return true, wi, nil
	})
	client := noWatchList{fc.CoreV1()}
	store := NewStore()
	events := make(chan bool, 10)
	w := &Watcher{
		Client: client, Namespace: ns, Name: name, Networks: []radius.Profile{radius.Testnet},
		Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		notify: func(ok bool) { events <- ok },
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	select {
	case <-watching:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never opened")
	}

	next := func(want bool) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event accepted=%v, want %v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
	}
	cms := client.ConfigMaps(ns)

	// Another ConfigMap in the namespace is never read, whatever it holds.
	if _, err := cms.Create(ctx, configMap("other", map[string]string{ConfigMapKey: "garbage"}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cms.Create(ctx, configMap(name, map[string]string{ConfigMapKey: good}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	next(true)
	<-store.Loaded()
	first := store.Load()
	if first.Len() != 1 {
		t.Fatalf("%d routes", first.Len())
	}

	if _, err := cms.Update(ctx, configMap(name, map[string]string{ConfigMapKey: `{"schemaVersion":1,"routes":{"httproute/demo/httpbin":{"price":"0.001"}}}`}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	next(false)
	if store.Load() != first {
		t.Fatal("a rejected table replaced the good one")
	}

	if _, err := cms.Update(ctx, configMap(name, map[string]string{"wrong-key": good}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	next(false)
	if store.Load() != first {
		t.Fatal("a ConfigMap without the key replaced the good table")
	}

	if _, err := cms.Update(ctx, configMap(name, map[string]string{ConfigMapKey: twoRoutes}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	next(true)
	if store.Load().Len() != 2 {
		t.Fatalf("%d routes after the good edit", store.Load().Len())
	}

	if err := cms.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	next(false)
	if store.Load().Len() != 2 {
		t.Fatal("delete dropped the last good table")
	}
}

// The selector is what RBAC's resourceNames needs on list and watch.
func TestWatcherSelector(t *testing.T) {
	var o metav1.ListOptions
	(&Watcher{Name: name}).selector(&o)
	if o.FieldSelector != "metadata.name="+name {
		t.Errorf("field selector %q", o.FieldSelector)
	}
}

func TestStartsUnloadedWithoutAConfigMap(t *testing.T) {
	store := NewStore()
	w := &Watcher{Client: noWatchList{fake.NewClientset().CoreV1()}, Namespace: ns, Name: name, Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)
	select {
	case <-store.Loaded():
		t.Fatal("loaded with no ConfigMap")
	default:
	}
}
