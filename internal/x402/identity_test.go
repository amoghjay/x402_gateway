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
		payload("eip155:84532", sbc, `{"signature":"0x2","permit2Authorization":{"from":"`+payer+`","nonce":"0xff","deadline":"1760000300"}}`),      // other signature, hex nonce
		payload("eip155:84532", "0xother", `{"permit2Authorization":{"from":"0X857B06519E91E3A54538791BDBB0E22373E36B66","nonce":"0255"}}`),         // case, leading zero, other token
		payload("eip155:84532", sbc, `{"authorization":{"from":"0xdead","nonce":"0x1"},"permit2Authorization":{"from":"`+payer+`","nonce":"255"}}`), // Permit2 read first
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
