// Package controller reconciles PaymentPolicy objects into what the data plane reads:
// one EnvoyExtensionPolicy per priced route (so Envoy sends that route, and only that
// route, through the filter), the ReferenceGrant that lets those policies name the
// filter's Service across namespaces, and the price-table ConfigMap the filter watches.
//
// Every object the controller writes is applied with Server-Side Apply under one field
// manager: the controller states the fields it wants and the API server merges them, so
// a hand edit to a field it owns is put back on the next reconcile and a field it never
// set (an annotation someone adds) is left alone. The three cluster-wide objects are a
// full recompute from the current list of policies on every reconcile, never an
// incremental edit, so there is no state to drift.
package controller

import (
	"encoding/json"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// fieldManager names this controller in each object's managedFields.
const fieldManager = "x402-controller"

// Backend is the filter's Service, where Envoy sends priced requests.
type Backend struct {
	Namespace string
	Name      string
	Port      int64
}

var (
	eepGVK            = schema.GroupVersionKind{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "EnvoyExtensionPolicy"}
	httpRouteGVK      = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	referenceGrantGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrant"}
	configMapGVK      = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
)

// routeKey is the policy's key in the price table: the HTTPRoute it targets, in the
// form Envoy names routes (internal/pricing.RouteKey).
func routeKey(pp *v1alpha1.PaymentPolicy) string {
	return "httproute/" + pp.Namespace + "/" + pp.Spec.TargetRef.Name
}

// eepName is the EnvoyExtensionPolicy written for a policy. The prefix keeps it clear
// of any policy an app team writes by hand for the same route.
func eepName(pp *v1alpha1.PaymentPolicy) string { return "x402-" + pp.Name }

// routeOf projects a policy into its price-table entry. The network name becomes the
// chain ID and the SBC contract: a policy never carries an address that could be mistyped.
// The fields the schema defaults (assetTransferMethod, maxTimeoutSeconds, failureMode,
// captureMode) are copied as they are; the API server has filled them in by the time the
// controller sees the object.
func routeOf(pp *v1alpha1.PaymentPolicy) (pricing.Route, error) {
	p, err := radius.Lookup(pp.Spec.Network)
	if err != nil {
		return pricing.Route{}, err
	}
	r := pricing.Route{
		Network:             p.Network(),
		Price:               pp.Spec.Price,
		Asset:               radius.SBC,
		PayTo:               pp.Spec.PayTo,
		AssetTransferMethod: pp.Spec.AssetTransferMethod,
		MaxTimeoutSeconds:   int(pp.Spec.MaxTimeoutSeconds),
		FailureMode:         pp.Spec.FailureMode,
		CaptureMode:         pp.Spec.CaptureMode,
		Description:         pp.Spec.Description,
		MimeType:            pp.Spec.MimeType,
	}
	if h := pp.Spec.Help; h != nil && (h.DocsURL != "" || h.Message != "") {
		r.Help = &pricing.Help{DocsURL: h.DocsURL, Message: h.Message}
	}
	return r, nil
}

// validate runs the filter's own table validation on this one policy, so a policy the
// controller accepts is one the filter will load. The schema has already refused the
// obvious mistakes; this catches the rest (a network the filter does not charge on, the
// zero address) with the same wording the filter would log.
func validate(pp *v1alpha1.PaymentPolicy, networks []radius.Profile) error {
	r, err := routeOf(pp)
	if err != nil {
		return err
	}
	_, err = tableJSON(map[string]pricing.Route{routeKey(pp): r}, networks)
	return err
}

// tableJSON is the price table for the accepted routes, validated as the filter would.
func tableJSON(routes map[string]pricing.Route, networks []radius.Profile) ([]byte, error) {
	if routes == nil {
		routes = map[string]pricing.Route{} // "routes": {} rather than null
	}
	data, err := json.MarshalIndent(pricing.Table{SchemaVersion: pricing.SchemaVersion, Routes: routes}, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := pricing.Parse(data, networks...); err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// desiredEEP is the EnvoyExtensionPolicy for one accepted policy. Everything here is
// the controller's call, not the app team's: the route pays the ext_proc hop to the
// filter, headers only, with the route attribute the filter prices on; failOpen is never
// true (a hung filter is a 504, not a free call); a filter error is a retryable 503.
func desiredEEP(pp *v1alpha1.PaymentPolicy, filter Backend) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":      eepName(pp),
			"namespace": pp.Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": fieldManager,
				"x402.dev/policy":              pp.Name,
			},
		},
		"spec": map[string]any{
			"targetRefs": []any{map[string]any{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  pp.Spec.TargetRef.Name,
			}},
			"extProc": []any{map[string]any{
				"backendRefs": []any{map[string]any{
					"name":      filter.Name,
					"namespace": filter.Namespace,
					"port":      filter.Port,
				}},
				"processingMode": map[string]any{
					"request":  map[string]any{"attributes": []any{"xds.route_name"}},
					"response": map[string]any{},
				},
				"failOpen": false,
				// Envoy's per-message budget. Raised from the 200 ms default on 2026-11-04:
				// the testnet RPC answered in 130-270 ms on the first live run, and the
				// filter's own deadline (250 ms) must fit under this with the hop to spare.
				"messageTimeout": "300ms",
				"statusOnError":  int64(503),
			}},
		},
	}}
	u.SetGroupVersionKind(eepGVK)
	return u
}

