package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

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
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
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
	Poller          *Poller          // reads Radius Platform projects; nil disables spec.platform
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
	b := ctrl.NewControllerManagedBy(mgr).
		Named("paymentpolicy").
		For(&v1alpha1.PaymentPolicy{}).
		Owns(eep).
		WatchesMetadata(route, handler.EnqueueRequestsFromMapFunc(r.policiesForRoute))
	if r.Poller != nil {
		// A project's new version or fetch error re-reconciles the policies naming it.
		b = b.WatchesRawSource(r.Poller.Source())
		if err := mgr.Add(r.Poller); err != nil {
			return err
		}
	}
	return b.Complete(r)
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

	var all v1alpha1.PaymentPolicyList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, err
	}
	cond, err := r.reconcilePolicy(ctx, &pp, all.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pp.Spec.Platform != nil {
		// After a restart, resume from the document the filter enforces before reporting
		// on it, so the status does not flap to Pending while the first fetch runs.
		if _, err := r.policyDocuments(ctx, map[string]bool{projectID(pp.Spec.Platform): true}); err != nil {
			return ctrl.Result{}, err
		}
	}
	synced := r.syncedCondition(&pp)
	if err := r.setConditions(ctx, &pp, cond, synced); err != nil {
		return ctrl.Result{}, err
	}
	r.Log.Info("reconciled", "policy", req.String(), "accepted", cond.Status, "reason", cond.Reason)
	return ctrl.Result{}, r.project(ctx)
}

// syncedCondition is a platform policy's Synced condition, from the Poller's state of its
// project; nil for a policy without spec.platform.
func (r *Reconciler) syncedCondition(pp *v1alpha1.PaymentPolicy) *metav1.Condition {
	if pp.Spec.Platform == nil {
		return nil
	}
	c := &metav1.Condition{Type: v1alpha1.ConditionSynced, Status: metav1.ConditionFalse}
	id := pp.Spec.Platform.ProjectID
	if r.Poller == nil {
		c.Reason, c.Message = string(v1alpha1.ReasonInvalid), "this controller was started without Radius Platform support"
		return c
	}
	st, ok := r.Poller.State(id)
	if !ok || !st.Tried {
		r.Poller.Kick()
	}
	enforcing := "no compiled policy loaded yet, so the route answers per noPolicy"
	if st.Version > 0 {
		enforcing = fmt.Sprintf("version %d stays in force", st.Version)
	}
	switch {
	case st.Err != "":
		c.Reason, c.Message = st.Reason, st.Err+"; "+enforcing
	case st.Version == 0:
		c.Reason, c.Message = v1alpha1.ReasonPending, "waiting for the first fetch of project "+id
	default:
		c.Status, c.Reason = metav1.ConditionTrue, v1alpha1.ReasonSynced
		c.Message = fmt.Sprintf("version %d of project %s loaded", st.Version, id)
		if len(st.Warnings) > 0 {
			c.Message += "; " + strings.Join(st.Warnings, "; ")
		}
		if agents := r.agentsStatus(id); agents != "" {
			c.Message += "; signature agents: " + agents
		}
	}
	c.Message = clipMessage(c.Message)
	return c
}

// agentsStatus says, for each origin a project's policy names, how many keys the gate holds
// and whether the last directory fetch failed.
func (r *Reconciler) agentsStatus(id string) string {
	var parts []string
	for _, o := range r.Poller.ProjectOrigins(id) {
		st, _ := r.Poller.Directory(o)
		n := 0
		if st.Directory != nil {
			n = len(st.Directory.Keys)
		}
		switch {
		case st.Err != "":
			parts = append(parts, fmt.Sprintf("%s fetch failed, %d keys in force: %s", o, n, st.Err))
		case n == 0:
			parts = append(parts, o+" not fetched yet")
		default:
			parts = append(parts, fmt.Sprintf("%s %d keys", o, n))
		}
	}
	return strings.Join(parts, ", ")
}

