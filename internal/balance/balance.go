// Package balance is the payer's SBC balance as the request path sees it: a Redis
// cache in front of one balanceOf read, shared by every filter replica.
//
// The policy, decided from the GKE baseline (a warm read fits the deadline 99.26% of
// the time, a cold one never, and a returning payer is a cache hit):
//
//   - a balance younger than Fresh is served as is;
//   - between Fresh and LastKnown the last known value is served at once and a refresh
//     starts in the background, rather than fail a payer for our slowness (the risk is
//     bounded by one cache window per payer). Waiting for the refresh was tried first
//     and measured on GKE: a read that does not fit the deadline burns the whole budget
//     and then starves the reserve step that needed one millisecond of it;
//   - older than that, or never seen, the request waits for the read and fails with
//     ErrDeadline if it overruns: the caller answers 503 + Retry-After.
//
// A read runs detached from the request that started it (context.WithoutCancel, its
// own timeout), so a read that overran one request's deadline still fills the cache
// for the retry. Concurrent misses for one payer share one read (singleflight), so a
// burst from a new buyer costs one RPC call, not N.
package balance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// Defaults, overridable per Cache.
const (
	DefaultFresh        = 5 * time.Second
	DefaultLastKnown    = 60 * time.Second
	DefaultFetchTimeout = 2 * time.Second
)

// ErrDeadline is returned when no usable balance arrived before the request's deadline.
var ErrDeadline = errors.New("balance: no usable balance before the deadline")

// Source says where a balance came from, for the decision log.
type Source string

const (
	SourceFresh     Source = "fresh"      // cache, younger than Fresh
	SourceLastKnown Source = "last_known" // cache, between Fresh and LastKnown; a refresh is under way
	SourceFetched   Source = "fetched"    // not in cache, read within the deadline
)

// Balance is a payer's balance with its provenance.
type Balance struct {
	Value  *big.Int
	Age    time.Duration // how old the cache entry was; 0 when just read
	Source Source
}

// Cache reads balances through Redis.
type Cache struct {
	Redis redis.Cmdable
	RPC   *radius.RPC
	Token evm.Address
	Log   *slog.Logger

	Fresh        time.Duration // default DefaultFresh
	LastKnown    time.Duration // default DefaultLastKnown; also the key's TTL
	FetchTimeout time.Duration // default DefaultFetchTimeout; the detached read's own limit
	Now          func() time.Time

	sf singleflight.Group
}

// Key is the Redis key for a payer. The hash tag keeps it in the same slot as the
// payer's other keys (nonce, in-flight), so a Lua script may touch all of them.
func Key(payer evm.Address) string { return "bal:{" + strings.ToLower(payer.String()) + "}" }

// Get returns the payer's balance under ctx's deadline, per the package policy.
func (c *Cache) Get(ctx context.Context, payer evm.Address) (Balance, error) {
	v, at, ok, err := c.read(ctx, payer)
	if err != nil {
		return Balance{}, err // Redis is down: the caller fails closed
	}
	if ok {
		age := c.now().Sub(at)
		switch {
		case age <= c.fresh():
			return Balance{Value: v, Age: age, Source: SourceFresh}, nil
		case age <= c.lastKnown():
			c.fetch(ctx, payer) // in the background; the next request is a hit
			return Balance{Value: v, Age: age, Source: SourceLastKnown}, nil
		}
	}
	select {
	case r := <-c.fetch(ctx, payer):
		if r.Err != nil {
			return Balance{}, r.Err
		}
		return Balance{Value: r.Val.(*big.Int), Source: SourceFetched}, nil
	case <-ctx.Done():
		return Balance{}, ErrDeadline
	}
}

// fetch starts (or joins) the one in-flight read for a payer. The read is detached
// from ctx: the request may give up, the read finishes and fills the cache.
func (c *Cache) fetch(ctx context.Context, payer evm.Address) <-chan singleflight.Result {
	return c.sf.DoChan(Key(payer), func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fetchTimeout())
		defer cancel()
		t0 := time.Now()
		v, err := c.RPC.BalanceOf(fctx, c.Token, payer)
		if err != nil {
			c.log().Warn("balanceOf failed", "payer", payer.String(), "ms", time.Since(t0).Milliseconds(), "err", err)
			return nil, err
		}
		c.log().Debug("balanceOf", "payer", payer.String(), "ms", time.Since(t0).Milliseconds())
		if err := c.write(fctx, payer, v, c.now()); err != nil {
			c.log().Warn("balance cache write failed", "payer", payer.String(), "err", err)
		}
		return v, nil
	})
}

// read is HMGET bal:{payer} v t.
func (c *Cache) read(ctx context.Context, payer evm.Address) (v *big.Int, at time.Time, ok bool, err error) {
	vals, err := c.Redis.HMGet(ctx, Key(payer), "v", "t").Result()
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("balance: read: %w", err)
	}
	vs, _ := vals[0].(string)
	ts, _ := vals[1].(string)
	if vs == "" || ts == "" {
		return nil, time.Time{}, false, nil
	}
	v, okV := new(big.Int).SetString(vs, 10)
	ms, errT := strconv.ParseInt(ts, 10, 64)
	if !okV || errT != nil {
		return nil, time.Time{}, false, nil // a corrupt entry is a miss
	}
	return v, time.UnixMilli(ms), true, nil
}

// write is HSET + PEXPIRE in one round trip. The TTL is LastKnown: past that the
// entry is useless anyway.
func (c *Cache) write(ctx context.Context, payer evm.Address, v *big.Int, at time.Time) error {
	key := Key(payer)
	_, err := c.Redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key, "v", v.String(), "t", strconv.FormatInt(at.UnixMilli(), 10))
		p.PExpire(ctx, key, c.lastKnown())
		return nil
	})
	return err
}

func (c *Cache) fresh() time.Duration {
	if c.Fresh > 0 {
		return c.Fresh
	}
	return DefaultFresh
}

func (c *Cache) lastKnown() time.Duration {
	if c.LastKnown > 0 {
		return c.LastKnown
	}
	return DefaultLastKnown
}

func (c *Cache) fetchTimeout() time.Duration {
	if c.FetchTimeout > 0 {
		return c.FetchTimeout
	}
	return DefaultFetchTimeout
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
