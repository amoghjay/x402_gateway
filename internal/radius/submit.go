package radius

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

// The write side of the RPC client: what a submitter needs to put a signed
// transaction on Radius and learn its outcome in one round trip.
//
// eth_sendRawTransactionSync (EIP-7966) returns the receipt itself, final, because
// Radius has no blocks to wait for. Its error codes, from the Radius docs:
//   4  timed out waiting; the transaction WAS accepted and `data` is its hash
//   5  not added to the mempool; `data` is the hash
//   6  nonce gap; `data` is the nonce the node expects, as hex
// A revert is expected to come back as a receipt with Status 0, not as an error
// (unverified until the bench runs; see tools/latency/README.md).

const (
	SyncErrTimeout  = 4
	SyncErrRejected = 5
	SyncErrNonceGap = 6
)

// Receipt is the subset of a transaction receipt the gateway reads.
type Receipt struct {
	TransactionHash   string
	Status            uint64 // 1 success, 0 reverted
	GasUsed           uint64
	EffectiveGasPrice *big.Int
	BlockNumber       uint64 // on Radius, a millisecond timestamp
}

// Fee is what the sender paid, in wei of the native token.
func (r *Receipt) Fee() *big.Int {
	if r.EffectiveGasPrice == nil {
		return new(big.Int)
	}
	return new(big.Int).Mul(r.EffectiveGasPrice, new(big.Int).SetUint64(r.GasUsed))
}

type rawReceipt struct {
	TransactionHash   string `json:"transactionHash"`
	Status            string `json:"status"`
	GasUsed           string `json:"gasUsed"`
	EffectiveGasPrice string `json:"effectiveGasPrice"`
	BlockNumber       string `json:"blockNumber"`
}

func hexQuantity(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("bad quantity %q", s)
	}
	return v, nil
}

func (rr rawReceipt) parse() (*Receipt, error) {
	status, err := hexQuantity(rr.Status)
	if err != nil {
		return nil, err
	}
	gas, err := hexQuantity(rr.GasUsed)
	if err != nil {
		return nil, err
	}
	price, err := hexQuantity(rr.EffectiveGasPrice)
	if err != nil {
		return nil, err
	}
	block, err := hexQuantity(rr.BlockNumber)
	if err != nil {
		return nil, err
	}
	return &Receipt{TransactionHash: rr.TransactionHash, Status: status.Uint64(), GasUsed: gas.Uint64(), EffectiveGasPrice: price, BlockNumber: block.Uint64()}, nil
}

// SendRawTransactionSync submits raw and waits up to maxWait for the receipt. A
// maxWait of zero leaves the node's default (2 s on Radius). The context bounds the
// HTTP call; keep it a little longer than maxWait so a code-4 answer (accepted, still
// executing) is read rather than lost.
func (r *RPC) SendRawTransactionSync(ctx context.Context, raw []byte, maxWait time.Duration) (*Receipt, error) {
	params := []any{evm.Hex(raw)}
	if maxWait > 0 {
		params = append(params, maxWait.Milliseconds())
	}
	var rr rawReceipt
	if err := r.Call(ctx, &rr, "eth_sendRawTransactionSync", params...); err != nil {
		return nil, err
	}
	rcpt, err := rr.parse()
	if err != nil {
		return nil, fmt.Errorf("eth_sendRawTransactionSync: receipt: %w", err)
	}
	return rcpt, nil
}

// GasPrice is eth_gasPrice. Radius has a fixed price; the node's answer is still the
// source of truth.
func (r *RPC) GasPrice(ctx context.Context) (*big.Int, error) {
	var h string
	if err := r.Call(ctx, &h, "eth_gasPrice"); err != nil {
		return nil, err
	}
	return hexQuantity(h)
}

// TransactionCount is eth_getTransactionCount at "pending": the next nonce for addr
// once the node's queue for it is counted.
func (r *RPC) TransactionCount(ctx context.Context, addr evm.Address) (uint64, error) {
	var h string
	if err := r.Call(ctx, &h, "eth_getTransactionCount", addr.String(), "pending"); err != nil {
		return 0, err
	}
	v, err := hexQuantity(h)
	if err != nil {
		return 0, err
	}
	return v.Uint64(), nil
}

// NativeBalance is eth_getBalance: RUSD in wei, the token Radius charges gas in. The
// Turnstile tops it up from the sender's SBC when it runs short.
func (r *RPC) NativeBalance(ctx context.Context, addr evm.Address) (*big.Int, error) {
	var h string
	if err := r.Call(ctx, &h, "eth_getBalance", addr.String(), "latest"); err != nil {
		return nil, err
	}
	return hexQuantity(h)
}

// EthCall simulates data against to at "latest" from `from`. A revert surfaces as an
// *RPCError whose Reason() carries the contract's message; it costs nothing, so it is
// a complete pre-flight for a transfer (signature, window, nonce and balance).
func (r *RPC) EthCall(ctx context.Context, from, to evm.Address, data []byte) ([]byte, error) {
	var hexOut string
	call := map[string]string{"from": from.String(), "to": to.String(), "data": evm.Hex(data)}
	if err := r.Call(ctx, &hexOut, "eth_call", call, "latest"); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(hexOut, "0x"))
}
