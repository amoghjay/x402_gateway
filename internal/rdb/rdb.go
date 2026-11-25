// Package rdb builds the Redis clients. Two shapes: the request path, where every
// command runs under a deadline of tens of milliseconds and a stale pooled connection
// is worth one quick retry; and the settler, whose blocking stream reads wait seconds.
// go-redis's defaults (5 s timeouts, a pool of 10 × GOMAXPROCS) fit neither.
package rdb

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/metrics"
)

// RequestPath is the filter's client. ContextTimeoutEnabled makes each command honour
// the request's own deadline, so the 150 ms budget, not ReadTimeout, is what cuts a
// slow Redis off; ReadTimeout is the ceiling for commands without one.
func RequestPath(addr string) *redis.Client {
	return instrumented(redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           100 * time.Millisecond,
		ReadTimeout:           50 * time.Millisecond,
		WriteTimeout:          50 * time.Millisecond,
		PoolSize:              32,
		MinIdleConns:          8,
		PoolTimeout:           20 * time.Millisecond,
		MaxRetries:            1,
		MinRetryBackoff:       2 * time.Millisecond,
		MaxRetryBackoff:       5 * time.Millisecond,
		ContextTimeoutEnabled: true,
	}))
}

// Settler is the settler's client: its XREADGROUP blocks for `block`, so the read
// timeout must outlast it.
func Settler(addr string, block time.Duration) *redis.Client {
	return instrumented(redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           time.Second,
		ReadTimeout:           block + time.Second,
		WriteTimeout:          time.Second,
		PoolSize:              4,
		MaxRetries:            3,
		ContextTimeoutEnabled: true,
	}))
}

// Ping is the readiness check: one round trip under the given timeout.
func Ping(ctx context.Context, c *redis.Client, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.Ping(ctx).Err()
}

func instrumented(c *redis.Client) *redis.Client {
	c.AddHook(hook{})
	return c
}

// hook records every command's duration and result as x402_dependency_duration_seconds,
// labelled with the command name (a small fixed set: evalsha, xadd, get, ...). A
// transaction is one observation, "multi". redis.Nil is a miss, not a failure.
type hook struct{}

func (hook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (hook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		t0 := time.Now()
		err := next(ctx, cmd)
		metrics.Dependency(metrics.DepRedis, cmd.Name(), ignoreNil(err), time.Since(t0))
		return err
	}
}

func (hook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		t0 := time.Now()
		err := next(ctx, cmds)
		metrics.Dependency(metrics.DepRedis, "multi", ignoreNil(err), time.Since(t0))
		return err
	}
}

func ignoreNil(err error) error {
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return err
}
