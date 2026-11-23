package webbotauth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// DirectoryPath is where an origin publishes its signing keys (gate standard §4.1).
const DirectoryPath = "/.well-known/http-message-signatures-directory"

// MaxDirectoryKeys bounds how many keys one directory may hold.
const MaxDirectoryKeys = 64

// Key is one Ed25519 key from a directory. NotBefore and Expires are Unix seconds, 0 when the
// directory gives none.
type Key struct {
	Kid        string `json:"kid,omitempty"`
	Thumbprint string `json:"thumbprint"`
	X          string `json:"x"` // base64url, as published
	NotBefore  int64  `json:"nbf,omitempty"`
	Expires    int64  `json:"exp,omitempty"`
}

// Public is the key's Ed25519 public key, nil when X is not one. Decoded on each call, so a
// Directory can be shared between goroutines without locking.
func (k *Key) Public() ed25519.PublicKey {
	b, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil
	}
	return b
}

// Directory is the keys an origin published.
type Directory struct {
	Keys []Key `json:"keys"`
}

// Find picks the key a signature names: a key whose kid is keyid, else one whose RFC 7638
// thumbprint is. Google's directory uses short kids that are not thumbprints; OpenAI's kids
// are thumbprints.
func (d *Directory) Find(keyid string) (*Key, bool) {
	if d == nil || keyid == "" {
		return nil, false
	}
	for i := range d.Keys {
		if d.Keys[i].Kid == keyid {
			return &d.Keys[i], true
		}
	}
	for i := range d.Keys {
		if d.Keys[i].Thumbprint == keyid {
			return &d.Keys[i], true
		}
	}
	return nil, false
}

// ValidAt reports whether the key's nbf and exp, when given, allow it at now, with skew
// seconds of tolerance.
func (k *Key) ValidAt(now, skew int64) bool {
	if k.NotBefore != 0 && k.NotBefore > now+skew {
		return false
	}
	if k.Expires != 0 && k.Expires < now-skew {
		return false
	}
	return true
}

type jwk struct {
	Kty string       `json:"kty"`
	Crv string       `json:"crv"`
	X   string       `json:"x"`
	Kid string       `json:"kid"`
	Nbf *json.Number `json:"nbf"`
	Exp *json.Number `json:"exp"`
}

// ParseDirectory reads a JWK Set and keeps its Ed25519 keys. Other key types are skipped, not
// refused: the standard asks for Ed25519 signatures only. A set with no usable key is an
// error, so a caller keeps the keys it had.
func ParseDirectory(body []byte) (*Directory, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	if len(set.Keys) > MaxDirectoryKeys {
		return nil, fmt.Errorf("directory: %d keys, at most %d", len(set.Keys), MaxDirectoryKeys)
	}
	d := &Directory{}
	for _, raw := range set.Keys {
		var k jwk
		if json.Unmarshal(raw, &k) != nil || k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		pub, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		key := Key{Kid: k.Kid, Thumbprint: Thumbprint(k.X), X: k.X}
		if key.NotBefore, err = seconds(k.Nbf); err != nil {
			continue
		}
		if key.Expires, err = seconds(k.Exp); err != nil {
			continue
		}
		d.Keys = append(d.Keys, key)
	}
	if len(d.Keys) == 0 {
		return nil, errors.New("directory: no Ed25519 key")
	}
	return d, nil
}

// seconds reads a NumericDate. It is taken as given: a directory that publishes milliseconds
// gets keys that are not yet valid, which reads as no signature agent, never as a wrong one.
func seconds(n *json.Number) (int64, error) {
	if n == nil {
		return 0, nil
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

// Thumbprint is the RFC 7638 SHA-256 thumbprint of an Ed25519 key (RFC 8037 §A.3), base64url
// without padding.
func Thumbprint(x string) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
