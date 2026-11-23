package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"

	compiledpolicy "github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
)

// Watcher keeps a Store in step with the price-table ConfigMap.
//
// It is an informer (an API watch), never a mounted volume: a mounted ConfigMap refreshes
// on the kubelet's sync period, so a new price could take a minute to show; a watch
// delivers it in about a second. The informer lists and watches exactly one object, by a
// metadata.name field selector, which is also what lets RBAC grant list/watch on that one
// name (resourceNames) and nothing else.
//
// A table that cannot be read at all, or a deleted ConfigMap, leaves the last good table
// in place: the controller being down or wrong must not make priced routes free. A table
// with broken routes is loaded with those routes blocked (Parse).
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
	if p == nil {
		log.Error("price table rejected; serving the last good table", "err", err, "loaded", w.Store.Load() != nil)
		w.done(false)
		return
	}
	p.LoadPolicies(cm.Data, w.Store.Load(), log)
	p.LoadAgents(cm.Data, w.Store.Load(), log)
	w.Store.Set(p)
	if blocked := p.BlockedKeys(); len(blocked) > 0 {
		// Loaded: the blocked routes answer 503 until their policy is fixed.
		log.Error("price table loaded with blocked routes; they answer 503", "routes", p.Len(), "platformRoutes", p.PlatformLen(), "blocked", blocked, "err", errText(err))
	} else {
		log.Info("price table loaded", "routes", p.Len(), "platformRoutes", p.PlatformLen())
	}
	w.done(true)
}

func (w *Watcher) done(accepted bool) {
	if w.notify != nil {
		w.notify(accepted)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// LoadPolicies reads the compiled policy of each project the table names, from the
// ConfigMap's data, the table's own key aside. Call it once, before the table is stored. A document replaces the one already loaded only when it validates, is the
// project's, and is not older: the controller never writes an older one, and a filter that
// cannot read a newer one (a schema it does not know) keeps enforcing what it had. A project
// with nothing loadable and nothing loaded before has no policy: its routes answer per
// noPolicy.
func (p *Prices) LoadPolicies(data map[string]string, prev *Prices, log *slog.Logger) {
	out := map[string]*compiledpolicy.Policy{}
	defer func() {
		p.policies = out
		p.origins = map[string]map[string]bool{}
		for project, pol := range out {
			if o := signatureOrigins(pol); len(o) > 0 {
				p.origins[project] = o
			}
		}
	}()
	for _, r := range p.platform {
		if _, done := out[r.Project]; done {
			continue
		}
		old := prev.Policy(r.Project)
		keep := func(why string, err error) {
			if old != nil {
				out[r.Project] = old
			}
			log.Error("compiled policy not loaded; "+why, "project", r.Project, "err", errText(err), "version", versionOf(old))
		}
		doc, ok := data[PolicyKey(r.Project)]
		if !ok {
			if old != nil {
				out[r.Project] = old
			}
			continue // not fetched yet; the controller's Synced condition says why
		}
		pol, err := compiledpolicy.Parse([]byte(doc))
		switch {
		case err != nil:
			keep("keeping the one in force", err)
		case !strings.EqualFold(pol.ProjectID, r.Project):
			keep("keeping the one in force", fmt.Errorf("the document is for project %q", pol.ProjectID))
		case old != nil && pol.Version < old.Version:
			keep("it is older than the one in force", fmt.Errorf("version %d", pol.Version))
		default:
			out[r.Project] = pol
			if old == nil || pol.Version != old.Version {
				log.Info("compiled policy loaded", "project", r.Project, "version", pol.Version, "schema", pol.Schema, "rules", len(pol.Rules))
			}
		}
	}
}

// signatureOrigins is the set of origins a policy names in signature_agent rules,
// normalised as the verifier reports them.
func signatureOrigins(pol *compiledpolicy.Policy) map[string]bool {
	var out map[string]bool
	for _, r := range pol.Rules {
		if r.Match.Agent == nil {
			continue
		}
		for _, o := range r.Match.Agent.SignatureAgent {
			if n, err := webbotauth.Origin(webbotauth.Item{Value: o}); err == nil {
				if out == nil {
					out = map[string]bool{}
				}
				out[n] = true
			}
		}
	}
	return out
}

// LoadAgents reads the key directories the controller fetched. Call it after LoadPolicies.
// An unreadable value keeps the directories already loaded, so signatures that verified keep
// verifying; a missing one means no origin has keys yet.
func (p *Prices) LoadAgents(data map[string]string, prev *Prices, log *slog.Logger) {
	raw, ok := data[AgentsKey]
	if !ok {
		return
	}
	var dirs map[string]*webbotauth.Directory
	if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
		if prev != nil {
			p.agents = prev.agents
		}
		log.Error("key directories not loaded; keeping the ones in force", "err", err)
		return
	}
	p.agents = dirs
}

func versionOf(p *compiledpolicy.Policy) uint64 {
	if p == nil {
		return 0
	}
	return p.Version
}
