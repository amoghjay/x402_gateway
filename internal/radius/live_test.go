//go:build live

// Live checks: every constant in this package against the network it describes.
// Run with: make verify-radius (go test -tags live ./internal/radius -v -run Live).
// Needs outbound HTTPS to both RPCs, both facilitators, both explorers and the faucet.
// Re-run before a mainnet cutover and whenever a Radius doc contradicts this package.
package radius

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/amoghjay/x402_gateway/internal/x402"
)

func keccak(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func selector(sig string) []byte { return keccak([]byte(sig))[:4] }

func word(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) } // left-pad to 32 bytes

func addrWord(hexAddr string) []byte {
	b, err := hex.DecodeString(strings.TrimPrefix(hexAddr, "0x"))
	if err != nil {
		panic(err)
	}
	return word(b)
}

type rpcClient struct {
	t   *testing.T
	url string
}

func (c rpcClient) call(method string, params ...any) json.RawMessage {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", c.url, method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatalf("%s %s: %v", c.url, method, err)
	}
	if out.Error != nil {
		c.t.Fatalf("%s %s: %s", c.url, method, out.Error.Message)
	}
	return out.Result
}

// ethCall returns the raw return data of a view call.
func (c rpcClient) ethCall(to string, data []byte) []byte {
	c.t.Helper()
	var hexOut string
	if err := json.Unmarshal(c.call("eth_call", map[string]string{"to": to, "data": "0x" + hex.EncodeToString(data)}, "latest"), &hexOut); err != nil {
		c.t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(hexOut, "0x"))
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

// abiString decodes a single ABI-encoded string return value (offset word, length
// word, bytes).
func abiString(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 64 {
		t.Fatalf("short string return: %x", b)
	}
	return abiStringAt(t, b, 32)
}

// abiStringAt decodes the string whose length word starts at off.
func abiStringAt(t *testing.T, b []byte, off int64) string {
	t.Helper()
	if int64(len(b)) < off+32 {
		t.Fatalf("short string at %d: %x", off, b)
	}
	n := new(big.Int).SetBytes(b[off : off+32]).Int64()
	return string(b[off+32 : off+32+n])
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(b)
}

func TestLiveProfiles(t *testing.T) {
	for _, p := range []Profile{Testnet, Mainnet} {
		t.Run(p.Name, func(t *testing.T) {
			c := rpcClient{t, p.RPC}

			// Chain id behind the RPC URL.
			var chainHex string
			if err := json.Unmarshal(c.call("eth_chainId"), &chainHex); err != nil {
				t.Fatal(err)
			}
			if got, _ := new(big.Int).SetString(strings.TrimPrefix(chainHex, "0x"), 16); got.Uint64() != p.ChainID {
				t.Errorf("eth_chainId = %d, profile says %d", got, p.ChainID)
			}

			// The three contracts are deployed.
			for name, addr := range map[string]string{"SBC": SBC, "Permit2": Permit2, "ExactPermit2Proxy": ExactPermit2Proxy} {
				var code string
				if err := json.Unmarshal(c.call("eth_getCode", addr, "latest"), &code); err != nil {
					t.Fatal(err)
				}
				if len(code) <= 2 {
					t.Errorf("%s at %s has no code", name, addr)
				}
			}

			// SBC: name, decimals, and the EIP-712 domain via ERC-5267 eip712Domain().
			if got := abiString(t, c.ethCall(SBC, selector("name()"))); got != SBCName {
				t.Errorf("name() = %q, want %q", got, SBCName)
			}
			if got := new(big.Int).SetBytes(c.ethCall(SBC, selector("decimals()"))).Int64(); got != SBCDecimals {
				t.Errorf("decimals() = %d, want %d", got, SBCDecimals)
			}
			dom := c.ethCall(SBC, selector("eip712Domain()"))
			// (bytes1 fields, string name, string version, uint256 chainId, address verifyingContract, ...)
			nameOff := new(big.Int).SetBytes(dom[32:64]).Int64()
			verOff := new(big.Int).SetBytes(dom[64:96]).Int64()
			domName := abiStringAt(t, dom, nameOff)
			domVersion := abiStringAt(t, dom, verOff)
			domChain := new(big.Int).SetBytes(dom[96:128]).Uint64()
			domContract := "0x" + hex.EncodeToString(dom[140:160])
			if domName != SBCName || domVersion != SBCVersion || domChain != p.ChainID || !strings.EqualFold(domContract, SBC) {
				t.Errorf("eip712Domain() = {%q, %q, %d, %s}, profile says {%q, %q, %d, %s}",
					domName, domVersion, domChain, domContract, SBCName, SBCVersion, p.ChainID, SBC)
			}

			// DOMAIN_SEPARATOR() equals the one computed from our constants: the signature
			// check in the filter will hash exactly this.
			typeHash := keccak([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
			want := keccak(typeHash, keccak([]byte(SBCName)), keccak([]byte(SBCVersion)), word(new(big.Int).SetUint64(p.ChainID).Bytes()), addrWord(SBC))
			if got := c.ethCall(SBC, selector("DOMAIN_SEPARATOR()")); !bytes.Equal(got, want) {
				t.Errorf("DOMAIN_SEPARATOR() = %x, computed from the profile %x", got, want)
			}

			// EIP-3009 is there, whatever the docs say: the standard typehash and a live
			// authorizationState.
			twa := keccak([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))
			if got := c.ethCall(SBC, selector("TRANSFER_WITH_AUTHORIZATION_TYPEHASH()")); !bytes.Equal(got, twa) {
				t.Errorf("TRANSFER_WITH_AUTHORIZATION_TYPEHASH() = %x, want the EIP-3009 standard %x", got, twa)
			}
			state := c.ethCall(SBC, append(append(selector("authorizationState(address,bytes32)"), addrWord(SBC)...), make([]byte, 32)...))
			if len(state) != 32 {
				t.Errorf("authorizationState returned %d bytes", len(state))
			}

			// Facilitator: healthy, and it advertises exactly the kind ExactRequirements builds.
			code, body := get(t, p.Facilitator+"/health")
			if code != 200 || !strings.Contains(body, `"status":"ok"`) {
				t.Errorf("%s/health: %d %s", p.Facilitator, code, body)
			}
			_, supBody := get(t, p.Facilitator+"/supported")
			var sup x402.Supported
			if err := json.Unmarshal([]byte(supBody), &sup); err != nil {
				t.Fatalf("/supported: %v: %s", err, supBody)
			}
			req := p.ExactRequirements("1000", "0x209693Bc6afc0C5328bA36FaF03C514EF312287C", 300)
			found := false
			for _, k := range sup.Kinds {
				if k.X402Version == x402.Version && k.Scheme == req.Scheme && k.Network == req.Network &&
					strings.EqualFold(k.Asset, req.Asset) && k.Extra != nil && reflect.DeepEqual(*k.Extra, *req.Extra) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s/supported has no kind matching %+v extra=%+v: %s", p.Facilitator, req, req.Extra, supBody)
			}

			// Explorer answers.
			if code, _ := get(t, p.Explorer+"/"); code != 200 {
				t.Errorf("explorer %s: HTTP %d", p.Explorer, code)
			}
		})
	}
}

// TestLiveVectorVerifies sends the golden Radius vector to the testnet facilitator's
// /verify. A valid answer (or insufficient_funds, should the public wallet be drained)
// proves the facilitator parses our PaymentPayload shape and hashes the same EIP-712
// domain; a corrupted signature must be refused as a signature error.
func TestLiveVectorVerifies(t *testing.T) {
	raw, err := os.ReadFile("../x402/testdata/radius_eip3009.json")
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		Header string `json:"header"`
		Signer string `json:"signer"`
	}
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}
	pp, err := x402.DecodePaymentPayload(vec.Header)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(pp x402.PaymentPayload) x402.VerifyResponse {
		t.Helper()
		body, _ := json.Marshal(x402.FacilitatorRequest{X402Version: x402.Version, PaymentPayload: pp, PaymentRequirements: pp.Accepted})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, Testnet.Facilitator+"/verify", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out x402.VerifyResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("/verify: HTTP %d: %v", resp.StatusCode, err)
		}
		return out
	}

	good := verify(pp)
	if !strings.EqualFold(good.Payer, vec.Signer) {
		t.Errorf("payer %q, want the vector's signer %s", good.Payer, vec.Signer)
	}
	if !good.IsValid && !strings.Contains(strings.ToLower(good.InvalidReason), "fund") && !strings.Contains(strings.ToLower(good.InvalidReason), "balance") {
		t.Errorf("vector refused for a reason other than funds: %+v", good)
	}

	// Flip the last byte of the signature.
	e, err := pp.EIP3009()
	if err != nil {
		t.Fatal(err)
	}
	sig := e.Signature
	if strings.HasSuffix(sig, "00") {
		sig = sig[:len(sig)-2] + "11"
	} else {
		sig = sig[:len(sig)-2] + "00"
	}
	e.Signature = sig
	bad := pp
	bad.Payload, _ = json.Marshal(e)
	if r := verify(bad); r.IsValid || !strings.Contains(strings.ToLower(r.InvalidReason), "signature") {
		t.Errorf("corrupted signature not refused as a signature error: %+v", r)
	}
}

func TestLiveFaucet(t *testing.T) {
	// The drip endpoint exists: an empty body is a 400 with the faucet's error shape,
	// not a 404. Nothing is requested.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, Testnet.Faucet+"/drip", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 400 || !strings.Contains(string(b), "address") {
		t.Errorf("faucet drip without an address: %d %s", resp.StatusCode, b)
	}
}
