package extproc

import (
	"encoding/json"
	"fmt"
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
	onCall atomic.Value // func(), run on each /settle before answering

	verifyStatus atomic.Int64 // /verify; 0 = 200
	verifyBody   atomic.Value // string; unset = {"isValid":true}
	verifyCalls  atomic.Int32
}

func newFakeFacilitator() *fakeFacilitator {
	f := &fakeFacilitator{}
	f.answer(200, `{"success":true,"transaction":"0xfeed","network":"eip155:72344"}`)
	return f
}

// handler serves /verify and /settle.
func (f *fakeFacilitator) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/verify" {
			f.verifyCalls.Add(1)
			code, body := int(f.verifyStatus.Load()), `{"isValid":true}`
			if code == 0 {
				code = 200
			}
			if b, ok := f.verifyBody.Load().(string); ok {
				body = b
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
			return
		}
		if r.URL.Path != "/settle" {
			http.NotFound(w, r)
			return
		}
		f.calls.Add(1)
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		f.last.Store(string(raw))
		if fn, ok := f.onCall.Load().(func()); ok {
			fn()
		}
		time.Sleep(time.Duration(f.delay.Load()))
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(f.body.Load().(string)))
	})
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

func setupSync(t *testing.T, settleTimeout time.Duration, opts ...func(*Payments)) (*paid, *fakeFacilitator) {
	t.Helper()
	f := newFakeFacilitator()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	p := setupWith(t, 5000, 0, syncPrices(t), func(pay *Payments, node *radius.RPC) {
		pay.Facilitators = map[string]*facilitator.Client{radius.Testnet.Network(): {URL: srv.URL, HTTP: srv.Client()}}
		pay.RPCs = map[string]*radius.RPC{radius.Testnet.Network(): node}
		pay.SettleTimeout = settleTimeout
		pay.ConfirmWait = 300 * time.Millisecond // the default would add 2.5 s to every unclear case
		for _, o := range opts {
			o(pay)
		}
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
	f.onCall.Store(func() { p.node.used.Store(true) }) // the transfer lands; only the answer is lost
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

// The settle call times out, and the transfer lands a little later: seen on Radius
// testnet, where it left a buyer charged with a 503. The filter keeps asking the chain
// for a while, so the buyer gets the response.
func TestSyncLateTransferAfterATimeoutIsStillReleased(t *testing.T) {
	p, f := setupSync(t, 100*time.Millisecond)
	f.delay.Store(int64(400 * time.Millisecond)) // longer than the settle timeout
	f.onCall.Store(func() {
		go func() { time.Sleep(250 * time.Millisecond); p.node.used.Store(true) }() // lands 150 ms after we gave up
	})
	stream, _ := p.request(p.payment())
	resp := p.respond(stream, "200")
	if resp.GetResponseHeaders() == nil {
		t.Fatalf("response = %v, want released once the chain shows the transfer", resp)
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
func TestSyncNotServedNeverSettles(t *testing.T) {
	for _, code := range []string{"500", "404", "304"} {
		t.Run(code, func(t *testing.T) {
			p, f := setupSync(t, 0)
			stream, _ := p.request(p.payment())
			p.respond(stream, code)
			if f.calls.Load() != 0 {
				t.Errorf("/settle called %d times for a %s", f.calls.Load(), code)
			}
			if p.inflight() != "" {
				t.Errorf("in-flight = %q, want released", p.inflight())
			}
			p.decided(ReleasedNotServed)
		})
	}
}

// A 302 is held and settled like a 200: its Location can be the content. Before
// 2026-11-19 it was released unpaid.
func TestSyncRedirectIsSettledBeforeRelease(t *testing.T) {
	p, f := setupSync(t, 0)
	stream, _ := p.request(p.payment())
	resp := p.respond(stream, "302")
	if ir := resp.GetImmediateResponse(); ir != nil {
		t.Fatalf("response phase = %v, want the 302 released after settling", ir.GetStatus().GetCode())
	}
	set, _ := mutations(t, resp.GetResponseHeaders().GetResponse())
	if r, err := x402.DecodeSettleResponse(set["payment-response"]); err != nil || !r.Success || r.Transaction != "0xfeed" {
		t.Errorf("receipt = %+v, %v", r, err)
	}
	if f.calls.Load() != 1 {
		t.Errorf("/settle calls = %d, want 1", f.calls.Load())
	}
	p.decided(Settled)
}

// A Sync route with no facilitator, or no RPC, is refused before the backend is called.
func TestSyncWithoutAFacilitatorOrRPCFailsClosed(t *testing.T) {
	fac := map[string]*facilitator.Client{radius.Testnet.Network(): {URL: "http://127.0.0.1:1"}}
	for name, mod := range map[string]func(*Payments, *radius.RPC){
		"neither": nil,
		"no RPC":  func(pay *Payments, _ *radius.RPC) { pay.Facilitators = fac },
		"no facilitator": func(pay *Payments, node *radius.RPC) {
			pay.RPCs = map[string]*radius.RPC{radius.Testnet.Network(): node}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := setupWith(t, 5000, 0, syncPrices(t), mod)
			_, resp := p.request(p.payment())
			ir := resp.GetImmediateResponse()
			if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_dependency_unavailable" {
				t.Fatalf("request phase = %v, want a 503", resp)
			}
			if p.inflight() != "" {
				t.Errorf("in-flight = %q, want nothing reserved", p.inflight())
			}
		})
	}
}

// A facilitator may answer /settle with success for a payment that already settled
// (Radius testnet does). So a payment whose nonce is used on chain before this request
// settles is a replay: it gets a 402 and /settle is never called. This is the case
// where Redis lost the nonce claim; with the claim, it is refused at the door.
func TestSyncReplayOfASpentPaymentIsWithheld(t *testing.T) {
	p, f := setupSync(t, 0) // the facilitator would say success
	p.node.used.Store(true)
	stream, _ := p.request(p.payment())
	ir := p.respond(stream, "200").GetImmediateResponse()
	if ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_replay" {
		t.Fatalf("response phase = %v %q, want a 402 for the replay", ir.GetStatus().GetCode(), ir.GetDetails())
	}
	receipt, err := x402.DecodeSettleResponse(immediateHeaders(t, ir)["payment-response"])
	if err != nil || receipt.Success || receipt.ErrorReason != ReasonReplay {
		t.Errorf("receipt = %+v, %v", receipt, err)
	}
	if f.calls.Load() != 0 {
		t.Errorf("/settle called %d times for a spent payment", f.calls.Load())
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want released", p.inflight())
	}
}

// Without the chain read a replay cannot be ruled out, so the response is withheld and
// nothing is settled.
func TestSyncChainReadFailureBeforeSettleFailsClosed(t *testing.T) {
	p, f := setupSync(t, 0)
	p.node.down.Store(true)
	stream, _ := p.request(p.payment())
	ir := p.respond(stream, "200").GetImmediateResponse()
	if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_dependency_unavailable" {
		t.Fatalf("response phase = %v %q, want a 503", ir.GetStatus().GetCode(), ir.GetDetails())
	}
	if f.calls.Load() != 0 || p.inflight() != "" {
		t.Errorf("/settle calls = %d, in-flight = %q; want no settle and the reservation released", f.calls.Load(), p.inflight())
	}
}

// One slow authorizationState read before settling is asked again, not turned into a
// 503 for a valid payer (seen once in 100 requests from four concurrent buyers).
func TestSyncSlowChainReadBeforeSettleIsAskedAgain(t *testing.T) {
	p, f := setupSync(t, 0)
	p.node.stall.Store(int64(chainCheckTimeout + 200*time.Millisecond))
	stream, _ := p.request(p.payment())
	resp := p.respond(stream, "200")
	if ir := resp.GetImmediateResponse(); ir != nil {
		t.Fatalf("response phase = %v %q, want the response released", ir.GetStatus().GetCode(), ir.GetDetails())
	}
	if f.calls.Load() != 1 {
		t.Errorf("/settle calls = %d, want 1", f.calls.Load())
	}
	p.decided(Settled)
}

// A facilitator nobody is listening on: the settle never left, so the buyer is not
// charged, and the 503 comes at once instead of after the wait for the chain.
func TestSyncUnreachableFacilitatorAnswersAtOnce(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close() // nothing listens on this port now: connection refused
	p := setupWith(t, 5000, 0, syncPrices(t), func(pay *Payments, node *radius.RPC) {
		pay.Facilitators = map[string]*facilitator.Client{radius.Testnet.Network(): {URL: url, HTTP: &http.Client{}}}
		pay.RPCs = map[string]*radius.RPC{radius.Testnet.Network(): node}
		pay.ConfirmWait = 5 * time.Second // would show up in the elapsed time if it were used
	})
	stream, _ := p.request(p.payment())
	t0 := time.Now()
	ir := p.respond(stream, "200").GetImmediateResponse()
	if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_dependency_unavailable" {
		t.Fatalf("response phase = %v %q, want a 503", ir.GetStatus().GetCode(), ir.GetDetails())
	}
	if el := time.Since(t0); el > time.Second {
		t.Errorf("answered after %s, want at once (no wait for the chain)", el)
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want released: nothing was charged", p.inflight())
	}
	p.decided(DependencyUnavailable)
}

