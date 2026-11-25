package extproc

import (
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/metrics/metricstest"
)

const demoRoute = "httproute/demo/httpbin" // the table key of `priced`

func decisionLabels(phase string, d Decision, reason string) map[string]string {
	return map[string]string{"route": demoRoute, "phase": phase, "decision": string(d), "reason": reason}
}

func phaseLabels(phase string, d Decision) map[string]string {
	return map[string]string{"route": demoRoute, "phase": phase, "decision": string(d)}
}

// A Sync payment is two decisions and two phase timings: the exit test reads the paid
// request's latency from exactly these two series.
func TestMetricsPaidSyncRequest(t *testing.T) {
	p, _ := setupSync(t, 0)
	accepted := decisionLabels("request", PaymentAccepted, "")
	settled := decisionLabels("response", Settled, "")
	a0, s0 := metricstest.Value(t, "x402_filter_decisions_total", accepted), metricstest.Value(t, "x402_filter_decisions_total", settled)
	h0 := metricstest.Value(t, "x402_filter_phase_duration_seconds", phaseLabels("response", Settled))
	fetched0 := metricstest.Value(t, "x402_balance_reads_total", map[string]string{"source": "fetched"})

	stream, _ := p.request(p.payment())
	p.respond(stream, "200")

	metricstest.WaitFor(t, "x402_filter_decisions_total", accepted, a0+1)
	metricstest.WaitFor(t, "x402_filter_decisions_total", settled, s0+1)
	metricstest.WaitFor(t, "x402_filter_phase_duration_seconds", phaseLabels("response", Settled), h0+1)
	metricstest.WaitFor(t, "x402_balance_reads_total", map[string]string{"source": "fetched"}, fetched0+1)
	if v := metricstest.Value(t, "x402_dependency_duration_seconds", map[string]string{"dependency": "facilitator", "operation": "settle", "result": "ok"}); v == 0 {
		t.Error("the facilitator's /settle was not recorded as a dependency call")
	}
	// A script's first run on a fresh Redis is EVALSHA (NOSCRIPT), then EVAL.
	evalsha := metricstest.Value(t, "x402_dependency_duration_seconds", map[string]string{"dependency": "redis", "operation": "evalsha", "result": "ok"})
	eval := metricstest.Value(t, "x402_dependency_duration_seconds", map[string]string{"dependency": "redis", "operation": "eval", "result": "ok"})
	if evalsha+eval == 0 {
		t.Error("the reserve script was not recorded as a Redis call")
	}
}

func TestMetricsPaywallAndReplay(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	required := decisionLabels("request", PaymentRequired, "")
	replay := decisionLabels("request", Replay, ReasonReplay)
	r0, rp0 := metricstest.Value(t, "x402_filter_decisions_total", required), metricstest.Value(t, "x402_filter_decisions_total", replay)

	stream := open(t, p.conn)
	roundTrip(t, stream, requestHeaders(priced))
	metricstest.WaitFor(t, "x402_filter_decisions_total", required, r0+1)

	hdr := p.payment()
	s, _ := p.request(hdr)
	p.respond(s, "200")
	p.request(hdr) // the same signature again
	metricstest.WaitFor(t, "x402_filter_decisions_total", replay, rp0+1)
}

func TestMetricsStreamEnd(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	ended := decisionLabels("stream_end", ReleasedStreamEnd, "")
	e0 := metricstest.Value(t, "x402_filter_decisions_total", ended)
	stream, _ := p.request(p.payment())
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	p.waitDecided(ReleasedStreamEnd, 2*time.Second)
	metricstest.WaitFor(t, "x402_filter_decisions_total", ended, e0+1)
}

// Reasons become labels only from closed sets, so neither a facilitator's free text nor
// a seller's own deny codes can add series.
func TestReasonLabelIsBounded(t *testing.T) {
	for _, c := range []struct {
		d      Decision
		reason string
		want   string
	}{
		{PaymentInvalid, "invalid_exact_evm_payload_signature", "invalid_exact_evm_payload_signature"},
		{Replay, ReasonReplay, ReasonReplay},
		{SettleRefused, "the facilitator's own words about this payment", "other"},
		{RuleDenied, "rate_limited", "rate_limited"},
		{RuleDenied, "members_only", "custom"},
		{PolicyUnavailable, "policy_unavailable", "policy_unavailable"},
		{Settled, "", ""},
	} {
		if got := reasonLabel(c.d, c.reason); got != c.want {
			t.Errorf("reasonLabel(%s, %q) = %q, want %q", c.d, c.reason, got, c.want)
		}
	}
}

// Every header that carries a payment or a credential is redacted in debug logs.
func TestRedactsPaymentsAndCredentials(t *testing.T) {
	h := map[string]string{
		"payment-signature": "p2", "x-payment": "p1", "authorization": "a", "cookie": "c", "set-cookie": "s",
		"signature": "sig", "signature-input": "si", "accept": "text/html",
	}
	out := redact(h)
	for k := range h {
		if k == "accept" {
			continue
		}
		if out[k] != "[redacted]" {
			t.Errorf("%s logged as %q", k, out[k])
		}
	}
	if out["accept"] != "text/html" {
		t.Errorf("accept = %q, want it kept", out["accept"])
	}
}
