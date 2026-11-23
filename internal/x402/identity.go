package x402

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Method  string    // TransferPermit2 or TransferEIP3009: the form the payload carries
	Expires time.Time // when the signature stops being valid; zero if the payload does not say
}

// Identify reads a payment's identity: a Permit2 payment (permit2Authorization) by network,
// Permit2, owner and nonce; an EIP-3009 payment (authorization) by network, token, payer and
// nonce. Addresses are compared in lower case and nonces as numbers, so a re-encoded copy of
// one payment (other case, leading zeros, hex for decimal, another signature encoding) is the
// same payment.
//
// The payload goes to the facilitator byte for byte, and the facilitator's JSON reader is not
// ours. So the identity must not depend on how a reader resolves an ambiguous document: a
// member name spelled twice, in any case, is refused (strictObject), and so is a payload
// carrying both forms. Otherwise one signed payment could be given a new identity each time
// by adding a decoy member that this reader picks and the facilitator's does not.
//
// Ported from radius-payment-gateway's src/x402/claim.rs (MIT). Unlike that function, a payload
// with neither form is an error, not identified by its header (a header can be re-encoded, so
// it guards nothing), and a payload with both forms is an error, not read as Permit2.
func Identify(pp PaymentPayload) (Identity, error) {
	top, err := strictObject(pp.Payload, "permit2Authorization", "authorization")
	if err != nil {
		return Identity{}, fmt.Errorf("payload: %w", err)
	}
	network := strings.TrimSpace(pp.Accepted.Network)
	if network == "" {
		return Identity{}, errors.New("accepted.network is empty")
	}
	permit2, hasPermit2 := top["permit2Authorization"]
	auth, hasAuth := top["authorization"]
	var id Identity
	var key string
	switch {
	case hasPermit2 && hasAuth:
		return Identity{}, errors.New("payload has both a permit2Authorization and an authorization")
	case hasPermit2:
		from, nonce, deadline, err := signerFields(permit2, "permit2Authorization", "deadline")
		if err != nil {
			return Identity{}, err
		}
		id.Method, id.Payer = TransferPermit2, from
		key = network + ":" + Permit2 + ":" + from + ":" + number(nonce)
		id.Expires = unix(deadline)
	case hasAuth:
		from, nonce, validBefore, err := signerFields(auth, "authorization", "validBefore")
		if err != nil {
			return Identity{}, err
		}
		asset, ok := lowerAddress(pp.Accepted.Asset)
		if !ok {
			return Identity{}, fmt.Errorf("accepted.asset %q is not an address", pp.Accepted.Asset)
		}
		id.Method, id.Payer = TransferEIP3009, from
		key = network + ":" + asset + ":" + from + ":" + number(nonce)
		id.Expires = unix(validBefore)
	default:
		return Identity{}, errors.New("payload has neither a permit2Authorization nor an authorization")
	}
	sum := sha256.Sum256([]byte(key))
	id.ID = hex.EncodeToString(sum[:])
	return id, nil
}

// signerFields reads the signer, the nonce and the expiry member of one authorization object.
// The signer and the nonce are required; the expiry is "" when absent.
func signerFields(raw json.RawMessage, name, expiryMember string) (from, nonce, expiry string, err error) {
	obj, err := strictObject(raw, "from", "nonce", expiryMember)
	if err != nil {
		return "", "", "", fmt.Errorf("%s: %w", name, err)
	}
	text := func(member string) (string, error) {
		v, ok := obj[member]
		if !ok {
			return "", nil
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			return "", fmt.Errorf("%s.%s is not a string", name, member)
		}
		return s, nil
	}
	signer, err := text("from")
	if err != nil {
		return "", "", "", err
	}
	if nonce, err = text("nonce"); err != nil {
		return "", "", "", err
	}
	if expiry, err = text(expiryMember); err != nil {
		return "", "", "", err
	}
	if signer == "" || nonce == "" {
		return "", "", "", fmt.Errorf("%s has no signer or no nonce", name)
	}
	from, ok := lowerAddress(signer)
	if !ok {
		return "", "", "", fmt.Errorf("%s.from %q is not an address", name, signer)
	}
	return from, nonce, expiry, nil
}

// strictObject reads a JSON object's members under their exact names. It refuses a document
// that another reader could resolve differently: a name that is not plain ASCII, or two
// names equal when case is ignored (encoding/json would match either to a field and keep the
// last; a reader that matches exactly would keep a different one), or a name that is one of
// the members read here in another case (present for one reader, absent for the other).
func strictObject(raw json.RawMessage, read ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	out, seen := map[string]json.RawMessage{}, map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("not valid JSON")
		}
		name, _ := tok.(string)
		for i := range len(name) {
			if name[i] >= 0x80 {
				return nil, fmt.Errorf("member name %q is not ASCII", name)
			}
		}
		for _, r := range read {
			if name != r && strings.EqualFold(name, r) {
				return nil, fmt.Errorf("member %q must be spelled %q", name, r)
			}
		}
		if fold := strings.ToLower(name); seen[fold] {
			return nil, fmt.Errorf("member %q appears more than once (names compare without case)", name)
		} else {
			seen[fold] = true
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("not valid JSON")
		}
		out[name] = v
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, errors.New("not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the object")
	}
	return out, nil
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