// held counts the payments recorded before /settle and not yet cleared.
func (p *paid) held() int {
	p.t.Helper()
	n, err := p.store.Redis.ZCard(p.t.Context(), reserve.HeldSet).Result()
	if err != nil {
		p.t.Fatal(err)
	}
	return int(n)
}

// The record written before /settle: cleared once the response is handed to Envoy or
// the settle is refused, kept when the outcome is unknown (the settler resolves it).
func TestSyncHeldRecord(t *testing.T) {
	t.Run("delivered: cleared", func(t *testing.T) {
		p, _ := setupSync(t, 0)
		stream, _ := p.request(p.payment())
		if ir := p.respond(stream, "200").GetImmediateResponse(); ir != nil {
			t.Fatalf("not released: %v", ir)
		}
		// Cleared just after the server's Send returns, so poll briefly.
		deadline := time.Now().Add(time.Second)
		for p.held() != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := p.held(); n != 0 {
			t.Errorf("%d records left after a delivered response", n)
		}
	})
	t.Run("refused: cleared", func(t *testing.T) {
		p, f := setupSync(t, 0)
		f.answer(400, `{"success":false,"errorReason":"invalid_exact_evm_insufficient_balance","transaction":"","network":"eip155:72344"}`)
		stream, _ := p.request(p.payment())
		p.respond(stream, "200")
		if n := p.held(); n != 0 {
			t.Errorf("%d records left after a refusal", n)
		}
	})
	t.Run("unknown: kept with the details", func(t *testing.T) {
		p, f := setupSync(t, 100*time.Millisecond)
		f.answer(502, `bad gateway`)
		stream, _ := p.request(p.payment())
		if ir := p.respond(stream, "200").GetImmediateResponse(); ir.GetStatus().GetCode() != 503 {
			t.Fatalf("response phase = %v", ir)
		}
		hs, err := p.store.HeldBefore(t.Context(), time.Now().Add(time.Minute), 10)
		if err != nil || len(hs) != 1 {
			t.Fatalf("held = %+v, %v; want the one payment", hs, err)
		}
		if h := hs[0]; h.Payer != p.w.Address().String() || h.Amount != "1000" || h.Route != "httproute/demo/httpbin" || h.Nonce != h.PaymentID || h.ValidBefore == 0 {
			t.Errorf("record = %+v", h)
		}
	})
	t.Run("record cannot be written: nothing settled", func(t *testing.T) {
		p, f := setupSync(t, 0)
		stream, _ := p.request(p.payment())
		p.redis.SetError("READONLY drill") // Redis refuses writes from here on
		ir := p.respond(stream, "200").GetImmediateResponse()
		if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_dependency_unavailable" {
			t.Fatalf("response phase = %v %q, want a 503", ir.GetStatus().GetCode(), ir.GetDetails())
		}
		if f.calls.Load() != 0 {
			t.Errorf("/settle called %d times without a record", f.calls.Load())
		}
	})
}

