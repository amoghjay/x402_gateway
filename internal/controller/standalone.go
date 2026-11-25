package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/amoghjay/x402_gateway/internal/platform"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
)

// Standalone keeps a directory in step with one Radius Platform project, with no cluster:
// `x402-controller sync`. It runs the same Poller as the cluster controller, with three
// seams swapped: the project comes from its own settings rather than PaymentPolicies, the
// API key from a file rather than a Secret, and a change rewrites the directory rather than
// the price-table ConfigMap.
//
// The directory gets the files a filter with -prices-dir reads: policy-<project>.json (the
// document as served), agents.json (the key directories its signature_agent rules name)
// and prices.json (each route the project governs). Each is written to a temporary file
// and renamed, policy and agents before prices, so the filter never reads half a file or a
// route before its policy; a file whose content did not change is left alone.
//
// One Standalone per directory: nothing arbitrates between two.
type Standalone struct {
	Dir            string
	OrganizationID string
	ProjectID      string
	KeyFile        string   // the Platform API key; re-read on every fetch, so a rotated key needs no restart
	Routes         []string // the Envoy route names the project governs
	NoPolicy       string   // what those routes answer before any policy was loaded: "deny" (default) or "allow"
	Poller         *Poller  // from NewPoller, with BaseURL, Fetcher, Interval and Log set; Run sets the seams
	Log            *slog.Logger

	changed chan struct{}
}

// Run seeds from the directory, writes it, then polls and rewrites on every change until
// ctx is done.
func (s *Standalone) Run(ctx context.Context) error {
	id, err := s.setup()
	if err != nil {
		return err
	}
	s.seed(id)
	if err := s.write(id); err != nil {
		s.Log.Error("writing the table directory", "dir", s.Dir, "err", err)
	}
	go func() { _ = s.Poller.Start(ctx) }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.changed:
			if err := s.write(id); err != nil {
				s.Log.Error("writing the table directory; the files in place stay in force", "dir", s.Dir, "err", err)
			}
		}
	}
}

// setup checks the settings and points the Poller's seams at this Standalone. It returns
// the project ID as the Poller keys it.
func (s *Standalone) setup() (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	id := strings.ToLower(s.ProjectID)
	settings := platformSettings{OrganizationID: s.OrganizationID}
	s.changed = make(chan struct{}, 1)
	p := s.Poller
	p.listProjects = func(context.Context) (map[string]*platformRef, error) {
		return map[string]*platformRef{id: {settings: settings}}, nil
	}
	p.readKey = func(context.Context, platformSettings) (string, error) { return s.readKey() }
	p.onChange = func() {
		select {
		case s.changed <- struct{}{}:
		default: // a rewrite is already due; it reads the latest state
		}
	}
	return id, nil
}

func (s *Standalone) check() error {
	if s.Poller == nil || s.Poller.projects == nil {
		return errors.New("Standalone.Poller must come from NewPoller")
	}
	if s.Dir == "" || s.OrganizationID == "" || s.ProjectID == "" || s.KeyFile == "" {
		return errors.New("sync needs --out, --organization, --project and --key-file")
	}
	if len(s.Routes) == 0 {
		return errors.New("sync needs at least one --route: the Envoy route names the project governs")
	}
	for _, r := range s.Routes {
		if _, ok := pricing.KeysName.Key(r); !ok {
			return fmt.Errorf("--route %q: not a route name the filter can key on (1 to 128 of A-Z a-z 0-9 . _ : / -)", r)
		}
	}
	switch s.NoPolicy {
	case "":
		s.NoPolicy = "deny"
	case "deny", "allow":
	default:
		return fmt.Errorf("--no-policy %q: want deny or allow", s.NoPolicy)
	}
	if _, err := (platform.Project{BaseURL: s.Poller.BaseURL, OrganizationID: s.OrganizationID, ProjectID: s.ProjectID}).URL(); err != nil {
		return err
	}
	if _, err := s.readKey(); err != nil {
		return err
	}
	return os.MkdirAll(s.Dir, 0o750)
}

func (s *Standalone) readKey() (string, error) {
	b, err := os.ReadFile(filepath.Clean(s.KeyFile))
	if err != nil {
		return "", fmt.Errorf("--key-file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("--key-file %s is empty", s.KeyFile)
	}
	return key, nil
}

// seed offers the Poller what the directory already holds, so a restart keeps enforcing
// the version in force while the first fetch runs, as the controller seeds from the
// ConfigMap.
func (s *Standalone) seed(id string) {
	if b, err := os.ReadFile(filepath.Join(s.Dir, pricing.PolicyKey(id))); err == nil {
		s.Poller.Seed(id, b)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, pricing.AgentsKey)); err == nil {
		var dirs map[string]*webbotauth.Directory
		if json.Unmarshal(b, &dirs) == nil {
			for _, o := range s.Poller.Origins() {
				s.Poller.SeedDirectory(o, dirs[o])
			}
		}
	}
	if st, _ := s.Poller.State(id); st.Version > 0 {
		s.Log.Info("resuming from the directory", "dir", s.Dir, "project", id, "policyVersion", st.Version)
	}
}

// write puts the Poller's state in the directory: policy, agents, then prices.
func (s *Standalone) write(id string) error {
	if st, _ := s.Poller.State(id); st.Version > 0 {
		if err := s.put(pricing.PolicyKey(id), st.Body); err != nil {
			return err
		}
	}
	agents := map[string]*webbotauth.Directory{}
	for _, o := range s.Poller.Origins() {
		if st, _ := s.Poller.Directory(o); st.Directory != nil {
			agents[o] = st.Directory
		}
	}
	if len(agents) > 0 {
		b, err := json.Marshal(agents)
		if err != nil {
			return err
		}
		if err := s.put(pricing.AgentsKey, b); err != nil {
			return err
		}
	}
	routes := make(map[string]pricing.PlatformRoute, len(s.Routes))
	for _, r := range s.Routes {
		routes[r] = pricing.PlatformRoute{Project: id, NoPolicy: s.NoPolicy}
	}
	table, err := tableJSON(nil, nil, routes, pricing.KeysName, nil)
	if err != nil {
		return err
	}
	return s.put(pricing.ConfigMapKey, table)
}

// put replaces one file atomically, and only when its content changed.
func (s *Standalone) put(name string, content []byte) error {
	path := filepath.Join(s.Dir, name)
	if old, err := os.ReadFile(filepath.Clean(path)); err == nil && bytes.Equal(old, content) {
		return nil
	}
	tmp, err := os.CreateTemp(s.Dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename; cleans up after a failure
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec // the filter, another user in compose, reads it; a policy is not a secret
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	s.Log.Info("wrote", "file", path, "bytes", len(content))
	return nil
}
