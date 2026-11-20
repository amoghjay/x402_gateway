package policy

import (
	"net/netip"
	"strconv"
	"strings"
)

// Request is what the evaluator knows about a request.
type Request struct {
	Method string
	// Path as the gate received it. A query string and a fragment are ignored; nothing else is
	// normalised here.
	Path string
	// ClientIP resolved per defaults.client_ip (see ClientIP). The zero Addr means unknown.
	ClientIP netip.Addr
	// Caller is who the seller's own authentication verified the caller as; empty for nobody.
	Caller string
	// Agent is what the gate verified about the request's agent; nil for browsers and for
	// agents it could not verify.
	Agent *Agent
	// Now is Unix time in seconds.
	Now uint64
}

// Agent holds the facts a gate verified. An empty field was not verified.
type Agent struct {
	Category       string
	SignatureAgent string // the origin of the key directory the signature verified against
}

// Decision types.
const (
	TypeAllow           = "allow"
	TypeDeny            = "deny"
	TypePaymentRequired = "payment_required"
)

// Decision is what to do with a request.
type Decision struct {
	// Rule is the deciding rule's id; empty when no rule matched or there is no policy.
	Rule string
	Type string
	// Status and Code are set for a denial.
	Status int
	Code   string
	// Price, Description and Accepts are set when payment is required.
	Price       string
	Description string
	Accepts     []Option
	// Headers are response headers to add: RateLimit-* and Retry-After.
	Headers map[string]string
}

// Limit kinds, as the conformance vectors name them.
const (
	RateLimit = "rate_limit"
	FreeQuota = "free_quota"
)

// Counter is one counter to add the request to.
type Counter struct {
	Limit string // RateLimit or FreeQuota
	// Key is <uid>:<rate|free>:<window index>:<key>, where the key is ip:<address>,
	// ip:<IPv6 /64>, ip:unknown, caller:<id>, global, agent:sig:<origin> or agent:cat:<category>.
	Key string
	// ResetS is the seconds until the window ends: the counter's expiry and Retry-After.
	ResetS uint64
}

// Count is a request to count before deciding.
type Count struct {
	Rule *Rule
	// Counters holds the rate limit's counter, then the free quota's, for the limits the rule has.
	Counters []Counter
}

// Reading is a counter's value after adding the request, or the store's failure.
type Reading struct {
	Count  uint64
	Failed bool
}

// The fixed denials.
const (
	codeUnmatched   = "no_matching_rule"
	codeNoPolicy    = "policy_unavailable"
	codeRateLimited = "rate_limited"
)

// NoPolicy is the decision while no policy has ever been loaded. The verdict comes from the
// gate's configuration, since there is no document to read it from.
func NoPolicy(verdict string) Decision {
	if verdict == Deny {
		return Decision{Type: TypeDeny, Status: 503, Code: codeNoPolicy}
	}
	return Decision{Type: TypeAllow}
}

// ResolveClientIP resolves the client's address per defaults.client_ip: the connection's address, or
// the first entry of the named header. header looks a request header up by its lower-case
// name. A missing or unparseable header gives no address: falling back to the connection would
// let a client pick its own counter by leaving the header out.
func ResolveClientIP(p *Policy, header func(name string) (string, bool), remote netip.Addr) netip.Addr {
	name := p.Defaults.ClientIP.Header
	if name == "" {
		return remote
	}
	v, ok := header(name)
	if !ok {
		return netip.Addr{}
	}
	first, _, _ := strings.Cut(v, ",")
	return ParseIP(first)
}

// ParseIP reads an address as proxies write it in a header: 203.0.113.5, 203.0.113.5:4711,
// 2001:db8::1, [2001:db8::1] or [2001:db8::1]:443, ignoring surrounding whitespace. Anything
// else is the zero Addr.
func ParseIP(text string) netip.Addr {
	text = strings.TrimSpace(text)
	if a, err := netip.ParseAddr(text); err == nil {
		return noZone(a)
	}
	if ap, err := netip.ParseAddrPort(text); err == nil {
		return noZone(ap.Addr())
	}
	if inner, ok := strings.CutPrefix(text, "["); ok {
		if inner, ok = strings.CutSuffix(inner, "]"); ok {
			if a, err := netip.ParseAddr(inner); err == nil && a.Is6() {
				return noZone(a)
			}
		}
	}
	return netip.Addr{}
}

