package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// The reconciler against a real kube-apiserver and etcd (envtest), with the real CRDs:
// ours from deploy/k8s/crd, HTTPRoute and ReferenceGrant from the gateway-api module
// (the version the cluster runs), EnvoyExtensionPolicy from Envoy Gateway v1.9.2 in
// testdata. So a wrong field name in what the controller writes fails here, not on GKE.
//
// envtest runs no other controllers: nothing garbage-collects owned objects and nothing
// plays Envoy Gateway, so the test asserts owner references and writes the
// EnvoyExtensionPolicy status itself where the real controller would.
//
// Needs KUBEBUILDER_ASSETS (`make test-envtest`); skipped without it, except in CI.
func TestReconciler(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is unset in CI: the envtest step is missing")
		}
		t.Skip("set KUBEBUILDER_ASSETS to run against envtest (make test-envtest)")
	}
	ctrl.SetLogger(logr.Discard())

	gatewayAPI, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		t.Fatalf("locate the gateway-api module: %v", err)
	}
	crds := filepath.Join(strings.TrimSpace(string(gatewayAPI)), "config", "crd", "standard")
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "deploy", "k8s", "crd"),
			filepath.Join(crds, "gateway.networking.k8s.io_httproutes.yaml"),
			filepath.Join(crds, "gateway.networking.k8s.io_referencegrants.yaml"),
			filepath.Join("testdata", "gateway.envoyproxy.io_envoyextensionpolicies.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{
		Client:          mgr.GetClient(),
		Scheme:          scheme,
		Log:             slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Filter:          Backend{Namespace: "x402-system", Name: "x402-filter", Port: 9002},
		PricesNamespace: "x402-system",
		PricesName:      "x402-prices",
		GrantName:       "extension-policies-to-filter",
		Networks:        []radius.Profile{radius.Testnet},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Error("manager:", err)
		}
	}()

	// Assertions read straight from the API server, never through the manager's cache.
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: ctx, c: c}
	for _, ns := range []string{"demo", "shop", "x402-system"} {
		h.create(h.obj("v1", "Namespace", "", ns, nil))
	}

	// The subtests share state and run in order.
	t.Run("accept", func(t *testing.T) {
		h.t = t
		h.create(h.httpRoute("demo", "httpbin"))
		// The deployed sample, as applied: a bad edit to it fails here, not on the cluster.
		h.create(h.fromFile(filepath.Join("..", "..", "deploy", "k8s", "demo", "40-paymentpolicy.yaml")))

		h.eventually("policy accepted", func() error { return h.accepted("demo", "httpbin", metav1.ConditionTrue, v1alpha1.ReasonAccepted) })

		eep := h.get(eepGVK, "demo", "x402-httpbin")
		owner := metav1.GetControllerOf(eep)
		if owner == nil || owner.Kind != "PaymentPolicy" || owner.Name != "httpbin" {
			t.Errorf("EnvoyExtensionPolicy owner = %+v, want the PaymentPolicy", owner)
		}
		// What the API server stored, after the CRD's own validation and defaulting.
		raw, _ := json.Marshal(eep.Object["spec"])
		for _, want := range []string{`"messageTimeout":"300ms"`, `"statusOnError":503`, `"failOpen":false`, `"xds.route_name"`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("EnvoyExtensionPolicy spec lacks %s: %s", want, raw)
			}
		}

		h.eventually("table has the route", func() error { return h.table("httproute/demo/httpbin") })
		h.eventually("grant lists demo", func() error { return h.grant("demo") })
	})

	t.Run("target not found, then found", func(t *testing.T) {
		h.t = t
		h.create(policy("shop", "catalog", "catalog", "2500"))
		h.eventually("TargetNotFound", func() error {
			return h.accepted("shop", "catalog", metav1.ConditionFalse, v1alpha1.ReasonTargetNotFound)
		})
		if err := h.table("httproute/demo/httpbin"); err != nil {
			t.Error(err)
		}
		if _, err := h.tryGet(eepGVK, "shop", "x402-catalog"); !apierrors.IsNotFound(err) {
			t.Errorf("EnvoyExtensionPolicy written for a missing route: %v", err)
		}

		h.create(h.httpRoute("shop", "catalog"))
		h.eventually("accepted once the route exists", func() error {
			return h.accepted("shop", "catalog", metav1.ConditionTrue, v1alpha1.ReasonAccepted)
		})
		h.eventually("table has both routes", func() error { return h.table("httproute/demo/httpbin", "httproute/shop/catalog") })
		h.eventually("grant lists both namespaces", func() error { return h.grant("demo", "shop") })
	})

	t.Run("price change", func(t *testing.T) {
		h.t = t
		var pp v1alpha1.PaymentPolicy
		h.must(c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "catalog"}, &pp))
		pp.Spec.Price = "3000"
		h.must(c.Update(ctx, &pp))
		h.eventually("new price in the table", func() error {
			prices, err := h.prices()
			if err != nil {
				return err
			}
			e, ok := prices.Lookup("httproute/shop/catalog")
			if !ok || e.Price.String() != "3000" {
				return fmt.Errorf("price = %v", e)
			}
			return nil
		})
	})

	t.Run("mirrors Envoy Gateway's verdict", func(t *testing.T) {
		h.t = t
		h.setEEPAccepted("demo", "x402-httpbin", "False", "Conflicted", "another policy targets the route")
		h.eventually("Conflicted mirrored", func() error {
			return h.accepted("demo", "httpbin", metav1.ConditionFalse, v1alpha1.ReasonConflicted)
		})
		h.setEEPAccepted("demo", "x402-httpbin", "True", "Accepted", "Policy has been accepted.")
		h.eventually("Accepted again", func() error {
			return h.accepted("demo", "httpbin", metav1.ConditionTrue, v1alpha1.ReasonAccepted)
		})
	})

	t.Run("delete policy", func(t *testing.T) {
		h.t = t
		h.must(c.Delete(ctx, policy("shop", "catalog", "catalog", "")))
		h.eventually("route dropped from the table", func() error { return h.table("httproute/demo/httpbin") })
		h.eventually("namespace dropped from the grant", func() error { return h.grant("demo") })
		// envtest has no garbage collector: the owner reference asserted above is what
		// deletes the EnvoyExtensionPolicy on a real cluster.
	})

	t.Run("delete route", func(t *testing.T) {
		h.t = t
		h.must(c.Delete(ctx, h.httpRoute("demo", "httpbin")))
		h.eventually("TargetNotFound after the route is gone", func() error {
			return h.accepted("demo", "httpbin", metav1.ConditionFalse, v1alpha1.ReasonTargetNotFound)
		})
		h.eventually("EnvoyExtensionPolicy removed", func() error {
			_, err := h.tryGet(eepGVK, "demo", "x402-httpbin")
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("still there: %w", err)
			}
			return nil
		})
		h.eventually("empty table", func() error { return h.table() })
		h.eventually("grant removed", func() error {
			_, err := h.tryGet(referenceGrantGVK, "x402-system", "extension-policies-to-filter")
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("still there: %w", err)
			}
			return nil
		})
	})

	t.Run("schema refuses", func(t *testing.T) {
		h.t = t
		cases := []struct {
			name string
			edit func(*v1alpha1.PaymentPolicy)
			want string
		}{
			{"unit mistake", func(p *v1alpha1.PaymentPolicy) { p.Spec.Price = "0.001" }, "spec.price"},
			{"short payTo", func(p *v1alpha1.PaymentPolicy) { p.Spec.PayTo = "0xabc" }, "spec.payTo"},
			{"timeout too long", func(p *v1alpha1.PaymentPolicy) { p.Spec.MaxTimeoutSeconds = 601 }, "spec.maxTimeoutSeconds"},
			{"mainnet", func(p *v1alpha1.PaymentPolicy) { p.Spec.Network = "mainnet" }, "spec.network"},
			{"other kind", func(p *v1alpha1.PaymentPolicy) { p.Spec.TargetRef.Kind = "GRPCRoute" }, "spec.targetRef.kind"},
		}
		for _, cs := range cases {
			p := policy("demo", "bad-"+strings.ReplaceAll(cs.name, " ", "-"), "httpbin", "1000")
			cs.edit(p)
			err := c.Create(ctx, p)
			if err == nil || !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), cs.want) {
				t.Errorf("%s: err = %v, want Invalid mentioning %s", cs.name, err, cs.want)
			}
		}

		var pp v1alpha1.PaymentPolicy
		h.must(c.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "httpbin"}, &pp))
		pp.Spec.TargetRef.Name = "other"
		if err := c.Update(ctx, &pp); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("targetRef change: err = %v, want immutable", err)
		}
	})
}

