package v1alpha1

import (
	"os"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

// The generated CRD is what the API server enforces. This pins the rules the filter and
// the paywall rely on, so a dropped or mistyped marker fails here, not at `kubectl apply`.
func TestGeneratedCRD(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/k8s/crd/x402.dev_paymentpolicies.yaml")
	if err != nil {
		t.Fatalf("read CRD (run `make generate`): %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}

	if got := path(crd, "spec", "names", "shortNames"); !reflect.DeepEqual(got, []any{"pp"}) {
		t.Errorf("shortNames = %v, want [pp]", got)
	}
	versions, _ := path(crd, "spec", "versions").([]any)
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	v := versions[0].(map[string]any)
	if path(v, "subresources", "status") == nil {
		t.Error("status subresource missing")
	}
	if got := path(v, "name"); got != "v1alpha1" {
		t.Errorf("version = %v", got)
	}

	spec := path(v, "schema", "openAPIV3Schema", "properties", "spec").(map[string]any)
	if got := spec["required"]; !reflect.DeepEqual(got, []any{"network", "payTo", "price", "targetRef"}) {
		t.Errorf("spec.required = %v", got)
	}
	props := spec["properties"].(map[string]any)

	checks := []struct {
		field string
		key   string
		want  any
	}{
		{"price", "pattern", "^[0-9]+$"},
		{"price", "maxLength", float64(15)},
		{"payTo", "pattern", "^0x[0-9a-fA-F]{40}$"},
		{"network", "enum", []any{"testnet"}},
		{"assetTransferMethod", "enum", []any{"eip3009"}},
		{"assetTransferMethod", "default", "eip3009"},
		{"maxTimeoutSeconds", "minimum", float64(30)},
		{"maxTimeoutSeconds", "maximum", float64(600)},
		{"maxTimeoutSeconds", "default", float64(300)},
		{"failureMode", "enum", []any{"Closed"}},
		{"failureMode", "default", "Closed"},
		{"captureMode", "enum", []any{"Async"}},
		{"captureMode", "default", "Async"},
		{"description", "maxLength", float64(256)},
	}
	for _, c := range checks {
		if got := path(props, c.field, c.key); !reflect.DeepEqual(got, c.want) {
			t.Errorf("spec.%s.%s = %#v, want %#v", c.field, c.key, got, c.want)
		}
	}

	rules, _ := path(props, "targetRef", "x-kubernetes-validations").([]any)
	if len(rules) != 1 || path(rules[0], "rule") != "self == oldSelf" {
		t.Errorf("targetRef immutability rule missing: %v", rules)
	}
	if got := path(props, "targetRef", "properties", "kind", "enum"); !reflect.DeepEqual(got, []any{"HTTPRoute"}) {
		t.Errorf("targetRef.kind.enum = %v", got)
	}
	if got := path(props, "targetRef", "properties", "group", "default"); got != "gateway.networking.k8s.io" {
		t.Errorf("targetRef.group.default = %v", got)
	}
	if got := path(props, "help", "properties", "message", "maxLength"); got != float64(200) {
		t.Errorf("help.message.maxLength = %v", got)
	}

	// Status is the Gateway API PolicyStatus: ancestors, capped at 16 (GEP-713).
	status := path(v, "schema", "openAPIV3Schema", "properties", "status").(map[string]any)
	if got := path(status, "properties", "ancestors", "maxItems"); got != float64(16) {
		t.Errorf("status.ancestors.maxItems = %v, want 16", got)
	}

	cols, _ := v["additionalPrinterColumns"].([]any)
	var names []any
	for _, c := range cols {
		names = append(names, path(c, "name"))
	}
	if want := []any{"Route", "Network", "Price", "Accepted", "Reason", "Age"}; !reflect.DeepEqual(names, want) {
		t.Errorf("printer columns = %v, want %v", names, want)
	}
}

func path(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
