// Package reserve is the filter's ledger in Redis: the replay guard, the per-payer
// in-flight sum, and the capture stream the settler drains. It is where the two
// findings this project exists to fix are enforced: a signature is served once
// (§10.5), and concurrent requests cannot overdraw a balance (§10.4).
//
// Keys, all hash-tagged on the payer so one Lua script may touch them together:
//
//	nonce:{payer}:<nonce>  claimed when a payment is accepted; lives until the
//	                       signature is dead on chain (validBefore) plus a margin, and
//	                       is never released: a refused or failed request costs the
//	                       buyer a new signature, which x402 clients make anyway
//	inflight:{payer}       sum of accepted-but-unsettled amounts, so the cached
//	                       balance is spent at most once; short TTL, overcounting is
//	                       the safe direction
//	bal:{payer}            the cached balance (package balance)
//
// reserve and release are Lua scripts, so each is one atomic round trip; capture is
// one XADD. The arithmetic in Lua is on doubles, exact below 2^53, which is why the
// price table caps prices (pricing.MaxPrice) and Reserve clamps the balance.
package reserve

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Defaults, overridable per Store.
const (
	DefaultInflightTTL = 30 * time.Second // reserve → capture or release takes well under this
	DefaultNonceMargin = 60 * time.Second // added to validBefore: clock skew between us and the chain
	Stream             = "settle"
	StreamMaxLen       = 100_000 // far above any realistic backlog; trimming drops jobs
)

// MaxExact is the largest integer Lua compares exactly (2^53 - 1).
var MaxExact = new(big.Int).SetUint64(1<<53 - 1)

// Outcome is what Reserve decided.
type Outcome int

const (
	OK           Outcome = iota
	Replay               // this signature was already served
	Insufficient         // balance minus in-flight is below the price
)

