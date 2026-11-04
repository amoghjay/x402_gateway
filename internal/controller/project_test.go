package controller

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// sample is a policy as the API server hands it to the controller: defaults filled in.
func sample() *v1alpha1.PaymentPolicy {
	return &v1alpha1.PaymentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "httpbin", Namespace: "demo"},
		Spec: v1alpha1.PaymentPolicySpec{
			TargetRef:           v1alpha1.TargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "httpbin"},
			Network:             "testnet",
			Price:               "1000",
			PayTo:               "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4",
			AssetTransferMethod: "eip3009",
			MaxTimeoutSeconds:   300,
			FailureMode:         "Closed",
			CaptureMode:         "Async",
			Description:         "httpbin, one call",
			MimeType:            "application/json",
			Help:                &v1alpha1.Help{Message: "Demo route."},
		},
	}
}

var testnet = []radius.Profile{radius.Testnet}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		edit func(*v1alpha1.PaymentPolicy)
		want string // substring of the error, "" for accepted
	}{
		{"valid", func(*v1alpha1.PaymentPolicy) {}, ""},
		{"unit mistake", func(p *v1alpha1.PaymentPolicy) { p.Spec.Price = "0.001" }, "price"},
		{"zero price", func(p *v1alpha1.PaymentPolicy) { p.Spec.Price = "0" }, "price"},
		{"unknown network", func(p *v1alpha1.PaymentPolicy) { p.Spec.Network = "devnet" }, "unknown network"},
		{"network not charged on", func(p *v1alpha1.PaymentPolicy) { p.Spec.Network = "mainnet" }, "not one this filter charges on"},
		{"zero address", func(p *v1alpha1.PaymentPolicy) { p.Spec.PayTo = "0x0000000000000000000000000000000000000000" }, "zero address"},
		{"defaults not applied", func(p *v1alpha1.PaymentPolicy) { p.Spec.AssetTransferMethod = "" }, "assetTransferMethod"},
		{"timeout out of range", func(p *v1alpha1.PaymentPolicy) { p.Spec.MaxTimeoutSeconds = 601 }, "maxTimeoutSeconds"},
		{"bad docs URL", func(p *v1alpha1.PaymentPolicy) { p.Spec.Help = &v1alpha1.Help{DocsURL: "http://x"} }, "docsURL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := sample()
			c.edit(p)
			err := validate(p, testnet)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.want != "" && err == nil:
				t.Errorf("accepted, want error containing %q", c.want)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestRouteOf(t *testing.T) {
	r, err := routeOf(sample())
	if err != nil {
		t.Fatal(err)
	}
	want := pricing.Route{
		Network: "eip155:72344", Price: "1000", Asset: radius.SBC,
		PayTo: "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", AssetTransferMethod: "eip3009",
		MaxTimeoutSeconds: 300, FailureMode: "Closed", CaptureMode: "Async",
		Description: "httpbin, one call", MimeType: "application/json",
		Help: &pricing.Help{Message: "Demo route."},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("routeOf = %+v, want %+v", r, want)
	}

	p := sample()
	p.Spec.Help = &v1alpha1.Help{}
	if r, _ := routeOf(p); r.Help != nil {
		t.Error("an empty help block should project to no help")
	}
}

// The table the controller writes is the table the filter loads: round-trip it through
// the filter's parser and check the entry is the one the paywall would serve.
func TestTableJSON(t *testing.T) {
	a, b := sample(), sample()
	b.Namespace, b.Spec.TargetRef.Name, b.Spec.Price = "shop", "catalog", "2500"
	ra, _ := routeOf(a)
	rb, _ := routeOf(b)
	data, err := tableJSON(map[string]pricing.Route{routeKey(a): ra, routeKey(b): rb}, testnet)
	if err != nil {
		t.Fatal(err)
	}
	prices, err := pricing.Parse(data, testnet...)
	if err != nil {
		t.Fatal(err)
	}
	if prices.Len() != 2 {
		t.Fatalf("routes = %d, want 2", prices.Len())
	}
	e, ok := prices.Lookup("httproute/shop/catalog")
	if !ok || e.Requirements.Amount != "2500" || e.Requirements.PayTo != b.Spec.PayTo {
		t.Errorf("entry = %+v", e)
	}

	empty, err := tableJSON(nil, testnet)
	if err != nil || !strings.Contains(string(empty), `"routes": {}`) {
		t.Errorf("empty table = %q, %v", empty, err)
	}
}

func TestDesiredEEP(t *testing.T) {
	u := desiredEEP(sample(), Backend{Namespace: "x402-system", Name: "x402-filter", Port: 9002})
	if u.GetAPIVersion() != "gateway.envoyproxy.io/v1alpha1" || u.GetKind() != "EnvoyExtensionPolicy" {
		t.Errorf("GVK = %s", u.GroupVersionKind())
	}
	if u.GetName() != "x402-httpbin" || u.GetNamespace() != "demo" {
		t.Errorf("name = %s/%s", u.GetNamespace(), u.GetName())
	}
	ext, _, _ := unstructured.NestedSlice(u.Object, "spec", "extProc")
	if len(ext) != 1 {
		t.Fatalf("extProc = %v", ext)
	}
	ep := ext[0].(map[string]any)
	checks := map[string]any{
		"failOpen":       false,
		"messageTimeout": "300ms",
		"statusOnError":  int64(503),
	}
	for k, want := range checks {
		if got := ep[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("extProc.%s = %#v, want %#v", k, got, want)
		}
	}
	attrs, _, _ := unstructured.NestedStringSlice(ep, "processingMode", "request", "attributes")
	if !reflect.DeepEqual(attrs, []string{"xds.route_name"}) {
		t.Errorf("attributes = %v", attrs)
	}
	if _, ok, _ := unstructured.NestedMap(ep, "processingMode", "response"); !ok {
		t.Error("response phase not requested: the filter would never see response headers")
	}
	refs, _, _ := unstructured.NestedSlice(ep, "backendRefs")
	if want := map[string]any{"name": "x402-filter", "namespace": "x402-system", "port": int64(9002)}; !reflect.DeepEqual(refs[0], want) {
		t.Errorf("backendRefs[0] = %v", refs[0])
	}
	targets, _, _ := unstructured.NestedSlice(u.Object, "spec", "targetRefs")
	if want := map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "httpbin"}; !reflect.DeepEqual(targets[0], want) {
		t.Errorf("targetRefs[0] = %v", targets[0])
	}
}

func TestDesiredGrant(t *testing.T) {
	u := desiredGrant(Backend{Namespace: "x402-system", Name: "x402-filter"}, "extension-policies-to-filter", []string{"shop", "demo"})
	from, _, _ := unstructured.NestedSlice(u.Object, "spec", "from")
	var nss []string
	for _, f := range from {
		nss = append(nss, f.(map[string]any)["namespace"].(string))
	}
	if !reflect.DeepEqual(nss, []string{"demo", "shop"}) {
		t.Errorf("from namespaces = %v, want sorted [demo shop]", nss)
	}
	to, _, _ := unstructured.NestedSlice(u.Object, "spec", "to")
	if want := map[string]any{"group": "", "kind": "Service", "name": "x402-filter"}; !reflect.DeepEqual(to[0], want) {
		t.Errorf("to[0] = %v", to[0])
	}
}

func TestEEPAccepted(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	if _, _, _, found := eepAccepted(u); found {
		t.Error("found a condition on an object with no status")
	}
	u.Object["status"] = map[string]any{"ancestors": []any{map[string]any{
		"conditions": []any{
			map[string]any{"type": "Programmed", "status": "True"},
			map[string]any{"type": "Accepted", "status": "False", "reason": "Conflicted", "message": "already attached"},
		},
	}}}
	status, reason, msg, found := eepAccepted(u)
	if !found || status != metav1.ConditionFalse || reason != "Conflicted" || msg != "already attached" {
		t.Errorf("got %v %q %q %v", status, reason, msg, found)
	}
}
