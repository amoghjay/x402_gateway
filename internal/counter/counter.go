// Package counter keeps the Platform policy's rate-limit and free-quota counts in Redis, so
// every filter replica adds to and reads one shared count.
//
// policy.Select names the counters a request goes into; Add increments them all in one Lua
// call (one round trip, atomic) and returns each new value for policy.Decide. A counter lives
// for the rest of its fixed window: it gets an expiry of the counter's ResetS the first time it
// is incremented, and the window index in its key starts a fresh count in the next window.
//
// Keys are the gate standard's, under a prefix and with the rule uid as the hash tag:
//
//	pc:{<rule uid>}:<rate|free>:<window index>:<key>
//
// so a rule's rate-limit and free-quota counters always share a slot and one script may touch
// both, the same reason package reserve tags its keys on the payer.
package counter

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/policy"
)

// Prefix starts every counter key.
const Prefix = "pc:"

// addScript increments each key and returns the new values in order. The expiry is set when
// the counter is created, and again on any counter found without one, so a key can never
// outlive its window and pin a client over its limit for good.
var addScript = redis.NewScript(`
local out = {}
for i, key in ipairs(KEYS) do
  local n = redis.call('INCR', key)
  if n == 1 or redis.call('TTL', key) == -1 then
    redis.call('EXPIRE', key, ARGV[i])
  end
  out[i] = n
end
return out
`)

// Store adds requests to counters in Redis.
type Store struct {
	rdb *redis.Client
}

// New returns a Store on the given client. The filter passes its request-path client, whose
// commands honour the request's deadline.
func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

// Add counts the request in each counter and returns the values after adding it, in order. If
// Redis fails, every reading is Failed and the error says why; the policy's counter_error
// decides what a failed count means, so the caller logs the error and decides anyway.
func (s *Store) Add(ctx context.Context, counters []policy.Counter) ([]policy.Reading, error) {
	readings := make([]policy.Reading, len(counters))
	if len(counters) == 0 {
		return readings, nil
	}
	keys := make([]string, len(counters))
	ttls := make([]any, len(counters))
	for i, c := range counters {
		k, err := Key(c)
		if err != nil {
			return failed(readings), err
		}
		keys[i], ttls[i] = k, max(c.ResetS, 1)
	}
	values, err := addScript.Run(ctx, s.rdb, keys, ttls...).Int64Slice()
	if err == nil && len(values) != len(counters) {
		err = fmt.Errorf("the counter script returned %d values for %d counters", len(values), len(counters))
	}
	if err != nil {
		return failed(readings), fmt.Errorf("counting: %w", err)
	}
	for i, v := range values {
		readings[i].Count = uint64(max(v, 0))
	}
	return readings, nil
}

// Key is the Redis key of a counter: the standard's key with the rule uid as the hash tag.
func Key(c policy.Counter) (string, error) {
	uid, rest, ok := strings.Cut(c.Key, ":")
	if !ok || uid == "" || rest == "" {
		return "", fmt.Errorf("counter key %q does not start with a rule uid", c.Key)
	}
	return Prefix + "{" + uid + "}:" + rest, nil
}

func failed(readings []policy.Reading) []policy.Reading {
	for i := range readings {
		readings[i] = policy.Reading{Failed: true}
	}
	return readings
}
