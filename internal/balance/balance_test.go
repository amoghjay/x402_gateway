package balance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/rdb"
)

var payer = func() evm.Address { a, _ := evm.ParseAddress("0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4"); return a }()

// node is a fake RPC: balanceOf answers `balance` after `delay`, and counts calls.
type node struct {
	srv     *httptest.Server
	balance atomic.Int64
	delay   atomic.Int64 // nanoseconds
	calls   atomic.Int32
}

func newNode(t *testing.T, balance int64) *node {
	t.Helper()
	n := &node{}
	n.balance.Store(balance)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "eth_call" {
			t.Errorf("unexpected method %s", req.Method)
		}
		n.calls.Add(1)
		time.Sleep(time.Duration(n.delay.Load()))
		word := evm.Uint256Word(big.NewInt(n.balance.Load()))
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + evm.Hex(word[:]) + `"}`))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

type fixture struct {
	t     *testing.T
	redis *miniredis.Miniredis
	node  *node
	cache *Cache
}

func setup(t *testing.T, balance int64) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	n := newNode(t, balance)
	client := rdb.RequestPath(mr.Addr())
	t.Cleanup(func() { _ = client.Close() })
	c := &Cache{
		Redis: client,
		RPC:   &radius.RPC{URL: n.srv.URL, HTTP: n.srv.Client()},
		Token: radius.SBCAddress,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &fixture{t: t, redis: mr, node: n, cache: c}
}

// seed writes a cache entry `age` old.
func (f *fixture) seed(v int64, age time.Duration) {
	f.t.Helper()
	at := time.Now().Add(-age)
	f.redis.HSet(Key(payer), "v", strconv.FormatInt(v, 10), "t", strconv.FormatInt(at.UnixMilli(), 10))
}

func (f *fixture) get(timeout time.Duration) (Balance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return f.cache.Get(ctx, payer)
}

func (f *fixture) want(b Balance, err error, value int64, source Source) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("Get: %v", err)
	}
	if b.Value.Int64() != value || b.Source != source {
		f.t.Fatalf("got %s from %s, want %d from %s", b.Value, b.Source, value, source)
	}
}

func TestMissThenHit(t *testing.T) {
	f := setup(t, 5000)
	b, err := f.get(time.Second)
	f.want(b, err, 5000, SourceFetched)
	b, err = f.get(time.Second)
	f.want(b, err, 5000, SourceFresh)
	if n := f.node.calls.Load(); n != 1 {
		t.Errorf("RPC calls = %d, want 1", n)
	}
	if ttl := f.redis.TTL(Key(payer)); ttl <= 0 || ttl > DefaultLastKnown {
		t.Errorf("TTL = %v", ttl)
	}
}

// A read that overruns the request's deadline still finishes and fills the cache, so
// the retry is a hit: the 503 + Retry-After path.
func TestOverrunFillsTheCache(t *testing.T) {
	f := setup(t, 5000)
	f.node.delay.Store(int64(200 * time.Millisecond))
	_, err := f.get(30 * time.Millisecond)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("err = %v, want ErrDeadline", err)
	}
	time.Sleep(300 * time.Millisecond)
	b, err := f.get(30 * time.Millisecond)
	f.want(b, err, 5000, SourceFresh)
	if n := f.node.calls.Load(); n != 1 {
		t.Errorf("RPC calls = %d, want 1", n)
	}
}

// A returning payer whose entry is stale is served the last known balance when the
// refresh overruns, and the refresh still lands for the next request.
func TestStaleServesLastKnownWhenRefreshOverruns(t *testing.T) {
	f := setup(t, 7000)
	f.seed(5000, 10*time.Second)
	f.node.delay.Store(int64(200 * time.Millisecond))
	b, err := f.get(30 * time.Millisecond)
	f.want(b, err, 5000, SourceLastKnown)
	if b.Age < 10*time.Second {
		t.Errorf("age = %v", b.Age)
	}
	time.Sleep(300 * time.Millisecond)
	b, err = f.get(30 * time.Millisecond)
	f.want(b, err, 7000, SourceFresh)
}

func TestStaleRefreshesInTime(t *testing.T) {
	f := setup(t, 7000)
	f.seed(5000, 10*time.Second)
	b, err := f.get(time.Second)
	f.want(b, err, 7000, SourceRefreshed)
}

// Past LastKnown an entry is no better than nothing: the read must complete.
func TestTooOldIsAMiss(t *testing.T) {
	f := setup(t, 7000)
	f.seed(5000, 2*time.Minute)
	f.node.delay.Store(int64(200 * time.Millisecond))
	if _, err := f.get(30 * time.Millisecond); !errors.Is(err, ErrDeadline) {
		t.Fatalf("err = %v, want ErrDeadline", err)
	}
}

// Ten concurrent misses for one payer are one RPC call.
func TestSingleflight(t *testing.T) {
	f := setup(t, 5000)
	f.node.delay.Store(int64(100 * time.Millisecond))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			b, err := f.get(time.Second)
			if err != nil || b.Value.Int64() != 5000 {
				t.Errorf("got %v, %v", b, err)
			}
		})
	}
	wg.Wait()
	if n := f.node.calls.Load(); n != 1 {
		t.Errorf("RPC calls = %d, want 1", n)
	}
}

// Redis down is not a deadline: the caller must fail closed, not retry.
func TestRedisDownIsAnError(t *testing.T) {
	f := setup(t, 5000)
	f.redis.Close()
	_, err := f.get(time.Second)
	if err == nil || errors.Is(err, ErrDeadline) {
		t.Fatalf("err = %v, want a Redis error", err)
	}
}

func TestRPCErrorSurfaces(t *testing.T) {
	f := setup(t, 5000)
	f.node.srv.Close()
	_, err := f.get(time.Second)
	if err == nil || errors.Is(err, ErrDeadline) {
		t.Fatalf("err = %v, want an RPC error", err)
	}
}

// The request-path client honours the context deadline per command.
func TestRequestPathClientHonoursContext(t *testing.T) {
	mr := miniredis.RunT(t)
	client := rdb.RequestPath(mr.Addr())
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if err := client.Ping(ctx).Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if err := rdb.Ping(context.Background(), client, time.Second); err != nil {
		t.Errorf("ping: %v", err)
	}
	var _ redis.Cmdable = client
}
