package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	org     = "org_test"
	project = "00000000-0000-4000-8000-0000000000aa" // three_options.json's project
	key     = "sk_test_secret"
)

func document(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../policy/testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// platformServer serves the document at the standard's path and records what it was sent.
type platformServer struct {
	*httptest.Server
	doc  []byte
	last atomic.Pointer[http.Request]
}

func newServer(t *testing.T, doc []byte, handler func(w http.ResponseWriter, r *http.Request) bool) *platformServer {
	s := &platformServer{doc: doc}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.last.Store(r.Clone(context.Background()))
		if handler != nil && handler(w, r) {
			return
		}
		if r.URL.Path != "/organizations/"+org+"/projects/"+project+"/policy/compiled" {
			http.Error(w, `{"code":"not_found","message":"no such route"}`, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"unauthorized","message":"Invalid API key"}`))
			return
		}
		if r.Header.Get("If-None-Match") == `"2"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"2"`)
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.doc)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *platformServer) project() Project {
	return Project{BaseURL: s.URL + "/", OrganizationID: org, ProjectID: project}
}

func TestFetch(t *testing.T) {
	s := newServer(t, document(t), nil)
	c := &Client{UserAgent: "x402-envoy-gate/test", Instance: "controller-0"}

	res, err := c.Fetch(context.Background(), s.project(), key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.NotModified || res.Policy == nil || res.Policy.Version != 2 || len(res.Policy.Rules) != 5 {
		t.Fatalf("result %+v", res)
	}
	if string(res.Body) != string(s.doc) {
		t.Error("the body is not the document as served")
	}
	r := s.last.Load()
	if got := r.Header.Get("User-Agent"); got != "x402-envoy-gate/test" {
		t.Errorf("User-Agent %q", got)
	}
	if got := r.Header.Get(InstanceHeader); got != "controller-0" {
		t.Errorf("%s %q", InstanceHeader, got)
	}
	if got := r.Header.Get("If-None-Match"); got != "" {
		t.Errorf("If-None-Match %q sent with no version held", got)
	}

	res, err = c.Fetch(context.Background(), s.project(), key, 2)
	if err != nil || !res.NotModified || res.Policy != nil {
		t.Fatalf("with version 2 held: %+v, %v", res, err)
	}
	if got := s.last.Load().Header.Get("If-None-Match"); got != `"2"` {
		t.Errorf("If-None-Match %q, want \"2\"", got)
	}
}

func TestFetchErrors(t *testing.T) {
	s := newServer(t, document(t), func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/projects/down/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("<html>bad gateway</html>"))
			return true
		}
		return false
	})
	c := &Client{}

	_, err := c.Fetch(context.Background(), s.project(), "sk_wrong", 0)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 401 || pe.Code != "unauthorized" || pe.Message != "Invalid API key" {
		t.Fatalf("wrong key: %v", err)
	}
	if strings.Contains(err.Error(), "sk_wrong") {
		t.Error("the error names the key")
	}

	p := s.project()
	p.ProjectID = "down"
	if _, err = c.Fetch(context.Background(), p, key, 0); !errors.As(err, &pe) || pe.Status != 503 || pe.Code != "" {
		t.Fatalf("503 without a JSON body: %v", err)
	}

	if _, err = c.Fetch(context.Background(), s.project(), "", 0); err == nil {
		t.Error("an empty key was sent")
	}

	s.Close()
	if _, err = c.Fetch(context.Background(), s.project(), key, 0); err == nil || errors.As(err, &pe) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestFetchRefuses(t *testing.T) {
	good := string(document(t))
	cases := map[string]struct {
		doc     string
		version uint64
	}{
		"invalid":         {strings.Replace(good, `"type": "deny"`, `"type": "redirect"`, 1), 2},
		"unknown schema":  {strings.Replace(good, "radius.policy/0.1", "radius.policy/0.9", 1), 2},
		"another project": {strings.Replace(good, project, "00000000-0000-4000-8000-0000000000bb", 1), 2},
		"too large":       {strings.Replace(good, `"mode"`, `"padding": "`+strings.Repeat("x", MaxDocument)+`", "mode"`, 1), 0},
		"not JSON":        {"<html>", 0},
	}
	for name, c := range cases {
		s := newServer(t, []byte(c.doc), nil)
		_, err := (&Client{}).Fetch(context.Background(), s.project(), key, 0)
		var re *RefusedError
		if !errors.As(err, &re) {
			t.Errorf("%s: %v, want a refusal", name, err)
			continue
		}
		if re.Version != c.version {
			t.Errorf("%s: refused version %d, want %d", name, re.Version, c.version)
		}
	}
}

// The project ID matches whatever case the CRD was written in: UUIDs are case-insensitive.
func TestFetchProjectIDCase(t *testing.T) {
	doc := document(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(doc) }))
	defer srv.Close()
	p := Project{BaseURL: srv.URL, OrganizationID: org, ProjectID: strings.ToUpper(project)}
	if _, err := (&Client{}).Fetch(context.Background(), p, key, 0); err != nil {
		t.Fatal(err)
	}
}

func TestFetchTimeout(t *testing.T) {
	s := newServer(t, document(t), func(w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	})
	c := &Client{HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	start := time.Now()
	if _, err := c.Fetch(context.Background(), s.project(), key, 0); err == nil {
		t.Fatal("no error from a hung platform")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestURL(t *testing.T) {
	got, err := Project{OrganizationID: "org_1", ProjectID: project}.URL()
	if err != nil || got != DefaultBaseURL+"/organizations/org_1/projects/"+project+"/policy/compiled" {
		t.Errorf("default base: %q, %v", got, err)
	}
	got, _ = Project{BaseURL: "https://p.example/api/", OrganizationID: "o/1", ProjectID: "p"}.URL()
	if got != "https://p.example/api/organizations/o%2F1/projects/p/policy/compiled" {
		t.Errorf("escaped: %q", got)
	}
	for _, base := range []string{"ftp://x", "https://u:p@x", "https://x?q=1", "not a url"} {
		if _, err := (Project{BaseURL: base, OrganizationID: "o", ProjectID: "p"}).URL(); err == nil {
			t.Errorf("%q accepted", base)
		}
	}
	if _, err := (Project{OrganizationID: "o"}).URL(); err == nil {
		t.Error("an empty project ID accepted")
	}
}
