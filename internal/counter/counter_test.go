package counter

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/policy"
)

func setup(t *testing.T) (*miniredis.Miniredis, *redis.Client, *Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c, New(c)
}

const uid = "00000000-0000-4000-8000-000000000001"

func counters(now uint64) []policy.Counter {
	return []policy.Counter{
		{Limit: policy.RateLimit, Key: uid + ":rate:" + itoa(now/60) + ":global", ResetS: 60 - now%60},
		{Limit: policy.FreeQuota, Key: uid + ":free:" + itoa(now/300) + ":ip:203.0.113.5", ResetS: 300 - now%300},
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

func TestAddCountsAndExpires(t *testing.T) {
	mr, _, s := setup(t)
	ctx := context.Background()
	cs := counters(1_760_000_010) // 30 s left in the minute, 90 s in the 5 minutes

	for want := uint64(1); want <= 3; want++ {
		got, err := s.Add(ctx, cs)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Count != want || got[1].Count != want || got[0].Failed || got[1].Failed {
			t.Fatalf("add %d: readings %+v", want, got)
		}
	}
	rate, _ := Key(cs[0])
	free, _ := Key(cs[1])
	if ttl := mr.TTL(rate); ttl != 30*time.Second {
		t.Errorf("rate counter TTL %v, want 30s", ttl)
	}
	if ttl := mr.TTL(free); ttl != 90*time.Second {
		t.Errorf("free counter TTL %v, want 90s", ttl)
	}

	// A later increment must not push the expiry out: the window is fixed.
	mr.FastForward(20 * time.Second)
	if _, err := s.Add(ctx, cs); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL(rate); ttl != 10*time.Second {
		t.Errorf("rate counter TTL after a later add %v, want 10s", ttl)
	}
	mr.FastForward(11 * time.Second)
	if mr.Exists(rate) {
		t.Error("the rate counter outlived its window")
	}
	got, err := s.Add(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Count != 1 || got[1].Count != 5 {
		t.Errorf("after the rate window: readings %+v, want 1 and 5", got)
	}
}

// A counter left without an expiry (written by hand, or by an older gate) gets one on its next
// increment instead of counting forever.
func TestAddRepairsAMissingExpiry(t *testing.T) {
	mr, _, s := setup(t)
	cs := counters(1_760_000_010)[:1]
	k, _ := Key(cs[0])
	if err := mr.Set(k, "7"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Add(context.Background(), cs)
	if err != nil || got[0].Count != 8 {
		t.Fatalf("readings %+v, %v", got, err)
	}
	if ttl := mr.TTL(k); ttl != 30*time.Second {
		t.Errorf("TTL %v, want 30s", ttl)
	}
}

// Two filter replicas share Redis: concurrent requests each get a distinct count, so a quota of
// N admits exactly N of them.
func TestAddConcurrent(t *testing.T) {
	_, _, s := setup(t)
	cs := counters(1_760_000_010)[:1]
	const n = 64
	seen := make([]bool, n+1)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Add(context.Background(), cs)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if c := got[0].Count; c < 1 || c > n || seen[c] {
				t.Errorf("count %d repeated or out of range", c)
			} else {
				seen[c] = true
			}
		}()
	}
	wg.Wait()
}

func TestAddFailsEveryCounterWhenRedisFails(t *testing.T) {
	mr, _, s := setup(t)
	mr.Close()
	got, err := s.Add(context.Background(), counters(1_760_000_010))
	if err == nil {
		t.Fatal("no error with Redis down")
	}
	if len(got) != 2 || !got[0].Failed || !got[1].Failed {
		t.Fatalf("readings %+v, want both failed", got)
	}
}

func TestAddNothing(t *testing.T) {
	_, _, s := setup(t)
	got, err := s.Add(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestKey(t *testing.T) {
	cs := counters(1_760_000_010)
	rate, err := Key(cs[0])
	if err != nil || rate != "pc:{"+uid+"}:rate:29333333:global" {
		t.Errorf("rate key %q, %v", rate, err)
	}
	free, _ := Key(cs[1])
	// Redis Cluster hashes only the text inside the first {…}: the same tag is the same slot.
	if hashTag(rate) != "{"+uid+"}" || hashTag(free) != hashTag(rate) {
		t.Errorf("hash tags %q and %q, want both {%s}", hashTag(rate), hashTag(free), uid)
	}
	for _, bad := range []string{"", "nouid", ":rate:1:global", uid + ":"} {
		if _, err := Key(policy.Counter{Key: bad}); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}

// The keys policy.Select builds go into Redis as they are, end to end with a real document.
func TestWithAPolicy(t *testing.T) {
	_, _, s := setup(t)
	data, err := os.ReadFile("../policy/testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := policy.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	// The metered rule: 3 free a minute per IP, then the charge.
	var types []string
	for range 4 {
		r := policy.Request{Method: "GET", Path: "/anything/platform/metered", Now: 1_760_000_010}
		done, count := policy.Select(p, r)
		if done != nil {
			t.Fatalf("decided without counting: %+v", done)
		}
		readings, err := s.Add(context.Background(), count.Counters)
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, policy.Decide(p, count, readings).Type)
	}
	want := []string{policy.TypeAllow, policy.TypeAllow, policy.TypeAllow, policy.TypePaymentRequired}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("decisions %v, want %v", types, want)
		}
	}
}

func hashTag(key string) string {
	i := strings.Index(key, "{")
	j := strings.Index(key, "}")
	if i < 0 || j < i {
		return ""
	}
	return key[i : j+1]
}
