package x402

import (
	"encoding/json"
	"testing"
)

func payload(network, asset, body string) PaymentPayload {
	return PaymentPayload{X402Version: 2, Accepted: Requirements{Network: network, Asset: asset}, Payload: json.RawMessage(body)}
}

const (
	sbc   = "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb"
	payer = "0x857b06519E91e3A54538791bDbb0E22373e36b66"
)

func TestIdentifySpellsOnePaymentOneWay(t *testing.T) {
	base := payload("eip155:84532", sbc, `{"signature":"0x1","permit2Authorization":{"from":"`+payer+`","nonce":"255","deadline":"1760000300"}}`)
	a, err := Identify(base)
	if err != nil {
		t.Fatal(err)
	}
	if a.Payer != "0x857b06519e91e3a54538791bdbb0e22373e36b66" || a.Expires.Unix() != 1760000300 || len(a.ID) != 64 {
		t.Errorf("identity %+v", a)
	}
	same := []PaymentPayload{
		payload("eip155:84532", sbc, `{"signature":"0x2","permit2Authorization":{"from":"`+payer+`","nonce":"0xff","deadline":"1760000300"}}`), // other signature, hex nonce
		payload("eip155:84532", "0xother", `{"permit2Authorization":{"from":"0X857B06519E91E3A54538791BDBB0E22373E36B66","nonce":"0255"}}`),    // case, leading zero, other token
	}
	for i, pp := range same {
		if b, err := Identify(pp); err != nil || b.ID != a.ID {
			t.Errorf("same payment %d: %+v, %v", i, b, err)
		}
	}
	different := []PaymentPayload{
		payload("eip155:72344", sbc, `{"permit2Authorization":{"from":"`+payer+`","nonce":"255"}}`), // another network
		payload("eip155:84532", sbc, `{"permit2Authorization":{"from":"`+payer+`","nonce":"256"}}`), // another nonce
		payload("eip155:84532", sbc, `{"authorization":{"from":"`+payer+`","nonce":"255"}}`),        // EIP-3009, not Permit2
	}
	for i, pp := range different {
		if b, _ := Identify(pp); b.ID == a.ID {
			t.Errorf("different payment %d has the same identity", i)
		}
	}

	e1, _ := Identify(payload("eip155:72344", sbc, `{"authorization":{"from":"`+payer+`","nonce":"0x00ff","validBefore":"1760000060"}}`))
	e2, _ := Identify(payload("eip155:72344", "0x33AD9E4BD16B69B5BFDED37D8B5D9FF9ABA014FB", `{"authorization":{"from":"`+payer+`","nonce":"0xFF"}}`))
	e3, _ := Identify(payload("eip155:72344", "0x036CbD53842c5426634e7929541eC2318f3dCF7e", `{"authorization":{"from":"`+payer+`","nonce":"0xff"}}`))
	if e1.ID != e2.ID || e1.ID == e3.ID || e1.Expires.Unix() != 1760000060 {
		t.Errorf("EIP-3009: %+v %+v %+v", e1, e2, e3)
	}

	for _, body := range []string{`{}`, `{"signature":"0x1"}`, `[]`, `{"authorization":{"from":"` + payer + `"}}`,
		`{"permit2Authorization":{"from":"` + payer[2:] + `","nonce":"1"}}`, // no 0x: another spelling of the payer
		`{"permit2Authorization":{"from":"0xzz","nonce":"1"}}`,
	} {
		if _, err := Identify(payload("eip155:1", sbc, body)); err == nil {
			t.Errorf("%s: identified", body)
		}
	}
	if _, err := Identify(payload("", sbc, `{"authorization":{"from":"`+payer+`","nonce":"1"}}`)); err == nil {
		t.Error("identified without a network")
	}
}

// The payload reaches the facilitator byte for byte, read by a JSON reader that is not ours. A
// document two readers could resolve differently would let one signed payment take a new
// identity on every copy, so it is refused.
func TestIdentifyRefusesAmbiguousPayloads(t *testing.T) {
	real := `"from":"` + payer + `","nonce":"255","deadline":"1760000300"`
	for name, body := range map[string]string{
		"a second nonce in another case":  `{"permit2Authorization":{` + real + `,"Nonce":"9"}}`,
		"the same member twice":           `{"permit2Authorization":{` + real + `,"nonce":"9"}}`,
		"a second signer in another case": `{"permit2Authorization":{` + real + `,"FROM":"0x0000000000000000000000000000000000000001"}}`,
		"a decoy form in another case":    `{"authorization":{"from":"` + payer + `","nonce":"1"},"Permit2Authorization":{"from":"` + payer + `","nonce":"9"}}`,
		"both forms":                      `{"authorization":{"from":"` + payer + `","nonce":"1"},"permit2Authorization":{` + real + `}}`,
		"a name that folds to nonce":      `{"permit2Authorization":{` + real + `,"nonc\u0113":"9","\u017fignature":"0x"}}`,
		"a numeric nonce":                 `{"permit2Authorization":{"from":"` + payer + `","nonce":255}}`,
		"data after the object":           `{"permit2Authorization":{` + real + `}} {}`,
	} {
		if id, err := Identify(payload("eip155:84532", sbc, body)); err == nil {
			t.Errorf("%s: identified as %s", name, id.ID)
		}
	}
	// Other members, and nested objects, are nobody's business here.
	ok := `{"signature":"0x1","permit2Authorization":{` + real + `,"permitted":{"token":"0x1","Token":"0x2"},"spender":"0x3"}}`
	id, err := Identify(payload("eip155:84532", sbc, ok))
	if err != nil || id.Method != TransferPermit2 {
		t.Errorf("a plain payment: %+v, %v", id, err)
	}
	if id, err := Identify(payload("eip155:72344", sbc, `{"authorization":{"from":"`+payer+`","nonce":"0x1"}}`)); err != nil || id.Method != TransferEIP3009 {
		t.Errorf("an EIP-3009 payment: %+v, %v", id, err)
	}
}
