package reserve

import (
	"context"
	"crypto/rand"
	"math/big"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/rdb"
)

var (
	payer = func() evm.Address { a, _ := evm.ParseAddress("0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4"); return a }()
	price = big.NewInt(1000)
	ctx   = context.Background()
)

func newNonce(t *testing.T) [32]byte {
	t.Helper()
	var n [32]byte
	if _, err := rand.Read(n[:]); err != nil {
		t.Fatal(err)
	}
	return n
}

func setup(t *testing.T) (*miniredis.Miniredis, *Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := rdb.RequestPath(mr.Addr())
	t.Cleanup(func() { _ = c.Close() })
	return mr, &Store{Redis: c}
}

func TestReserveReplayRelease(t *testing.T) {
	mr, s := setup(t)
	nonce := newNonce(t)
	expires := time.Now().Add(300 * time.Second)

	out, r, err := s.Reserve(ctx, payer, nonce, price, big.NewInt(5000), expires)
	if err != nil || out != OK || r == nil || r.Inflight.Int64() != 1000 {
		t.Fatalf("first: %v %v %v", out, r, err)
	}
	// The nonce lives past the signature: validBefore + margin.
	if ttl := mr.TTL(nonceKey(payer, nonce)); ttl < 350*time.Second || ttl > 361*time.Second {
		t.Errorf("nonce TTL = %v, want ~360 s", ttl)
	}
	if ttl := mr.TTL(inflightKey(payer)); ttl <= 0 || ttl > DefaultInflightTTL {
		t.Errorf("inflight TTL = %v", ttl)
	}

	// The same signature again: refused, nothing changes.
	out, r, err = s.Reserve(ctx, payer, nonce, price, big.NewInt(5000), expires)
	if err != nil || out != Replay || r != nil {
		t.Fatalf("replay: %v %v %v", out, r, err)
	}
	if v, _ := mr.Get(inflightKey(payer)); v != "1000" {
		t.Errorf("inflight after replay = %s, want 1000", v)
	}

	// A second payment from the same wallet: in-flight is counted.
	out, _, err = s.Reserve(ctx, payer, newNonce(t), big.NewInt(4500), big.NewInt(5000), expires)
	if err != nil || out != Insufficient {
		t.Fatalf("over balance: %v %v", out, err)
	}
	if mr.Exists(inflightKey(payer)) && func() bool { v, _ := mr.Get(inflightKey(payer)); return v != "1000" }() {
		t.Error("a refused reservation changed the in-flight sum")
	}

	// Release returns the amount; at zero the key goes away.
	if err := s.Release(ctx, payer, price); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(inflightKey(payer)) {
		t.Error("in-flight key should be deleted at zero")
	}
	// The nonce is still claimed after release: a signature is served once, ever.
	if !mr.Exists(nonceKey(payer, nonce)) {
		t.Error("release must not free the nonce")
	}
}

// Twenty concurrent payments against a balance worth five: exactly five are served.
// This is §10.4, the overdraft, closed by the in-flight sum inside one atomic script.
func TestConcurrentOverdraft(t *testing.T) {
	_, s := setup(t)
	balance := big.NewInt(5 * 1000)
	var ok, insufficient atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			out, _, err := s.Reserve(ctx, payer, newNonce(t), price, balance, time.Now().Add(time.Minute))
			switch {
			case err != nil:
				t.Error(err)
			case out == OK:
				ok.Add(1)
			case out == Insufficient:
				insufficient.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 5 || insufficient.Load() != 15 {
		t.Errorf("ok=%d insufficient=%d, want 5/15", ok.Load(), insufficient.Load())
	}
}

func TestBounds(t *testing.T) {
	_, s := setup(t)
	huge := new(big.Int).Lsh(big.NewInt(1), 60)
	if _, _, err := s.Reserve(ctx, payer, newNonce(t), huge, huge, time.Now().Add(time.Minute)); err == nil {
		t.Error("an amount above 2^53 must be refused, not miscounted")
	}
	// A balance above 2^53 is clamped, and still covers the price.
	out, _, err := s.Reserve(ctx, payer, newNonce(t), price, huge, time.Now().Add(time.Minute))
	if err != nil || out != OK {
		t.Errorf("huge balance: %v %v", out, err)
	}
	// An already-expired signature still gets a short nonce TTL, never a negative one.
	out, _, err = s.Reserve(ctx, payer, newNonce(t), price, big.NewInt(5000), time.Now().Add(-time.Hour))
	if err != nil || out != OK {
		t.Errorf("expired: %v %v", out, err)
	}
}

func TestCaptureRoundTrip(t *testing.T) {
	mr, s := setup(t)
	job := Job{
		PaymentID: "pay_1", Payer: payer.String(), Route: "httproute/demo/httpbin", Amount: "1000",
		Network: radius.Testnet.Network(), ValidBefore: 1_900_000_000, Payment: "eyJ4NDAyVmVyc2lvbiI6Mn0",
		Requirements: radius.Testnet.ExactRequirements("1000", payer.String(), 300),
		ServedAt:     time.UnixMilli(1_700_000_000_000),
	}
	id, err := s.Capture(ctx, job)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	entries, err := s.Redis.XRange(ctx, Stream, "-", "+").Result()
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	got, err := ParseJob(entries[0].Values)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ServedAt.Equal(job.ServedAt) {
		t.Errorf("servedAt = %v, want %v", got.ServedAt, job.ServedAt)
	}
	got.ServedAt = job.ServedAt // compared above; time.Time has a monotonic part
	if !reflect.DeepEqual(got, job) {
		t.Errorf("round trip\n got %+v\nwant %+v", got, job)
	}
	if _, err := ParseJob(map[string]any{"paymentId": "x"}); err == nil {
		t.Error("a job without a payment must not parse")
	}
	if mr.Exists(Stream) != true {
		t.Error("stream missing")
	}
}
