package controller

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/platform"
	"github.com/amoghjay/x402_gateway/internal/pricing"
)

type syncTest struct {
	t     *testing.T
	f     *fakePlatform
	s     *Standalone
	id    string
	clock time.Time
}

func newSyncTest(t *testing.T, f *fakePlatform, dir string) *syncTest {
	t.Helper()
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &syncTest{t: t, f: f, clock: time.Unix(1_760_000_000, 0)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := NewPoller()
	p.BaseURL, p.Interval, p.Log = f.URL, 30*time.Second, log
	p.Fetcher = &platform.Client{HTTP: f.Client(), UserAgent: "x402-envoy-gate/test"}
	p.now = func() time.Time { return st.clock }
	st.s = &Standalone{Dir: dir, OrganizationID: testOrg, ProjectID: testProject, KeyFile: key, Routes: []string{"api", "search"}, Poller: p, Log: log}
	id, err := st.s.setup()
	if err != nil {
		t.Fatal(err)
	}
	st.id = id
	return st
}

// step is one turn of Run's loop: a poll when one is due, then a rewrite if it changed.
func (st *syncTest) step() {
	st.t.Helper()
	st.s.Poller.pollDue(st.t.Context())
	select {
	case <-st.s.changed:
		if err := st.s.write(st.id); err != nil {
			st.t.Fatal(err)
		}
	default:
	}
	st.clock = st.clock.Add(time.Minute) // the next poll is due
}

// enforced is what a filter reading the directory would enforce on route "api".
func (st *syncTest) enforced() (version uint64, noPolicy string) {
	st.t.Helper()
	data, err := pricing.ReadDir(st.s.Dir)
	if err != nil {
		st.t.Fatal(err)
	}
	p, err := pricing.Build(data, nil, pricing.KeysName, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if p == nil || err != nil {
		st.t.Fatalf("the directory does not load: %v", err)
	}
	r, ok := p.Platform("api")
	if !ok {
		st.t.Fatal("route api is not governed by the project")
	}
	if pol := p.Policy(r.Project); pol != nil {
		version = pol.Version
	}
	return version, r.NoPolicy
}

func (st *syncTest) file(name string) os.FileInfo {
	st.t.Helper()
	fi, err := os.Stat(filepath.Join(st.s.Dir, name))
	if err != nil {
		st.t.Fatal(err)
	}
	return fi
}

func TestStandaloneSync(t *testing.T) {
	f := newFakePlatform(t)
	st := newSyncTest(t, f, t.TempDir())

	st.step()
	if v, np := st.enforced(); v != 2 || np != "deny" {
		t.Fatalf("after the first fetch: version %d noPolicy %q, want 2 deny", v, np)
	}
	if _, err := os.Stat(filepath.Join(st.s.Dir, pricing.AgentsKey)); !os.IsNotExist(err) {
		t.Errorf("agents.json written for a policy that names no signature agents: %v", err)
	}
	prices := st.file(pricing.ConfigMapKey)

	// A new version: the policy file is replaced, the table is left alone.
	f.set(docVersion(t, 3), 0)
	st.step()
	if v, _ := st.enforced(); v != 3 {
		t.Errorf("after version 3: enforced %d", v)
	}
	if !os.SameFile(prices, st.file(pricing.ConfigMapKey)) {
		t.Error("prices.json was rewritten though it did not change")
	}

	// Platform down, then an older version offered: version 3 stays.
	f.set(nil, http.StatusServiceUnavailable)
	st.step()
	f.set(docVersion(t, 2), 0)
	st.step()
	if v, _ := st.enforced(); v != 3 {
		t.Errorf("after an outage and an older version: enforced %d, want 3", v)
	}

	// The key file is read on every fetch: a wrong key fails, the right one works again.
	if err := os.WriteFile(st.s.KeyFile, []byte("sk_wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.set(docVersion(t, 4), 0)
	st.step()
	if v, _ := st.enforced(); v != 3 {
		t.Errorf("with a wrong key: enforced %d, want 3 kept", v)
	}
	if err := os.WriteFile(st.s.KeyFile, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	st.step()
	if v, _ := st.enforced(); v != 4 {
		t.Errorf("with the key fixed: enforced %d, want 4", v)
	}

	// No temporary files are left behind.
	entries, _ := os.ReadDir(st.s.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

// A restart resumes at the version in the directory, even with the Platform down.
func TestStandaloneResumesFromTheDirectory(t *testing.T) {
	f := newFakePlatform(t)
	dir := t.TempDir()
	first := newSyncTest(t, f, dir)
	first.step()

	f.set(nil, http.StatusServiceUnavailable)
	again := newSyncTest(t, f, dir)
	again.s.seed(again.id)
	if err := again.s.write(again.id); err != nil {
		t.Fatal(err)
	}
	again.step()
	if v, _ := again.enforced(); v != 2 {
		t.Errorf("after a restart with the Platform down: enforced %d, want 2", v)
	}
	if st, _ := again.s.Poller.State(again.id); st.Version != 2 {
		t.Errorf("the Poller resumed at version %d, want 2", st.Version)
	}
}

// Before any policy, the routes are listed with noPolicy, so the filter answers per it.
func TestStandaloneWithoutAPolicy(t *testing.T) {
	f := newFakePlatform(t)
	f.set(nil, http.StatusServiceUnavailable)
	st := newSyncTest(t, f, t.TempDir())
	st.s.NoPolicy = "allow"
	if err := st.s.write(st.id); err != nil {
		t.Fatal(err)
	}
	if v, np := st.enforced(); v != 0 || np != "allow" {
		t.Errorf("no policy yet: version %d noPolicy %q, want 0 allow", v, np)
	}
}

func TestStandaloneSettings(t *testing.T) {
	f := newFakePlatform(t)
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Standalone){
		"no route":         func(s *Standalone) { s.Routes = nil },
		"bad route name":   func(s *Standalone) { s.Routes = []string{"has space"} },
		"bad no-policy":    func(s *Standalone) { s.NoPolicy = "maybe" },
		"empty key file":   func(s *Standalone) { s.KeyFile = empty },
		"missing key file": func(s *Standalone) { s.KeyFile = filepath.Join(t.TempDir(), "none") },
		"no project":       func(s *Standalone) { s.ProjectID = "" },
	} {
		st := newSyncTest(t, f, t.TempDir())
		mod(st.s)
		if err := st.s.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
