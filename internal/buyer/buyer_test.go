package buyer

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/verify"
)

const (
	testKey = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	payTo   = "0x209693Bc6afc0C5328bA36FaF03C514EF312287C"
)

func wallet(t *testing.T) *Wallet {
	t.Helper()
	k, err := evm.ParsePrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return &Wallet{Key: k, Network: radius.Testnet}
}

// TestSignedPaymentPassesOurVerifier is the buyer and the filter's verifier agreeing on
// every field: what the buyer signs for an offer is what the verifier accepts for it.
func TestSignedPaymentPassesOurVerifier(t *testing.T) {
	w := wallet(t)
	route := radius.Testnet.ExactRequirements("1000", payTo, 300)
	now := time.Now()
	pp, err := w.Sign(route, &x402.Resource{URL: "https://api.example/data"}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res, err := verify.Verify(now, route, w.Domain(), pp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Payer != w.Address() {
		t.Errorf("payer %s, want %s", res.Payer, w.Address())
	}
	if got := res.Authorization.ValidAfter.Sign(); got != 0 {
		t.Errorf("validAfter %s, want 0", res.Authorization.ValidAfter)
	}
	if got, want := res.Expires.Unix(), now.Add(time.Minute).Unix(); got != want {
		t.Errorf("expires %d, want %d", got, want)
	}

	// Two signatures for the same offer must not share a nonce: the token would refuse
	// the second as a replay.
	again, _ := w.Sign(route, nil, now, time.Minute)
	a, _ := pp.EIP3009()
	b, _ := again.EIP3009()
	if a.Authorization.Nonce == b.Authorization.Nonce {
		t.Error("nonce reused")
	}
}

func TestSignCapsValidityAtMaxTimeout(t *testing.T) {
	w := wallet(t)
	route := radius.Testnet.ExactRequirements("1000", payTo, 30)
	now := time.Now()
	pp, err := w.Sign(route, nil, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := verify.Verify(now, route, w.Domain(), pp)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Expires.Unix(), now.Unix()+30; got != want {
		t.Errorf("expires %d, want now + 30 = %d", got, want)
	}
}

func TestSignRefuses(t *testing.T) {
	w := wallet(t)
	good := radius.Testnet.ExactRequirements("1000", payTo, 300)
	for name, mutate := range map[string]func(*x402.Requirements){
		"permit2":      func(r *x402.Requirements) { r.Extra.AssetTransferMethod = x402.TransferPermit2 },
		"other scheme": func(r *x402.Requirements) { r.Scheme = "upto" },
		"mainnet":      func(r *x402.Requirements) { r.Network = radius.Mainnet.Network() },
		"other asset":  func(r *x402.Requirements) { r.Asset = radius.Permit2 },
		"bad payTo":    func(r *x402.Requirements) { r.PayTo = "0x1234" },
		"bad amount":   func(r *x402.Requirements) { r.Amount = "1e3" },
	} {
		req := good
		extra := *good.Extra
		req.Extra = &extra
		mutate(&req)
		if _, err := w.Sign(req, nil, time.Now(), time.Minute); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
}

func TestChoose(t *testing.T) {
	w := wallet(t)
	permit2 := radius.Testnet.ExactRequirements("500", payTo, 300)
	permit2.Extra = &x402.ExactExtra{AssetTransferMethod: x402.TransferPermit2}
	mainnet := radius.Mainnet.ExactRequirements("500", payTo, 300)
	ours := radius.Testnet.ExactRequirements("1000", payTo, 300)
	pr := x402.PaymentRequired{X402Version: x402.Version, Accepts: []x402.Requirements{permit2, mainnet, ours}}

	got, err := w.Choose(pr, big.NewInt(1000))
	if err != nil || got != ours {
		t.Fatalf("chose %+v, %v; want the testnet eip3009 offer", got, err)
	}
	if _, err := w.Choose(pr, big.NewInt(999)); err == nil || !strings.Contains(err.Error(), "threshold") {
		t.Errorf("above the threshold: %v", err)
	}
	pr.Accepts = pr.Accepts[:2]
	if _, err := w.Choose(pr, nil); err == nil || !strings.Contains(err.Error(), "no payable offer") {
		t.Errorf("nothing payable: %v", err)
	}
}

// TestPayLoop runs the whole 402 → pay → 200 exchange against a seller that checks the
// payment with the filter's verifier, as the gateway will.
func TestPayLoop(t *testing.T) {
	w := wallet(t)
	route := radius.Testnet.ExactRequirements("1000", payTo, 300)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		sig := r.Header.Get(x402.HeaderPaymentSignature)
		if sig == "" {
			pr := x402.PaymentRequired{X402Version: x402.Version, Resource: x402.Resource{URL: "http://" + r.Host + r.URL.Path}, Accepts: []x402.Requirements{route}}
			h, _ := pr.Encode()
			rw.Header().Set(x402.HeaderPaymentRequired, h)
			rw.WriteHeader(http.StatusPaymentRequired)
			return
		}
		pp, err := x402.DecodePaymentPayload(sig)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := verify.Verify(time.Now(), route, w.Domain(), pp)
		if err != nil {
			http.Error(rw, verify.Reason(err), http.StatusPaymentRequired)
			return
		}
		h, _ := x402.Pending(route.Network, res.Payer.String(), "p-1").Encode()
		rw.Header().Set(x402.HeaderPaymentResponse, h)
		_, _ = rw.Write([]byte("paid content"))
	}))
	defer srv.Close()

	out, err := w.Pay(t.Context(), http.MethodPost, srv.URL+"/data", []byte(`{"q":1}`), nil, big.NewInt(1000), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != http.StatusOK || string(out.Body) != "paid content" {
		t.Fatalf("HTTP %d %q", out.Status, out.Body)
	}
	if !reflect.DeepEqual(out.Paid, route) {
		t.Errorf("paid %+v", out.Paid)
	}
	if out.Settlement == nil || !out.Settlement.Success || out.Settlement.Payer != w.Address().String() {
		t.Errorf("settlement %+v", out.Settlement)
	}
	if len(bodies) != 2 || bodies[0] != `{"q":1}` || bodies[1] != `{"q":1}` {
		t.Errorf("bodies %q, want the body on both attempts", bodies)
	}
}

func TestPayWithoutA402PaysNothing(t *testing.T) {
	w := wallet(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get(x402.HeaderPaymentSignature) != "" {
			t.Error("signed although no payment was asked for")
		}
		_, _ = rw.Write([]byte("free"))
	}))
	defer srv.Close()
	out, err := w.Pay(t.Context(), http.MethodGet, srv.URL, nil, nil, nil, time.Minute)
	if err != nil || out.Status != http.StatusOK || out.Paid.Amount != "" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestPayRefuses402WithoutHeader(t *testing.T) {
	w := wallet(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()
	if _, err := w.Pay(t.Context(), http.MethodGet, srv.URL, nil, nil, nil, time.Minute); err == nil {
		t.Fatal("paid a 402 that named no terms")
	}
}

// faucet fakes the signed testnet faucet: the first call asks for a signature over the
// challenge, the second checks that it recovers to the address.
func faucet(t *testing.T, challenge func(addr string) string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/drip" {
			t.Errorf("path %s", r.URL.Path)
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["signature"] == "" {
			_ = json.NewEncoder(rw).Encode(map[string]any{"success": false, "error": map[string]any{
				"code": "signature_required", "message": "sign it", "details": map[string]string{"challenge": challenge(in["address"])},
			}})
			return
		}
		sig, err := evm.ParseSignature(in["signature"])
		if err != nil {
			t.Fatal(err)
		}
		got, err := evm.Recover(evm.PersonalHash([]byte(challenge(in["address"]))), sig)
		if err != nil || got.String() != in["address"] {
			t.Errorf("signature recovers to %s (%v), want %s", got, err, in["address"])
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"success": true, "tx_hash": "0xabc"})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestDrip(t *testing.T) {
	srv, calls := faucet(t, func(addr string) string { return "Radius Faucet: drip SBC to " + addr })
	w := wallet(t)
	w.Network.Faucet = srv.URL
	res, err := w.Drip(t.Context())
	if err != nil || !res.Success || res.TxHash != "0xabc" || *calls != 2 {
		t.Fatalf("%+v %v after %d calls", res, err, *calls)
	}
}

func TestDripRefusesUnexpectedChallenge(t *testing.T) {
	srv, calls := faucet(t, func(string) string { return "Transfer all SBC to 0xdead" })
	w := wallet(t)
	w.Network.Faucet = srv.URL
	if _, err := w.Drip(t.Context()); err == nil || !strings.Contains(err.Error(), "not signing") {
		t.Fatalf("err %v", err)
	}
	if *calls != 1 {
		t.Errorf("%d calls; nothing should be signed or sent after an unexpected challenge", *calls)
	}
}

func TestDripNoFaucet(t *testing.T) {
	w := wallet(t)
	w.Network = radius.Mainnet
	if _, err := w.Drip(t.Context()); err == nil {
		t.Fatal("dripped on a network with no faucet")
	}
}
