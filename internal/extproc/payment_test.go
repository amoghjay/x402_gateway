package extproc

import (
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"

	"github.com/amoghjay/x402_gateway/internal/balance"
	"github.com/amoghjay/x402_gateway/internal/buyer"
	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/rdb"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// The paid path end to end, against miniredis and a fake RPC node, with payments
// signed by our own buyer: the same code radius clients run, minus the network.

type node struct {
	balance atomic.Int64
	delay   atomic.Int64
	calls   atomic.Int32
}

type paid struct {
	t     *testing.T
	redis *miniredis.Miniredis
	node  *node
	conn  *grpc.ClientConn
	logs  *logBuf
	w     *buyer.Wallet
	store *reserve.Store
}

func setupPaid(t *testing.T, balanceAtomic int64, deadline time.Duration) *paid {
	t.Helper()
	mr := miniredis.RunT(t)
	n := &node{}
	n.balance.Store(balanceAtomic)
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.calls.Add(1)
		time.Sleep(time.Duration(n.delay.Load()))
		word := evm.Uint256Word(big.NewInt(n.balance.Load()))
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + evm.Hex(word[:]) + `"}`))
	}))
	t.Cleanup(rpc.Close)

	client := rdb.RequestPath(mr.Addr())
	t.Cleanup(func() { _ = client.Close() })
	logs := &logBuf{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := &reserve.Store{Redis: client}
	s := NewServer(log, 0, loaded(t)).WithPayments(&Payments{
		Balances: map[string]*balance.Cache{radius.Testnet.Network(): {
			Redis: client, RPC: &radius.RPC{URL: rpc.URL, HTTP: rpc.Client()}, Token: radius.SBCAddress, Log: log,
		}},
		Reserve:  store,
		Deadline: deadline,
	})
	key, err := evm.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &paid{t: t, redis: mr, node: n, conn: serve(t, s), logs: logs, w: &buyer.Wallet{Key: key, Network: radius.Testnet}, store: store}
}

// payment signs for the demo route's terms, as a client does after reading the 402.
func (p *paid) payment() string {
	p.t.Helper()
	terms := radius.Testnet.ExactRequirements("1000", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", 300)
	pp, err := p.w.Sign(terms, &x402.Resource{URL: "http://34.173.224.114/get?x=1"}, time.Now(), 5*time.Minute)
	if err != nil {
		p.t.Fatal(err)
	}
	hdr, err := pp.Encode()
	if err != nil {
		p.t.Fatal(err)
	}
	return hdr
}

func (p *paid) request(hdr string, extra ...string) (extprocv3.ExternalProcessor_ProcessClient, *extprocv3.ProcessingResponse) {
	p.t.Helper()
	stream := open(p.t, p.conn)
	kv := append([]string{"payment-signature", hdr}, extra...)
	return stream, roundTrip(p.t, stream, requestHeaders(priced, kv...))
}

func (p *paid) respond(stream extprocv3.ExternalProcessor_ProcessClient, status string) *extprocv3.ProcessingResponse {
	p.t.Helper()
	return roundTrip(p.t, stream, &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &extprocv3.HttpHeaders{Headers: headers(":status", status, "cache-control", "public, max-age=60")},
	}})
}

func (p *paid) inflight() string {
	v, _ := p.redis.Get("inflight:{" + strings.ToLower(p.w.Address().String()) + "}")
	return v
}

func (p *paid) decided(d Decision) {
	p.t.Helper()
	if !strings.Contains(p.logs.String(), `"decision":"`+string(d)+`"`) {
		p.t.Errorf("decision %q not logged:\n%s", d, p.logs)
	}
}