func (o Outcome) String() string {
	switch o {
	case OK:
		return "ok"
	case Replay:
		return "replay"
	case Insufficient:
		return "insufficient_funds"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// Reservation is an accepted payment's hold on the payer's balance.
type Reservation struct {
	Payer    evm.Address
	Nonce    [32]byte
	Amount   *big.Int
	Inflight *big.Int // the payer's in-flight sum after this reservation
}

// Store runs the scripts against one Redis.
type Store struct {
	Redis       redis.Cmdable
	InflightTTL time.Duration // default DefaultInflightTTL
	NonceMargin time.Duration // default DefaultNonceMargin
}

// Keys for a payer. Lowercase hex: the same payer must never get two slots.
func nonceKey(payer evm.Address, nonce [32]byte) string {
	return "nonce:{" + strings.ToLower(payer.String()) + "}:" + evm.Hex(nonce[:])
}

func inflightKey(payer evm.Address) string {
	return "inflight:{" + strings.ToLower(payer.String()) + "}"
}

// reserveScript: KEYS[1] nonce, KEYS[2] in-flight; ARGV price, balance, nonce TTL ms,
// in-flight TTL ms. Returns {code, inflight after}. The replay check comes first so a
// replayed signature is named as such, and the balance check before the claim so a
// refused request never burns a nonce.
var reserveScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return {1, redis.call('GET', KEYS[2]) or '0'}
end
local price = tonumber(ARGV[1])
local balance = tonumber(ARGV[2])
local inflight = tonumber(redis.call('GET', KEYS[2]) or '0')
if balance - inflight < price then
  return {2, tostring(inflight)}
end
if not redis.call('SET', KEYS[1], '1', 'NX', 'PX', ARGV[3]) then
  return {1, tostring(inflight)}
end
local after = redis.call('INCRBY', KEYS[2], ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return {0, tostring(after)}
`)

// releaseScript: KEYS[1] in-flight; ARGV amount. The key is deleted at zero so a
// payer who stops paying leaves nothing behind.
var releaseScript = redis.NewScript(`
local after = redis.call('DECRBY', KEYS[1], ARGV[1])
if after <= 0 then
  redis.call('DEL', KEYS[1])
  return 0
end
return after
`)

// Reserve claims the nonce and adds the amount to the payer's in-flight sum, if
// balance minus in-flight covers it. balance is the cached value from package
// balance; expires is the signature's validBefore.
func (s *Store) Reserve(ctx context.Context, payer evm.Address, nonce [32]byte, amount, balance *big.Int, expires time.Time) (Outcome, *Reservation, error) {
	if amount.Sign() <= 0 || amount.Cmp(MaxExact) > 0 {
		return 0, nil, fmt.Errorf("reserve: amount %s is out of range", amount)
	}
	if balance.Cmp(MaxExact) > 0 {
		balance = MaxExact // more than Lua can count is more than enough
	}
	nonceTTL := max(time.Until(expires)+s.nonceMargin(), time.Second)
	res, err := reserveScript.Run(ctx, s.Redis,
		[]string{nonceKey(payer, nonce), inflightKey(payer)},
		amount.String(), balance.String(), nonceTTL.Milliseconds(), s.inflightTTL().Milliseconds(),
	).Slice()
	if err != nil {
		return 0, nil, fmt.Errorf("reserve: %w", err)
	}
	code, _ := res[0].(int64)
	after, _ := new(big.Int).SetString(fmt.Sprint(res[1]), 10)
	switch Outcome(code) {
	case OK:
		return OK, &Reservation{Payer: payer, Nonce: nonce, Amount: amount, Inflight: after}, nil
	case Replay, Insufficient:
		return Outcome(code), nil, nil
	}
	return 0, nil, fmt.Errorf("reserve: unexpected script result %v", res)
}

// Release gives the amount back to the payer's in-flight sum: the request failed
// before anything was served, or the settlement is done. The nonce stays claimed.
func (s *Store) Release(ctx context.Context, payer evm.Address, amount *big.Int) error {
	if err := releaseScript.Run(ctx, s.Redis, []string{inflightKey(payer)}, amount.String()).Err(); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// Job is a served request's settlement, the only copy of its signed authorization.
type Job struct {
	PaymentID    string            // the filter's id for this payment (logs, receipts)
	Payer        string            // 0x address
	Route        string            // price-table key
	Amount       string            // atomic units
	Network      string            // CAIP-2
	ValidBefore  int64             // unix seconds: after this nothing can settle it
	Payment      string            // the PAYMENT-SIGNATURE header value, as received
	Requirements x402.Requirements // the route's terms the payment was verified against
	ServedAt     time.Time
}

// Capture appends the job to the settle stream. It is the last thing the response
// phase does; if it fails the caller releases the reservation and the request is
// served unpaid (and logged as such).
func (s *Store) Capture(ctx context.Context, job Job) (string, error) {
	reqs, err := json.Marshal(job.Requirements)
	if err != nil {
		return "", err
	}
	id, err := s.Redis.XAdd(ctx, &redis.XAddArgs{
		Stream: Stream,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: map[string]any{
			"paymentId":    job.PaymentID,
			"payer":        job.Payer,
			"route":        job.Route,
			"amount":       job.Amount,
			"network":      job.Network,
			"validBefore":  strconv.FormatInt(job.ValidBefore, 10),
			"payment":      job.Payment,
			"requirements": string(reqs),
			"servedAt":     strconv.FormatInt(job.ServedAt.UnixMilli(), 10),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	return id, nil
}

// ParseJob is the settler's side of Capture.
func ParseJob(values map[string]any) (Job, error) {
	str := func(k string) string { s, _ := values[k].(string); return s }
	vb, err := strconv.ParseInt(str("validBefore"), 10, 64)
	if err != nil {
		return Job{}, fmt.Errorf("job: validBefore: %w", err)
	}
	served, err := strconv.ParseInt(str("servedAt"), 10, 64)
	if err != nil {
		return Job{}, fmt.Errorf("job: servedAt: %w", err)
	}
	var reqs x402.Requirements
	if err := json.Unmarshal([]byte(str("requirements")), &reqs); err != nil {
		return Job{}, fmt.Errorf("job: requirements: %w", err)
	}
	j := Job{
		PaymentID: str("paymentId"), Payer: str("payer"), Route: str("route"), Amount: str("amount"),
		Network: str("network"), ValidBefore: vb, Payment: str("payment"), Requirements: reqs,
		ServedAt: time.UnixMilli(served),
	}
	if j.PaymentID == "" || j.Payer == "" || j.Payment == "" {
		return Job{}, fmt.Errorf("job: missing fields: %v", values)
	}
	return j, nil
}

func (s *Store) inflightTTL() time.Duration {
	if s.InflightTTL > 0 {
		return s.InflightTTL
	}
	return DefaultInflightTTL
}

func (s *Store) nonceMargin() time.Duration {
	if s.NonceMargin > 0 {
		return s.NonceMargin
	}
	return DefaultNonceMargin
}
