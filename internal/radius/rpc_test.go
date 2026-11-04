package radius

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

// node fakes a JSON-RPC endpoint: it hands each call to answer and writes the result,
// or the error object when answer returns one.
func node(t *testing.T, answer func(method string, params []json.RawMessage) (any, *RPCError)) *RPC {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			JSONRPC string            `json:"jsonrpc"`
			Method  string            `json:"method"`
			Params  []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.JSONRPC != "2.0" {
			t.Fatalf("request %+v %v", in, err)
		}
		res, rpcErr := answer(in.Method, in.Params)
		out := map[string]any{"jsonrpc": "2.0", "id": 1}
		if rpcErr != nil {
			out["error"] = rpcErr
		} else {
			out["result"] = res
		}
		_ = json.NewEncoder(rw).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return &RPC{URL: srv.URL}
}

func TestChainID(t *testing.T) {
	r := node(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		if method != "eth_chainId" || len(params) != 0 {
			t.Errorf("%s %s", method, params)
		}
		return "0x11a98", nil
	})
	if id, err := r.ChainID(t.Context()); err != nil || id != Testnet.ChainID {
		t.Fatalf("%d %v", id, err)
	}
}

func TestBalanceOf(t *testing.T) {
	holder, _ := evm.ParseAddress("0x209693Bc6afc0C5328bA36FaF03C514EF312287C")
	r := node(t, func(method string, params []json.RawMessage) (any, *RPCError) {
		var call struct{ To, Data string }
		var block string
		if method != "eth_call" || len(params) != 2 || json.Unmarshal(params[0], &call) != nil || json.Unmarshal(params[1], &block) != nil {
			t.Fatalf("%s %s", method, params)
		}
		// balanceOf(address) is 0x70a08231, then the holder left-padded to 32 bytes.
		want := "0x70a08231000000000000000000000000209693bc6afc0c5328ba36faf03c514ef312287c"
		if call.To != SBC || call.Data != want || block != "latest" {
			t.Errorf("to %s data %s block %s", call.To, call.Data, block)
		}
		return "0x00000000000000000000000000000000000000000000000000000000000f4240", nil
	})
	got, err := r.BalanceOf(t.Context(), SBCAddress, holder)
	if err != nil || got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestBalanceOfRejectsShortReturn(t *testing.T) {
	r := node(t, func(string, []json.RawMessage) (any, *RPCError) { return "0x", nil })
	if _, err := r.BalanceOf(t.Context(), SBCAddress, SBCAddress); err == nil {
		t.Fatal("accepted an empty return (a call to an address with no code)")
	}
}

func TestRPCError(t *testing.T) {
	r := node(t, func(string, []json.RawMessage) (any, *RPCError) {
		return nil, &RPCError{Code: -32005, Message: "rate limited"}
	})
	_, err := r.ChainID(t.Context())
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -32005 {
		t.Fatalf("err %v", err)
	}
}

func TestSBCAmounts(t *testing.T) {
	for _, c := range []struct {
		text   string
		atomic int64
		format string
	}{
		{"0.001", 1000, "0.001"},
		{"1", 1_000_000, "1"},
		{"1.5", 1_500_000, "1.5"},
		{".25", 250_000, "0.25"},
		{"0", 0, "0"},
		{"0.000001", 1, "0.000001"},
		{"12.340000", 12_340_000, "12.34"},
	} {
		got, err := ParseSBC(c.text)
		if err != nil || got.Int64() != c.atomic {
			t.Errorf("ParseSBC(%q) = %v %v, want %d", c.text, got, err, c.atomic)
			continue
		}
		if f := FormatSBC(got); f != c.format {
			t.Errorf("FormatSBC(%d) = %q, want %q", c.atomic, f, c.format)
		}
	}
	for _, bad := range []string{"", ".", "0.0000001", "-1", "1e3", "1.2.3", "abc"} {
		if v, err := ParseSBC(bad); err == nil {
			t.Errorf("ParseSBC(%q) = %v, want an error", bad, v)
		}
	}
}