func noZone(a netip.Addr) netip.Addr {
	if a.Zone() != "" {
		return netip.Addr{}
	}
	return a
}

// Select finds the rule that applies: the first whose match holds. A rule without a rate limit
// or a free quota decides at once, and Select returns the decision. Otherwise it returns the
// counters to add the request to; the caller increments each (expiring after its ResetS) and
// calls Decide with the new values.
func Select(p *Policy, r Request) (*Decision, *Count) {
	var rule *Rule
	for i := range p.Rules {
		if p.Rules[i].Match.holds(r) {
			rule = &p.Rules[i]
			break
		}
	}
	if rule == nil {
		if p.Defaults.Unmatched == Deny {
			return &Decision{Type: TypeDeny, Status: 403, Code: codeUnmatched}, nil
		}
		return &Decision{Type: TypeAllow}, nil
	}
	var counters []Counter
	for _, l := range rule.limits() {
		var part string
		switch l.limit.Key {
		case KeyCaller:
			if r.Caller != "" {
				part = "caller:" + r.Caller
			} else {
				part = ipPart(r)
			}
		case KeyGlobal:
			part = "global"
		case KeyAgent:
			part = agentPart(r)
		default:
			part = ipPart(r)
		}
		// Compiled policies never have an empty window; a hand-written one counts per second.
		window := max(l.limit.WindowS, 1)
		counters = append(counters, Counter{
			Limit:  l.kind,
			Key:    rule.UID + ":" + l.tag + ":" + strconv.FormatUint(r.Now/window, 10) + ":" + part,
			ResetS: window - r.Now%window,
		})
	}
	if len(counters) == 0 {
		d := rule.apply()
		return &d, nil
	}
	return nil, &Count{Rule: rule, Counters: counters}
}

// Decide decides a counted request. readings holds each counter's value after adding this
// request, in the order of Count.Counters; a missing reading is a failed counter. Past the rate
// limit the request is denied with 429 rate_limited; otherwise within the free quota it is
// allowed, and beyond it the rule's action applies. A failed counter reads per
// defaults.failure.counter_error: within its limit, or beyond it.
func Decide(p *Policy, c *Count, readings []Reading) Decision {
	rule := c.Rule
	overOnError := p.Defaults.Failure.CounterError == CounterOver

	type checked struct {
		limit   *Limit
		counter Counter
		count   uint64
		ok      bool // the counter was read
		over    bool
	}
	var rate, free *checked
	for i, l := range rule.limits() {
		if i >= len(c.Counters) {
			break
		}
		ch := &checked{limit: l.limit, counter: c.Counters[i]}
		if i < len(readings) && !readings[i].Failed {
			ch.count, ch.ok = readings[i].Count, true
			ch.over = ch.count > l.limit.Quota
		} else {
			ch.over = overOnError
		}
		if l.kind == RateLimit {
			rate = ch
		} else {
			free = ch
		}
	}

	var d Decision
	var deciding *checked
	switch {
	case rate != nil && rate.over:
		d, deciding = Decision{Rule: rule.ID, Type: TypeDeny, Status: 429, Code: codeRateLimited}, rate
	case free != nil && !free.over:
		d, deciding = Decision{Rule: rule.ID, Type: TypeAllow}, free
	default:
		d, deciding = rule.apply(), free
	}

	// The rate limit's headers when the rule has one, else the free quota's; none from a failed
	// counter, and no falling back from a failed rate counter to the free quota's.
	shown := rate
	if shown == nil {
		shown = free
	}
	if p.Defaults.Headers.RateLimit && shown != nil && shown.ok {
		remaining := uint64(0)
		if shown.limit.Quota > shown.count {
			remaining = shown.limit.Quota - shown.count
		}
		d.header("RateLimit-Limit", shown.limit.Quota)
		d.header("RateLimit-Remaining", remaining)
		d.header("RateLimit-Reset", shown.counter.ResetS)
	}
	if p.Defaults.Headers.RetryAfter && d.Type == TypeDeny && deciding != nil && deciding.ok {
		d.header("Retry-After", deciding.counter.ResetS)
	}
	return d
}

