package eip3009

import (
	"fmt"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

// Calldata for the two EIP-3009 entry points a submitter uses: settling an
// authorization and (for tests and the cancel drill) revoking one. Both take the
// (v, r, s) form; SBC exposes the `bytes` overload too, but this one needs no length
// prefix.

var (
	transferSelector = selectorOf("transferWithAuthorization(address,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)")
	cancelSelector   = selectorOf("cancelAuthorization(address,bytes32,uint8,bytes32,bytes32)")

	// CancelTypeHash is the EIP-712 type of the cancel message.
	CancelTypeHash = evm.Keccak256([]byte("CancelAuthorization(address authorizer,bytes32 nonce)"))
)

func selectorOf(sig string) []byte { h := evm.Keccak256([]byte(sig)); return h[:4] }

// splitSig turns a 65-byte r||s||v signature into ABI words.
func splitSig(sig []byte) (v, r, s [32]byte, err error) {
	if len(sig) != 65 {
		return v, r, s, fmt.Errorf("eip3009: signature is %d bytes, want 65", len(sig))
	}
	copy(r[:], sig[:32])
	copy(s[:], sig[32:64])
	v[31] = sig[64]
	return v, r, s, nil
}

// TransferCalldata is transferWithAuthorization(from, to, value, validAfter,
// validBefore, nonce, v, r, s) for this authorization and the payer's signature.
func (a Authorization) TransferCalldata(sig []byte) ([]byte, error) {
	v, r, s, err := splitSig(sig)
	if err != nil {
		return nil, err
	}
	from, to := evm.AddressWord(a.From), evm.AddressWord(a.To)
	value, after, before := evm.Uint256Word(a.Value), evm.Uint256Word(a.ValidAfter), evm.Uint256Word(a.ValidBefore)
	out := make([]byte, 0, 4+9*32)
	out = append(out, transferSelector...)
	for _, w := range [][32]byte{from, to, value, after, before, a.Nonce, v, r, s} {
		out = append(out, w[:]...)
	}
	return out, nil
}

// CancelDigest is what the authorizer signs to revoke a nonce.
func CancelDigest(d Domain, authorizer evm.Address, nonce [32]byte) [32]byte {
	sep := d.Separator()
	who := evm.AddressWord(authorizer)
	sh := evm.Keccak256(CancelTypeHash[:], who[:], nonce[:])
	return evm.Keccak256([]byte{0x19, 0x01}, sep[:], sh[:])
}

// CancelCalldata is cancelAuthorization(authorizer, nonce, v, r, s).
func CancelCalldata(authorizer evm.Address, nonce [32]byte, sig []byte) ([]byte, error) {
	v, r, s, err := splitSig(sig)
	if err != nil {
		return nil, err
	}
	who := evm.AddressWord(authorizer)
	out := make([]byte, 0, 4+5*32)
	out = append(out, cancelSelector...)
	for _, w := range [][32]byte{who, nonce, v, r, s} {
		out = append(out, w[:]...)
	}
	return out, nil
}
