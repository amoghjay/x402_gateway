// Package settle drains the capture stream: each job is a served request's signed
// authorization, and the settler is what turns it into a transfer on chain through
// the facilitator's /settle. It runs after the response, off the request path, which
// is the thesis: serving and settling cannot both fit inside Envoy's budget.
//
// No payment is lost: a job is acknowledged only once its outcome is known. A settle
// that succeeds, or is refused for good (the chain or the facilitator said no), is
// done. One whose outcome is uncertain (a timeout, a 5xx, no answer) is checked on
// chain first: authorizationState(payer, nonce) says whether the transfer happened,
// and the job is retried only if it did not. Retries are redeliveries: a job left
// pending is claimed back after MinIdle by XAUTOCLAIM, from any consumer, including a
// restarted one with a new name. A job that cannot ever settle (past validBefore,
// malformed, too many deliveries) goes to the dead-letter stream, acknowledged, and is
// logged: it is money the seller did not get.
//
// The in-flight amount the filter reserved is released here, when the job is done,
// so the cached balance cannot be spent twice before the chain catches up.
package settle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

// Defaults, overridable per Settler.
const (
	DefaultGroup         = "settlers"
	DefaultBlock         = 5 * time.Second
	DefaultCount         = 10
	DefaultClaimEvery    = 30 * time.Second
	DefaultMinIdle       = 60 * time.Second
	DefaultMaxDeliveries = 5
	DefaultSettleTimeout = 5 * time.Second
	DeadStream           = reserve.Stream + ":dead"
	walletPoolRetry      = 2 * time.Second // the facilitator's documented "retry in 1-2 s"
)

// Outcome labels one job's end, for the log (and later the metrics).
type Outcome string

const (
	Settled          Outcome = "settled"            // on chain, through /settle
	SettledOnChain   Outcome = "settled_on_chain"   // /settle was uncertain, but the chain says the nonce is used
	SettleFailed     Outcome = "settle_failed"      // refused for good: the seller is not paid
	SettleUncertain  Outcome = "settle_uncertain"   // left pending for a retry
	ExpiredUnsettled Outcome = "expired_unsettled"  // past validBefore: dead-lettered
	DeadLettered     Outcome = "dead_lettered"      // malformed or over the delivery limit
	Claimed          Outcome = "claimed"            // taken over from an idle consumer
)

// Settler is one consumer of the settle stream.
type Settler struct {
	Redis        *redis.Client
	Facilitators map[string]*facilitator.Client // by CAIP-2 network
	RPCs         map[string]*radius.RPC         // by CAIP-2 network, for authorizationState
	Reserve      *reserve.Store
	Log          *slog.Logger
	Consumer     string // this replica's name in the group (the pod name)

	Group         string
	Block         time.Duration
	Count         int64
	ClaimEvery    time.Duration
	MinIdle       time.Duration
	MaxDeliveries int64
	SettleTimeout time.Duration
	Now           func() time.Time
}

// Run settles until ctx is done. It creates the group if needed, claims what an idle
// consumer left behind on start and then every ClaimEvery, and otherwise blocks on
// new jobs.
func (s *Settler) Run(ctx context.Context) error {
	if err := s.ensureGroup(ctx); err != nil {
		return err
	}
	s.claim(ctx)
	t := time.NewTicker(s.claimEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.claim(ctx)
		default:
		}
		if _, err := s.step(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("reading the settle stream", "err", err)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// ensureGroup creates the consumer group at the stream's start, so jobs captured
// before any settler ran are delivered too. An existing group is fine.
func (s *Settler) ensureGroup(ctx context.Context) error {
	err := s.Redis.XGroupCreateMkStream(ctx, reserve.Stream, s.group(), "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("settle: create group: %w", err)
	}
	return nil
}

// step reads one batch of new jobs and handles them. It returns how many it got.
func (s *Settler) step(ctx context.Context) (int, error) {
	streams, err := s.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: s.group(), Consumer: s.Consumer, Streams: []string{reserve.Stream, ">"},
		Count: s.count(), Block: s.block(),
	}).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range streams {
		for _, msg := range st.Messages {
			s.handle(ctx, msg)
			n++
		}
	}
	return n, nil
}

