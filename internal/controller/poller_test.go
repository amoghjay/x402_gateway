package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/platform"
	compiled "github.com/amoghjay/x402_gateway/internal/policy"
)

const (
	testProject = "00000000-0000-4000-8000-0000000000aa" // the project in ../policy/testdata/three_options.json
	testOrg     = "org_test"
	testKey     = "sk_test_connector"
)

// fakePlatform serves a compiled policy for testProject and lets a test change it.
type fakePlatform struct {
	*httptest.Server
	mu          sync.Mutex
	doc         []byte
	status      int // non-zero: answer this instead
	ifNoneMatch []string
}

func newFakePlatform(t *testing.T) *fakePlatform {
	f := &fakePlatform{doc: docVersion(t, 2)}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ifNoneMatch = append(f.ifNoneMatch, r.Header.Get("If-None-Match"))
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"bad key"}`))
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		var head struct {
			Version uint64 `json:"version"`
		}
		_ = json.Unmarshal(f.doc, &head)
		if r.Header.Get("If-None-Match") == strconv.Quote(strconv.FormatUint(head.Version, 10)) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(f.doc)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePlatform) set(doc []byte, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doc, f.status = doc, status
}

func (f *fakePlatform) lastIfNoneMatch() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ifNoneMatch) == 0 {
		return "<none>"
	}
	return f.ifNoneMatch[len(f.ifNoneMatch)-1]
}

func docVersion(t *testing.T, v int) []byte {
	t.Helper()
	b, err := os.ReadFile("../policy/testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.Replace(string(b), `"version": 2`, `"version": `+strconv.Itoa(v), 1))
}

func platformPolicy(ns, name, secret string) *v1alpha1.PaymentPolicy {
	return &v1alpha1.PaymentPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.PaymentPolicySpec{
			TargetRef: v1alpha1.TargetRef{Name: name},
			Platform: &v1alpha1.Platform{
				OrganizationID:       testOrg,
				ProjectID:            testProject,
				CredentialsSecretRef: v1alpha1.SecretKeyRef{Name: secret},
			},
		},
	}
}

type pollerTest struct {
	p     *Poller
	c     client.Client
	clock time.Time
}

func newPollerTest(t *testing.T, f *fakePlatform, objs ...client.Object) *pollerTest {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "x402-system", Name: "radius-platform", Labels: map[string]string{CredentialsLabel: "true"}},
		Data:       map[string][]byte{"apiKey": []byte(testKey + "\n")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, secret)...).Build()
	pt := &pollerTest{c: c, clock: time.Unix(1_760_000_000, 0)}
	p := NewPoller()
	p.Policies, p.Secrets, p.Namespace, p.BaseURL = c, c, "x402-system", f.URL
	p.Fetcher = &platform.Client{HTTP: f.Client(), UserAgent: "x402-envoy-gate/test"}
	p.Interval = 30 * time.Second
	p.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 4}))
	p.now = func() time.Time { return pt.clock }
	pt.p = p
	return pt
}

// step advances the clock and runs one pass of the poll loop; it returns the policies
// enqueued for reconciling.
func (pt *pollerTest) step(d time.Duration) []string {
	pt.clock = pt.clock.Add(d)
	pt.p.pollDue(context.Background())
	var out []string
	for {
		select {
		case e := <-pt.p.events:
			out = append(out, e.Object.GetNamespace()+"/"+e.Object.GetName())
		default:
			return out
		}
	}
}

func (pt *pollerTest) state(t *testing.T) ProjectState {
	t.Helper()
	st, ok := pt.p.State(strings.ToUpper(testProject)) // any case finds it
	if !ok {
		t.Fatal("project unknown")
	}
	return st
}

func TestPollerLoadsAndKeepsTheBestVersion(t *testing.T) {
	f := newFakePlatform(t)
	pt := newPollerTest(t, f, platformPolicy("demo", "a", "radius-platform"), platformPolicy("shop", "b", "radius-platform"))

	if got := pt.step(0); strings.Join(got, ",") != "demo/a,shop/b" {
		t.Fatalf("enqueued %v after the first load", got)
	}
	st := pt.state(t)
	if st.Version != 2 || st.Err != "" || !st.Tried || string(st.Body) != string(f.doc) {
		t.Fatalf("after the first fetch: %+v", st)
	}
	if got := f.lastIfNoneMatch(); got != "" {
		t.Errorf("first fetch sent If-None-Match %q", got)
	}

	// Not due yet: no fetch.
	n := len(f.ifNoneMatch)
	pt.step(20 * time.Second)
	if len(f.ifNoneMatch) != n {
		t.Error("fetched before the interval")
	}
	// Due: 304, nothing changes, nobody is enqueued.
	if got := pt.step(15 * time.Second); len(got) != 0 {
		t.Errorf("enqueued %v on a 304", got)
	}
	if got := f.lastIfNoneMatch(); got != `"2"` {
		t.Errorf("If-None-Match %q, want \"2\"", got)
	}

	f.set(docVersion(t, 3), 0)
	if got := pt.step(40 * time.Second); len(got) != 2 || pt.state(t).Version != 3 {
		t.Fatalf("v3: enqueued %v, state %+v", got, pt.state(t))
	}

	// An older document (an out-of-order answer) never rolls back.
	f.set(docVersion(t, 1), 0)
	pt.step(40 * time.Second)
	if st := pt.state(t); st.Version != 3 || st.Err != "" {
		t.Fatalf("after an older document: %+v", st)
	}

	// An invalid newer document is refused; v3 stays.
	f.set([]byte(strings.Replace(string(docVersion(t, 4)), `"type": "deny"`, `"type": "redirect"`, 1)), 0)
	if got := pt.step(40 * time.Second); len(got) != 2 {
		t.Errorf("enqueued %v on a refusal", got)
	}
	st = pt.state(t)
	if st.Version != 3 || st.Reason != v1alpha1.ReasonRefused || !strings.Contains(st.Err, "version 4 refused") {
		t.Fatalf("after an invalid v4: %+v", st)
	}

	// The Platform is down; v3 stays, the error says so.
	f.set(docVersion(t, 4), http.StatusServiceUnavailable)
	pt.step(40 * time.Second)
	if st := pt.state(t); st.Version != 3 || st.Reason != v1alpha1.ReasonFetchFailed || !strings.Contains(st.Err, "503") {
		t.Fatalf("Platform down: %+v", st)
	}

	// Back up with a valid v4.
	f.set(docVersion(t, 4), 0)
	pt.step(40 * time.Second)
	if st := pt.state(t); st.Version != 4 || st.Err != "" {
		t.Fatalf("recovered: %+v", st)
	}
}

func TestPollerWithoutCredentials(t *testing.T) {
	f := newFakePlatform(t)
	pt := newPollerTest(t, f, platformPolicy("demo", "a", "missing"))
	pt.step(0)
	st := pt.state(t)
	if st.Version != 0 || st.Reason != v1alpha1.ReasonFetchFailed || !strings.Contains(st.Err, "x402-system/missing") {
		t.Fatalf("no Secret: %+v", st)
	}
	if strings.Contains(st.Err, testKey) {
		t.Error("an error names the key")
	}
	// Nothing loaded: retries after 2 s, then 4 s, not after the 30 s interval.
	n := len(f.ifNoneMatch)
	pt.step(2 * time.Second)
	pt.step(2 * time.Second)
	pt.step(2 * time.Second)
	if len(f.ifNoneMatch) != n {
		t.Error("the Platform was called without a key")
	}

	// A Secret appears, but without the label: still not read.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "x402-system", Name: "missing"},
		Data:       map[string][]byte{"apiKey": []byte(testKey)},
	}
	if err := pt.c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	pt.step(8 * time.Second)
	if st := pt.state(t); st.Version != 0 || !strings.Contains(st.Err, "is not labelled") {
		t.Fatalf("unlabelled Secret: %+v", st)
	}
	if len(f.ifNoneMatch) != n {
		t.Error("an unlabelled Secret's value was sent to the Platform")
	}

	// Labelled: the next retry loads.
	sec.Labels = map[string]string{CredentialsLabel: "true"}
	if err := pt.c.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	pt.step(16 * time.Second)
	if st := pt.state(t); st.Version != 2 || st.Err != "" {
		t.Fatalf("after the Secret appeared: %+v", st)
	}
}

func TestPollerSeedAndForget(t *testing.T) {
	f := newFakePlatform(t)
	pp := platformPolicy("demo", "a", "radius-platform")
	pt := newPollerTest(t, f, pp)

	pt.p.Seed(testProject, []byte("not json"))
	pt.p.Seed("00000000-0000-4000-8000-0000000000bb", docVersion(t, 2)) // another project's document
	if _, ok := pt.p.State("00000000-0000-4000-8000-0000000000bb"); ok {
		t.Error("a document was seeded under the wrong project")
	}
	pt.p.Seed(testProject, docVersion(t, 2))
	if st := pt.state(t); st.Version != 2 || st.Tried {
		t.Fatalf("seeded: %+v", st)
	}
	pt.step(0)
	if got := f.lastIfNoneMatch(); got != `"2"` {
		t.Errorf("the first fetch after a seed sent If-None-Match %q", got)
	}

	if err := pt.c.Delete(context.Background(), pp); err != nil {
		t.Fatal(err)
	}
	pt.step(time.Minute)
	if _, ok := pt.p.State(testProject); ok {
		t.Error("a project no policy names is still held")
	}
}

func TestPlatformConflict(t *testing.T) {
	a := platformPolicy("demo", "a", "radius-platform")
	b := platformPolicy("shop", "b", "other-secret")
	c := platformPolicy("shop", "c", "radius-platform")
	c.Spec.Platform.ProjectID = strings.ToUpper(testProject) // same project, other case
	items := []v1alpha1.PaymentPolicy{*b, *c, *a}
	if why := platformConflict(a, items); why != "" {
		t.Errorf("the first policy is in conflict: %s", why)
	}
	if why := platformConflict(c, items); why != "" {
		t.Errorf("a policy agreeing with the first is in conflict: %s", why)
	}
	if why := platformConflict(b, items); !strings.Contains(why, "demo/a") {
		t.Errorf("conflict = %q, want it to name demo/a", why)
	}
	ref := platformProjects(items)[testProject]
	if len(ref.policies) != 2 {
		t.Errorf("policies reading the project: %v", ref.policies)
	}
}

func TestWarnings(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(docVersion(t, 2), &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema"] = "radius.policy/0.2"
	rule := doc["rules"].([]any)[1].(map[string]any) // get-anything-platform-free/allow
	rule["match"].(map[string]any)["agent"] = map[string]any{
		"category": []any{"ai_crawler"}, "signature_agent": []any{"https://agent.example"},
	}
	b, _ := json.Marshal(doc)
	pol, err := compiled.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	// Signature agents are verified (Web Bot Auth); only categories never match.
	w := warnings(pol)
	if len(w) != 1 || !strings.Contains(w[0], "category") || !strings.Contains(w[0], "get-anything-platform-free/allow") {
		t.Errorf("warnings %q", w)
	}
	pol, _ = compiled.Parse(docVersion(t, 2))
	if w := warnings(pol); len(w) != 0 {
		t.Errorf("warnings for a policy without agent rules: %q", w)
	}
}
