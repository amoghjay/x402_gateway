package reserve

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestHeldLifecycle(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	s := &Store{Redis: c}
	ctx := t.Context()
	t0 := time.Unix(1_900_000_000, 0)

	a := Held{PaymentID: "0xaa", Payer: "0x1", Nonce: "0xaa", Network: "eip155:72344", Token: "0x2", Route: "httproute/demo/x", Amount: "1000", ValidBefore: 1_900_000_300, HeldAt: t0}
	b := a
	b.PaymentID, b.Nonce, b.HeldAt = "0xbb", "0xbb", t0.Add(time.Minute)
	for _, h := range []Held{a, b} {
		if err := s.Hold(ctx, h); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.HeldBefore(ctx, t0.Add(30*time.Second), 10)
	if err != nil || len(got) != 1 || got[0] != a {
		t.Fatalf("HeldBefore = %+v, %v; want only the older one, intact", got, err)
	}

	if err := s.Unhold(ctx, "0xbb"); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(heldKey("0xbb")) {
		t.Error("unheld record's hash is still there")
	}

	done, err := s.Record(ctx, a, "charged_unconfirmed", t0.Add(time.Hour))
	if err != nil || !done {
		t.Fatalf("Record = %v, %v", done, err)
	}
	again, err := s.Record(ctx, a, "charged_unconfirmed", t0.Add(time.Hour))
	if err != nil || again {
		t.Errorf("second Record = %v, %v; want false (recorded once)", again, err)
	}
	msgs, err := c.XRange(ctx, Outcomes, "-", "+").Result()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("outcomes = %v, %v", msgs, err)
	}
	if v := msgs[0].Values; v["outcome"] != "charged_unconfirmed" || v["paymentId"] != "0xaa" || v["amount"] != "1000" || v["route"] != "httproute/demo/x" {
		t.Errorf("outcome entry = %v", v)
	}
	if left, _ := s.HeldBefore(ctx, t0.Add(time.Hour), 10); len(left) != 0 {
		t.Errorf("records left: %+v", left)
	}
}