// claim takes over jobs that have sat with any consumer for longer than MinIdle: a
// settler that died mid-job, or one that left a job pending for a retry.
func (s *Settler) claim(ctx context.Context) {
	start := "0-0"
	for {
		msgs, next, err := s.Redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: reserve.Stream, Group: s.group(), Consumer: s.Consumer,
			MinIdle: s.minIdle(), Start: start, Count: 100,
		}).Result()
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Error("claiming idle jobs", "err", err)
			}
			return
		}
		for _, msg := range msgs {
			s.Log.Info("claimed an idle job", "outcome", Claimed, "id", msg.ID)
			s.handle(ctx, msg)
		}
		if next == "0-0" || len(msgs) == 0 {
			return
		}
		start = next
	}
}

// handle settles one job to a known outcome, or leaves it pending.
func (s *Settler) handle(ctx context.Context, msg redis.XMessage) {
	log := s.Log.With("id", msg.ID)
	job, err := reserve.ParseJob(msg.Values)
	if err != nil {
		s.dead(ctx, log, msg, nil, DeadLettered, err.Error())
		return
	}
	log = log.With("paymentId", job.PaymentID, "payer", job.Payer, "amount", job.Amount, "route", job.Route)

	if n := s.deliveries(ctx, msg.ID); n > s.maxDeliveries() {
		s.dead(ctx, log, msg, &job, DeadLettered, fmt.Sprintf("delivered %d times", n))
		return
	}
	if s.now().Unix() >= job.ValidBefore {
		s.dead(ctx, log, msg, &job, ExpiredUnsettled, "validBefore has passed; nothing can settle it now")
		return
	}
	fac, rpc := s.Facilitators[job.Network], s.RPCs[job.Network]
	if fac == nil || rpc == nil {
		s.dead(ctx, log, msg, &job, DeadLettered, "no facilitator or RPC for network "+job.Network)
		return
	}
	pp, err := x402.DecodePaymentPayload(job.Payment)
	if err != nil {
		s.dead(ctx, log, msg, &job, DeadLettered, "payment: "+err.Error())
		return
	}

	sctx, cancel := context.WithTimeout(ctx, s.settleTimeout())
	resp, err := fac.Settle(sctx, pp, job.Requirements)
	cancel()
	t0 := s.now()
	if err == nil && resp.Success {
		s.done(ctx, log, msg, &job, Settled, "tx", resp.Transaction, "url", s.txURL(job.Network, resp.Transaction), "ms", time.Since(t0).Milliseconds())
		return
	}

	// A failure of any kind: ask the chain before believing it. A used nonce means the
	// transfer happened (this attempt, or an earlier one whose answer was lost).
	var st *facilitator.Status
	definite := errors.As(err, &st) && st.Definite()
	if definite && strings.Contains(st.Body, "No available wallets") {
		// The facilitator's own pool is empty: its documented answer is to retry shortly.
		definite = false
		log.Warn("facilitator has no wallet free; retrying", "outcome", SettleUncertain)
		select {
		case <-time.After(walletPoolRetry):
		case <-ctx.Done():
			return
		}
	}
	if used, serr := s.usedOnChain(ctx, rpc, pp); serr == nil && used {
		s.done(ctx, log, msg, &job, SettledOnChain, "settleErr", errString(err))
		return
	} else if serr != nil {
		log.Warn("authorizationState check failed", "err", serr)
	}
	if definite {
		reason := resp.ErrorReason
		if reason == "" {
			reason = st.Body
		}
		s.done(ctx, log, msg, &job, SettleFailed, "reason", reason)
		return
	}
	// Uncertain: leave it pending; claim() brings it back after MinIdle.
	log.Warn("settle uncertain; left pending for a retry", "outcome", SettleUncertain, "err", errString(err))
}

// usedOnChain reads authorizationState for the job's payer and nonce.
func (s *Settler) usedOnChain(ctx context.Context, rpc *radius.RPC, pp x402.PaymentPayload) (bool, error) {
	raw, err := pp.EIP3009()
	if err != nil {
		return false, err
	}
	auth, err := eip3009.Parse(raw.Authorization)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.settleTimeout())
	defer cancel()
	return rpc.AuthorizationState(ctx, radius.SBCAddress, auth.From, auth.Nonce)
}

