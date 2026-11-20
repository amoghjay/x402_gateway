package extproc

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/amoghjay/x402_gateway/internal/counter"
	"github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// A route governed by a Radius Platform project is decided by its compiled policy (gate
// standard §2, §4, §5): select the first matching rule, count the request against the rule's
// limits in Redis (shared by every filter replica), decide, respond.
//
// The path the policy is evaluated on is Envoy's :path, which Envoy has already normalized
// (dot segments removed, slashes merged) before routing on it. That is the path the backend
// receives, so a rule cannot be dodged with /free/../paid. The standard says to compare the
// path as the client sent it; this is a stated deviation.

// Decisions on a platform route.
const (
	RuleAllowed       Decision = "rule_allowed"       // continue: the deciding rule, or the policy's default, allows it
	RuleDenied        Decision = "rule_denied"        // the rule's status and {"code"}: a deny action, a rate limit, or no matching rule
	PolicyUnavailable Decision = "policy_unavailable" // 503: no compiled policy was ever loaded and noPolicy is deny
	ChargeNotBuilt    Decision = "charge_not_built"   // 503: the rule asks for payment; paid platform rules come with the next filter version
)

// sourceAddressAttr is the downstream peer's address ("ip:port"), which client_ip:
// remote_addr means. The controller asks Envoy for it on platform routes.
const sourceAddressAttr = "source.address"

// countTimeout bounds the counter round trip. The request-path Redis client gives up on a
// read after 50 ms; this is the whole budget, retry included.
const countTimeout = 150 * time.Millisecond

// WithCounters wires the rate-limit and free-quota counters. Without them every counter
// reads as failed, and the policy's counter_error decides.
func (s *Server) WithCounters(c *counter.Store) *Server {
	s.counters = c
	return s
}

// platformRequest decides a request on a platform route.
func (s *Server) platformRequest(ctx context.Context, req *extprocv3.ProcessingRequest, h map[string]string, st *request, table *pricing.Prices, pr pricing.PlatformRoute) *extprocv3.ProcessingResponse {
	st.log = st.log.With("project", pr.Project)
	pol := table.Policy(pr.Project)
	if pol == nil {
		// Never loaded: the controller has not fetched it yet, or it never validated.
		d := policy.NoPolicy(pr.NoPolicy)
		st.log = st.log.With("noPolicy", pr.NoPolicy)
		return s.respondRule(st, d, 0)
	}

	remote := policy.ParseIP(req.GetAttributes()[attributesKey].GetFields()[sourceAddressAttr].GetStringValue())
	r := policy.Request{
		Method: h[":method"],
		Path:   h[":path"],
		ClientIP: policy.ResolveClientIP(pol, func(name string) (string, bool) {
			v, ok := h[name]
			return v, ok
		}, remote),
		Now: uint64(s.now().Unix()), //nolint:gosec // a wall clock after 1970
	}
	done, count := policy.Select(pol, r)
	if done != nil {
		return s.respondRule(st, *done, pol.Version)
	}
	return s.respondRule(st, policy.Decide(pol, count, s.count(ctx, st, count)), pol.Version)
}

// count adds the request to the rule's counters. A failure reads every counter as failed; the
// policy's counter_error says what that means.
func (s *Server) count(ctx context.Context, st *request, c *policy.Count) []policy.Reading {
	if s.counters == nil {
		st.log.Warn("no counter store; the counters read as failed")
		failed := make([]policy.Reading, len(c.Counters))
		for i := range failed {
			failed[i].Failed = true
		}
		return failed
	}
	ctx, cancel := context.WithTimeout(ctx, countTimeout)
	defer cancel()
	readings, err := s.counters.Add(ctx, c.Counters)
	if err != nil {
		st.log.Warn("counter store failed; counter_error applies", "rule", c.Rule.ID, "err", err)
	}
	return readings
}

// respondRule turns a compiled policy's decision into the answer to Envoy.
func (s *Server) respondRule(st *request, d policy.Decision, version uint64) *extprocv3.ProcessingResponse {
	log := st.log.With("policyVersion", version, "rule", d.Rule)
	switch d.Type {
	case policy.TypeAllow:
		// The limit headers go on the backend's response (§5), in the response phase.
		st.ruleHeaders = d.Headers
		log.Info("allowed", "decision", RuleAllowed, "limitHeaders", len(d.Headers) > 0)
		return continueWith(withoutPaymentHeaders())

	case policy.TypeDeny:
		decision := RuleDenied
		if d.Code == "policy_unavailable" {
			decision = PolicyUnavailable
		}
		body, _ := json.Marshal(map[string]string{"code": d.Code})
		hdr := http.Header{}
		for k, v := range d.Headers {
			hdr.Set(k, v)
		}
		hdr.Set("Content-Type", "application/json")
		hdr.Set("Cache-Control", "no-store")
		log.Info("denied", "decision", decision, "status", d.Status, "code", d.Code)
		return immediate(d.Status, hdr, append(body, '\n'), decision)

	default: // payment_required
		log.Error("the rule asks for payment, which this filter version does not take on platform routes; refused", "decision", ChargeNotBuilt)
		return plain(http.StatusServiceUnavailable, "x402 filter: this route's paid rules are not supported by this filter version\n", ChargeNotBuilt)
	}
}

// ruleResponseHeaders are the limit headers of an allowed request's decision, set on the
// backend's response. Ours overwrite any of the same names the backend sent (§4.5: the gate
// sends the headers the decision carries and no others of these names).
func ruleResponseHeaders(h map[string]string) *extprocv3.HeaderMutation {
	if len(h) == 0 {
		return nil
	}
	var kv []string
	for _, k := range slices.Sorted(maps.Keys(h)) {
		kv = append(kv, k, h[k])
	}
	return &extprocv3.HeaderMutation{SetHeaders: setHeaders(kv...)}
}

// withoutPaymentHeaders keeps a client's payment headers from reaching the backend on an
// allowed request (§5): the seller did not ask for them.
func withoutPaymentHeaders() *extprocv3.HeaderMutation {
	return &extprocv3.HeaderMutation{RemoveHeaders: []string{
		strings.ToLower(x402.HeaderPaymentSignature),
		"x-payment", // x402 v1's name
	}}
}
