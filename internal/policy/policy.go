// Package policy reads and evaluates a Radius Platform compiled policy: one document per
// project, an ordered list of rules, where the first rule whose match holds decides a request
// (allow, deny, or require payment), after counting it against the rule's rate limit and free
// quota.
//
// It is a Go port of the reference evaluator published under the MIT licence in
// github.com/radiustechsystems/radius-payment-gateway (vendor/radius-platform/crates/policy,
// Copyright (c) 2026 Radius Technology Systems). The conformance vectors in testdata/vectors
// come from the same repository and pin the behaviour; see testdata/vectors/SOURCE.md.
//
// The package is pure: no network, no clock, no store. Select finds the rule and says which
// counters to add the request to; the caller increments them and passes the new values to
// Decide.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// The format versions this package accepts. A document with any other schema is refused whole.
const (
	Schema01 = "radius.policy/0.1"
	Schema02 = "radius.policy/0.2"
)

// Schemas lists the accepted format versions, for the README and the startup log line.
var Schemas = []string{Schema01, Schema02}

// Closed sets. A document with a value outside one of them is refused: a rule the gate cannot
// enforce changes what every later rule means, so skipping it is not an option.
const (
	Allow = "allow"
	Deny  = "deny"

	ActionAllow  = "allow"
	ActionDeny   = "deny"
	ActionCharge = "charge"
	actionX402   = "x402" // the charge action's former tag, read as charge

	KeyIP     = "ip"
	KeyCaller = "caller"
	KeyGlobal = "global"
	KeyAgent  = "agent"

	CounterWithin = "within"
	CounterOver   = "over"

	SchemeExact = "exact"

	remoteAddr = "remote_addr"
)

// Methods a non-empty method list may name. A request with any other method (HEAD, OPTIONS)
// never matches a rule that lists methods.
var methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

var agentCategories = map[string]bool{
	"academic_research": true, "accessibility": true, "advertising_and_marketing": true,
	"aggregator": true, "ai_assistant": true, "ai_crawler": true, "ai_search": true,
	"archiver": true, "feed_fetcher": true, "monitoring_and_analytics": true,
	"page_preview": true, "search_engine_crawler": true, "search_engine_optimization": true,
	"security": true, "social_media_marketing": true, "webhooks": true, "other": true,
}

// Policy is a validated compiled policy. Fields the package does not know are ignored, as the
// gate standard requires of a gate that is released separately from the platform.
type Policy struct {
	Schema      string   `json:"schema"`
	Version     uint64   `json:"version"`
	ProjectID   string   `json:"project_id"`
	GeneratedAt string   `json:"generated_at"`
	Mode        string   `json:"mode"`
	Defaults    Defaults `json:"defaults"`
	Rules       []Rule   `json:"rules"`
}

// Defaults is what applies when no rule decides, and how the gate behaves around the rules.
type Defaults struct {
	Unmatched string   `json:"unmatched"`
	Failure   Failure  `json:"failure"`
	ClientIP  ClientIP `json:"client_ip"`
	Headers   Headers  `json:"headers"`
}

// Failure says how a count reads when the counter store fails. no_policy is not read from the
// document: it applies only while no document was ever loaded, so it is gate configuration.
type Failure struct {
	CounterError string `json:"counter_error"`
}

// Headers switches the response headers a decision carries.
type Headers struct {
	RateLimit  bool `json:"ratelimit"`
	RetryAfter bool `json:"retry_after"`
}

// ClientIP is where the client's address comes from: the connection (Header empty), or the
// first entry of a header a trusted proxy sets.
type ClientIP struct {
	Header string // lower case; empty means the connection's address
}

// UnmarshalJSON reads "remote_addr" or {"header": "<name>"}.
func (c *ClientIP) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != remoteAddr {
			return fmt.Errorf("client_ip %q is not a source this gate knows", s)
		}
		c.Header = ""
		return nil
	}
	var h struct {
		Header string `json:"header"`
	}
	if err := json.Unmarshal(b, &h); err != nil || strings.TrimSpace(h.Header) == "" {
		return errors.New(`client_ip must be "remote_addr" or {"header": "<name>"}`)
	}
	c.Header = strings.ToLower(strings.TrimSpace(h.Header))
	return nil
}

// Rule is one entry of the document's ordered rules.
type Rule struct {
	UID       string `json:"uid"`
	ID        string `json:"id"`
	Match     Match  `json:"match"`
	RateLimit *Limit `json:"rate_limit"`
	FreeQuota *Limit `json:"free_quota"`
	Action    Action `json:"action"`
}

// Match says which requests a rule is for. Fields present are ANDed; an empty Match holds for
// every request.
type Match struct {
	Endpoint *Endpoint   `json:"endpoint"`
	IPIn     []string    `json:"ip_in"`
	Agent    *AgentMatch `json:"agent"`

	nets []netip.Prefix // IPIn, parsed, with IPv4-mapped ranges as IPv4
}

