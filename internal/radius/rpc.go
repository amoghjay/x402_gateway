package radius

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

// RPC is a minimal JSON-RPC client: the two or three calls the gateway makes (balance,
// chain id, keepalive) need no ABI library. The HTTP client is the caller's, so the
// filter can keep its connection warm and own its timeouts.
type RPC struct {
	URL  string
	HTTP *http.Client
}

// RPCError is a JSON-RPC error object returned by the node.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call performs one JSON-RPC call and unmarshals its result into out.
func (r *RPC) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := r.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("%s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if env.Error != nil {
		return env.Error
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// ChainID returns eth_chainId.
func (r *RPC) ChainID(ctx context.Context) (uint64, error) {
	var hexID string
	if err := r.Call(ctx, &hexID, "eth_chainId"); err != nil {
		return 0, err
	}
	id, ok := new(big.Int).SetString(strings.TrimPrefix(hexID, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("eth_chainId: %q", hexID)
	}
	return id.Uint64(), nil
}

var balanceOfSelector = func() []byte { h := evm.Keccak256([]byte("balanceOf(address)")); return h[:4] }()

// BalanceOf reads an ERC-20 balance with eth_call at "latest".
func (r *RPC) BalanceOf(ctx context.Context, token, holder evm.Address) (*big.Int, error) {
	w := evm.AddressWord(holder)
	data := append(append([]byte{}, balanceOfSelector...), w[:]...)
	var hexOut string
	if err := r.Call(ctx, &hexOut, "eth_call", map[string]string{"to": token.String(), "data": evm.Hex(data)}, "latest"); err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(strings.TrimPrefix(hexOut, "0x"))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("balanceOf: bad return %q", hexOut)
	}
	return new(big.Int).SetBytes(b), nil
}

// SBCAddress is SBC as a typed address.
var SBCAddress = func() evm.Address { a, _ := evm.ParseAddress(SBC); return a }()

// FormatSBC renders atomic units as a decimal SBC amount ("1000" → "0.001").
func FormatSBC(atomic *big.Int) string {
	s := atomic.String()
	if len(s) <= SBCDecimals {
		s = strings.Repeat("0", SBCDecimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-SBCDecimals], s[len(s)-SBCDecimals:]
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// ParseSBC parses a decimal SBC amount ("0.001") into atomic units (1000).
func ParseSBC(s string) (*big.Int, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" && frac == "" {
		return nil, fmt.Errorf("amount %q: empty", s)
	}
	if whole == "" {
		whole = "0"
	}
	if len(frac) > SBCDecimals {
		return nil, fmt.Errorf("amount %q has more than %d decimals", s, SBCDecimals)
	}
	frac += strings.Repeat("0", SBCDecimals-len(frac))
	v, err := evm.ParseUint256(strings.TrimLeft(whole+frac, "0") + "0")
	if err != nil {
		return nil, fmt.Errorf("amount %q: %w", s, err)
	}
	return v.Div(v, big.NewInt(10)), nil
}