// A refusal that can pass is settled once more while the payment is unspent; one about
// the payment, or a payment already spent, is not.
func TestSyncPassingRefusalIsTriedOnceMore(t *testing.T) {
	const failed = `{"success":false,"errorReason":"invalid_exact_evm_transaction_failed","transaction":"","network":"eip155:72344"}`
	const ok = `{"success":true,"transaction":"0xfeed","network":"eip155:72344"}`
	for name, c := range map[string]struct {
		first     string
		then      string
		spent     bool // the nonce is used on chain by the time of the second look
		wantCode  int32
		wantCalls int32
	}{
		"transaction failed, then settles": {failed, ok, false, 0, 2},
		"transaction failed twice":         {failed, failed, false, 402, 2},
		"transaction failed, nonce spent":  {failed, ok, true, 402, 1},
		"insufficient balance: never retried": {
			`{"success":false,"errorReason":"invalid_exact_evm_insufficient_balance","transaction":"","network":"eip155:72344"}`, ok, false, 402, 1},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := setupSync(t, 0)
			f.onCall.Store(func() {
				if f.calls.Load() == 1 {
					f.answer(400, c.first)
					p.node.used.Store(c.spent)
					return
				}
				f.answer(200, c.then)
			})
			stream, _ := p.request(p.payment())
			resp := p.respond(stream, "200")
			if code := resp.GetImmediateResponse().GetStatus().GetCode(); int32(code) != c.wantCode {
				t.Errorf("response phase = %v, want %d (0 = released)", code, c.wantCode)
			}
			if n := f.calls.Load(); n != c.wantCalls {
				t.Errorf("/settle calls = %d, want %d", n, c.wantCalls)
			}
		})
	}
}

