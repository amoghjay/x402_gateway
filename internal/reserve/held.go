package reserve

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// The record of Sync payments whose outcome the filter has not confirmed.
//
// A Sync route settles while Envoy holds the response. Most settles end with a known
// outcome and a response the client gets. Two ways they do not: the filter cannot tell
// whether the transfer happened (it answers 503 settlement_unknown), or the filter dies
// or loses the stream after the transfer and before Envoy has the response. In both the
// buyer may have paid for nothing, and in the second the process that knew is gone, so
// no log line can say so.
//
// So the filter writes the payment here BEFORE it calls /settle, and removes it only
// once the outcome is settled for good: the response handed to Envoy, or a refusal
// (nothing charged). Whatever is left behind is resolved by the settler from the chain
// (authorizationState): used means the buyer paid and the gate did not confirm
// delivery, and the payment goes on the Outcomes stream, the seller's list to refund or
// redeliver from; unused past validBefore means nobody was charged, and it is dropped.
//
//	held                 sorted set: paymentId, scored by the time it was written (ms)
//	held:<paymentId>     hash: the payment's details
//	outcomes             stream: payments charged without a confirmed delivery
const (
	HeldSet  = "held"
	Outcomes = "outcomes"
)

func heldKey(id string) string { return HeldSet + ":" + id }

// Held is one payment written before its /settle.
type Held struct {
	PaymentID   string // the authorization nonce, hex
	Payer       string
	Nonce       string // hex, as authorizationState takes it
	Network     string // CAIP-2
	Token       string
	Route       string
	Amount      string // atomic units
	ValidBefore int64  // unix seconds
	HeldAt      time.Time
}

// Hold writes the record. It must succeed before /settle is called: a settle with no
// record could charge a buyer with nobody the wiser.
func (s *Store) Hold(ctx context.Context, h Held) error {
	if h.PaymentID == "" {
		return errors.New("reserve: hold without a payment id")
	}
	_, err := s.Redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, heldKey(h.PaymentID), map[string]any{
			"paymentId": h.PaymentID, "payer": h.Payer, "nonce": h.Nonce, "network": h.Network,
			"token": h.Token, "route": h.Route, "amount": h.Amount,
			"validBefore": h.ValidBefore, "heldAt": h.HeldAt.UnixMilli(),
		})
		p.ZAdd(ctx, HeldSet, redis.Z{Score: float64(h.HeldAt.UnixMilli()), Member: h.PaymentID})
		return nil
	})
	return err
}

// Unhold removes the record: the outcome is known and nothing is owed.
func (s *Store) Unhold(ctx context.Context, id string) error {
	_, err := s.Redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, HeldSet, id)
		p.Del(ctx, heldKey(id))
		return nil
	})
	return err
}

// HeldBefore lists up to n records written before cutoff, oldest first.
func (s *Store) HeldBefore(ctx context.Context, cutoff time.Time, n int64) ([]Held, error) {
	ids, err := s.Redis.ZRangeByScore(ctx, HeldSet, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(cutoff.UnixMilli(), 10), Count: n,
	}).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Held, 0, len(ids))
	for _, id := range ids {
		m, err := s.Redis.HGetAll(ctx, heldKey(id)).Result()
		if err != nil {
			return nil, err
		}
		vb, _ := strconv.ParseInt(m["validBefore"], 10, 64)
		at, _ := strconv.ParseInt(m["heldAt"], 10, 64)
		out = append(out, Held{
			PaymentID: id, Payer: m["payer"], Nonce: m["nonce"], Network: m["network"], Token: m["token"],
			Route: m["route"], Amount: m["amount"], ValidBefore: vb, HeldAt: time.UnixMilli(at),
		})
	}
	return out, nil
}

// recordScript: KEYS[1] held set, KEYS[2] held hash, KEYS[3] outcomes; ARGV[1] id,
// then field/value pairs. Only the caller that removes the id from the set writes the
// outcome, so two settlers cannot record one payment twice.
var recordScript = redis.NewScript(`
if redis.call('ZREM', KEYS[1], ARGV[1]) == 0 then
  return 0
end
local fields = {}
for i = 2, #ARGV do fields[#fields + 1] = ARGV[i] end
redis.call('XADD', KEYS[3], '*', unpack(fields))
redis.call('DEL', KEYS[2])
return 1
`)

// Record moves a held payment onto the Outcomes stream with the given outcome. It
// reports whether this call did it (false: another settler already had).
func (s *Store) Record(ctx context.Context, h Held, outcome string, at time.Time) (bool, error) {
	n, err := recordScript.Run(ctx, s.Redis, []string{HeldSet, heldKey(h.PaymentID), Outcomes},
		h.PaymentID,
		"outcome", outcome, "paymentId", h.PaymentID, "payer", h.Payer, "route", h.Route,
		"amount", h.Amount, "network", h.Network, "heldAt", h.HeldAt.UTC().Format(time.RFC3339),
		"resolvedAt", at.UTC().Format(time.RFC3339),
	).Int()
	return n == 1, err
}
