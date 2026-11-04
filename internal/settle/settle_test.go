package settle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/buyer"
	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/rdb"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// fakeFacilitator answers /settle with whatever the test set.
type fakeFacilitator struct {
	code  atomic.Int64
	body  atomic.Value // string
	calls atomic.Int32
}

func (f *fakeFacilitator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.URL.Path != "/settle" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(int(f.code.Load()))
	_, _ = io.WriteString(w, f.body.Load().(string))
}

func (f *fakeFacilitator) answer(code int, body string) {
	f.code.Store(int64(code))
	f.body.Store(body)
}

// fakeChain answers authorizationState with used or not.
type fakeChain struct{ used atomic.Bool }

func (c *fakeChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	word := "0x" + strings.Repeat("00", 32)
	if c.used.Load() {
		word = "0x" + strings.Repeat("00", 31) + "01"
	}
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"`+word+`"}`)
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type fixture struct {
	t     *testing.T
	redis *miniredis.Miniredis
	fac   *fakeFacilitator
	chain *fakeChain
	store *reserve.Store
	s     *Settler
	logs  *logBuf
	w     *buyer.Wallet
	ctx   context.Context
}

func setup(t *testing.T) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	fac := &fakeFacilitator{}
	fac.answer(200, `{"success":true,"transaction":"0xabc","network":"eip155:72344","payer":"0x1"}`)
	facSrv := httptest.NewServer(fac)
	t.Cleanup(facSrv.Close)
	chain := &fakeChain{}
	chainSrv := httptest.NewServer(chain)
	t.Cleanup(chainSrv.Close)

	client := rdb.Settler(mr.Addr(), 100*time.Millisecond)
	t.Cleanup(func() { _ = client.Close() })
	logs := &logBuf{}
	store := &reserve.Store{Redis: client}
	key, _ := evm.GeneratePrivateKey()
	s := &Settler{
		Redis:        client,
		Facilitators: map[string]*facilitator.Client{radius.Testnet.Network(): {URL: facSrv.URL, HTTP: facSrv.Client()}},
		RPCs:         map[string]*radius.RPC{radius.Testnet.Network(): {URL: chainSrv.URL, HTTP: chainSrv.Client()}},
		Reserve:      store,
		Log:          slog.New(slog.NewJSONHandler(logs, nil)),
		Consumer:     "settler-test",
		Block:        50 * time.Millisecond,
		MinIdle:      time.Millisecond,
	}
	ctx := t.Context()
	if err := s.ensureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, redis: mr, fac: fac, chain: chain, store: store, s: s, logs: logs, w: &buyer.Wallet{Key: key, Network: radius.Testnet}, ctx: ctx}
}

// capture puts one job on the stream as the filter would, with its reservation.
func (f *fixture) capture(validFor time.Duration) reserve.Job {
	f.t.Helper()
	terms := radius.Testnet.ExactRequirements("1000", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", 300)
	pp, err := f.w.Sign(terms, nil, time.Now(), validFor)
	if err != nil {
		f.t.Fatal(err)
	}
	hdr, _ := pp.Encode()
	raw, _ := pp.EIP3009()
	nonce, _ := evm.ParseBytes32(raw.Authorization.Nonce)
	out, _, err := f.store.Reserve(f.ctx, f.w.Address(), nonce, big.NewInt(1000), big.NewInt(5000), time.Now().Add(validFor))
	if err != nil || out != reserve.OK {
		f.t.Fatal(out, err)
	}
	job := reserve.Job{
		PaymentID: evm.Hex(nonce[:]), Payer: f.w.Address().String(), Route: "httproute/demo/httpbin", Amount: "1000",
		Network: radius.Testnet.Network(), ValidBefore: time.Now().Add(validFor).Unix(), Payment: hdr,
		Requirements: terms, ServedAt: time.Now(),
	}
	if _, err := f.store.Capture(f.ctx, job); err != nil {
		f.t.Fatal(err)
	}
	return job
}

func (f *fixture) pending() int64 {
	p, err := f.s.Redis.XPending(f.ctx, reserve.Stream, f.s.group()).Result()
	if err != nil {
		return -1
	}
	return p.Count
}

func (f *fixture) inflight() string {
	v, _ := f.redis.Get("inflight:{" + strings.ToLower(f.w.Address().String()) + "}")
	return v
}

func (f *fixture) dead() int64 { return f.s.Redis.XLen(f.ctx, DeadStream).Val() }

func (f *fixture) step() {
	f.t.Helper()
	if _, err := f.s.step(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) outcome(o Outcome) {
	f.t.Helper()
	if !strings.Contains(f.logs.String(), `"outcome":"`+string(o)+`"`) {
		f.t.Errorf("outcome %q not logged:\n%s", o, f.logs)
	}
}

func TestSettled(t *testing.T) {
	f := setup(t)
	f.capture(5 * time.Minute)
	if f.inflight() != "1000" {
		t.Fatal("not reserved")
	}
	f.step()
	if f.pending() != 0 || f.inflight() != "" || f.dead() != 0 {
		t.Errorf("pending=%d inflight=%q dead=%d, want 0 / released / 0", f.pending(), f.inflight(), f.dead())
	}
	f.outcome(Settled)
	if !strings.Contains(f.logs.String(), radius.Testnet.TxURL("0xabc")) {
		t.Error("explorer link not logged")
	}
	if n := f.fac.calls.Load(); n != 1 {
		t.Errorf("facilitator calls = %d", n)
	}
}

// A 402 from the facilitator is final: acknowledged, released, and loudly logged.
func TestDefiniteFailure(t *testing.T) {
	f := setup(t)
	f.fac.answer(402, `{"success":false,"errorReason":"insufficient_funds","transaction":"","network":"eip155:72344"}`)
	f.capture(5 * time.Minute)
	f.step()
	if f.pending() != 0 || f.inflight() != "" {
		t.Errorf("pending=%d inflight=%q, want acked and released", f.pending(), f.inflight())
	}
	f.outcome(SettleFailed)
	if !strings.Contains(f.logs.String(), `"reason":"insufficient_funds"`) {
		t.Errorf("reason not logged:\n%s", f.logs)
	}
}

// An uncertain answer is checked on chain: used means it settled after all.
func TestUncertainButUsedOnChain(t *testing.T) {
	f := setup(t)
	f.fac.answer(502, `{"error":"facilitator_error"}`)
	f.chain.used.Store(true)
	f.capture(5 * time.Minute)
	f.step()
	if f.pending() != 0 || f.inflight() != "" {
		t.Errorf("pending=%d inflight=%q, want acked and released", f.pending(), f.inflight())
	}
	f.outcome(SettledOnChain)
}

// Uncertain and not used: left pending, claimed back, retried; after too many
// deliveries it is dead-lettered rather than retried forever.
func TestUncertainIsRetriedThenDeadLettered(t *testing.T) {
	f := setup(t)
	f.s.MaxDeliveries = 2
	f.fac.answer(503, "upstream unavailable")
	f.capture(5 * time.Minute)

	f.step() // delivery 1: uncertain
	if f.pending() != 1 || f.inflight() != "1000" {
		t.Fatalf("pending=%d inflight=%q, want still pending and reserved", f.pending(), f.inflight())
	}
	f.outcome(SettleUncertain)

	time.Sleep(5 * time.Millisecond)
	f.s.claim(f.ctx) // delivery 2: claimed, still uncertain
	f.outcome(Claimed)
	if f.pending() != 1 {
		t.Fatalf("pending=%d after the first claim", f.pending())
	}

	time.Sleep(5 * time.Millisecond)
	f.s.claim(f.ctx) // delivery 3: over the limit
	if f.pending() != 0 || f.dead() != 1 || f.inflight() != "" {
		t.Errorf("pending=%d dead=%d inflight=%q, want dead-lettered and released", f.pending(), f.dead(), f.inflight())
	}
	f.outcome(DeadLettered)
	if n := f.fac.calls.Load(); n != 2 {
		t.Errorf("facilitator calls = %d, want 2 (the third delivery never tries)", n)
	}
}

// A job past its validBefore can never settle: dead-lettered without a call.
func TestExpiredIsDeadLettered(t *testing.T) {
	f := setup(t)
	f.capture(5 * time.Minute)
	f.s.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	f.step()
	if f.pending() != 0 || f.dead() != 1 || f.fac.calls.Load() != 0 {
		t.Errorf("pending=%d dead=%d calls=%d", f.pending(), f.dead(), f.fac.calls.Load())
	}
	f.outcome(ExpiredUnsettled)
	entries, _ := f.s.Redis.XRange(f.ctx, DeadStream, "-", "+").Result()
	if len(entries) != 1 || entries[0].Values["outcome"] != string(ExpiredUnsettled) || entries[0].Values["payment"] == "" {
		t.Errorf("dead-letter entry = %v", entries)
	}
}

func TestMalformedIsDeadLettered(t *testing.T) {
	f := setup(t)
	if err := f.s.Redis.XAdd(f.ctx, &redis.XAddArgs{Stream: reserve.Stream, Values: map[string]any{"paymentId": "x"}}).Err(); err != nil {
		t.Fatal(err)
	}
	f.step()
	if f.pending() != 0 || f.dead() != 1 {
		t.Errorf("pending=%d dead=%d", f.pending(), f.dead())
	}
}

// Jobs captured before the group existed are delivered too (the group starts at 0).
func TestGroupStartsAtTheBeginning(t *testing.T) {
	mr := miniredis.RunT(t)
	client := rdb.Settler(mr.Addr(), 100*time.Millisecond)
	defer func() { _ = client.Close() }()
	store := &reserve.Store{Redis: client}
	if _, err := store.Capture(t.Context(), reserve.Job{PaymentID: "p", Payer: "0x1", Payment: "x", Requirements: x402.Requirements{}}); err != nil {
		t.Fatal(err)
	}
	s := &Settler{Redis: client, Consumer: "c", Block: 50 * time.Millisecond, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Reserve: store}
	if err := s.ensureGroup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureGroup(t.Context()); err != nil {
		t.Fatalf("second ensureGroup must accept BUSYGROUP: %v", err)
	}
	streams, err := client.XReadGroup(t.Context(), &redis.XReadGroupArgs{Group: DefaultGroup, Consumer: "c", Streams: []string{reserve.Stream, ">"}, Count: 1, Block: 50 * time.Millisecond}).Result()
	if err != nil || len(streams) != 1 || len(streams[0].Messages) != 1 {
		t.Fatalf("the pre-existing job was not delivered: %v %v", streams, err)
	}
}

// The facilitator client now tells a definite refusal from an uncertain failure.
func TestFacilitatorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_ = json.NewEncoder(w).Encode(x402.SettleResponse{Success: false, ErrorReason: "insufficient_funds"})
	}))
	defer srv.Close()
	c := &facilitator.Client{URL: srv.URL, HTTP: srv.Client()}
	resp, err := c.Settle(t.Context(), x402.PaymentPayload{}, x402.Requirements{})
	var st *facilitator.Status
	if !errors.As(err, &st) || !st.Definite() || resp.ErrorReason != "insufficient_funds" {
		t.Errorf("err=%v resp=%+v: want a definite Status with the decoded reason", err, resp)
	}
}
