package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/metrics"
	"github.com/amoghjay/x402_gateway/internal/platform"
	compiled "github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
)

// Poll pacing. The gate standard asks for every 30 to 60 s and never more than once a second.
const (
	DefaultPollInterval = 30 * time.Second
	minPollGap          = time.Second
	firstRetry          = 2 * time.Second // while no document is loaded, retry sooner, doubling up to the interval
)

// Poller keeps each Radius Platform project that a PaymentPolicy names at its latest valid
// compiled policy. It fetches; the Reconciler writes: when a project's state changes, the
// Poller enqueues the policies that name it, and the Reconciler puts the document in the
// price-table ConfigMap and the state in each policy's Synced condition. So status and the
// ConfigMap keep a single writer.
//
// A document replaces the one in force only when its version is greater (§3: an answer
// delivered out of order never rolls a policy back) and it validates; otherwise the loaded
// one stays, however long the Platform is unreachable. The Reconciler seeds the Poller from
// the ConfigMap, so a controller restart does not drop a policy while the first fetch runs.
//
// It runs only on the leader, like the Reconciler.
type Poller struct {
	BaseURL   string        // the Platform API; platform.DefaultBaseURL when empty
	Policies  client.Reader // PaymentPolicies, from the manager's cache
	Secrets   client.Reader // uncached: Secrets are read by name, never listed or watched
	Namespace string        // where credential Secrets live
	Fetcher   *platform.Client
	Interval  time.Duration // DefaultPollInterval when zero
	Log       *slog.Logger

	// DirectoryClient fetches Web Bot Auth key directories; NewDirectoryClient when nil.
	DirectoryClient *http.Client

	now    func() time.Time // tests
	events chan event.GenericEvent
	kick   chan struct{}

	mu       sync.Mutex
	projects map[string]*project   // by lower-case project ID
	dirs     map[string]*directory // by origin, for origins a loaded policy names
}

// project is one project's state.
type project struct {
	settings platformSettings
	version  uint64
	body     []byte
	warnings []string
	origins  []string // the signature_agent origins the loaded policy names
	err      string   // the last fetch's failure; "" when it worked
	reason   string   // ReasonFetchFailed or ReasonRefused with err
	tried    bool     // a fetch has finished, either way
	next     time.Time
	retry    time.Duration
}

// ProjectState is what the Reconciler reads about a project.
type ProjectState struct {
	Version  uint64 // 0: none loaded
	Body     []byte // the loaded document as served
	Warnings []string
	Err      string
	Reason   string // why Err: ReasonFetchFailed or ReasonRefused
	Tried    bool
}

// platformSettings is how a project is read. Every policy naming a project must agree on it.
type platformSettings struct {
	OrganizationID string
	SecretName     string
	SecretKey      string
}

// CredentialsLabel marks the Secrets a PaymentPolicy may name as Platform credentials. The
// label is the platform team's opt-in: without it, a policy could name any Secret in the
// controller's namespace (a settler's signing key, say) and have it sent to the Platform.
const CredentialsLabel = "x402.dev/platform-credentials" //nolint:gosec // a label name, not a credential

func settingsOf(p *v1alpha1.Platform) platformSettings {
	key := p.CredentialsSecretRef.Key
	if key == "" {
		key = "apiKey"
	}
	return platformSettings{OrganizationID: p.OrganizationID, SecretName: p.CredentialsSecretRef.Name, SecretKey: key}
}

func projectID(p *v1alpha1.Platform) string { return strings.ToLower(p.ProjectID) }

// NewPoller makes a Poller; set its fields before the manager starts.
func NewPoller() *Poller {
	return &Poller{
		now:      time.Now,
		events:   make(chan event.GenericEvent, 64),
		kick:     make(chan struct{}, 1),
		projects: map[string]*project{},
		dirs:     map[string]*directory{},
	}
}

// Source is the event source the Reconciler watches for project changes.
func (p *Poller) Source() source.Source {
	return source.Channel(p.events, &handler.EnqueueRequestForObject{})
}

// Kick asks for a poll now, for a project the Poller has not seen yet.
func (p *Poller) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// State returns a project's state; ok is false for a project the Poller does not know.
func (p *Poller) State(id string) (ProjectState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.projects[strings.ToLower(id)]
	if !ok {
		return ProjectState{}, false
	}
	return ProjectState{Version: pr.version, Body: pr.body, Warnings: pr.warnings, Err: pr.err, Reason: pr.reason, Tried: pr.tried}, true
}

// Seed offers a document read back from the ConfigMap. It is taken only when it is valid,
// for this project, and newer than what the Poller holds, so a restart resumes at the version
// the filter was enforcing and polls with If-None-Match from there.
func (p *Poller) Seed(id string, body []byte) {
	pol, err := compiled.Parse(body)
	if err != nil || !strings.EqualFold(pol.ProjectID, id) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.projects[strings.ToLower(id)]
	if pr == nil {
		pr = &project{}
		p.projects[strings.ToLower(id)] = pr
	}
	if pol.Version > pr.version {
		pr.version, pr.body, pr.warnings, pr.origins = pol.Version, body, warnings(pol), signatureOrigins(pol)
	}
}

