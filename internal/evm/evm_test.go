package evm

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Hardhat account #1: public knowledge, used only as a test vector.
const (
	vectorKey  = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	vectorAddr = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
)

func TestAddressParseAndChecksum(t *testing.T) {
	a, err := ParseAddress("0x70997970c51812dc3a010c7d01b50e0d17dc79c8")
	if err != nil {
		t.Fatal(err)
	}
	if a.String() != vectorAddr {
		t.Errorf("EIP-55: %s", a)
	}
	for _, bad := range []string{"", "0x", "70997970c51812dc3a010c7d01b50e0d17dc79c8", "0x7099", "0xzz997970c51812dc3a010c7d01b50e0d17dc79c8"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseUint256(t *testing.T) {
	v, err := ParseUint256("1000")
	if err != nil || v.Int64() != 1000 {
		t.Fatal(v, err)
	}
	// "9" >= "1000" as strings; as integers it is not.
	nine, _ := ParseUint256("9")
	if nine.Cmp(v) >= 0 {
		t.Error("string comparison leaked in")
	}
	for _, bad := range []string{"", "-1", "0x10", "1e3", " 1", "1 ", "115792089237316195423570985008687907853269984665640564039457584007913129639936"} {
		if _, err := ParseUint256(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseUint256("115792089237316195423570985008687907853269984665640564039457584007913129639935"); err != nil {
		t.Error("max uint256 refused")
	}
}

func TestKeyAddressSignRecover(t *testing.T) {
	k, err := ParsePrivateKey(vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	if k.Address().String() != vectorAddr || k.Hex() != vectorKey {
		t.Errorf("address %s hex %s", k.Address(), k.Hex())
	}
	digest := Keccak256([]byte("anything"))
	sig := k.Sign(digest)
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		t.Fatalf("sig %x", sig)
	}
	got, err := Recover(digest, sig)
	if err != nil || got != k.Address() {
		t.Errorf("recover: %s %v", got, err)
	}
	// v as 0/1 is accepted too.
	alt := append([]byte(nil), sig...)
	alt[64] -= 27
	if got, err := Recover(digest, alt); err != nil || got != k.Address() {
		t.Errorf("recover v-27: %s %v", got, err)
	}
	// A different digest recovers a different address, not an error.
	if got, err := Recover(Keccak256([]byte("other")), sig); err != nil || got == k.Address() {
		t.Errorf("wrong digest: %s %v", got, err)
	}
	// Random keys round-trip.
	for i := 0; i < 20; i++ {
		r, err := GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Recover(digest, r.Sign(digest)); err != nil || got != r.Address() {
			t.Fatalf("random key %d: %s %v", i, got, err)
		}
	}
}

func TestRecoverRejects(t *testing.T) {
	k, _ := ParsePrivateKey(vectorKey)
	digest := Keccak256([]byte("anything"))
	sig := k.Sign(digest)

	// High-s: s' = N - s with the recovery bit flipped is the same signature to a
	// lenient ecrecover, and a different one to OpenZeppelin's ECDSA. We refuse it.
	var s secp256k1.ModNScalar
	s.SetByteSlice(sig[32:64])
	s.Negate()
	high := append([]byte(nil), sig...)
	sb := s.Bytes()
	copy(high[32:64], sb[:])
	high[64] ^= 1
	if _, err := Recover(digest, high); !errors.Is(err, ErrSignature) {
		t.Errorf("high-s accepted: %v", err)
	}
	for name, bad := range map[string][]byte{
		"short":       sig[:64],
		"long":        append(append([]byte(nil), sig...), 0),
		"recovery 29": append(append([]byte(nil), sig[:64]...), 29),
		"recovery 2":  append(append([]byte(nil), sig[:64]...), 2),
		"zero r":      append(append(make([]byte, 32), sig[32:64]...), sig[64]),
	} {
		if _, err := Recover(digest, bad); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseSignature("0x1234"); !errors.Is(err, ErrSignature) {
		t.Error("short hex signature accepted")
	}
}

func TestPrivateKeyRejects(t *testing.T) {
	for _, bad := range []string{"", "0x00", "0x" + string(make([]byte, 64)),
		"0x0000000000000000000000000000000000000000000000000000000000000000",   // zero
		"0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141"} { // = N
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Vectors from eth_account (encode_defunct + sign_message) with the same key.
func TestPersonalHashAndSign(t *testing.T) {
	if got := Hex(hashSlice(PersonalHash([]byte("hello")))); got != "0x50b2c43fd39106bafbba0da34fc430e1f91e3c96ea2acee2bc34119f92b37750" {
		t.Errorf("personal hash of hello: %s", got)
	}
	msg := "Radius Faucet: drip SBC to " + vectorAddr
	h := PersonalHash([]byte(msg))
	if got := Hex(hashSlice(h)); got != "0x2273ef69c5943ae3de38201d57fb9a937386cffa38033b71d28f4f3fe19bc7b9" {
		t.Errorf("personal hash of the faucet challenge: %s", got)
	}
	pySig, _ := hex.DecodeString("cb4c7097a37ebc581f2753a8b33bf391b575e5b2eb115d4d351f38e15231e3984f72fa4e1a36a6f48092c987e9c440d89625c852e83d8d98073f4d2bcb3d9e771b")
	if got, err := Recover(h, pySig); err != nil || got.String() != vectorAddr {
		t.Errorf("eth_account's personal signature recovers to %s, %v", got, err)
	}
	k, _ := ParsePrivateKey(vectorKey)
	if ours := k.Sign(h); !bytesEqual(ours, pySig) {
		// Both are RFC 6979 deterministic, so they must be byte-identical.
		t.Errorf("our signature differs from eth_account's:\n ours %x\n  py  %x", ours, pySig)
	}
}

func hashSlice(h [32]byte) []byte { return h[:] }

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
