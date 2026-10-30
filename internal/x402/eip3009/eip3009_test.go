package eip3009

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

func sbcDomain(p radius.Profile) Domain {
	c, _ := evm.ParseAddress(radius.SBC)
	return Domain{Name: radius.SBCName, Version: radius.SBCVersion, ChainID: p.ChainID, VerifyingContract: c}
}

// The separators SBC's DOMAIN_SEPARATOR() returned on 2026-10-30 (make verify-radius).
func TestDomainSeparatorMatchesChain(t *testing.T) {
	cases := map[string]struct {
		p    radius.Profile
		want string
	}{
		"testnet": {radius.Testnet, "0x153e02f2c4163dc3c9109ee8a3e7adb2a19e6071115ec74f8573c6638068081d"},
		"mainnet": {radius.Mainnet, "0xad9d505934aea9cff020dd1fe24aafd01930aed2b6a477d2cd73d55d6f45cc27"},
	}
	for name, c := range cases {
		sep := sbcDomain(c.p).Separator()
		if got := evm.Hex(sep[:]); got != c.want {
			t.Errorf("%s: separator %s, chain says %s", name, got, c.want)
		}
	}
	if got := evm.Hex(TypeHash[:]); got != "0x7c7c6cdb67a18743f49ec6fa9b35f50d52ed05cbed4cc592e13b44501c1a2267" {
		t.Errorf("type hash %s", got)
	}
}

func loadVector(t *testing.T) (x402.EIP3009Payload, string) {
	t.Helper()
	b, err := os.ReadFile("../testdata/radius_eip3009.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Signer        string             `json:"signer"`
		Signature     string             `json:"signature"`
		Authorization x402.Authorization `json:"authorization"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return x402.EIP3009Payload{Signature: v.Signature, Authorization: v.Authorization}, v.Signer
}

// The vector was signed by eth_account; if our digest differs in any byte, the
// recovered address is not the signer.
func TestVectorRecoversToSigner(t *testing.T) {
	p, signer := loadVector(t)
	auth, err := Parse(p.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := evm.ParseSignature(p.Signature)
	if err != nil {
		t.Fatal(err)
	}
	got, err := evm.Recover(Digest(sbcDomain(radius.Testnet), auth), sig)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != signer {
		t.Errorf("recovered %s, vector signer %s", got, signer)
	}
	// Under the mainnet domain the same bytes recover someone else.
	if other, _ := evm.Recover(Digest(sbcDomain(radius.Mainnet), auth), sig); other == got {
		t.Error("chain id is not part of the digest")
	}
	if w := auth.Wire(); w != p.Authorization {
		t.Errorf("wire round trip:\n got  %+v\n want %+v", w, p.Authorization)
	}
}

func TestParseRejects(t *testing.T) {
	p, _ := loadVector(t)
	good := p.Authorization
	for name, mutate := range map[string]func(a *x402.Authorization){
		"from":        func(a *x402.Authorization) { a.From = "0x12" },
		"to":          func(a *x402.Authorization) { a.To = "" },
		"value":       func(a *x402.Authorization) { a.Value = "0x3e8" },
		"validAfter":  func(a *x402.Authorization) { a.ValidAfter = "-1" },
		"validBefore": func(a *x402.Authorization) { a.ValidBefore = "soon" },
		"nonce":       func(a *x402.Authorization) { a.Nonce = "0xabab" },
	} {
		a := good
		mutate(&a)
		if _, err := Parse(a); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
