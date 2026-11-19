package eip3009

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

func TestSelectors(t *testing.T) {
	// Both selectors as seen in live SBC transactions and the 2026-11-07 eth_call probe.
	if got := hex.EncodeToString(transferSelector); got != "e3ee160e" {
		t.Errorf("transferWithAuthorization selector %s", got)
	}
	if got := hex.EncodeToString(cancelSelector); got != "5a049a70" {
		t.Errorf("cancelAuthorization selector %s", got)
	}
}

func TestTransferCalldataLayout(t *testing.T) {
	from, _ := evm.ParseAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	to, _ := evm.ParseAddress("0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4")
	a := Authorization{From: from, To: to, Value: big.NewInt(1000), ValidAfter: big.NewInt(0), ValidBefore: big.NewInt(1_800_000_000)}
	a.Nonce[31] = 7
	sig := make([]byte, 65)
	sig[0], sig[32], sig[64] = 0xaa, 0xbb, 28
	data, err := a.TransferCalldata(sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4+9*32 {
		t.Fatalf("len %d", len(data))
	}
	word := func(i int) []byte { return data[4+32*i : 4+32*(i+1)] }
	if hex.EncodeToString(word(0)[12:]) != "70997970c51812dc3a010c7d01b50e0d17dc79c8" {
		t.Errorf("from word %x", word(0))
	}
	if new(big.Int).SetBytes(word(2)).Int64() != 1000 || new(big.Int).SetBytes(word(4)).Int64() != 1_800_000_000 {
		t.Errorf("value/validBefore %x %x", word(2), word(4))
	}
	if word(5)[31] != 7 || word(6)[31] != 28 || word(7)[0] != 0xaa || word(8)[0] != 0xbb {
		t.Errorf("nonce/v/r/s words wrong")
	}
	if _, err := a.TransferCalldata(sig[:64]); err == nil {
		t.Error("64-byte signature accepted")
	}
}

func TestCancelRoundTrip(t *testing.T) {
	key, _ := evm.ParsePrivateKey("0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d")
	d := Domain{Name: "Stable Coin", Version: "1", ChainID: 72344}
	var nonce [32]byte
	nonce[0] = 1
	sig := key.Sign(CancelDigest(d, key.Address(), nonce))
	if who, err := evm.Recover(CancelDigest(d, key.Address(), nonce), sig); err != nil || who != key.Address() {
		t.Fatalf("recover %v %v", who, err)
	}
	data, err := CancelCalldata(key.Address(), nonce, sig)
	if err != nil || len(data) != 4+5*32 || data[4+32+31] != 0 || data[4+32] != 1 {
		t.Fatalf("calldata %x %v", data, err)
	}
}