// clipMessage keeps a condition message under the API's 32 KiB limit, with room to spare.
func clipMessage(s string) string {
	const limit = 4096
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// reconcilePolicy decides one policy's Accepted condition and writes or removes its
// EnvoyExtensionPolicy accordingly.
func (r *Reconciler) reconcilePolicy(ctx context.Context, pp *v1alpha1.PaymentPolicy, all []v1alpha1.PaymentPolicy) (metav1.Condition, error) {
	accepted := func(status metav1.ConditionStatus, reason gatewayv1.PolicyConditionReason, msg string) metav1.Condition {
		return metav1.Condition{Type: string(gatewayv1.PolicyConditionAccepted), Status: status, Reason: string(reason), Message: msg}
	}

	invalid := r.validate(pp, all)

	route := &metav1.PartialObjectMetadata{}
	route.SetGroupVersionKind(httpRouteGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: pp.Spec.TargetRef.Name}, route); err != nil {
		if !apierrors.IsNotFound(err) {
			return metav1.Condition{}, err
		}
		if invalid != nil {
			return accepted(metav1.ConditionFalse, v1alpha1.ReasonInvalid, invalid.Error()), r.removeEEP(ctx, pp)
		}
		msg := fmt.Sprintf("HTTPRoute %s/%s not found", pp.Namespace, pp.Spec.TargetRef.Name)
		return accepted(metav1.ConditionFalse, v1alpha1.ReasonTargetNotFound, msg), r.removeEEP(ctx, pp)
	}

	// The route exists, so it stays behind the filter even when the policy is invalid:
	// removing the EnvoyExtensionPolicy would serve a priced route free. The route is
	// listed as blocked in the price table instead (project), and answers 503 until the
	// policy is fixed.

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
	if invalid != nil {
		return accepted(metav1.ConditionFalse, v1alpha1.ReasonInvalid, invalid.Error()+"; the route answers 503 until this is fixed"), nil
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

// validate checks one policy as project does: its own spec, and for a platform policy, that
// the controller can read platforms and no other policy names its project differently.
func (r *Reconciler) validate(pp *v1alpha1.PaymentPolicy, all []v1alpha1.PaymentPolicy) error {
	if pp.Spec.Platform == nil {
		return validate(pp, r.Networks)
	}
	if r.Poller == nil {
		return errors.New("spec.platform: this controller was started without Radius Platform support")
	}
	if why := platformConflict(pp, all); why != "" {
		return errors.New("spec.platform: " + why)
	}
	return nil
}

// setConditions writes the policy's status: one ancestor, the HTTPRoute, with the Accepted
// condition and, for a platform policy, the Synced condition (removed when synced is nil).
// The existing conditions are kept so lastTransitionTime only moves when the status does;
// nothing is written when nothing changed.
func (r *Reconciler) setConditions(ctx context.Context, pp *v1alpha1.PaymentPolicy, cond metav1.Condition, synced *metav1.Condition) error {
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
	changed := meta.SetStatusCondition(&want.Conditions, cond)
	if synced != nil {
		synced.ObservedGeneration = pp.Generation
		changed = meta.SetStatusCondition(&want.Conditions, *synced) || changed
	} else {
		changed = meta.RemoveStatusCondition(&want.Conditions, v1alpha1.ConditionSynced) || changed
	}
	if !changed && have {
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
	blocked := map[string]string{}
	namespaces := map[string]bool{}
	projects := map[string]bool{} // platform projects named by a policy on an existing route
	platformRoutes := map[string]pricing.PlatformRoute{}
	for i := range list.Items {
		pp := &list.Items[i]
		route := &metav1.PartialObjectMetadata{}
		route.SetGroupVersionKind(httpRouteGVK)
		if err := r.Get(ctx, client.ObjectKey{Namespace: pp.Namespace, Name: pp.Spec.TargetRef.Name}, route); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		namespaces[pp.Namespace] = true // the EnvoyExtensionPolicy is there either way
		if err := r.validate(pp, list.Items); err != nil {
			blocked[routeKey(pp)] = pp.Name + ": " + err.Error()
			continue
		}
		if pp.Spec.Platform != nil {
			id := projectID(pp.Spec.Platform)
			projects[id] = true
			platformRoutes[routeKey(pp)] = pricing.PlatformRoute{Project: id, NoPolicy: noPolicy(pp.Spec.Platform)}
			continue
		}
		rt, _ := routeOf(pp) // validate has called it
		routes[routeKey(pp)] = rt
	}

	table, err := tableJSON(routes, blocked, platformRoutes, pricing.KeysHTTPRoute, r.Networks)
	if err != nil {
		// Each route passed validate on its own, so this cannot happen; if it does, the
		// last good table stays in place and the error surfaces in the log.
		return fmt.Errorf("price table would not load: %w", err)
	}
	docs, err := r.policyDocuments(ctx, projects)
	if err != nil {
		return err
	}
	cm := desiredConfigMap(r.PricesNamespace, r.PricesName, table, docs)
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
	r.Log.Debug("projected", "routes", len(routes), "blocked", len(blocked), "namespaces", len(namespaces))
	return nil
}

// policyDocuments is the compiled policy of each named project, by ConfigMap key, as the
// Poller holds it, and the key directories of the origins those policies name, under
// pricing.AgentsKey. A project or origin the Poller has nothing for yet (just after a
// restart) keeps what is already in the ConfigMap, which is offered to the Poller as its
// starting point: dropping it would turn the route to noPolicy, or stop signatures
// verifying, until the first fetch.
func (r *Reconciler) policyDocuments(ctx context.Context, projects map[string]bool) (map[string]string, error) {
	docs := map[string]string{}
	if len(projects) == 0 || r.Poller == nil {
		return docs, nil
	}
	var current map[string]any
	stored := func() (map[string]any, error) {
		if current != nil {
			return current, nil
		}
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapGVK)
		err := r.Get(ctx, client.ObjectKey{Namespace: r.PricesNamespace, Name: r.PricesName}, cm)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		current, _, _ = unstructured.NestedMap(cm.Object, "data")
		if current == nil {
			current = map[string]any{}
		}
		return current, nil
	}
	for _, id := range slices.Sorted(maps.Keys(projects)) {
		st, _ := r.Poller.State(id)
		if st.Version == 0 {
			data, err := stored()
			if err != nil {
				return nil, err
			}
			if old, ok := data[pricing.PolicyKey(id)].(string); ok {
				r.Poller.Seed(id, []byte(old))
				st, _ = r.Poller.State(id)
			}
		}
		if st.Version > 0 {
			docs[pricing.PolicyKey(id)] = string(st.Body)
		}
	}

	agents := map[string]*webbotauth.Directory{}
	var seeded map[string]*webbotauth.Directory
	for _, o := range r.Poller.Origins() {
		st, _ := r.Poller.Directory(o)
		if st.Directory == nil {
			if seeded == nil {
				data, err := stored()
				if err != nil {
					return nil, err
				}
				seeded = map[string]*webbotauth.Directory{}
				if old, ok := data[pricing.AgentsKey].(string); ok {
					_ = json.Unmarshal([]byte(old), &seeded) // unreadable: nothing to seed
				}
			}
			r.Poller.SeedDirectory(o, seeded[o])
			st, _ = r.Poller.Directory(o)
		}
		if st.Directory != nil {
			agents[o] = st.Directory
		}
	}
	if len(agents) > 0 {
		b, err := json.Marshal(agents)
		if err != nil {
			return nil, err
		}
		docs[pricing.AgentsKey] = string(b)
	}
	return docs, nil
}