// Endpoint matches on method and path.
type Endpoint struct {
	Slug    string   `json:"slug"`
	Methods []string `json:"methods"` // empty means any method
	Path    string   `json:"path"`

	segments []segment
}

// AgentMatch matches on what the gate verified about the request's agent.
type AgentMatch struct {
	Category       []string `json:"category"`
	SignatureAgent []string `json:"signature_agent"`
}

// Limit is a number of requests per fixed window.
type Limit struct {
	Key     string `json:"key"`
	Quota   uint64 `json:"quota"`
	WindowS uint64 `json:"window_s"`
}

// Action is what a rule does. Type is allow, deny or charge after validation.
type Action struct {
	Type        string   `json:"type"`
	Status      int      `json:"status"`
	Code        string   `json:"code"`
	Price       string   `json:"price"`
	Description string   `json:"description"`
	Accepts     []Option `json:"accepts"`
}

// Option is one resolved way to pay a charge. Raw is the option exactly as the document wrote
// it, so nothing the package does not model (a new field of `extra`, say) is lost.
type Option struct {
	Scheme      string         `json:"scheme"`
	Network     string         `json:"network"`
	Asset       string         `json:"asset"`
	Decimals    int            `json:"decimals"`
	Amount      string         `json:"amount"`
	PayTo       string         `json:"pay_to"`
	Facilitator *string        `json:"facilitator"`
	Extra       map[string]any `json:"extra"`

	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the option's own bytes beside the decoded fields.
func (o *Option) UnmarshalJSON(b []byte) error {
	type plain Option
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*o = Option(p)
	o.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON writes the option as the document wrote it.
func (o Option) MarshalJSON() ([]byte, error) {
	if len(o.Raw) > 0 {
		return o.Raw, nil
	}
	type plain Option
	return json.Marshal(plain(o))
}

type segmentKind int

const (
	literal segmentKind = iota
	param
	wildcard
)

type segment struct {
	kind segmentKind
	text string
}

// Parse reads and validates a compiled policy. Any problem refuses the whole document: the
// caller keeps the policy it had and reports the error.
func Parse(data []byte) (*Policy, error) {
	// The schema is checked first and on its own, so a document of a version this gate does not
	// list is refused for that reason and not for whatever field changed shape in it.
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("the policy is not JSON: %w", err)
	}
	if head.Schema != Schema01 && head.Schema != Schema02 {
		return nil, fmt.Errorf("schema %q is not one this gate accepts (%s)", head.Schema, strings.Join(Schemas, ", "))
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("the policy does not parse: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Policy) validate() error {
	if p.Version < 1 {
		return errors.New("version must be at least 1")
	}
	switch p.Defaults.Unmatched {
	case Allow, Deny:
	default:
		return fmt.Errorf("defaults.unmatched %q is not allow or deny", p.Defaults.Unmatched)
	}
	switch p.Defaults.Failure.CounterError {
	case "":
		p.Defaults.Failure.CounterError = CounterWithin
	case CounterWithin, CounterOver:
	default:
		return fmt.Errorf("defaults.failure.counter_error %q is not within or over", p.Defaults.Failure.CounterError)
	}
	ids := make(map[string]bool, len(p.Rules))
	for i := range p.Rules {
		r := &p.Rules[i]
		if err := r.validate(); err != nil {
			return fmt.Errorf("rules[%d] (%s): %w", i, r.ID, err)
		}
		if ids[r.ID] {
			return fmt.Errorf("rules[%d]: id %q appears twice", i, r.ID)
		}
		ids[r.ID] = true
	}
	return nil
}

func (r *Rule) validate() error {
	if r.ID == "" {
		return errors.New("id is empty")
	}
	uid, err := canonicalUUID(r.UID)
	if err != nil {
		return fmt.Errorf("uid: %w", err)
	}
	r.UID = uid
	if err := r.Match.validate(); err != nil {
		return fmt.Errorf("match: %w", err)
	}
	for name, l := range map[string]*Limit{"rate_limit": r.RateLimit, "free_quota": r.FreeQuota} {
		if l == nil {
			continue
		}
		switch l.Key {
		case KeyIP, KeyCaller, KeyGlobal, KeyAgent:
		default:
			return fmt.Errorf("%s.key %q is not a key this gate knows", name, l.Key)
		}
		if l.Quota < 1 {
			return fmt.Errorf("%s.quota must be at least 1", name)
		}
	}
	return r.Action.validate()
}

func (m *Match) validate() error {
	if e := m.Endpoint; e != nil {
		for i, method := range e.Methods {
			up := strings.ToUpper(strings.TrimSpace(method))
			if !contains(methods, up) {
				return fmt.Errorf("endpoint.methods: %q is not a method this gate knows", method)
			}
			e.Methods[i] = up
		}
		segs, err := parsePattern(e.Path)
		if err != nil {
			return fmt.Errorf("endpoint.path %q: %w", e.Path, err)
		}
		e.segments = segs
	}
	m.nets = m.nets[:0]
	for _, cidr := range m.IPIn {
		n, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil || n.Addr().Zone() != "" {
			return fmt.Errorf("ip_in: %q is not an address range", cidr)
		}
		m.nets = append(m.nets, canonicalNet(n))
	}
	if a := m.Agent; a != nil {
		if len(a.Category) == 0 && len(a.SignatureAgent) == 0 {
			return errors.New("agent: needs a category or a signature_agent")
		}
		for _, c := range a.Category {
			if !agentCategories[c] {
				return fmt.Errorf("agent.category: %q is not a category this gate knows", c)
			}
		}
		for _, o := range a.SignatureAgent {
			if o == "" {
				return errors.New("agent.signature_agent: an origin is empty")
			}
		}
	}
	return nil
}

func (a *Action) validate() error {
	switch a.Type {
	case ActionAllow:
	case ActionDeny:
		if a.Status < 400 || a.Status > 599 {
			return fmt.Errorf("action.status %d is not an error status", a.Status)
		}
		if a.Code == "" {
			return errors.New("action.code is empty")
		}
	case ActionCharge, actionX402:
		a.Type = ActionCharge
		if len(a.Accepts) == 0 {
			return errors.New("action.accepts is empty: there is no way to pay this charge")
		}
		for i := range a.Accepts {
			if err := a.Accepts[i].validate(); err != nil {
				return fmt.Errorf("action.accepts[%d]: %w", i, err)
			}
		}
	default:
		return fmt.Errorf("action.type %q is not an action this gate knows", a.Type)
	}
	return nil
}

func (o *Option) validate() error {
	if o.Scheme != SchemeExact {
		return fmt.Errorf("scheme %q is not a scheme this gate knows", o.Scheme)
	}
	if o.Network == "" {
		return errors.New("network is empty")
	}
	if !isAddress(o.Asset) {
		return fmt.Errorf("asset %q is not an address", o.Asset)
	}
	if !isAddress(o.PayTo) {
		return fmt.Errorf("pay_to %q is not an address", o.PayTo)
	}
	if !isAmount(o.Amount) {
		return fmt.Errorf("amount %q is not a positive whole number", o.Amount)
	}
	// A null facilitator is reserved for networks without a default one; the compiler refuses
	// them today, so meeting one means a document this gate does not understand.
	if o.Facilitator == nil {
		return errors.New("facilitator is null")
	}
	u, err := url.Parse(*o.Facilitator)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("facilitator %q is not an https URL", *o.Facilitator)
	}
	return nil
}

// parsePattern reads a route: literal segments, `:name` for one segment, and a trailing `*` for
// the rest of the path. "/" is the root, with no segments.
func parsePattern(path string) ([]segment, error) {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return nil, errors.New("must start with /")
	}
	if rest == "" {
		return nil, nil
	}
	parts := strings.Split(rest, "/")
	segs := make([]segment, 0, len(parts))
	for i, part := range parts {
		switch {
		case part == "":
			return nil, errors.New("has an empty segment")
		case part == "*":
			if i != len(parts)-1 {
				return nil, errors.New("* is only allowed as the last segment")
			}
			segs = append(segs, segment{kind: wildcard})
		case strings.HasPrefix(part, ":"):
			if len(part) == 1 {
				return nil, errors.New("has a parameter without a name")
			}
			segs = append(segs, segment{kind: param, text: part[1:]})
		default:
			segs = append(segs, segment{kind: literal, text: part})
		}
	}
	return segs, nil
}

// canonicalNet reads an IPv4-mapped IPv6 range (::ffff:192.0.2.0/120) as the IPv4 range it
// covers, so mapped and plain addresses compare the same way.
func canonicalNet(n netip.Prefix) netip.Prefix {
	n = n.Masked()
	if n.Addr().Is4In6() && n.Bits() >= 96 {
		return netip.PrefixFrom(n.Addr().Unmap(), n.Bits()-96)
	}
	return n
}

// canonicalUUID checks the 8-4-4-4-12 form and lower-cases it, since the uid is part of every
// counter key and two spellings of one rule must share their counters.
func canonicalUUID(s string) (string, error) {
	if len(s) != 36 {
		return "", fmt.Errorf("%q is not a UUID", s)
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", fmt.Errorf("%q is not a UUID", s)
			}
		default:
			if !isHex(c) {
				return "", fmt.Errorf("%q is not a UUID", s)
			}
		}
	}
	return strings.ToLower(s), nil
}

func isAddress(s string) bool {
	if len(s) != 42 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return false
	}
	for _, c := range []byte(s[2:]) {
		if !isHex(c) {
			return false
		}
	}
	return true
}

func isAmount(s string) bool {
	if s == "" || len(s) > 78 {
		return false
	}
	nonZero := false
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
		nonZero = nonZero || c != '0'
	}
	return nonZero
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
