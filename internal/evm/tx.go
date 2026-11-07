package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// This file is what a submitter needs to put a transaction on Radius without an
// Ethereum library: RLP, the EIP-155 legacy transaction, and the revert-reason decoder.
// Radius accepts legacy and EIP-1559 transactions alike and its priority fee is zero
// (measured 2026-11-07), so the simpler legacy shape is enough.

// rlpBytes encodes a byte string.
func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return append(rlpLength(len(b), 0x80), b...)
}

// rlpUint encodes an integer as its minimal big-endian bytes (zero is the empty string).
func rlpUint(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return []byte{0x80}
	}
	return rlpBytes(v.Bytes())
}

// rlpList encodes already-encoded items as a list.
func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(rlpLength(len(payload), 0xc0), payload...)
}

// rlpLength is the prefix for a string (offset 0x80) or list (offset 0xc0) of n bytes.
func rlpLength(n int, offset byte) []byte {
	if n < 56 {
		return []byte{offset + byte(n)} //nolint:gosec // n < 56
	}
	be := new(big.Int).SetInt64(int64(n)).Bytes()
	return append([]byte{offset + 55 + byte(len(be))}, be...) //nolint:gosec // len(be) <= 8
}

// LegacyTx is a pre-EIP-1559 transaction. Gas is a uint64 everywhere Ethereum
// tooling prints it; the rest are 256-bit quantities.
type LegacyTx struct {
	Nonce    uint64
	GasPrice *big.Int
	Gas      uint64
	To       Address
	Value    *big.Int
	Data     []byte
}

// SignLegacy signs tx for chainID (EIP-155) and returns the raw bytes for
// eth_sendRawTransaction, plus the transaction hash.
func SignLegacy(key *PrivateKey, chainID uint64, tx LegacyTx) (raw []byte, hash [32]byte, err error) {
	if key == nil {
		return nil, hash, errors.New("evm: sign: nil key")
	}
	if chainID == 0 {
		return nil, hash, errors.New("evm: sign: chain id 0")
	}
	chain := new(big.Int).SetUint64(chainID)
	fields := func(v, r, s *big.Int) []byte {
		return rlpList(
			rlpUint(new(big.Int).SetUint64(tx.Nonce)),
			rlpUint(tx.GasPrice),
			rlpUint(new(big.Int).SetUint64(tx.Gas)),
			rlpBytes(tx.To[:]),
			rlpUint(tx.Value),
			rlpBytes(tx.Data),
			rlpUint(v), rlpUint(r), rlpUint(s),
		)
	}
	// EIP-155: the signed payload ends in (chainId, 0, 0); v then carries the chain id.
	sig := key.Sign(Keccak256(fields(chain, nil, nil)))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:64])
	v := new(big.Int).SetUint64(uint64(sig[64]-27) + 35 + 2*chainID)
	raw = fields(v, r, s)
	return raw, Keccak256(raw), nil
}

var errorStringSelector = func() []byte { h := Keccak256([]byte("Error(string)")); return h[:4] }()

// RevertReason decodes the `data` of an EVM revert. A Solidity `revert("…")` or
// `require(…, "…")` ABI-encodes Error(string); anything else comes back as hex so
// a custom error is still visible. ok is false when data is empty.
func RevertReason(data string) (reason string, ok bool) {
	b, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil || len(b) == 0 {
		return "", false
	}
	if len(b) >= 4+64 && string(b[:4]) == string(errorStringSelector) {
		body := b[4:]
		offset := new(big.Int).SetBytes(body[:32]).Int64()
		if offset+32 <= int64(len(body)) {
			n := new(big.Int).SetBytes(body[offset : offset+32]).Int64()
			if offset+32+n <= int64(len(body)) {
				return string(body[offset+32 : offset+32+n]), true
			}
		}
	}
	return fmt.Sprintf("0x%x", b), true
}
