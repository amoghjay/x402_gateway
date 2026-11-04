package facilitator

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

var req = radius.Testnet.ExactRequirements("1000", "0x209693Bc6afc0C5328bA36FaF03C514EF312287C", 300)

// echo is a facilitator that checks the request body and answers with out.
func echo(t *testing.T, path string, status int, out string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			t.Errorf("%s %s, want POST %s", r.Method, r.URL.Path, path)
		}
		var in x402.FacilitatorRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.X402Version != x402.Version || !reflect.DeepEqual(in.PaymentRequirements, req) || !reflect.DeepEqual(in.PaymentPayload.Accepted, req) {
			t.Errorf("request %+v", in)
		}
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL}
}

var pp = x402.PaymentPayload{X402Version: x402.Version, Accepted: req, Payload: json.RawMessage(`{}`)}

func TestVerify(t *testing.T) {
	c := echo(t, "/verify", http.StatusOK, `{"isValid":false,"invalidReason":"insufficient_funds","payer":"0x1"}`)
	got, err := c.Verify(t.Context(), pp, req)
	if err != nil || got.IsValid || got.InvalidReason != x402.ReasonInsufficientFunds || got.Payer != "0x1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSettle(t *testing.T) {
	// A 4xx with the spec's JSON body is an answer, not a transport error.
	c := echo(t, "/settle", http.StatusBadRequest, `{"success":false,"errorReason":"invalid_transaction_state","txHash":"0xfeed","network":"eip155:72344"}`)
	got, err := c.Settle(t.Context(), pp, req)
	if err != nil || got.Success || got.ErrorReason != x402.ReasonTransactionState || got.Transaction != "0xfeed" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestNonJSONAnswerIsAStatusError(t *testing.T) {
	c := echo(t, "/settle", http.StatusBadGateway, "upstream connect error\n")
	_, err := c.Settle(t.Context(), pp, req)
	var st *Status
	if !errors.As(err, &st) || st.Code != http.StatusBadGateway || st.Body != "upstream connect error" {
		t.Fatalf("err %v", err)
	}
}

func TestSupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/supported" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_, _ = rw.Write([]byte(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:72344","asset":"` + radius.SBC + `","extra":{"assetTransferMethod":"eip3009"}}],"extensions":[],"signers":{}}`))
	}))
	defer srv.Close()
	got, err := (&Client{URL: srv.URL}).Supported(t.Context())
	if err != nil || len(got.Kinds) != 1 || got.Kinds[0].Network != radius.Testnet.Network() || got.Kinds[0].Extra.AssetTransferMethod != x402.TransferEIP3009 {
		t.Fatalf("%+v %v", got, err)
	}
}
