package radius

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

// RPC is a minimal JSON-RPC client: the two or three calls the gateway makes (balance,
// chain id, keepalive) need no ABI library. The HTTP client is the caller's, so the
// filter can keep its connection warm and own its timeouts.
//
// The URL may carry an API key in its path; it is treated as a secret and never
// appears in an error (see redact).
type RPC struct {
	URL  string
	HTTP *http.Client
}

// NewHTTPClient is the client the filter keeps warm. The Radius RPC answers HTTP/2
// (behind Cloudflare), so one connection multiplexes a burst of cache misses; a few
// idle connections are kept longer than the keepalive period; every phase of a request
// has a timeout, so a dead edge cannot hold a goroutine.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
	}}
}

// KeepWarm calls eth_chainId at once and then every `every` until ctx is done. A cold
// connection to the RPC costs ~265 ms (measured on GKE), which never fits the request
// deadline; a 20 s cadence keeps the HTTP/2 connection open through the real path, not
// just a transport ping.
func (r *RPC) KeepWarm(ctx context.Context, every time.Duration, log *slog.Logger) {
	ping := func() {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		t0 := time.Now()
		if _, err := r.ChainID(c); err != nil {
			log.Warn("rpc keepalive failed", "err", err)
			return
		}
		log.Debug("rpc keepalive", "ms", time.Since(t0).Milliseconds())
	}
	ping()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ping()
		}
	}
}

// redact drops the request URL from a transport error: *url.Error prints the whole
// URL, and with a keyed RPC the key is in its path.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
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
		return fmt.Errorf("%s: %w", method, redact(err))
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

// selector is the first four bytes of the keccak of a Solidity signature.
func selector(signature string) []byte { h := evm.Keccak256([]byte(signature)); return h[:4] }

var (
	balanceOfSelector          = selector("balanceOf(address)")
	authorizationStateSelector = selector("authorizationState(address,bytes32)")
)

// call32 runs eth_call at "latest" for a function that returns one 32-byte word.
func (r *RPC) call32(ctx context.Context, to evm.Address, data []byte) ([]byte, error) {
	var hexOut string
	if err := r.Call(ctx, &hexOut, "eth_call", map[string]string{"to": to.String(), "data": evm.Hex(data)}, "latest"); err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(strings.TrimPrefix(hexOut, "0x"))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("eth_call: bad return %q", hexOut)
	}
	return b, nil
}

// BalanceOf reads an ERC-20 balance with eth_call at "latest".
func (r *RPC) BalanceOf(ctx context.Context, token, holder evm.Address) (*big.Int, error) {
	w := evm.AddressWord(holder)
	b, err := r.call32(ctx, token, append(append([]byte{}, balanceOfSelector...), w[:]...))
	if err != nil {
		return nil, fmt.Errorf("balanceOf: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}

// AuthorizationState reports whether an EIP-3009 nonce has been used on chain: true
// once the authorization was settled (or cancelled). It is the settler's answer to an
// uncertain settle (a timeout or a 502): check before resubmitting, never re-sign.
func (r *RPC) AuthorizationState(ctx context.Context, token, authorizer evm.Address, nonce [32]byte) (bool, error) {
	w := evm.AddressWord(authorizer)
	data := append(append(append([]byte{}, authorizationStateSelector...), w[:]...), nonce[:]...)
	b, err := r.call32(ctx, token, data)
	if err != nil {
		return false, fmt.Errorf("authorizationState: %w", err)
	}
	return new(big.Int).SetBytes(b).Sign() != 0, nil
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
