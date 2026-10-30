// Package eip3009 hashes EIP-3009 TransferWithAuthorization messages under EIP-712, so
// the filter can recover a payment's signer without any library's typed-data machinery:
// the struct has six fixed fields, and the token's domain is known.
//
// The digest is keccak256("\x19\x01" || domainSeparator || structHash). The domain
// separator computed here matches SBC's DOMAIN_SEPARATOR() on both Radius networks
// (checked live, see internal/radius and the tests here).
package eip3009

import (
	"fmt"
	"math/big"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// TypeHash is keccak256 of the EIP-3009 TransferWithAuthorization type string, the
// value SBC's TRANSFER_WITH_AUTHORIZATION_TYPEHASH() returns.
var TypeHash = evm.Keccak256([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))

var domainTypeHash = evm.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))

// Domain is the token's EIP-712 domain. Name and Version come from the facilitator's
// `extra` (SBC's version() reverts on chain) and from ERC-5267 eip712Domain().
type Domain struct {
	Name              string
	Version           string
	ChainID           uint64
	VerifyingContract evm.Address
}

// Separator is the EIP-712 domain separator.
func (d Domain) Separator() [32]byte {
	name := evm.Keccak256([]byte(d.Name))
	version := evm.Keccak256([]byte(d.Version))
	chain := evm.Uint256Word(new(big.Int).SetUint64(d.ChainID))
	contract := evm.AddressWord(d.VerifyingContract)
	return evm.Keccak256(domainTypeHash[:], name[:], version[:], chain[:], contract[:])
}

// Authorization is a parsed TransferWithAuthorization message.
type Authorization struct {
	From        evm.Address
	To          evm.Address
	Value       *big.Int
	ValidAfter  *big.Int // unix seconds
	ValidBefore *big.Int // unix seconds
	Nonce       [32]byte
}

// Parse converts the wire form (decimal strings, hex nonce) into typed values.
func Parse(a x402.Authorization) (Authorization, error) {
	var out Authorization
	var err error
	if out.From, err = evm.ParseAddress(a.From); err != nil {
		return out, fmt.Errorf("from: %w", err)
	}
	if out.To, err = evm.ParseAddress(a.To); err != nil {
		return out, fmt.Errorf("to: %w", err)
	}
	if out.Value, err = evm.ParseUint256(a.Value); err != nil {
		return out, fmt.Errorf("value: %w", err)
	}
	if out.ValidAfter, err = evm.ParseUint256(a.ValidAfter); err != nil {
		return out, fmt.Errorf("validAfter: %w", err)
	}
	if out.ValidBefore, err = evm.ParseUint256(a.ValidBefore); err != nil {
		return out, fmt.Errorf("validBefore: %w", err)
	}
	if out.Nonce, err = evm.ParseBytes32(a.Nonce); err != nil {
		return out, fmt.Errorf("nonce: %w", err)
	}
	return out, nil
}

// Wire renders the authorization back into the wire form.
func (a Authorization) Wire() x402.Authorization {
	return x402.Authorization{
		From:        a.From.String(),
		To:          a.To.String(),
		Value:       a.Value.String(),
		ValidAfter:  a.ValidAfter.String(),
		ValidBefore: a.ValidBefore.String(),
		Nonce:       evm.Hex(a.Nonce[:]),
	}
}

// StructHash is the EIP-712 hashStruct of the authorization.
func (a Authorization) StructHash() [32]byte {
	from := evm.AddressWord(a.From)
	to := evm.AddressWord(a.To)
	value := evm.Uint256Word(a.Value)
	after := evm.Uint256Word(a.ValidAfter)
	before := evm.Uint256Word(a.ValidBefore)
	return evm.Keccak256(TypeHash[:], from[:], to[:], value[:], after[:], before[:], a.Nonce[:])
}

// Digest is what the payer signs and the filter recovers.
func Digest(d Domain, a Authorization) [32]byte {
	sep := d.Separator()
	sh := a.StructHash()
	return evm.Keccak256([]byte{0x19, 0x01}, sep[:], sh[:])
}