// desiredGrant lets EnvoyExtensionPolicies in the listed namespaces name the filter's
// Service. Gateway API makes a cross-namespace reference an opt-in by the target's
// owner, which here is us, so the grant lives in the filter's namespace and lists every
// namespace that has a priced route.
func desiredGrant(filter Backend, name string, from []string) *unstructured.Unstructured {
	froms := make([]any, 0, len(from))
	for _, ns := range slices.Sorted(slices.Values(from)) {
		froms = append(froms, map[string]any{
			"group":     eepGVK.Group,
			"kind":      eepGVK.Kind,
			"namespace": ns,
		})
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": filter.Namespace},
		"spec": map[string]any{
			"from": froms,
			"to":   []any{map[string]any{"group": "", "kind": "Service", "name": filter.Name}},
		},
	}}
	u.SetGroupVersionKind(referenceGrantGVK)
	return u
}

// desiredConfigMap is the price table as the filter watches it.
func desiredConfigMap(namespace, name string, table []byte) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"data":     map[string]any{pricing.ConfigMapKey: string(table)},
	}}
	u.SetGroupVersionKind(configMapGVK)
	return u
}

// eepAccepted reads the Accepted condition Envoy Gateway wrote on an
// EnvoyExtensionPolicy (Gateway API PolicyStatus: status.ancestors[].conditions).
// found is false until Envoy Gateway has reconciled the object.
func eepAccepted(u *unstructured.Unstructured) (status metav1.ConditionStatus, reason, message string, found bool) {
	ancestors, _, _ := unstructured.NestedSlice(u.Object, "status", "ancestors")
	for _, a := range ancestors {
		am, _ := a.(map[string]any)
		conds, _, _ := unstructured.NestedSlice(am, "conditions")
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			if cm["type"] != "Accepted" {
				continue
			}
			s, _ := cm["status"].(string)
			r, _ := cm["reason"].(string)
			m, _ := cm["message"].(string)
			return metav1.ConditionStatus(s), r, m, true
		}
	}
	return "", "", "", false
}

// describe is the Accepted=True message: what the route now costs.
func describe(pp *v1alpha1.PaymentPolicy) string {
	return fmt.Sprintf("%s priced at %s atomic units on %s, paid to %s", routeKey(pp), pp.Spec.Price, pp.Spec.Network, pp.Spec.PayTo)
}
