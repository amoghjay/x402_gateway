package radius

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
)

// rpcServer is a fake JSON-RPC node that answers eth_call with one word.
func rpcServer(t *testing.T, word string, sawData *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		switch req.Method {
		case "eth_call":
			var call map[string]string
			_ = json.Unmarshal(req.Params[0], &call)
			*sawData = call["data"]
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + word + `"}`))
		case "eth_chainId":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x11a98"}`))
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
	}))
}

func TestAuthorizationState(t *testing.T) {
	// The standard EIP-3009 selector, so a wrong signature string would show here.
	if got := evm.Hex(authorizationStateSelector); got != "0xe94a0102" {
		t.Fatalf("selector = %s", got)
	}
	authorizer, _ := evm.ParseAddress("0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4")
	nonce, _ := evm.ParseBytes32("0x" + strings.Repeat("ab", 32))

	for _, c := range []struct {
		word string
		want bool
	}{
		{"0x" + strings.Repeat("00", 31) + "01", true},
		{"0x" + strings.Repeat("00", 32), false},
	} {
		var data string
		srv := rpcServer(t, c.word, &data)
		rpc := &RPC{URL: srv.URL, HTTP: srv.Client()}
		used, err := rpc.AuthorizationState(context.Background(), SBCAddress, authorizer, nonce)
		srv.Close()
		if err != nil || used != c.want {
			t.Errorf("word %s: used=%v err=%v, want %v", c.word, used, err, c.want)
		}
		// selector ++ address left-padded to 32 bytes ++ nonce
		want := "0xe94a0102" + strings.Repeat("00", 12) + strings.ToLower(authorizer.String()[2:]) + strings.Repeat("ab", 32)
		if data != want {
			t.Errorf("calldata\n got %s\nwant %s", data, want)
		}
	}
}

// A keyed RPC URL carries the key in its path; a transport error must not print it.
func TestErrorsRedactTheURL(t *testing.T) {
	rpc := &RPC{URL: "http://127.0.0.1:1/this-is-the-api-key", HTTP: &http.Client{Timeout: time.Second}}
	_, err := rpc.ChainID(context.Background())
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "api-key") {
		t.Errorf("error leaks the URL: %v", err)
	}
	if !strings.Contains(err.Error(), "eth_chainId") {
		t.Errorf("error should still name the method: %v", err)
	}
}

func TestKeepWarmPingsAtOnce(t *testing.T) {
	var data string
	srv := rpcServer(t, "", &data)
	defer srv.Close()
	rpc := &RPC{URL: srv.URL, HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Returns when ctx ends; the first ping happens before the first tick.
	rpc.KeepWarm(ctx, time.Hour, discardLogger())
}