// done releases the in-flight amount and acknowledges the job: its outcome is known.
func (s *Settler) done(ctx context.Context, log *slog.Logger, msg redis.XMessage, job *reserve.Job, o Outcome, kv ...any) {
	s.release(ctx, log, job)
	if err := s.Redis.XAck(ctx, reserve.Stream, s.group(), msg.ID).Err(); err != nil {
		// Not acknowledged: it comes back through claim(), settles again, and the chain
		// (or authorizationState) says it is already done. Nothing is charged twice.
		log.Error("ack failed; the job will be redelivered", "outcome", o, "err", err)
		return
	}
	if o == SettleFailed {
		log.Error("not settled: the seller is not paid", append([]any{"outcome", o}, kv...)...)
		return
	}
	log.Info("settled", append([]any{"outcome", o}, kv...)...)
}

// dead moves a job that can never settle to the dead-letter stream and acknowledges
// it. Logged at error: it is a served request the seller was not paid for.
func (s *Settler) dead(ctx context.Context, log *slog.Logger, msg redis.XMessage, job *reserve.Job, o Outcome, why string) {
	values := make(map[string]any, len(msg.Values)+3)
	for k, v := range msg.Values {
		values[k] = v
	}
	values["reason"], values["outcome"], values["deadAt"] = why, string(o), s.now().UTC().Format(time.RFC3339)
	if err := s.Redis.XAdd(ctx, &redis.XAddArgs{Stream: DeadStream, Values: values}).Err(); err != nil {
		log.Error("dead-letter write failed; leaving the job pending", "outcome", o, "why", why, "err", err)
		return
	}
	s.release(ctx, log, job)
	if err := s.Redis.XAck(ctx, reserve.Stream, s.group(), msg.ID).Err(); err != nil {
		log.Error("ack failed after dead-lettering", "err", err)
	}
	log.Error("dead-lettered: the seller is not paid", "outcome", o, "why", why)
}

// release returns the job's amount to the payer's in-flight sum.
func (s *Settler) release(ctx context.Context, log *slog.Logger, job *reserve.Job) {
	if job == nil {
		return
	}
	payer, err := evm.ParseAddress(job.Payer)
	amount, ok := new(big.Int).SetString(job.Amount, 10)
	if err != nil || !ok {
		return // the in-flight TTL is the backstop
	}
	if err := s.Reserve.Release(ctx, payer, amount); err != nil {
		log.Warn("in-flight release failed; the TTL will clear it", "err", err)
	}
}

// deliveries is how many times the job has been delivered, from the pending list.
func (s *Settler) deliveries(ctx context.Context, id string) int64 {
	p, err := s.Redis.XPendingExt(ctx, &redis.XPendingExtArgs{Stream: reserve.Stream, Group: s.group(), Start: id, End: id, Count: 1}).Result()
	if err != nil || len(p) == 0 {
		return 1
	}
	return p[0].RetryCount
}

func (s *Settler) txURL(network, tx string) string {
	if tx == "" {
		return ""
	}
	p, err := radius.ByNetwork(network)
	if err != nil {
		return ""
	}
	return p.TxURL(tx)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Settler) group() string {
	if s.Group != "" {
		return s.Group
	}
	return DefaultGroup
}

func (s *Settler) block() time.Duration {
	if s.Block > 0 {
		return s.Block
	}
	return DefaultBlock
}

func (s *Settler) count() int64 {
	if s.Count > 0 {
		return s.Count
	}
	return DefaultCount
}

func (s *Settler) claimEvery() time.Duration {
	if s.ClaimEvery > 0 {
		return s.ClaimEvery
	}
	return DefaultClaimEvery
}

func (s *Settler) minIdle() time.Duration {
	if s.MinIdle > 0 {
		return s.MinIdle
	}
	return DefaultMinIdle
}

func (s *Settler) maxDeliveries() int64 {
	if s.MaxDeliveries > 0 {
		return s.MaxDeliveries
	}
	return DefaultMaxDeliveries
}

func (s *Settler) settleTimeout() time.Duration {
	if s.SettleTimeout > 0 {
		return s.SettleTimeout
	}
	return DefaultSettleTimeout
}

func (s *Settler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