// NeedLeaderElection: only the leader polls.
func (p *Poller) NeedLeaderElection() bool { return true }

// Start polls until ctx is done.
func (p *Poller) Start(ctx context.Context) error {
	if p.Interval <= 0 {
		p.Interval = DefaultPollInterval
	}
	p.Log.Info("polling Radius Platform projects", "interval", p.Interval, "schemas", compiled.Schemas)
	tick := time.NewTicker(minPollGap)
	defer tick.Stop()
	for {
		p.pollDue(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-p.kick:
		}
	}
}

// pollDue fetches every project that is due, one at a time: there are few, and a slow
// Platform should not open a connection per project at once.
func (p *Poller) pollDue(ctx context.Context) {
	var list v1alpha1.PaymentPolicyList
	if err := p.Policies.List(ctx, &list); err != nil {
		p.Log.Error("list PaymentPolicies", "err", err)
		return
	}
	named := platformProjects(list.Items)

	p.mu.Lock()
	for id := range p.projects {
		if _, ok := named[id]; !ok {
			delete(p.projects, id) // no policy names it any more
		}
	}
	now := p.now()
	var due []string
	for id, ref := range named {
		pr := p.projects[id]
		if pr == nil {
			pr = &project{}
			p.projects[id] = pr
		}
		if pr.settings != ref.settings {
			pr.settings, pr.next = ref.settings, time.Time{} // changed settings: fetch now
		}
		if !now.Before(pr.next) {
			due = append(due, id)
		}
	}
	p.mu.Unlock()

	slices.Sort(due)
	for _, id := range due {
		if ctx.Err() != nil {
			return
		}
		if p.poll(ctx, id, named[id].settings) {
			p.notify(named[id].policies)
		}
	}
	if p.pollDirectories(ctx) {
		// Keys changed: every platform policy's Reconcile rewrites the ConfigMap and the
		// status, and there are few.
		for _, ref := range named {
			p.notify(ref.policies)
		}
	}
}

// poll fetches one project and reports whether its state changed.
func (p *Poller) poll(ctx context.Context, id string, s platformSettings) bool {
	p.mu.Lock()
	have := p.projects[id].version
	p.mu.Unlock()

	var (
		res    platform.Result
		err    error
		reason = v1alpha1.ReasonFetchFailed
	)
	key, err := p.apiKey(ctx, s)
	if err == nil {
		res, err = p.Fetcher.Fetch(ctx, platform.Project{BaseURL: p.BaseURL, OrganizationID: s.OrganizationID, ProjectID: id}, key, have)
	}
	var refused *platform.RefusedError
	if errors.As(err, &refused) {
		reason = v1alpha1.ReasonRefused
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.projects[id]
	if !ok {
		return false // dropped while fetching
	}
	before := fmt.Sprint(pr.version, pr.err, pr.tried)
	pr.tried = true
	log := p.Log.With("project", id)
	switch {
	case err != nil:
		metrics.PlatformFetches.WithLabelValues("error").Inc()
		pr.err, pr.reason = err.Error(), reason
		log.Error("compiled policy fetch failed; keeping the policy in force", "err", err, "version", pr.version)
	case res.NotModified:
		metrics.PlatformFetches.WithLabelValues("unchanged").Inc()
		pr.err, pr.reason = "", ""
		log.Debug("compiled policy unchanged", "version", pr.version)
	case res.Policy.Version > pr.version:
		metrics.PlatformFetches.WithLabelValues("new_version").Inc()
		pr.version, pr.body, pr.warnings, pr.origins = res.Policy.Version, res.Body, warnings(res.Policy), signatureOrigins(res.Policy)
		pr.err, pr.reason = "", ""
		log.Info("compiled policy loaded", "version", pr.version, "rules", len(res.Policy.Rules), "schema", res.Policy.Schema, "warnings", pr.warnings)
	default:
		metrics.PlatformFetches.WithLabelValues("unchanged").Inc()
		pr.err, pr.reason = "", ""
		if res.Policy.Version < pr.version {
			log.Warn("compiled policy ignored: older than the one in force", "offered", res.Policy.Version, "version", pr.version)
		}
	}
	pr.next = p.now().Add(p.wait(pr))
	return fmt.Sprint(pr.version, pr.err, pr.tried) != before
}

// wait is the time to the project's next fetch: the interval ±10 %, or, while nothing is
// loaded, a retry from 2 s doubling up to the interval.
func (p *Poller) wait(pr *project) time.Duration {
	if pr.version == 0 && pr.err != "" {
		if pr.retry == 0 {
			pr.retry = firstRetry
		} else {
			pr.retry = min(2*pr.retry, p.Interval)
		}
		return max(pr.retry, minPollGap)
	}
	pr.retry = 0
	return max(time.Duration(float64(p.Interval)*(0.9+0.2*rand.Float64())), minPollGap) //nolint:gosec // poll jitter, not a secret
}

func (p *Poller) apiKey(ctx context.Context, s platformSettings) (string, error) {
	var sec corev1.Secret
	if err := p.Secrets.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: s.SecretName}, &sec); err != nil {
		return "", fmt.Errorf("credentials Secret %s/%s: %w", p.Namespace, s.SecretName, err)
	}
	if sec.Labels[CredentialsLabel] != "true" {
		return "", fmt.Errorf("credentials Secret %s/%s is not labelled %s: \"true\", so it is not used as Platform credentials", p.Namespace, s.SecretName, CredentialsLabel)
	}
	key := strings.TrimSpace(string(sec.Data[s.SecretKey]))
	if key == "" {
		return "", fmt.Errorf("credentials Secret %s/%s has no %q key", p.Namespace, s.SecretName, s.SecretKey)
	}
	return key, nil
}

