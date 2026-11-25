package extproc

import (
	"strings"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/amoghjay/x402_gateway/internal/metrics"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Decisions are counted in one place, Process, once per phase: an immediate answer
// carries its decision in Details ("x402_<decision>", also what Envoy's access log
// shows), and a "continue" answer's decision is set on the request next to its log line.

// phaseOf names the phase a message belongs to.
func phaseOf(req *extprocv3.ProcessingRequest) string {
	if _, ok := req.Request.(*extprocv3.ProcessingRequest_ResponseHeaders); ok {
		return metrics.PhaseResponse
	}
	return metrics.PhaseRequest
}

// decided is the decision behind an answer, and clears the one set for this phase.
func (st *request) decided(resp *extprocv3.ProcessingResponse) Decision {
	d := st.decision
	st.decision = ""
	if ir := resp.GetImmediateResponse(); ir != nil {
		if v, ok := strings.CutPrefix(ir.GetDetails(), "x402_"); ok {
			d = Decision(v)
		}
	}
	return d
}

// observe records one phase: its decision and how long the filter held it. A phase with
// no decision (the response phase of a free or allowed request) is not counted.
func (st *request) observe(phase string, d Decision, t0 time.Time) {
	reason := reasonLabel(d, st.reason)
	st.reason = ""
	if d == "" {
		return
	}
	route := st.routeLabel()
	metrics.Decision(route, phase, string(d), reason)
	metrics.Phase(route, phase, string(d), time.Since(t0))
}

func (st *request) routeLabel() string {
	if st.route == "" {
		return "unknown"
	}
	return st.route
}

// ruleCodes are the codes the policy evaluator itself produces; a seller's own deny codes
// are counted as "custom", so a policy cannot add series.
var ruleCodes = map[string]bool{"rate_limited": true, "no_matching_rule": true, "policy_unavailable": true}

// reasons are the 402 reasons this gate sends or a spec-following facilitator may give.
// Anything else, such as a facilitator's free text, is counted as "other".
var reasons = map[string]bool{
	x402.ReasonInsufficientFunds: true, x402.ReasonValidAfter: true, x402.ReasonValidBefore: true,
	x402.ReasonValueMismatch: true, x402.ReasonSignature: true, x402.ReasonRecipientMismatch: true,
	x402.ReasonInvalidNetwork: true, x402.ReasonInvalidPayload: true, x402.ReasonInvalidRequirements: true,
	x402.ReasonInvalidScheme: true, x402.ReasonUnsupportedScheme: true, x402.ReasonInvalidVersion: true,
	x402.ReasonTransactionState: true, x402.ReasonUnexpectedVerify: true, x402.ReasonUnexpectedSettle: true,
	x402.ReasonSettlementPending: true, ReasonReplay: true,
}

// reasonLabel bounds a decision's reason for use as a label.
func reasonLabel(d Decision, reason string) string {
	switch {
	case reason == "":
		return ""
	case d == RuleDenied || d == PolicyUnavailable:
		if ruleCodes[reason] {
			return reason
		}
		return "custom"
	case reasons[reason]:
		return reason
	default:
		return "other"
	}
}
