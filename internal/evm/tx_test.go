package evm

import (
	"encoding/hex"
	"math/big"
	"testing"
)

// The worked example from EIP-155 itself: key 0x4646…46, nonce 9, 20 gwei, 21000 gas,
// 1 ether to 0x3535…35 on chain 1. The raw transaction and hash are the ones the EIP prints.
func TestSignLegacyEIP155Vector(t *testing.T) {
	key, err := ParsePrivateKey("0x4646464646464646464646464646464646464646464646464646464646464646")
	if err != nil {
		t.Fatal(err)
	}
	to, _ := ParseAddress("0x3535353535353535353535353535353535353535")
	value, _ := new(big.Int).SetString("1000000000000000000", 10)
	raw, _, err := SignLegacy(key, 1, LegacyTx{Nonce: 9, GasPrice: big.NewInt(20_000_000_000), Gas: 21000, To: to, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	want := "f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"
	if got := hex.EncodeToString(raw); got != want {
		t.Fatalf("raw\n got %s\nwant %s", got, want)
	}
	if _, _, err := SignLegacy(key, 0, LegacyTx{}); err == nil {
		t.Error("chain id 0 accepted")
	}
}

func TestRLPEdges(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{nil, "80"},            // empty string
		{[]byte{0x00}, "00"},   // single byte below 0x80 is itself
		{[]byte{0x7f}, "7f"},   // boundary
		{[]byte{0x80}, "8180"}, // single byte at 0x80 needs a prefix
		{make([]byte, 56), "b838" + hex.EncodeToString(make([]byte, 56))}, // long string
	}
	for _, c := range cases {
		if got := hex.EncodeToString(rlpBytes(c.in)); got != c.want {
			t.Errorf("rlpBytes(%x) = %s, want %s", c.in, got, c.want)
		}
	}
	if got := hex.EncodeToString(rlpUint(big.NewInt(0))); got != "80" {
		t.Errorf("rlpUint(0) = %s", got)
	}
	if got := hex.EncodeToString(rlpUint(big.NewInt(1024))); got != "820400" {
		t.Errorf("rlpUint(1024) = %s", got)
	}
	if got := hex.EncodeToString(rlpList()); got != "c0" {
		t.Errorf("empty list = %s", got)
	}
}

func TestRevertReason(t *testing.T) {
	// SBC testnet, cancelAuthorization with a junk signature (eth_call, 2026-11-07).
	data := "0x08c379a00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000001a454950333030393a20696e76616c6964207369676e6174757265000000000000"
	if got, ok := RevertReason(data); !ok || got != "EIP3009: invalid signature" {
		t.Errorf("%q %v", got, ok)
	}
	if got, ok := RevertReason("0x"); ok || got != "" {
		t.Errorf("empty: %q %v", got, ok)
	}
	// A custom error or a truncated payload is shown as hex rather than mis-decoded.
	if got, ok := RevertReason("0x4e487b71"); !ok || got != "0x4e487b71" {
		t.Errorf("custom: %q %v", got, ok)
	}
	if got, ok := RevertReason("0x08c379a0" + "00"); !ok || got != "0x08c379a000" {
		t.Errorf("short: %q %v", got, ok)
	}
}
