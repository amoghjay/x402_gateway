// Package platform fetches a project's compiled policy from the Radius Platform API.
//
// The gate standard (§3) has a gate read
//
//	GET {base}/organizations/{organization_id}/projects/{project_id}/policy/compiled
//
// with a Bearer credential, poll it, send If-None-Match with the version it enforces, and
// validate every document before using it. This package does one fetch and the validation;
// the controller's poller decides when to fetch and whether a new version replaces the one in
// force. Nothing here runs on the request path: the standard forbids a gate to call the
// platform while handling a request.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/policy"
)

// DefaultBaseURL is the production Platform API.
const DefaultBaseURL = "https://platform-api.radiustech.xyz"

// DefaultTimeout bounds one fetch, connection to last byte.
const DefaultTimeout = 10 * time.Second

// MaxDocument is the largest document accepted. The controller stores each document in a
// ConfigMap, whose whole data is capped at 1 MiB; a bigger policy cannot be delivered to the
// filter, so it is refused here with a clear reason rather than by the API server later.
const MaxDocument = 768 << 10

// InstanceHeader tells the Platform which gate instance fetched, so its gate status can tell
// replicas apart.
const InstanceHeader = "Radius-Gate-Instance"

// Project names one project's compiled policy.
type Project struct {
	BaseURL        string // the Platform API; DefaultBaseURL when empty
	OrganizationID string
	ProjectID      string
}

// URL is the compiled-policy URL of the project.
func (p Project) URL() (string, error) {
	base := p.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("platform API URL %q is not an http(s) URL without a query", base)
	}
	if p.OrganizationID == "" || p.ProjectID == "" {
		return "", errors.New("the organization ID and the project ID are both needed")
	}
	return u.String() + "/organizations/" + url.PathEscape(p.OrganizationID) +
		"/projects/" + url.PathEscape(p.ProjectID) + "/policy/compiled", nil
}

// Client fetches compiled policies.
type Client struct {
	HTTP      *http.Client // nil: a client with DefaultTimeout
	UserAgent string       // names this gate to the Platform, e.g. x402-envoy-gate/<version>
	Instance  string       // sent as Radius-Gate-Instance when set
}

// Result is a successful fetch: either nothing changed since the version the gate holds, or a
// new document, validated.
type Result struct {
	NotModified bool
	Policy      *policy.Policy // the validated document; nil when NotModified
	Body        []byte         // the document exactly as served
}

// Error is an answer from the Platform other than 200 or 304. Code and Message come from its
// JSON error body when there is one.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	s := "the platform answered " + strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// RefusedError is a document the Platform served that this gate will not enforce: not valid,
// another project's, or too large. The gate keeps the policy it has.
type RefusedError struct {
	Version uint64 // the refused document's version, when it could be read; 0 otherwise
	Err     error
}

func (e *RefusedError) Error() string {
	if e.Version > 0 {
		return fmt.Sprintf("policy version %d refused: %v", e.Version, e.Err)
	}
	return "policy refused: " + e.Err.Error()
}

func (e *RefusedError) Unwrap() error { return e.Err }

// Fetch reads the project's compiled policy with the given credential. have is the version
// the gate enforces (0 for none): it is sent as If-None-Match, so the Platform answers 304 when
// nothing changed and records which version this gate runs.
//
// A transport failure or a non-2xx answer returns an error (an *Error for the latter); a
// document that does not validate returns a *RefusedError. Either way the caller keeps the
// policy it has.
func (c *Client) Fetch(ctx context.Context, p Project, apiKey string, have uint64) (Result, error) {
	target, err := p.URL()
	if err != nil {
		return Result{}, err
	}
	if apiKey == "" {
		return Result{}, errors.New("the API key is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Instance != "" {
		req.Header.Set(InstanceHeader, c.Instance)
	}
	if have > 0 {
		req.Header.Set("If-None-Match", strconv.Quote(strconv.FormatUint(have, 10)))
	}

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The URL holds no secret, so the transport error may be returned as it is.
		return Result{}, fmt.Errorf("fetching the compiled policy: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return Result{NotModified: true}, nil
	case resp.StatusCode != http.StatusOK:
		return Result{}, readError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocument+1))
	if err != nil {
		return Result{}, fmt.Errorf("reading the compiled policy: %w", err)
	}
	if len(body) > MaxDocument {
		return Result{}, &RefusedError{Err: fmt.Errorf("the document is over %d KiB, too large to deliver to the filter", MaxDocument>>10)}
	}
	pol, err := policy.Parse(body)
	if err != nil {
		return Result{}, &RefusedError{Version: versionOf(body), Err: err}
	}
	if !strings.EqualFold(pol.ProjectID, p.ProjectID) {
		return Result{}, &RefusedError{Version: pol.Version, Err: fmt.Errorf("the document is for project %q, not %q", pol.ProjectID, p.ProjectID)}
	}
	return Result{Policy: pol, Body: body}, nil
}

// readError turns a non-2xx answer into an *Error, reading at most a few KiB of its body.
func readError(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body) == nil {
		e.Code, e.Message = clip(body.Code, 64), clip(body.Message, 256)
	}
	return e
}

// versionOf reads a document's version, if it has a readable one, for the refusal's message.
func versionOf(body []byte) uint64 {
	var head struct {
		Version uint64 `json:"version"`
	}
	_ = json.Unmarshal(body, &head)
	return head.Version
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
