package settle

import (
	"strings"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/reserve"
)

// hold writes a record as the filter would before /settle.
func (f *fixture) hold(id string, heldAt time.Time, validBefore time.Time, network string) reserve.Held {
	f.t.Helper()
	h := reserve.Held{
		PaymentID: id, Payer: f.w.Address().String(), Nonce: id, Network: network,
		Token: radius.Testnet.TokenAddress().String(), Route: "httproute/demo/httpbin", Amount: "1000",
		ValidBefore: validBefore.Unix(), HeldAt: heldAt,
	}
	if err := f.store.Hold(f.ctx, h); err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fixture) heldIDs() []string {
	f.t.Helper()
	hs, err := f.store.HeldBefore(f.ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for _, h := range hs {
		ids = append(ids, h.PaymentID)
	}
	return ids
}

func (f *fixture) outcomes() int {
	f.t.Helper()
	n, err := f.s.Redis.XLen(f.ctx, reserve.Outcomes).Result()
	if err != nil {
		f.t.Fatal(err)
	}
	return int(n)
}

const (
	idA = "0x" + "aa00000000000000000000000000000000000000000000000000000000000000"
	idB = "0x" + "bb00000000000000000000000000000000000000000000000000000000000000"
)

// Paid on chain, delivery never confirmed (the filter died, or answered 503 unknown):
// the payment goes on the outcomes stream once, and the record is gone.
func TestResolveHeldChargedGoesOnTheOutcomesStream(t *testing.T) {
	f := setup(t)
	f.chain.used.Store(true)
	f.hold(idA, time.Now().Add(-time.Minute), time.Now().Add(4*time.Minute), radius.Testnet.Network())
	f.s.resolveHeld(f.ctx)
	if n := f.outcomes(); n != 1 {
		t.Fatalf("outcomes = %d, want 1", n)
	}
	msgs, _ := f.s.Redis.XRange(f.ctx, reserve.Outcomes, "-", "+").Result()
	if v := msgs[0].Values; v["outcome"] != string(ChargedUnconfirmed) || v["paymentId"] != idA || v["amount"] != "1000" {
		t.Errorf("entry = %v", v)
	}
	if ids := f.heldIDs(); len(ids) != 0 {
		t.Errorf("records left: %v", ids)
	}
	if !strings.Contains(f.logs.String(), `"outcome":"charged_unconfirmed"`) {
		t.Errorf("not logged: %s", f.logs)
	}
	f.s.resolveHeld(f.ctx)
	if n := f.outcomes(); n != 1 {
		t.Errorf("outcomes = %d after a second pass, want still 1", n)
	}
}

// Unused on chain: dropped once the authorization is dead, kept while it could still land.
func TestResolveHeldUnused(t *testing.T) {
	f := setup(t)
	f.hold(idA, time.Now().Add(-10*time.Minute), time.Now().Add(-5*time.Minute), radius.Testnet.Network()) // expired
	f.hold(idB, time.Now().Add(-time.Minute), time.Now().Add(4*time.Minute), radius.Testnet.Network())     // still valid
	f.s.resolveHeld(f.ctx)
	if ids := f.heldIDs(); len(ids) != 1 || ids[0] != idB {
		t.Errorf("records left = %v, want only the still-valid one", ids)
	}
	if n := f.outcomes(); n != 0 {
		t.Errorf("outcomes = %d for unused payments, want 0", n)
	}
}

// A record younger than the grace period may belong to a settle in progress: untouched,
// even if the chain already shows it used. So does one on a network this settler lacks.
func TestResolveHeldLeavesWhatItShouldNotJudge(t *testing.T) {
	f := setup(t)
	f.chain.used.Store(true)
	f.hold(idA, time.Now(), time.Now().Add(4*time.Minute), radius.Testnet.Network())
	f.hold(idB, time.Now().Add(-time.Minute), time.Now().Add(4*time.Minute), radius.BaseSepolia.Network())
	f.s.resolveHeld(f.ctx)
	if ids := f.heldIDs(); len(ids) != 2 {
		t.Errorf("records left = %v, want both", ids)
	}
	if n := f.outcomes(); n != 0 {
		t.Errorf("outcomes = %d, want 0", n)
	}
}
