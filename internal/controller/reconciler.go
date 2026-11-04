package controller

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// targetIndex is a cache index from a policy's target route name to the policy, so an
// HTTPRoute event finds the policies that price it without listing them all.
const targetIndex = "spec.targetRef.name"

// Reconciler turns PaymentPolicies into the objects the data plane reads.
//
// PaymentPolicies and HTTPRoute metadata are read from the manager's cache (a watch,
// kept in step by the API server). The three objects the controller writes are
// unstructured and so, with the default client options, read and written straight from
// the API server: the EnvoyExtensionPolicy's status is fresh when it is mirrored, and
// the controller needs no watch on ConfigMaps or ReferenceGrants at all.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    *slog.Logger

	Filter          Backend          // the filter's Service
	PricesNamespace string           // where the price-table ConfigMap and the grant live
	PricesName      string           // the ConfigMap the filter watches
	GrantName       string           // the ReferenceGrant in PricesNamespace
	Networks        []radius.Profile // networks a policy may charge on
}

// SetupWithManager registers the index and the three watches: the policies themselves,
// the EnvoyExtensionPolicies they own (status changes come back through here), and
// HTTPRoutes, by metadata only, so a route that appears or disappears re-evaluates the
// policies that target it.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.PaymentPolicy{}, targetIndex,
		func(o client.Object) []string { return []string{o.(*v1alpha1.PaymentPolicy).Spec.TargetRef.Name} })
	if err != nil {
		return err
	}
	eep := &unstructured.Unstructured{}
	eep.SetGroupVersionKind(eepGVK)
	route := &metav1.PartialObjectMetadata{}
	route.SetGroupVersionKind(httpRouteGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named("paymentpolicy").
		For(&v1alpha1.PaymentPolicy{}).
		Owns(eep).
		WatchesMetadata(route, handler.EnqueueRequestsFromMapFunc(r.policiesForRoute)).
		Complete(r)
}

func (r *Reconciler) policiesForRoute(ctx context.Context, route client.Object) []reconcile.Request {
	var list v1alpha1.PaymentPolicyList
	if err := r.List(ctx, &list, client.InNamespace(route.GetNamespace()), client.MatchingFields{targetIndex: route.GetName()}); err != nil {
		r.Log.Error("list policies for route", "namespace", route.GetNamespace(), "route", route.GetName(), "err", err)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// Reconcile handles one policy, then recomputes the cluster-wide objects from all of
// them. A deleted policy's EnvoyExtensionPolicy goes with it through its owner
// reference; its price-table entry and grant entry go in the recompute.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pp v1alpha1.PaymentPolicy
	if err := r.Get(ctx, req.NamespacedName, &pp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.project(ctx)
		}
		return ctrl.Result{}, err
	}

	cond, err := r.reconcilePolicy(ctx, &pp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setAccepted(ctx, &pp, cond); err != nil {
		return ctrl.Result{}, err
	}
	r.Log.Info("reconciled", "policy", req.String(), "accepted", cond.Status, "reason", cond.Reason)
	return ctrl.Result{}, r.project(ctx)
}

// reconcilePolicy decides one policy's Accepted condition and writes or removes its
// EnvoyExtensionPolicy accordingly.
func (r *Reconciler) reconcilePolicy(ctx context.Context, pp *v1alpha1.PaymentPolicy) (metav1.Condition, error) {
	accepted := func(status metav1.ConditionStatus, reason gatewayv1.PolicyConditionReason, msg string) metav1.Condition {
		return metav1.Condition{Type: string(gatewayv1.PolicyConditionAccepted), Status: status, Reason: string(reason), Message: msg}
	}

	if err := validate(pp, r.Networks); err != nil {
		return accepted(metav1.ConditionFalse, v1alpha1.ReasonInvalid, err.Error()), r.removeEEP(ctx, pp)
	}

	route := &metav1.PartialObjectMetadata{}
	route.SetGroupVersionKind(httpRouteGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: pp.Spec.TargetRef.Name}, route); err != nil {
		if !apierrors.IsNotFound(err) {
			return metav1.Condition{}, err
		}
		msg := fmt.Sprintf("HTTPRoute %s/%s not found", pp.Namespace, pp.Spec.TargetRef.Name)
		return accepted(metav1.ConditionFalse, v1alpha1.ReasonTargetNotFound, msg), r.removeEEP(ctx, pp)
	}

	eep := desiredEEP(pp, r.Filter)
	if err := controllerutil.SetControllerReference(pp, eep, r.Scheme); err != nil {
		return metav1.Condition{}, err
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(eep), client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return metav1.Condition{}, fmt.Errorf("apply EnvoyExtensionPolicy %s: %w", eep.GetName(), err)
	}

	// Mirror what Envoy Gateway made of it. Until it has reconciled the object there is
	// no condition yet; its later status change re-enters here through the Owns watch.
	if err := r.Get(ctx, client.ObjectKeyFromObject(eep), eep); err != nil {
		return metav1.Condition{}, err
	}
	if status, reason, msg, found := eepAccepted(eep); found && status != metav1.ConditionTrue {
		why := v1alpha1.ReasonInvalid
		if reason == string(v1alpha1.ReasonConflicted) {
			why = v1alpha1.ReasonConflicted
		}
		return accepted(metav1.ConditionFalse, why, "EnvoyExtensionPolicy "+eep.GetName()+": "+msg), nil
	}
	return accepted(metav1.ConditionTrue, v1alpha1.ReasonAccepted, describe(pp)), nil
}