// policy is a PaymentPolicy as a user would write it: the schema fills the defaults.
func policy(ns, name, route, price string) *v1alpha1.PaymentPolicy {
	return &v1alpha1.PaymentPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.PaymentPolicySpec{
			TargetRef: v1alpha1.TargetRef{Name: route},
			Network:   "testnet",
			Price:     price,
			PayTo:     "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4",
		},
	}
}

type harness struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) obj(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

func (h *harness) fromFile(path string) *unstructured.Unstructured {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path)) // a fixture in the repo, not user input
	h.must(err)
	u := &unstructured.Unstructured{}
	h.must(yaml.Unmarshal(raw, &u.Object))
	return u
}

func (h *harness) httpRoute(ns, name string) *unstructured.Unstructured {
	return h.obj("gateway.networking.k8s.io/v1", "HTTPRoute", ns, name, map[string]any{
		"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"name": name, "port": int64(80)}}}},
	})
}

func (h *harness) create(o client.Object) {
	h.t.Helper()
	h.must(h.c.Create(h.ctx, o))
}

func (h *harness) tryGet(gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u, h.c.Get(h.ctx, client.ObjectKey{Namespace: ns, Name: name}, u)
}

func (h *harness) get(gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	h.t.Helper()
	u, err := h.tryGet(gvk, ns, name)
	h.must(err)
	return u
}

