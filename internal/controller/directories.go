package controller

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	compiled "github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
)

// Web Bot Auth key directories (gate standard §4.1). The Poller fetches the directory of each
// origin a loaded policy names in a signature_agent rule, and only those; the Reconciler puts
// them in the price-table ConfigMap, where the filter reads them to verify signatures. The
// request path never fetches.
const (
	directoryTimeout  = 3 * time.Second
	directoryMaxBytes = 64 << 10
	directoryMinTTL   = 5 * time.Minute // the shortest a fetched directory is kept before refreshing
	directoryMaxTTL   = time.Hour       // and the longest; also what no-store or no max-age gets
	directoryRetry    = time.Minute     // after a failure, doubling up to directoryMinTTL
)

// directory is one origin's state.
type directory struct {
	dir     *webbotauth.Directory // the last good keys; nil until a fetch works
	fetched time.Time
	err     string // the last fetch's failure; "" when it worked
	next    time.Time
	retry   time.Duration
}

// DirectoryState is what the Reconciler reads about an origin.
type DirectoryState struct {
	Directory *webbotauth.Directory
	Err       string
}

// NewDirectoryClient is the HTTP client for directories: no redirects (a 3xx is a failure),
// a 3 s budget, and no connection to a loopback, private, link-local or otherwise
// non-public address, since a policy chooses the origins.
func NewDirectoryClient() *http.Client {
	dialer := &net.Dialer{Timeout: directoryTimeout, Control: refusePrivate}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   directoryTimeout,
		ResponseHeaderTimeout: directoryTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       directoryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// refusePrivate runs on the resolved address, so a public name that resolves inside the
// cluster is refused too.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := ap.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip) { // carrier-grade NAT, used by some clusters
		return fmt.Errorf("refusing to fetch a key directory from non-public address %s", ip)
	}
	return nil
}

// signatureOrigins lists the origins a policy names in signature_agent rules, normalised;
// a value that is not an https origin is skipped (it can never match a verified agent).
func signatureOrigins(pol *compiled.Policy) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range pol.Rules {
		if r.Match.Agent == nil {
			continue
		}
		for _, o := range r.Match.Agent.SignatureAgent {
			n, err := webbotauth.Origin(webbotauth.Item{Value: o})
			if err != nil || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// fetchDirectory GETs an origin's directory. It returns the keys and how long to keep them.
func (p *Poller) fetchDirectory(ctx context.Context, origin string) (*webbotauth.Directory, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+webbotauth.DirectoryPath, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/http-message-signatures-directory+json, application/json")
	if p.Fetcher != nil && p.Fetcher.UserAgent != "" {
		req.Header.Set("User-Agent", p.Fetcher.UserAgent)
	}
	if p.DirectoryClient == nil { // only the poll loop fetches, so no lock is needed
		p.DirectoryClient = NewDirectoryClient()
	}
	resp, err := p.DirectoryClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%s answered %d", req.URL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, directoryMaxBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > directoryMaxBytes {
		return nil, 0, fmt.Errorf("%s: directory larger than %d bytes", req.URL, directoryMaxBytes)
	}
	dir, err := webbotauth.ParseDirectory(body)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", req.URL, err)
	}
	return dir, directoryTTL(resp.Header.Get("Cache-Control")), nil
}

// directoryTTL keeps a directory for its max-age, between 5 minutes and an hour. no-store,
// no-cache or no max-age get the hour: keys must be held to verify at all.
func directoryTTL(cc string) time.Duration {
	for _, d := range strings.Split(cc, ",") {
		name, val, _ := strings.Cut(strings.TrimSpace(strings.ToLower(d)), "=")
		if name != "max-age" {
			continue
		}
		s, err := strconv.Atoi(strings.Trim(val, `"`))
		if err != nil || s < 0 {
			break
		}
		return min(max(time.Duration(s)*time.Second, directoryMinTTL), directoryMaxTTL)
	}
	return directoryMaxTTL
}