// The payer cancels the authorization (cancelAuthorization) while the request is in
// flight. On chain a cancelled nonce reads as used, so: cancelled before the response
// phase → caught by the read before /settle (402, never settled); cancelled while the
// settle runs → the facilitator refuses it (402). Either way nothing is served.
func TestSyncCancelledAuthorizationIsNeverServed(t *testing.T) {
	t.Run("before the response phase", func(t *testing.T) {
		p, f := setupSync(t, 0)
		stream, _ := p.request(p.payment())
		p.node.used.Store(true) // cancelAuthorization lands while the backend works
		ir := p.respond(stream, "200").GetImmediateResponse()
		if ir.GetStatus().GetCode() != 402 || f.calls.Load() != 0 {
			t.Errorf("response phase = %v, /settle calls = %d; want 402 and no settle", ir.GetStatus().GetCode(), f.calls.Load())
		}
	})
	t.Run("during the settle", func(t *testing.T) {
		p, f := setupSync(t, 0)
		f.answer(400, `{"success":false,"errorReason":"invalid_transaction_state","transaction":"","network":"eip155:72344"}`)
		f.onCall.Store(func() { p.node.used.Store(true) })
		stream, _ := p.request(p.payment())
		ir := p.respond(stream, "200").GetImmediateResponse()
		if ir.GetStatus().GetCode() != 402 {
			t.Errorf("response phase = %v, want 402", ir.GetStatus().GetCode())
		}
	})
}