func (h *harness) eventually(what string, check func() error) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("%s: %v", what, err)
}

func (h *harness) accepted(ns, name string, status metav1.ConditionStatus, reason gatewayv1.PolicyConditionReason) error {
	var pp v1alpha1.PaymentPolicy
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: ns, Name: name}, &pp); err != nil {
		return err
	}
	if len(pp.Status.Ancestors) != 1 {
		return fmt.Errorf("ancestors = %d", len(pp.Status.Ancestors))
	}
	a := pp.Status.Ancestors[0]
	if a.ControllerName != v1alpha1.ControllerName || string(a.AncestorRef.Name) != pp.Spec.TargetRef.Name {
		return fmt.Errorf("ancestor = %+v", a)
	}
	cond := meta.FindStatusCondition(a.Conditions, "Accepted")
	if cond == nil || cond.Status != status || cond.Reason != string(reason) {
		return fmt.Errorf("Accepted = %+v, want %s/%s", cond, status, reason)
	}
	if cond.ObservedGeneration != pp.Generation {
		return fmt.Errorf("observedGeneration %d, generation %d", cond.ObservedGeneration, pp.Generation)
	}
	return nil
}

func (h *harness) prices() (*pricing.Prices, error) {
	cm, err := h.tryGet(configMapGVK, "x402-system", "x402-prices")
	if err != nil {
		return nil, err
	}
	data, _, _ := unstructured.NestedString(cm.Object, "data", pricing.ConfigMapKey)
	return pricing.Parse([]byte(data), radius.Testnet)
}

// table checks the ConfigMap holds exactly these routes and parses as the filter would.
func (h *harness) table(keys ...string) error {
	prices, err := h.prices()
	if err != nil {
		return err
	}
	if prices.Len() != len(keys) {
		return fmt.Errorf("routes = %d, want %d", prices.Len(), len(keys))
	}
	for _, k := range keys {
		if _, ok := prices.Lookup(k); !ok {
			return fmt.Errorf("route %s missing", k)
		}
	}
	return nil
}

func (h *harness) grant(namespaces ...string) error {
	g, err := h.tryGet(referenceGrantGVK, "x402-system", "extension-policies-to-filter")
	if err != nil {
		return err
	}
	from, _, _ := unstructured.NestedSlice(g.Object, "spec", "from")
	var got []string
	for _, f := range from {
		m := f.(map[string]any)
		if m["kind"] != "EnvoyExtensionPolicy" {
			return fmt.Errorf("from kind = %v", m["kind"])
		}
		got = append(got, m["namespace"].(string))
	}
	if strings.Join(got, ",") != strings.Join(namespaces, ",") {
		return fmt.Errorf("from = %v, want %v", got, namespaces)
	}
	return nil
}

// setEEPAccepted plays Envoy Gateway: writes the Accepted condition on an
// EnvoyExtensionPolicy's status.
func (h *harness) setEEPAccepted(ns, name, status, reason, message string) {
	h.t.Helper()
	eep := h.get(eepGVK, ns, name)
	h.must(unstructured.SetNestedSlice(eep.Object, []any{map[string]any{
		"ancestorRef":    map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "httpbin"},
		"controllerName": "gateway.envoyproxy.io/gatewayclass-controller",
		"conditions": []any{map[string]any{
			"type": "Accepted", "status": status, "reason": reason, "message": message,
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		}},
	}}, "status", "ancestors"))
	h.must(h.c.Status().Update(h.ctx, eep))
}