// waitDecided is decided for an effect that happens after the stream closes, on the
// server's own goroutine: it polls the log until the decision appears or the wait ends.
func (p *paid) waitDecided(d Decision, wait time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(wait)
	for !strings.Contains(p.logs.String(), `"decision":"`+string(d)+`"`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	p.decided(d)
}

// mutations returns a CommonResponse's header mutation as maps, asserting overwrite.
func mutations(t *testing.T, cr *extprocv3.CommonResponse) (set map[string]string, removed []string) {
	t.Helper()
	set = map[string]string{}
	for _, o := range cr.GetHeaderMutation().GetSetHeaders() {
		if o.GetAppendAction() != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD || len(o.GetHeader().GetRawValue()) == 0 {
			t.Errorf("header %s: not an overwrite with a raw value", o.GetHeader().GetKey())
		}
		set[o.GetHeader().GetKey()] = string(o.GetHeader().GetRawValue())
	}
	return set, cr.GetHeaderMutation().GetRemoveHeaders()
}

func TestPaidRequestIsServedAndCaptured(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	hdr := p.payment()
	stream, resp := p.request(hdr, "x-x402-payer", "0xspoofed")

	set, removed := mutations(t, resp.GetRequestHeaders().GetResponse())
	if len(removed) != 1 || removed[0] != "payment-signature" {
		t.Errorf("removed = %v, want the payment header", removed)
	}
	if set[HeaderPayer] != p.w.Address().String() {
		t.Errorf("%s = %q, want the recovered payer (a client-sent value is overwritten)", HeaderPayer, set[HeaderPayer])
	}
	id := set[HeaderPaymentID]
	if !strings.HasPrefix(id, "0x") || len(id) != 66 {
		t.Errorf("%s = %q, want the nonce", HeaderPaymentID, id)
	}
	if p.inflight() != "1000" {
		t.Errorf("in-flight = %q, want 1000 after accept", p.inflight())
	}
	p.decided(PaymentAccepted)

	resp = p.respond(stream, "200")
	set, _ = mutations(t, resp.GetResponseHeaders().GetResponse())
	if set["cache-control"] != "private, no-store" {
		t.Errorf("cache-control = %q: a paid response must not be resold by a cache", set["cache-control"])
	}
	if set["access-control-expose-headers"] != "PAYMENT-RESPONSE" {
		t.Errorf("expose = %q", set["access-control-expose-headers"])
	}
	receipt, err := x402.DecodeSettleResponse(set["payment-response"])
	if err != nil {
		t.Fatal(err)
	}
	ext, _ := receipt.Extensions[x402.SettlementExtension].(map[string]any)
	if !receipt.Success || receipt.Transaction != "" || receipt.Payer != p.w.Address().String() ||
		receipt.Network != radius.Testnet.Network() || ext["status"] != "pending" || ext["paymentId"] != id {
		t.Errorf("receipt = %+v", receipt)
	}
	p.decided(Paid)

	// Captured: one job on the stream, and the in-flight amount stays until the settler
	// is done with it.
	entries, err := p.store.Redis.XRange(t.Context(), reserve.Stream, "-", "+").Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("stream: %v %v", entries, err)
	}
	job, err := reserve.ParseJob(entries[0].Values)
	if err != nil || job.Payer != p.w.Address().String() || job.Amount != "1000" || job.Payment != hdr || job.Route != "httproute/demo/httpbin" {
		t.Errorf("job = %+v, %v", job, err)
	}
	if p.inflight() != "1000" {
		t.Errorf("in-flight = %q after capture, want 1000 until settled", p.inflight())
	}
	if strings.Contains(p.logs.String(), hdr[:40]) {
		t.Error("the signature reached the logs")
	}
}

// The same signature a second time is refused at the door with its own reason.
func TestReplayIsRefused(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	hdr := p.payment()
	stream, resp := p.request(hdr)
	if resp.GetRequestHeaders() == nil {
		t.Fatalf("first: want continue, got %v", resp)
	}
	p.respond(stream, "200")

	_, resp = p.request(hdr)
	ir := resp.GetImmediateResponse()
	if ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_replay" {
		t.Fatalf("replay: got %v", resp)
	}
	pr, err := x402.DecodePaymentRequired(immediateHeaders(t, ir)["payment-required"])
	if err != nil || !strings.HasPrefix(pr.Error, ReasonReplay+": ") || !strings.Contains(pr.Error, "Pay with:") {
		t.Errorf("402 error = %q, %v", pr.Error, err)
	}
	if p.inflight() != "1000" {
		t.Errorf("in-flight = %q: a replay must not reserve again", p.inflight())
	}
	p.decided(Replay)
}

func TestInsufficientFundsIsRefused(t *testing.T) {
	p := setupPaid(t, 999, 0)
	_, resp := p.request(p.payment())
	ir := resp.GetImmediateResponse()
	if ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_insufficient_funds" {
		t.Fatalf("got %v", resp)
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q: a refusal must not reserve", p.inflight())
	}
	pr, _ := x402.DecodePaymentRequired(immediateHeaders(t, ir)["payment-required"])
	if !strings.HasPrefix(pr.Error, x402.ReasonInsufficientFunds+": ") {
		t.Errorf("402 error = %q", pr.Error)
	}
}

// Five payments against a balance worth three: the in-flight sum stops the fourth,
// before anything settles.
func TestConcurrentPaymentsCannotOverdraw(t *testing.T) {
	p := setupPaid(t, 3000, 0)
	served := 0
	for range 5 {
		stream, resp := p.request(p.payment())
		if resp.GetRequestHeaders() != nil {
			served++
			p.respond(stream, "200")
		} else if resp.GetImmediateResponse().GetDetails() != "x402_insufficient_funds" {
			t.Errorf("unexpected refusal %v", resp)
		}
	}
	if served != 3 {
		t.Errorf("served %d, want 3", served)
	}
}

