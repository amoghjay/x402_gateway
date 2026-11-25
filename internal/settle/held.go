package settle

import (
	"context"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/metrics"
	"github.com/amoghjay/x402_gateway/internal/reserve"
)

// Resolving the filter's held records (reserve.Held): Sync payments whose outcome the
// filter did not confirm. The chain decides each one.
const (
	// DefaultHeldGrace is how old a record must be before it is looked at: well past any
	// Sync settle in progress (Envoy gives up on a held response after 10 s).
	DefaultHeldGrace    = 30 * time.Second
	DefaultResolveEvery = 30 * time.Second
	heldBatch           = 100
	heldReadTimeout     = 5 * time.Second
	// heldExpiryMargin is added to validBefore before an unused authorization is called
	// dead: clock skew between us and the chain, as for the nonce claim.
	heldExpiryMargin = 60 * time.Second
)

const (
	ChargedUnconfirmed Outcome = "charged_unconfirmed" // paid on chain; the gate never confirmed the response was delivered
	HeldNotCharged     Outcome = "held_not_charged"    // expired unused: nobody was charged
)

// resolveHeld looks at every record older than the grace period:
//
//	nonce used on chain        → charged without a confirmed delivery: onto the
//	                             outcomes stream, the seller's refund list
//	unused, past validBefore   → can never be charged: dropped
//	unused, still valid        → could still land: asked again next time
//
// A record it cannot read (a network this settler does not serve, an RPC error) stays.
func (s *Settler) resolveHeld(ctx context.Context) {
	now := s.now()
	hs, err := s.Reserve.HeldBefore(ctx, now.Add(-s.heldGrace()), heldBatch)
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Error("reading held payments", "err", err)
		}
		return
	}
	for _, h := range hs {
		log := s.Log.With("paymentId", h.PaymentID, "payer", h.Payer, "route", h.Route, "amount", h.Amount, "network", h.Network)
		rpc := s.RPCs[h.Network]
		if rpc == nil {
			log.Warn("held payment on a network this settler does not serve; left for one that does")
			continue
		}
		token, err1 := evm.ParseAddress(h.Token)
		payer, err2 := evm.ParseAddress(h.Payer)
		nonce, err3 := evm.ParseBytes32(h.Nonce)
		if err1 != nil || err2 != nil || err3 != nil {
			log.Error("held payment record is malformed; left in place for a person to look at", "token", h.Token, "nonce", h.Nonce)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, heldReadTimeout)
		used, err := rpc.AuthorizationState(cctx, token, payer, nonce)
		cancel()
		switch {
		case err != nil:
			log.Warn("could not read authorizationState for a held payment; asking again later", "err", err)
		case used:
			done, err := s.Reserve.Record(ctx, h, string(ChargedUnconfirmed), now)
			if err != nil {
				log.Error("could not record a held payment's outcome; asking again later", "err", err)
			} else if done {
				metrics.SettlerOutcome(h.Network, string(ChargedUnconfirmed))
				log.Error("charged without a confirmed delivery: on the outcomes stream for a refund or redelivery",
					"outcome", ChargedUnconfirmed, "stream", reserve.Outcomes, "heldAt", h.HeldAt.UTC().Format(time.RFC3339))
			}
		case now.After(time.Unix(h.ValidBefore, 0).Add(heldExpiryMargin)):
			if err := s.Reserve.Unhold(ctx, h.PaymentID); err != nil {
				log.Warn("could not drop an expired held payment", "err", err)
			} else {
				metrics.SettlerOutcome(h.Network, string(HeldNotCharged))
				log.Info("held payment expired unused: nobody was charged", "outcome", HeldNotCharged)
			}
		}
	}
}

func (s *Settler) heldGrace() time.Duration {
	if s.HeldGrace > 0 {
		return s.HeldGrace
	}
	return DefaultHeldGrace
}

func (s *Settler) resolveEvery() time.Duration {
	if s.ResolveEvery > 0 {
		return s.ResolveEvery
	}
	return DefaultResolveEvery
}
