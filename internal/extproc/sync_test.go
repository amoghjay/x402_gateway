package extproc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Sync capture against a fake facilitator: what the client gets for each way a settle
// can end, and that a response is only ever released against a transfer.

// fakeFacilitator answers /settle with a status and a body, after a delay.
type fakeFacilitator struct {
	status atomic.Int64
	body   atomic.Value // string
	delay  atomic.Int64
	calls  atomic.Int32
	last   atomic.Value // the last request body, string
}

func (f *fakeFacilitator) answer(status int, body string) {
	f.status.Store(int64(status))
	f.body.Store(body)
}

// syncPrices is the example table with the demo route set to captureMode: Sync.
func syncPrices(t *testing.T) *pricing.Store {
	t.Helper()
	b, err := os.ReadFile("../pricing/testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	var tbl map[string]any
	if err := json.Unmarshal(b, &tbl); err != nil {
		t.Fatal(err)
	}
	tbl["routes"].(map[string]any)["httproute/demo/httpbin"].(map[string]any)["captureMode"] = pricing.CaptureSync
	b, _ = json.Marshal(tbl)
	p, err := pricing.Parse(b, radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	s := pricing.NewStore()
	s.Set(p)
	return s
}

func setupSync(t *testing.T, settleTimeout time.Duration) (*paid, *fakeFacilitator) {
	t.Helper()
	f := &fakeFacilitator{}
	f.answer(200, `{"success":true,"transaction":"0xfeed","network":"eip155:72344"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settle" {
			http.NotFound(w, r)
			return
		}
		f.calls.Add(1)
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		f.last.Store(string(raw))
		time.Sleep(time.Duration(f.delay.Load()))
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(f.body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	p := setupWith(t, 5000, 0, syncPrices(t), func(pay *Payments, node *radius.RPC) {
		pay.Facilitators = map[string]*facilitator.Client{radius.Testnet.Network(): {URL: srv.URL, HTTP: srv.Client()}}
		pay.RPCs = map[string]*radius.RPC{radius.Testnet.Network(): node}
		pay.SettleTimeout = settleTimeout
	})
	return p, f
}

func (p *paid) noJobs() {
	p.t.Helper()
	if n := p.store.Redis.XLen(p.t.Context(), reserve.Stream).Val(); n != 0 {
		p.t.Errorf("settle stream has %d jobs: a Sync route must not also queue for the settler", n)
	}
}

// The settle happens before the response is released, and the receipt names the
// transaction. Nothing is left for the settler.
func TestSyncSettlesThenReleases(t *testing.T) {
	p, f := setupSync(t, 0)
	stream, resp := p.request(p.payment())
	if resp.GetRequestHeaders() == nil {
		t.Fatalf("request phase: %v, want continue", resp)
	}
	if f.calls.Load() != 0 {
		t.Fatal("settled before the backend answered: a failed backend call would have been charged")
	}

	resp = p.respond(stream, "200")
	set, _ := mutations(t, resp.GetResponseHeaders().GetResponse())
	receipt, err := x402.DecodeSettleResponse(set["payment-response"])
	if err != nil {
		t.Fatalf("receipt: %v (response %v)", err, resp)
	}
	ext, _ := receipt.Extensions[x402.SettlementExtension].(map[string]any)
	if !receipt.Success || receipt.Transaction != "0xfeed" || receipt.Payer != p.w.Address().String() ||
		receipt.Network != radius.Testnet.Network() || ext["status"] != "settled" {
		t.Errorf("receipt = %+v", receipt)
	}
	if set["cache-control"] != "private, no-store" {
		t.Errorf("cache-control = %q", set["cache-control"])
	}
	if f.calls.Load() != 1 {
		t.Errorf("/settle called %d times, want 1", f.calls.Load())
	}
	// The facilitator got the route's own terms, not the client's echo.
	var sent x402.FacilitatorRequest
	if err := json.Unmarshal([]byte(f.last.Load().(string)), &sent); err != nil || sent.PaymentRequirements.Amount != "1000" ||
		sent.PaymentRequirements.PayTo != "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4" || sent.X402Version != x402.Version {
		t.Errorf("settle request = %+v, %v", sent, err)
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want released once settled", p.inflight())
	}
	p.noJobs()
	p.decided(Settled)
}

// The facilitator (or the chain, through it) says no: the client gets a 402 with the
// reason in place of the backend's response, and nobody is charged.
func TestSyncRefusedSettleWithholdsTheResponse(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		reason string
	}{
		"4xx with a reason":    {400, `{"success":false,"errorReason":"insufficient_funds","transaction":"","network":"eip155:72344"}`, "insufficient_funds"},
		"200 success false":    {200, `{"success":false,"errorReason":"invalid_transaction_state","transaction":"","network":"eip155:72344"}`, "invalid_transaction_state"},
		"4xx without a reason": {422, `{"error":"nope"}`, x402.ReasonUnexpectedSettle},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := setupSync(t, 0)
			f.answer(c.status, c.body)
			stream, _ := p.request(p.payment())
			ir := p.respond(stream, "200").GetImmediateResponse()
			if ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_settle_refused" {
				t.Fatalf("response phase = %v %q, want a 402 in place of the response", ir.GetStatus().GetCode(), ir.GetDetails())
			}
			h := immediateHeaders(t, ir)
			receipt, err := x402.DecodeSettleResponse(h["payment-response"])
			if err != nil || receipt.Success || receipt.ErrorReason != c.reason {
				t.Errorf("receipt = %+v, %v", receipt, err)
			}
			if h["payment-required"] == "" {
				t.Error("the 402 does not say how to pay again")
			}
			if p.inflight() != "" {
				t.Errorf("in-flight = %q, want released", p.inflight())
			}
			p.noJobs()
			p.decided(SettleRefused)
		})
	}
}

// No clear answer, and the chain says the nonce is unused: the response is withheld
// with a 503, the reservation is kept, and nothing retries the settle.
func TestSyncUnknownSettleWithholdsTheResponse(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		delay  time.Duration
	}{
		"5xx":                  {502, `bad gateway`, 0},
		"timeout":              {200, `{"success":true,"transaction":"0xfeed","network":"eip155:72344"}`, 400 * time.Millisecond},
		"pending":              {200, `{"success":false,"errorReason":"settlement_pending","transaction":"0xabc","network":"eip155:72344"}`, 0},
		"failed, names a tx":   {200, `{"success":false,"errorReason":"unexpected_settle_error","transaction":"0xabc","network":"eip155:72344"}`, 0},
		"2xx that is not JSON": {200, `<html>`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := setupSync(t, 100*time.Millisecond)
			f.answer(c.status, c.body)
			f.delay.Store(int64(c.delay))
			stream, _ := p.request(p.payment())
			ir := p.respond(stream, "200").GetImmediateResponse()
			if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_settlement_unknown" {
				t.Fatalf("response phase = %v %q, want a 503 in place of the response", ir.GetStatus().GetCode(), ir.GetDetails())
			}
			if !strings.HasPrefix(string(ir.GetBody()), ReasonSettlementUnknown) {
				t.Errorf("body = %q", ir.GetBody())
			}
			_ = stream.CloseSend()
			time.Sleep(50 * time.Millisecond) // the stream's exit must not release it either
			if p.inflight() != "1000" {
				t.Errorf("in-flight = %q, want kept while the outcome is unknown", p.inflight())
			}
			p.noJobs()
			p.decided(SettlementUnknown)
		})
	}
}

// The facilitator's answer was lost, but the chain says the transfer happened: the
// buyer paid, so the response is released.
func TestSyncUnclearSettleButUsedOnChainReleases(t *testing.T) {
	p, f := setupSync(t, 0)
	f.answer(500, `oops`)
	p.node.used.Store(true)
	stream, _ := p.request(p.payment())
	resp := p.respond(stream, "200")
	set, _ := mutations(t, resp.GetResponseHeaders().GetResponse())
	receipt, err := x402.DecodeSettleResponse(set["payment-response"])
	if err != nil || !receipt.Success {
		t.Fatalf("receipt = %+v, %v (response %v)", receipt, err, resp)
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want released", p.inflight())
	}
	p.decided(SettledOnChain)
}

// A busy wallet pool is asked again inside the budget instead of failing the buyer.
func TestSyncRetriesWhenTheFacilitatorHasNoWalletFree(t *testing.T) {
	p, f := setupSync(t, 0)
	f.answer(400, `{"error":"No available wallets"}`)
	go func() {
		time.Sleep(walletRetry / 2)
		f.answer(200, `{"success":true,"transaction":"0xfeed","network":"eip155:72344"}`)
	}()
	stream, _ := p.request(p.payment())
	resp := p.respond(stream, "200")
	if resp.GetResponseHeaders() == nil || f.calls.Load() != 2 {
		t.Fatalf("response = %v after %d settle calls, want released on the second", resp, f.calls.Load())
	}
	p.decided(Settled)
}

// A backend error is never settled: nothing was served.
func TestSyncNon2xxNeverSettles(t *testing.T) {
	p, f := setupSync(t, 0)
	stream, _ := p.request(p.payment())
	p.respond(stream, "500")
	if f.calls.Load() != 0 {
		t.Errorf("/settle called %d times for a failed response", f.calls.Load())
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want released", p.inflight())
	}
	p.decided(ReleasedNon2xx)
}

// A Sync route with no facilitator is refused before the backend is called.
func TestSyncWithoutAFacilitatorFailsClosed(t *testing.T) {
	p := setupWith(t, 5000, 0, syncPrices(t), nil)
	_, resp := p.request(p.payment())
	ir := resp.GetImmediateResponse()
	if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_dependency_unavailable" {
		t.Fatalf("request phase = %v, want a 503", resp)
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want nothing reserved", p.inflight())
	}
}

// The held response has to be answered before Envoy's per-message timeout for a Sync
// route, in the worst case: a settle that runs to its limit, then the chain read.
func TestSyncBudgetFitsUnderEnvoysTimeout(t *testing.T) {
	if worst := DefaultSettleTimeout + chainCheckTimeout; worst >= pricing.SyncMessageTimeout {
		t.Errorf("settle %s + chain check %s = %s, not under messageTimeout %s", DefaultSettleTimeout, chainCheckTimeout, worst, pricing.SyncMessageTimeout)
	}
}