func TestInvalidPaymentIsRefused(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	for name, hdr := range map[string]string{
		"garbage": "not-base64!!",
		"wrong terms": func() string { // signed for another price
			pp, _ := p.w.Sign(radius.Testnet.ExactRequirements("1", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", 300), nil, time.Now(), time.Minute)
			h, _ := pp.Encode()
			return h
		}(),
	} {
		_, resp := p.request(hdr)
		ir := resp.GetImmediateResponse()
		if ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_payment_invalid" {
			t.Errorf("%s: got %v", name, resp)
		}
	}
	if n := p.node.calls.Load(); n != 0 {
		t.Errorf("RPC called %d times for invalid payments; the signature check comes first", n)
	}
	p.decided(PaymentInvalid)
}

// A backend error (or Envoy's own 5xx after we continued) means nothing was served:
// the reservation is released and no receipt is sent.
func TestNon2xxReleases(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	stream, _ := p.request(p.payment())
	resp := p.respond(stream, "503")
	set, _ := mutations(t, resp.GetResponseHeaders().GetResponse())
	if set["payment-response"] != "" {
		t.Error("a receipt was sent for a failed response")
	}
	if p.inflight() != "" {
		t.Errorf("in-flight = %q, want released", p.inflight())
	}
	if n := p.store.Redis.XLen(t.Context(), reserve.Stream).Val(); n != 0 {
		t.Errorf("stream has %d jobs, want none", n)
	}
	p.decided(ReleasedNon2xx)
}

// The client goes away (or Envoy resets) between the phases: the stream closes with
// no response phase, and the reservation is released on the way out.
func TestStreamEndReleases(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	stream, _ := p.request(p.payment())
	if p.inflight() != "1000" {
		t.Fatal("not reserved")
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	// The release deletes the reservation, then logs. Wait for the log line, the last
	// effect, so the assertion never runs in the gap between the two.
	p.waitDecided(ReleasedStreamEnd, 2*time.Second)
	if p.inflight() != "" {
		t.Errorf("in-flight = %q after the stream ended, want released", p.inflight())
	}
}

// A slow balance read gets the buyer a 503 + Retry-After, not a charge; the read
// finishes anyway, and the retry with the same signature is served from the cache.
func TestDeadlineThenRetrySucceeds(t *testing.T) {
	p := setupPaid(t, 5000, 60*time.Millisecond)
	p.node.delay.Store(int64(200 * time.Millisecond))
	hdr := p.payment()
	_, resp := p.request(hdr)
	ir := resp.GetImmediateResponse()
	if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_deadline" || immediateHeaders(t, ir)["retry-after"] != "1" {
		t.Fatalf("got %v", resp)
	}
	if p.inflight() != "" {
		t.Error("a 503 must not reserve")
	}
	time.Sleep(300 * time.Millisecond)
	_, resp = p.request(hdr)
	if resp.GetRequestHeaders() == nil {
		t.Fatalf("retry: want continue, got %v", resp)
	}
	if n := p.node.calls.Load(); n != 1 {
		t.Errorf("RPC calls = %d, want 1: the retry is a cache hit", n)
	}
}

func TestRedisDownFailsClosed(t *testing.T) {
	p := setupPaid(t, 5000, 0)
	p.redis.Close()
	_, resp := p.request(p.payment())
	ir := resp.GetImmediateResponse()
	if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_dependency_unavailable" {
		t.Fatalf("got %v", resp)
	}
	p.decided(DependencyUnavailable)
}

// Without payments wired (the M3 shape) a payment still gets the 402.
func TestNoPaymentsWiredStillGets402(t *testing.T) {
	stream, logs := dial(t, 0, loaded(t), slog.LevelInfo)
	resp := roundTrip(t, stream, requestHeaders(priced, "payment-signature", "eyJ4NDAyVmVyc2lvbiI6Mn0="))
	if resp.GetImmediateResponse().GetStatus().GetCode() != 402 || !strings.Contains(logs.String(), `"decision":"payment_present_unverified"`) {
		t.Fatalf("got %v\n%s", resp, logs)
	}
}

func TestReceiptIsSpecShaped(t *testing.T) {
	r := x402.SettleResponse{Success: true, Network: "eip155:72344", Payer: "0xabc",
		Extensions: map[string]any{x402.SettlementExtension: map[string]string{"status": "pending", "paymentId": "0x1"}}}
	b, _ := json.Marshal(r)
	for _, want := range []string{`"success":true`, `"transaction":""`, `"network":"eip155:72344"`, `"x402.dev/settlement"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("receipt %s lacks %s", b, want)
		}
	}
}
