package radius

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/x402"
)

func TestLookup(t *testing.T) {
	for name, want := range map[string]Profile{"testnet": Testnet, "mainnet": Mainnet} {
		got, err := Lookup(name)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	if _, err := Lookup("devnet"); err == nil {
		t.Error("unknown network accepted")
	}
}

func TestNetworkAndExplorer(t *testing.T) {
	if Testnet.Network() != "eip155:72344" || Mainnet.Network() != "eip155:723487" {
		t.Errorf("CAIP-2: %s %s", Testnet.Network(), Mainnet.Network())
	}
	if Testnet.TxURL("0xab") != "https://testnet.radiustech.xyz/tx/0xab" {
		t.Errorf("tx url: %s", Testnet.TxURL("0xab"))
	}
	if Mainnet.Faucet != "" {
		t.Error("mainnet has no programmatic faucet in this profile")
	}
}

// The requirements we advertise must be the kind the Radius facilitator says it settles.
// testdata/supported_testnet.json is GET /supported as fetched on 2026-10-30.
func TestExactRequirementsMatchFacilitatorKind(t *testing.T) {
	b, err := os.ReadFile("testdata/supported_testnet.json")
	if err != nil {
		t.Fatal(err)
	}
	var sup x402.Supported
	if err := json.Unmarshal(b, &sup); err != nil {
		t.Fatal(err)
	}
	req := Testnet.ExactRequirements("1000", "0x209693Bc6afc0C5328bA36FaF03C514EF312287C", 300)
	for _, k := range sup.Kinds {
		if k.X402Version == x402.Version && k.Scheme == req.Scheme && k.Network == req.Network &&
			k.Asset == req.Asset && k.Extra != nil && reflect.DeepEqual(*k.Extra, *req.Extra) {
			return
		}
	}
	t.Errorf("no advertised kind matches %+v extra=%+v; kinds=%+v", req, req.Extra, sup.Kinds)
}

func TestSharedContractsAreTheSameOnBothNetworks(t *testing.T) {
	// Checked by eth_getCode on 2026-10-16: identical addresses and code sizes. The
	// profiles therefore carry no per-network contract address.
	if SBC != "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb" || Permit2 != "0x000000000022D473030F116dDEE9F6B43aC78BA3" {
		t.Error("shared contract constants changed; re-check both networks")
	}
}