// pollDirectories fetches each named origin that is due, drops origins no policy names any
// more, and reports whether any origin's state changed.
func (p *Poller) pollDirectories(ctx context.Context) bool {
	p.mu.Lock()
	named := map[string]bool{}
	for _, pr := range p.projects {
		for _, o := range pr.origins {
			named[o] = true
		}
	}
	changed := false
	for o := range p.dirs {
		if !named[o] {
			delete(p.dirs, o)
			changed = true
		}
	}
	now := p.now()
	var due []string
	for o := range named {
		d := p.dirs[o]
		if d == nil {
			d = &directory{}
			p.dirs[o] = d
		}
		if !now.Before(d.next) {
			due = append(due, o)
		}
	}
	p.mu.Unlock()

	for _, o := range due {
		if ctx.Err() != nil {
			return changed
		}
		dir, ttl, err := p.fetchDirectory(ctx, o)
		p.mu.Lock()
		d, ok := p.dirs[o]
		if ok {
			before := directoryFingerprint(d)
			now := p.now()
			if err != nil {
				// A failed fetch keeps the keys in force (WG draft §6.10) and retries sooner.
				d.err = err.Error()
				d.retry = min(max(2*d.retry, directoryRetry), directoryMinTTL)
				d.next = now.Add(d.retry)
				p.Log.Error("key directory fetch failed; keeping the keys in force", "origin", o, "err", err, "keys", keyCount(d.dir))
			} else {
				d.dir, d.fetched, d.err, d.retry, d.next = dir, now, "", 0, now.Add(ttl)
				p.Log.Debug("key directory fetched", "origin", o, "keys", len(dir.Keys), "ttl", ttl)
			}
			changed = changed || directoryFingerprint(d) != before
		}
		p.mu.Unlock()
	}
	return changed
}

func keyCount(d *webbotauth.Directory) int {
	if d == nil {
		return 0
	}
	return len(d.Keys)
}

// directoryFingerprint changes when what the Reconciler writes or reports changes.
func directoryFingerprint(d *directory) string {
	var b strings.Builder
	b.WriteString(d.err)
	if d.dir != nil {
		for _, k := range d.dir.Keys {
			fmt.Fprintf(&b, "|%s %s %s %d %d", k.Kid, k.Thumbprint, k.X, k.NotBefore, k.Expires)
		}
	}
	return b.String()
}

// Directory returns an origin's state; ok is false for an origin no loaded policy names.
func (p *Poller) Directory(origin string) (DirectoryState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.dirs[origin]
	if !ok {
		return DirectoryState{}, false
	}
	return DirectoryState{Directory: d.dir, Err: d.err}, true
}

// SeedDirectory offers keys read back from the ConfigMap after a restart, so signatures keep
// verifying while the first fetch runs. Taken only for an origin a loaded policy names, when
// the Poller has no keys for it yet.
func (p *Poller) SeedDirectory(origin string, dir *webbotauth.Directory) {
	if dir == nil || len(dir.Keys) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.dirs[origin]
	if d == nil {
		if !p.namedLocked(origin) {
			return
		}
		d = &directory{}
		p.dirs[origin] = d
	}
	if d.dir == nil {
		d.dir = dir
	}
}

// Origins lists every origin a loaded policy names, sorted.
func (p *Poller) Origins() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := map[string]bool{}
	for _, pr := range p.projects {
		for _, o := range pr.origins {
			set[o] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// ProjectOrigins lists the origins one project's loaded policy names.
func (p *Poller) ProjectOrigins(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, ok := p.projects[strings.ToLower(id)]; ok {
		return slices.Clone(pr.origins)
	}
	return nil
}

func (p *Poller) namedLocked(origin string) bool {
	for _, pr := range p.projects {
		if slices.Contains(pr.origins, origin) {
			return true
		}
	}
	return false
}