func (d *Decision) header(name string, v uint64) {
	if d.Headers == nil {
		d.Headers = make(map[string]string, 4)
	}
	d.Headers[name] = strconv.FormatUint(v, 10)
}

type ruleLimit struct {
	kind, tag string
	limit     *Limit
}

// limits lists the rule's limits in the order they are checked: the rate limit, then the free
// quota.
func (r *Rule) limits() []ruleLimit {
	var out []ruleLimit
	if r.RateLimit != nil {
		out = append(out, ruleLimit{RateLimit, "rate", r.RateLimit})
	}
	if r.FreeQuota != nil {
		out = append(out, ruleLimit{FreeQuota, "free", r.FreeQuota})
	}
	return out
}

// apply is the rule's action as a decision.
func (r *Rule) apply() Decision {
	d := Decision{Rule: r.ID}
	switch r.Action.Type {
	case ActionDeny:
		d.Type, d.Status, d.Code = TypeDeny, r.Action.Status, r.Action.Code
	case ActionCharge:
		d.Type, d.Price, d.Description, d.Accepts = TypePaymentRequired, r.Action.Price, r.Action.Description, r.Action.Accepts
	default:
		d.Type = TypeAllow
	}
	return d
}

func (m *Match) holds(r Request) bool {
	if m.Endpoint != nil && !m.Endpoint.holds(r) {
		return false
	}
	if len(m.nets) > 0 {
		if !r.ClientIP.IsValid() {
			return false
		}
		ip, in := r.ClientIP.Unmap(), false
		for _, n := range m.nets {
			if n.Contains(ip) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if a := m.Agent; a != nil {
		var got Agent
		if r.Agent != nil {
			got = *r.Agent
		}
		// Every field the rule lists must hold, and a fact the gate did not verify never does.
		if len(a.Category) > 0 && (got.Category == "" || !contains(a.Category, got.Category)) {
			return false
		}
		if len(a.SignatureAgent) > 0 && (got.SignatureAgent == "" || !contains(a.SignatureAgent, got.SignatureAgent)) {
			return false
		}
	}
	return true
}

func (e *Endpoint) holds(r Request) bool {
	if len(e.Methods) > 0 {
		method, ok := strings.TrimSpace(r.Method), false
		for _, m := range e.Methods {
			if strings.EqualFold(m, method) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return pathMatches(e.segments, r.Path)
}

// pathMatches compares a request path with a route: literal segments exactly (case-sensitive,
// no percent-decoding), `:name` any one non-empty segment, and a trailing `*` the path above
// it and everything below, including nothing. A query string, a fragment and one trailing `/`
// are ignored; repeated slashes are not merged.
func pathMatches(segs []segment, path string) bool {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path, ok := strings.CutPrefix(path, "/")
	if !ok {
		return false
	}
	path = strings.TrimSuffix(path, "/")
	var parts []string
	if path != "" {
		parts = strings.Split(path, "/")
	}
	for i, s := range segs {
		if s.kind == wildcard {
			return true
		}
		if i >= len(parts) {
			return false
		}
		switch s.kind {
		case literal:
			if parts[i] != s.text {
				return false
			}
		case param:
			if parts[i] == "" {
				return false
			}
		}
	}
	return len(parts) == len(segs)
}

// ipPart is the counter key of the client's address. IPv4 clients count per address and IPv6
// clients per /64: a host usually gets a whole /64, so a counter per address would let one
// client spread its requests over 2^64 of them.
func ipPart(r Request) string {
	if !r.ClientIP.IsValid() {
		return "ip:unknown"
	}
	ip := r.ClientIP.Unmap()
	if ip.Is4() {
		return "ip:" + ip.String()
	}
	return "ip:" + netip.PrefixFrom(ip, 64).Masked().String()
}

// agentPart is what a request counts as under `key: agent`: its verified signature agent's
// origin, else its verified category, else its address.
func agentPart(r Request) string {
	if r.Agent != nil {
		if r.Agent.SignatureAgent != "" {
			return "agent:sig:" + r.Agent.SignatureAgent
		}
		if r.Agent.Category != "" {
			return "agent:cat:" + r.Agent.Category
		}
	}
	return ipPart(r)
}
