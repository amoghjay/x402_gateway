// Package evm holds the few Ethereum primitives the gateway needs: keccak-256,
// addresses, secp256k1 signing and recovery, and the EIP-191 personal-message hash.
//
// It is deliberately small and pure Go (CGO_ENABLED=0 is how the images build): the
// filter recovers one signature per request and the buyer signs one, nothing more.
// secp256k1 comes from decred's dcrd, the same library go-ethereum uses without cgo.
package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// Keccak256 hashes the concatenation of parts.
func Keccak256(parts ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Address is a 20-byte account address.
type Address [20]byte

// ParseAddress accepts 0x-prefixed, 40-hex-digit addresses in any case. The EIP-55
// checksum is not enforced: the wire carries addresses from many signers, and the
// signature, not the casing, is what binds them.
func ParseAddress(s string) (Address, error) {
	var a Address
	if len(s) != 42 || (s[:2] != "0x" && s[:2] != "0X") {
		return a, fmt.Errorf("evm: address %q: want 0x and 40 hex digits", s)
	}
	if _, err := hex.Decode(a[:], []byte(s[2:])); err != nil {
		return a, fmt.Errorf("evm: address %q: %w", s, err)
	}
	return a, nil
}

// String renders the address with the EIP-55 mixed-case checksum.
func (a Address) String() string {
	lower := hex.EncodeToString(a[:])
	h := Keccak256([]byte(lower))
	out := []byte(lower)
	for i := range out {
		nibble := h[i/2] >> (4 * uint(1-i%2)) & 0xf
		if out[i] >= 'a' && nibble >= 8 {
			out[i] -= 'a' - 'A'
		}
	}
	return "0x" + string(out)
}

// Word left-pads b to a 32-byte ABI word.
func Word(b []byte) [32]byte {
	var w [32]byte
	copy(w[32-len(b):], b)
	return w
}

// Uint256Word renders a non-negative integer as a 32-byte ABI word.
func Uint256Word(v *big.Int) [32]byte { return Word(v.Bytes()) }

// AddressWord renders an address as a 32-byte ABI word.
func AddressWord(a Address) [32]byte { return Word(a[:]) }

var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// ParseUint256 parses a non-negative decimal string into a uint256. The wire carries
// integers as decimal strings; "9" >= "1000" is true as strings, so always go through
// here before comparing.
func ParseUint256(s string) (*big.Int, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return nil, fmt.Errorf("evm: integer %q: want decimal digits", s)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Cmp(maxUint256) > 0 {
		return nil, fmt.Errorf("evm: integer %q: out of range", s)
	}
	return v, nil
}

// ParseBytes32 parses a 0x-prefixed 64-hex-digit value.
func ParseBytes32(s string) ([32]byte, error) {
	var b [32]byte
	if len(s) != 66 || (s[:2] != "0x" && s[:2] != "0X") {
		return b, fmt.Errorf("evm: bytes32 %q: want 0x and 64 hex digits", s)
	}
	if _, err := hex.Decode(b[:], []byte(s[2:])); err != nil {
		return b, fmt.Errorf("evm: bytes32 %q: %w", s, err)
	}
	return b, nil
}

// Hex renders bytes as 0x-prefixed lowercase hex.
func Hex(b []byte) string { return "0x" + hex.EncodeToString(b) }

// ErrSignature is wrapped by every signature parsing or recovery failure.
var ErrSignature = errors.New("evm: signature")

// Recover returns the address that produced sig over digest. sig is the 65-byte
// Ethereum form r||s||v with v in {0, 1, 27, 28}. High-s signatures are rejected as
// OpenZeppelin's ECDSA does, so what the filter accepts is what the token accepts.
func Recover(digest [32]byte, sig []byte) (Address, error) {
	if len(sig) != 65 {
		return Address{}, fmt.Errorf("%w: %d bytes, want 65", ErrSignature, len(sig))
	}
	v := sig[64]
	if v >= 27 {
		v -= 27
	}
	if v > 1 {
		return Address{}, fmt.Errorf("%w: recovery id %d", ErrSignature, sig[64])
	}
	var s secp256k1.ModNScalar
	if overflow := s.SetByteSlice(sig[32:64]); overflow || s.IsOverHalfOrder() {
		return Address{}, fmt.Errorf("%w: s is not in the lower half of the curve order", ErrSignature)
	}
	compact := make([]byte, 65)
	compact[0] = 27 + v
	copy(compact[1:], sig[:64])
	pub, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return Address{}, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	return PubkeyAddress(pub), nil
}

// PubkeyAddress derives the address of a public key.
func PubkeyAddress(pub *secp256k1.PublicKey) Address {
	h := Keccak256(pub.SerializeUncompressed()[1:])
	var a Address
	copy(a[:], h[12:])
	return a
}

// ParseSignature decodes a 0x-prefixed 65-byte hex signature.
func ParseSignature(s string) ([]byte, error) {
	if len(s) != 132 || (s[:2] != "0x" && s[:2] != "0X") {
		return nil, fmt.Errorf("%w: want 0x and 130 hex digits, got %d chars", ErrSignature, len(s))
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	return b, nil
}

// PrivateKey is a secp256k1 signing key. It lives only in the buyer CLI and tests; the
// filter and the settler never hold one.
type PrivateKey struct{ k *secp256k1.PrivateKey }

// GeneratePrivateKey makes a fresh random key.
func GeneratePrivateKey() (*PrivateKey, error) {
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &PrivateKey{k}, nil
}

// ParsePrivateKey decodes a 0x-prefixed 32-byte hex key.
func ParsePrivateKey(s string) (*PrivateKey, error) {
	b, err := ParseBytes32(s)
	if err != nil {
		return nil, errors.New("evm: private key: want 0x and 64 hex digits")
	}
	var k secp256k1.ModNScalar
	if overflow := k.SetBytes(&b); overflow != 0 || k.IsZero() {
		return nil, errors.New("evm: private key: out of range")
	}
	return &PrivateKey{secp256k1.NewPrivateKey(&k)}, nil
}

// Hex renders the key for storage. Callers decide where it goes; never log it.
func (p *PrivateKey) Hex() string { return Hex(p.k.Serialize()) }

// Address of the key.
func (p *PrivateKey) Address() Address { return PubkeyAddress(p.k.PubKey()) }

// Sign produces the 65-byte r||s||v signature over digest, v in {27, 28}, low-s.
func (p *PrivateKey) Sign(digest [32]byte) []byte {
	compact := ecdsa.SignCompact(p.k, digest[:], false) // RFC 6979, low-s
	sig := make([]byte, 65)
	copy(sig, compact[1:])
	sig[64] = compact[0] // 27 + recovery id
	return sig
}

// PersonalHash is the EIP-191 digest that personal_sign signs:
// keccak256("\x19Ethereum Signed Message:\n" + len(message) + message).
func PersonalHash(message []byte) [32]byte {
	return Keccak256([]byte("\x19Ethereum Signed Message:\n"+strconv.Itoa(len(message))), message)
}
