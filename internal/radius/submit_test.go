package radius

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

func TestSendRawTransactionSync(t *testing.T) {
	r := node(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		var raw string
		var wait int64
		if method != "eth_sendRawTransactionSync" || len(params) != 2 || json.Unmarshal(params[0], &raw) != nil || json.Unmarshal(params[1], &wait) != nil {
			t.Fatalf("%s %s", method, params)
		}
		if raw != "0x01" || wait != 400 {
			t.Errorf("params %s %d", raw, wait)
		}
		return map[string]string{
			"transactionHash": "0xabc", "status": "0x1", "gasUsed": "0x2270c", // 141,068
			"effectiveGasPrice": "0x3ac525e0", "blockNumber": "0x19a5c0f3e10",
		}, nil
	})
	rcpt, err := r.SendRawTransactionSync(t.Context(), []byte{1}, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if rcpt.Status != 1 || rcpt.GasUsed != 141068 || rcpt.TransactionHash != "0xabc" {
		t.Errorf("%+v", rcpt)
	}
	// 141,068 gas × 985,998,816 wei = 0.000139 RUSD, the fee the bench expects per settle.
	if fee := rcpt.Fee().String(); fee != "139092880975488" {
		t.Errorf("fee %s", fee)
	}
}

func TestSendRawTransactionSyncErrors(t *testing.T) {
	r := node(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		return nil, &RPCError{Code: SyncErrTimeout, Message: "timed out", Data: json.RawMessage(`"0xdeadbeef"`)}
	})
	_, err := r.SendRawTransactionSync(t.Context(), []byte{1}, 0)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != SyncErrTimeout || rpcErr.DataString() != "0xdeadbeef" {
		t.Fatalf("%v", err)
	}
}

func TestEthCallRevertReason(t *testing.T) {
	from, _ := evm.ParseAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	r := node(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		var call struct{ From, To, Data string }
		if method != "eth_call" || json.Unmarshal(params[0], &call) != nil || call.From != from.String() {
			t.Fatalf("%s %s", method, params)
		}
		return nil, &RPCError{Code: 3, Message: "execution reverted", Data: json.RawMessage(`"0x08c379a00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000001a454950333030393a20696e76616c6964207369676e6174757265000000000000"`)}
	})
	_, err := r.EthCall(t.Context(), from, SBCAddress, []byte{1, 2, 3, 4})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Reason() != "EIP3009: invalid signature" {
		t.Fatalf("%v", err)
	}
	// An error without data has no reason, and still prints as an error.
	if (&RPCError{Code: 3, Message: "x"}).Reason() != "" {
		t.Error("reason from nothing")
	}
}

func TestQuantities(t *testing.T) {
	r := node(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		switch method {
		case "eth_gasPrice":
			return "0x3ac525e0", nil
		case "eth_getTransactionCount":
			var block string
			_ = json.Unmarshal(params[1], &block)
			if block != "pending" {
				t.Errorf("block %q", block)
			}
			return "0x2a", nil
		case "eth_getBalance":
			return "0xde0b6b3a7640000", nil
		}
		t.Fatalf("%s", method)
		return nil, nil
	})
	if p, err := r.GasPrice(t.Context()); err != nil || p.Int64() != 985998816 {
		t.Errorf("%v %v", p, err)
	}
	if n, err := r.TransactionCount(t.Context(), SBCAddress); err != nil || n != 42 {
		t.Errorf("%v %v", n, err)
	}
	if b, err := r.NativeBalance(t.Context(), SBCAddress); err != nil || b.String() != "1000000000000000000" {
		t.Errorf("%v %v", b, err)
	}
}