// notify enqueues each policy naming a project whose state changed. It never blocks the
// poll loop: the channel is buffered, and the Reconciler also runs on every policy event.
func (p *Poller) notify(policies []client.ObjectKey) {
	for _, k := range policies {
		pp := &v1alpha1.PaymentPolicy{}
		pp.Namespace, pp.Name = k.Namespace, k.Name
		select {
		case p.events <- event.GenericEvent{Object: pp}:
		default:
			p.Log.Warn("reconcile queue full; the policy catches up on its next event", "policy", k.String())
		}
	}
}

// platformRef is how one project is named across the policies that name it.
type platformRef struct {
	settings platformSettings
	owner    client.ObjectKey   // the policy whose settings win: the first by namespace/name
	policies []client.ObjectKey // every policy naming the project with those settings
}

// platformProjects groups the platform policies by project. When two policies name one
// project with different settings, the first by namespace/name wins and the other is in
// conflict (platformConflict); it is left out here, so it never steers the fetch.
func platformProjects(items []v1alpha1.PaymentPolicy) map[string]*platformRef {
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b v1alpha1.PaymentPolicy) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	out := map[string]*platformRef{}
	for i := range sorted {
		pp := &sorted[i]
		if pp.Spec.Platform == nil {
			continue
		}
		id, s, k := projectID(pp.Spec.Platform), settingsOf(pp.Spec.Platform), client.ObjectKeyFromObject(pp)
		ref, ok := out[id]
		if !ok {
			out[id] = &platformRef{settings: s, owner: k, policies: []client.ObjectKey{k}}
			continue
		}
		if ref.settings == s {
			ref.policies = append(ref.policies, k)
		}
	}
	return out
}

// platformConflict says why a platform policy cannot be used because another policy names
// the same project differently; "" when it can.
func platformConflict(pp *v1alpha1.PaymentPolicy, items []v1alpha1.PaymentPolicy) string {
	if pp.Spec.Platform == nil {
		return ""
	}
	ref := platformProjects(items)[projectID(pp.Spec.Platform)]
	if ref == nil || ref.settings == settingsOf(pp.Spec.Platform) {
		return ""
	}
	return fmt.Sprintf("project %s is already read with other settings (organization or credentials) by PaymentPolicy %s; policies naming one project must agree",
		pp.Spec.Platform.ProjectID, ref.owner)
}

// warnings lists what a loaded document asks for that this gate does not do, so the operator
// sees it on the policy's status rather than finding out from traffic.
func warnings(pol *compiled.Policy) []string {
	var category, origin []string
	for _, r := range pol.Rules {
		a := r.Match.Agent
		if a == nil {
			continue
		}
		if len(a.Category) > 0 {
			category = append(category, r.ID)
		}
		// A rule's origin is compared as written with the origin a signature verified for,
		// which is always in the normal form: lower-case https, no default port, no path.
		for _, o := range a.SignatureAgent {
			if n, err := webbotauth.Origin(webbotauth.Item{Value: o}); err != nil || n != o {
				origin = append(origin, fmt.Sprintf("%s (%q)", r.ID, o))
			}
		}
	}
	var out []string
	if len(category) > 0 {
		out = append(out, "rules matching an agent category never match on this gate: "+strings.Join(category, ", "))
	}
	if len(origin) > 0 {
		out = append(out, "signature_agent values that are not a plain https origin never match: "+strings.Join(origin, ", "))
	}
	if _, skipped := pricing.SignatureOrigins(pol); len(skipped) > 0 {
		out = append(out, fmt.Sprintf("more than %d signature agents; never matched: %s", pricing.MaxSignatureOrigins, strings.Join(skipped, ", ")))
	}
	return out
}
