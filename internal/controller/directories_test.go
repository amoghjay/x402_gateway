package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	compiled "github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
)

// The RFC 9421 B.1.4 test key's public half and thumbprint.
const (
	dirKeyX     = "JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs"
	dirKeyThumb = "poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U"
)

// fakeDirectory serves a key directory over TLS and counts its requests.
type fakeDirectory struct {
	*httptest.Server
	mu     sync.Mutex
	status int    // non-zero: answer this instead
	body   string // the directory
	cache  string // Cache-Control
	hits   int
}

func newFakeDirectory(t *testing.T) *fakeDirectory {
	t.Helper()
	f := &fakeDirectory{body: `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + dirKeyX + `"}]}`, cache: "max-age=600"}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits++
		if r.URL.Path != webbotauth.DirectoryPath {
			http.NotFound(w, r)
			return
		}
		switch f.status {
		case 0:
		case http.StatusFound:
			http.Redirect(w, r, "https://elsewhere.example/keys", http.StatusFound)
			return
		default:
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("Content-Type", "application/http-message-signatures-directory+json")
		w.Header().Set("Cache-Control", f.cache)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDirectory) set(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeDirectory) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// client is the test server's client with NewDirectoryClient's redirect rule. The address
// rule is tested on its own: the test server is on loopback.
func (f *fakeDirectory) client() *http.Client {
	c := f.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// agentDoc is the test document at version v with a signature_agent rule naming origins.
func agentDoc(t *testing.T, v int, origins ...string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(docVersion(t, v), &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema"] = "radius.policy/0.2"
	list := make([]any, len(origins))
	for i, o := range origins {
		list[i] = o
	}
	rule := doc["rules"].([]any)[1].(map[string]any) // get-anything-platform-free/allow
	rule["match"].(map[string]any)["agent"] = map[string]any{"signature_agent": list}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPollerFetchesNamedDirectories(t *testing.T) {
	fd := newFakeDirectory(t)
	other := newFakeDirectory(t) // a directory no policy names
	origin := fd.URL             // https://127.0.0.1:port
	fp := newFakePlatform(t)
	fp.set(agentDoc(t, 2, origin), 0)
	pt := newPollerTest(t, fp, platformPolicy("shop", "metered", "radius-platform"))
	pt.p.DirectoryClient = fd.client()

	// Enqueued for the document and again for its keys; a repeat reconcile is harmless.
	if got := pt.step(0); len(got) == 0 || got[len(got)-1] != "shop/metered" {
		t.Fatalf("enqueued %v after the first poll", got)
	}
	st, ok := pt.p.Directory(origin)
	if !ok || st.Err != "" || st.Directory == nil || len(st.Directory.Keys) != 1 || st.Directory.Keys[0].Thumbprint != dirKeyThumb {
		t.Fatalf("directory %+v %v", st, ok)
	}
	if o := pt.p.ProjectOrigins(testProject); len(o) != 1 || o[0] != origin {
		t.Fatalf("project origins %v", o)
	}
	if other.requests() != 0 {
		t.Fatal("fetched a directory the policy does not name")
	}

	// Kept for its max-age (10 minutes): no refetch before.
	pt.step(5 * time.Minute)
	if n := fd.requests(); n != 1 {
		t.Fatalf("%d fetches inside the max-age", n)
	}

	// A failed refresh keeps the keys in force, reports the failure, and retries in a minute.
	fd.set(http.StatusInternalServerError)
	if got := pt.step(6 * time.Minute); len(got) == 0 {
		t.Fatal("a failure enqueued nothing: the status would not say so")
	}
	st, _ = pt.p.Directory(origin)
	if st.Directory == nil || len(st.Directory.Keys) != 1 || !strings.Contains(st.Err, "500") {
		t.Fatalf("after a failure %+v", st)
	}
	pt.step(30 * time.Second)
	if n := fd.requests(); n != 2 {
		t.Fatalf("%d fetches; the retry is a minute after a failure", n)
	}

	// A redirect is a failure, not followed.
	fd.set(http.StatusFound)
	pt.step(2 * time.Minute)
	if st, _ = pt.p.Directory(origin); !strings.Contains(st.Err, "302") || st.Directory == nil {
		t.Fatalf("after a redirect %+v", st)
	}

	// Back: fresh keys, no error.
	fd.set(0)
	pt.step(5 * time.Minute)
	if st, _ = pt.p.Directory(origin); st.Err != "" || st.Directory == nil {
		t.Fatalf("recovered %+v", st)
	}

	// The policy stops naming the origin: its directory is dropped.
	fp.set(docVersion(t, 3), 0)
	pt.step(time.Hour)
	if _, ok := pt.p.Directory(origin); ok {
		t.Fatal("an origin no policy names is still held")
	}
}

func TestSeedDirectory(t *testing.T) {
	fp := newFakePlatform(t)
	pt := newPollerTest(t, fp, platformPolicy("shop", "metered", "radius-platform"))
	dir := &webbotauth.Directory{Keys: []webbotauth.Key{{Thumbprint: dirKeyThumb, X: dirKeyX}}}

	pt.p.SeedDirectory("https://agent.example", dir)
	if _, ok := pt.p.Directory("https://agent.example"); ok {
		t.Fatal("seeded an origin no loaded policy names")
	}
	pt.p.Seed(testProject, agentDoc(t, 2, "https://agent.example"))
	pt.p.SeedDirectory("https://agent.example", dir)
	if st, ok := pt.p.Directory("https://agent.example"); !ok || st.Directory != dir {
		t.Fatalf("seed not taken: %+v %v", st, ok)
	}
	pt.p.SeedDirectory("https://agent.example", &webbotauth.Directory{Keys: []webbotauth.Key{{X: "other"}}})
	if st, _ := pt.p.Directory("https://agent.example"); st.Directory != dir {
		t.Fatal("a seed replaced keys already held")
	}
}

func TestDirectoryTTL(t *testing.T) {
	for cc, want := range map[string]time.Duration{
		"":                          time.Hour,
		"no-store, no-transform":    time.Hour,
		"public, max-age=3600":      time.Hour,
		"max-age=86400":             time.Hour,
		"max-age=600":               10 * time.Minute,
		"max-age=10":                5 * time.Minute,
		`Max-Age="900", public`:     15 * time.Minute,
		"max-age=nonsense":          time.Hour,
		"s-maxage=60, max-age=1200": 20 * time.Minute,
	} {
		if got := directoryTTL(cc); got != want {
			t.Errorf("directoryTTL(%q) = %v, want %v", cc, got, want)
		}
	}
}

func TestRefusePrivate(t *testing.T) {
	for addr, refused := range map[string]bool{
		"127.0.0.1:443":         true,
		"10.1.2.3:443":          true,
		"172.16.0.1:443":        true,
		"192.168.1.1:443":       true,
		"169.254.169.254:80":    true, // cloud metadata
		"100.64.0.10:443":       true,
		"0.0.0.0:443":           true,
		"0.1.2.3:443":           true,
		"240.0.0.10:443":        true, // class E, a GKE Pod range option
		"198.18.0.1:443":        true,
		"192.0.0.8:443":         true,
		"[64:ff9b::a00:1]:443":  true, // NAT64 for 10.0.0.1
		"[::1]:443":             true,
		"[fd00::1]:443":         true,
		"[fe80::1]:443":         true,
		"[::ffff:10.0.0.1]:443": true,
		"8.8.8.8:443":           false,
		"[2001:4860::8888]:443": false,
	} {
		if err := refusePrivate("tcp", addr, nil); (err != nil) != refused {
			t.Errorf("refusePrivate(%s) = %v, want refused %v", addr, err, refused)
		}
	}
}

func TestSignatureOrigins(t *testing.T) {
	pol, err := compiled.Parse(agentDoc(t, 2, "https://agent.example", "https://Agent.example:443/", "http://plain.example"))
	if err != nil {
		t.Fatal(err)
	}
	if got := signatureOrigins(pol); len(got) != 1 || got[0] != "https://agent.example" {
		t.Fatalf("origins %v", got)
	}
	// The evaluator compares a rule's origin as written, so the two that are not in the form
	// a verified agent is reported in are called out.
	w := warnings(pol)
	if len(w) != 1 || !strings.Contains(w[0], `"https://Agent.example:443/"`) || !strings.Contains(w[0], `"http://plain.example"`) ||
		strings.Contains(w[0], `"https://agent.example"`) {
		t.Errorf("warnings %q", w)
	}
}