// With FacilitatorVerify, a Sync route's payment is also checked by the facilitator's
// /verify on the request, before the backend is called.
func TestSyncFacilitatorVerify(t *testing.T) {
	on := func(p *Payments) { p.FacilitatorVerify = true }
	t.Run("valid: forwarded and settled", func(t *testing.T) {
		p, f := setupSync(t, 0, on)
		stream, resp := p.request(p.payment())
		if resp.GetImmediateResponse() != nil {
			t.Fatalf("request phase refused: %v", resp)
		}
		if ir := p.respond(stream, "200").GetImmediateResponse(); ir != nil {
			t.Fatalf("response phase = %v", ir)
		}
		if f.verifyCalls.Load() != 1 || f.calls.Load() != 1 {
			t.Errorf("/verify calls %d, /settle calls %d; want 1 and 1", f.verifyCalls.Load(), f.calls.Load())
		}
	})
	for name, c := range map[string]struct {
		status  int64
		body    string
		code    int32
		details string
		reason  string
	}{
		"says no":           {200, `{"isValid":false,"invalidReason":"invalid_exact_evm_insufficient_balance"}`, 402, "x402_payment_invalid", "invalid_exact_evm_insufficient_balance"},
		"says no with 400":  {400, `{"isValid":false,"invalidReason":"invalid_exact_evm_payload_signature"}`, 402, "x402_payment_invalid", "invalid_exact_evm_payload_signature"},
		"5xx":               {503, `down`, 503, "x402_dependency_unavailable", ""},
		"a different payer": {200, `{"isValid":true,"payer":"0x0000000000000000000000000000000000000001"}`, 503, "x402_dependency_unavailable", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := setupSync(t, 0, on)
			f.verifyStatus.Store(c.status)
			f.verifyBody.Store(c.body)
			_, resp := p.request(p.payment())
			ir := resp.GetImmediateResponse()
			if int32(ir.GetStatus().GetCode()) != c.code || ir.GetDetails() != c.details {
				t.Fatalf("request phase = %v %q, want %d %q", ir.GetStatus().GetCode(), ir.GetDetails(), c.code, c.details)
			}
			if c.reason != "" && !strings.Contains(string(ir.GetBody())+fmt.Sprint(ir.GetHeaders()), c.reason) {
				t.Errorf("402 does not carry the facilitator's reason %q", c.reason)
			}
			if p.inflight() != "" {
				t.Errorf("in-flight = %q, want released", p.inflight())
			}
			if f.calls.Load() != 0 {
				t.Errorf("/settle called %d times", f.calls.Load())
			}
		})
	}
	t.Run("off: not called", func(t *testing.T) {
		p, f := setupSync(t, 0)
		stream, _ := p.request(p.payment())
		p.respond(stream, "200")
		if f.verifyCalls.Load() != 0 {
			t.Errorf("/verify called %d times with FacilitatorVerify off", f.verifyCalls.Load())
		}
	})
}

// A definite refusal is believed even if the nonce turns out used: another copy of the
// payment settled in the meantime, and that copy's request is the one that paid.
func TestSyncRefusalIsNotOverriddenByAUsedNonce(t *testing.T) {
	p, f := setupSync(t, 0)
	f.answer(400, `{"success":false,"errorReason":"invalid_transaction_state","transaction":"","network":"eip155:72344"}`)
	f.onCall.Store(func() { p.node.used.Store(true) })
	stream, _ := p.request(p.payment())
	ir := p.respond(stream, "200").GetImmediateResponse()
	if ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_settle_refused" {
		t.Fatalf("response phase = %v %q, want the 402", ir.GetStatus().GetCode(), ir.GetDetails())
	}
}

// The held response has to be answered before Envoy's per-message timeout for a Sync
// route, in the worst case: a settle that runs to its limit, then the chain read.
func TestSyncBudgetFitsUnderEnvoysTimeout(t *testing.T) {
	before := preSettleReads * chainCheckTimeout
	if worst := before + DefaultSettleTimeout + DefaultConfirmWait + chainCheckTimeout; worst >= pricing.SyncMessageTimeout {
		t.Errorf("chain reads %s + settle %s + confirm %s + one last read = %s, not under messageTimeout %s", before, DefaultSettleTimeout, DefaultConfirmWait, worst, pricing.SyncMessageTimeout)
	}
}
