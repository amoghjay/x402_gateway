package x402

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Permit2 is the Permit2 contract, at the same address on every EVM chain. A Permit2 nonce is
// single-use per owner in this contract, whatever the token or spender, so the contract is the
// nonce's namespace. (The payload's spender is x402's proxy, not Permit2.)
const Permit2 = "0x000000000022d473030f116ddee9f6b43ac78ba3"

// Identity is what makes a payment single-use on chain, for a replay guard that does not
// read the chain.
type Identity struct {
	ID      string    // SHA-256 hex of the identity, spelled one way; never logged with the payload
	Payer   string    // the signer, lower-case hex
	Expires time.Time // when the signature stops being valid; zero if the payload does not say
}

// Identify reads a payment's identity: a Permit2 payment (permit2Authorization) by network,
// Permit2, owner and nonce; an EIP-3009 payment (authorization) by network, token, payer and
// nonce. Addresses are compared in lower case and nonces as numbers, so a re-encoded copy of
// one payment (other case, leading zeros, hex for decimal, another signature encoding) is the
// same payment. permit2Authorization is read first, because facilitators settle it when a
// payload carries both.
//
// Ported from radius-payment-gateway's src/x402/claim.rs (MIT). Unlike that function, a payload
// with neither form is an error, not identified by its header: a header can be re-encoded,
// so it guards nothing.
func Identify(pp PaymentPayload) (Identity, error) {
	var p struct {
		Permit2 *struct {
			From     string `json:"from"`
			Nonce    string `json:"nonce"`
			Deadline string `json:"deadline"`
		} `json:"permit2Authorization"`
		Auth *struct {
			From        string `json:"from"`
			Nonce       string `json:"nonce"`
			ValidBefore string `json:"validBefore"`
		} `json:"authorization"`
	}
	if err := json.Unmarshal(pp.Payload, &p); err != nil {
		return Identity{}, errors.New("payload is not an object")
	}
	network := strings.TrimSpace(pp.Accepted.Network)
	if network == "" {
		return Identity{}, errors.New("accepted.network is empty")
	}
	var id Identity
	var key string
	var ok bool
	switch {
	case p.Permit2 != nil && p.Permit2.From != "" && p.Permit2.Nonce != "":
		if id.Payer, ok = lowerAddress(p.Permit2.From); !ok {
			return Identity{}, fmt.Errorf("permit2Authorization.from %q is not an address", p.Permit2.From)
		}
		key = network + ":" + Permit2 + ":" + id.Payer + ":" + number(p.Permit2.Nonce)
		id.Expires = unix(p.Permit2.Deadline)
	case p.Auth != nil && p.Auth.From != "" && p.Auth.Nonce != "":
		if id.Payer, ok = lowerAddress(p.Auth.From); !ok {
			return Identity{}, fmt.Errorf("authorization.from %q is not an address", p.Auth.From)
		}
		asset, ok := lowerAddress(pp.Accepted.Asset)
		if !ok {
			return Identity{}, fmt.Errorf("accepted.asset %q is not an address", pp.Accepted.Asset)
		}
		key = network + ":" + asset + ":" + id.Payer + ":" + number(p.Auth.Nonce)
		id.Expires = unix(p.Auth.ValidBefore)
	default:
		return Identity{}, errors.New("payload has neither a permit2Authorization nor an authorization with a signer and a nonce")
	}
	sum := sha256.Sum256([]byte(key))
	id.ID = hex.EncodeToString(sum[:])
	return id, nil
}

// lowerAddress is an address in lower-case hex with 0x. Anything else is refused: an
// identity built from text that is not one address spelled one way could be re-spelled.
func lowerAddress(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	h, ok := strings.CutPrefix(s, "0x")
	if !ok || len(h) != 40 {
		return "", false
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", false
	}
	return s, true
}

// number spells a decimal or 0x-hex integer in decimal; other text is trimmed and lower-cased.
// Two nonces equal as numbers are one payment: at worst a payment is refused.
func number(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	n, ok := new(big.Int), false
	if h, isHex := strings.CutPrefix(s, "0x"); isHex {
		_, ok = n.SetString(h, 16)
	} else {
		_, ok = n.SetString(s, 10)
	}
	if !ok || n.Sign() < 0 {
		return s
	}
	return n.String()
}

func unix(s string) time.Time {
	n, ok := new(big.Int).SetString(strings.TrimSpace(s), 10)
	if !ok || n.Sign() <= 0 || !n.IsInt64() {
		return time.Time{}
	}
	return time.Unix(n.Int64(), 0)
}