// removeEEP deletes the policy's EnvoyExtensionPolicy if it exists and is ours.
func (r *Reconciler) removeEEP(ctx context.Context, pp *v1alpha1.PaymentPolicy) error {
	eep := &unstructured.Unstructured{}
	eep.SetGroupVersionKind(eepGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: eepName(pp)}, eep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(eep, pp) {
		r.Log.Warn("EnvoyExtensionPolicy with our name is not ours; leaving it", "name", eep.GetName(), "namespace", eep.GetNamespace())
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, eep))
}

// setAccepted writes the policy's status: one ancestor, the HTTPRoute, with the Accepted
// condition. The existing conditions are kept so lastTransitionTime only moves when the
// status does; nothing is written when nothing changed.
func (r *Reconciler) setAccepted(ctx context.Context, pp *v1alpha1.PaymentPolicy, cond metav1.Condition) error {
	cond.ObservedGeneration = pp.Generation
	want := gatewayv1.PolicyAncestorStatus{
		AncestorRef: gatewayv1.ParentReference{
			Group:     ptr.To(gatewayv1.Group(httpRouteGVK.Group)),
			Kind:      ptr.To(gatewayv1.Kind(httpRouteGVK.Kind)),
			Namespace: ptr.To(gatewayv1.Namespace(pp.Namespace)),
			Name:      gatewayv1.ObjectName(pp.Spec.TargetRef.Name),
		},
		ControllerName: v1alpha1.ControllerName,
	}
	// targetRef is immutable, so our single ancestor entry can only be this one.
	have := len(pp.Status.Ancestors) == 1 && pp.Status.Ancestors[0].ControllerName == v1alpha1.ControllerName
	if have {
		want.Conditions = slices.Clone(pp.Status.Ancestors[0].Conditions)
	}
	if !meta.SetStatusCondition(&want.Conditions, cond) && have {
		return nil
	}
	pp.Status.Ancestors = []gatewayv1.PolicyAncestorStatus{want}
	return r.Status().Update(ctx, pp)
}

// project recomputes the price table and the ReferenceGrant from every policy in the
// cluster. Acceptance is decided here the same way as in reconcilePolicy (valid spec,
// route exists), from the cache, so the table never depends on a status write landing.
func (r *Reconciler) project(ctx context.Context) error {
	var list v1alpha1.PaymentPolicyList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	routes := map[string]pricing.Route{}
	namespaces := map[string]bool{}
	for i := range list.Items {
		pp := &list.Items[i]
		rt, err := routeOf(pp)
		if err != nil || validate(pp, r.Networks) != nil {
			continue
		}
		route := &metav1.PartialObjectMetadata{}
		route.SetGroupVersionKind(httpRouteGVK)
		if err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: pp.Spec.TargetRef.Name}, route); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		routes[routeKey(pp)] = rt
		namespaces[pp.Namespace] = true
	}

	table, err := tableJSON(routes, r.Networks)
	if err != nil {
		// Each route passed validate on its own, so this cannot happen; if it does, the
		// last good table stays in place and the error surfaces in the log.
		return fmt.Errorf("price table would not load: %w", err)
	}
	cm := desiredConfigMap(r.PricesNamespace, r.PricesName, table)
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(cm), client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply price table: %w", err)
	}

	grant := desiredGrant(r.Filter, r.GrantName, slices.Collect(maps.Keys(namespaces)))
	if len(namespaces) == 0 {
		// A grant needs at least one "from"; with no priced routes there is nothing to allow.
		return client.IgnoreNotFound(r.Delete(ctx, grant))
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(grant), client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply ReferenceGrant: %w", err)
	}
	r.Log.Debug("projected", "routes", len(routes), "namespaces", len(namespaces))
	return nil
}
