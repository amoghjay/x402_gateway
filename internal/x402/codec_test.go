package x402

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// specVectors are the four header values in the spec's HTTP transport document, with
// the JSON each decodes to. See testdata/spec_vectors.json for the source commit.
type specVector struct {
	Name   string          `json:"name"`
	Header string          `json:"header"`
	Value  string          `json:"value"`
	JSON   json.RawMessage `json:"json"`
}

func loadSpecVectors(t *testing.T) map[string]specVector {
	t.Helper()
	b, err := os.ReadFile("testdata/spec_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vectors []specVector `json:"vectors"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	out := map[string]specVector{}
	for _, v := range f.Vectors {
		out[v.Name] = v
	}
	if len(out) != 4 {
		t.Fatalf("want 4 vectors, got %d", len(out))
	}
	return out
}

// canonical parses JSON into a comparable value, so field order and whitespace don't
// matter but every field and value does.
func canonical(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonical: %v: %s", err, b)
	}
	return v
}

// roundTrip decodes the spec's header into typ, re-encodes it, and checks that nothing
// was lost or invented on the way: the spec's bytes and ours must mean the same object.
func roundTrip(t *testing.T, vec specVector, decode func(string) (any, error)) {
	t.Helper()
	got, err := decode(vec.Value)
	if err != nil {
		t.Fatalf("%s: decode: %v", vec.Name, err)
	}
	enc, err := EncodeHeader(got)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("%s: our header is not standard padded base64: %v", vec.Name, err)
	}
	if !reflect.DeepEqual(canonical(t, ours), canonical(t, vec.JSON)) {
		t.Errorf("%s: round trip changed the object\n spec: %s\n ours: %s", vec.Name, vec.JSON, ours)
	}
}

func TestSpecVectorsRoundTrip(t *testing.T) {
	vs := loadSpecVectors(t)

	roundTrip(t, vs["payment-required"], func(h string) (any, error) { return DecodePaymentRequired(h) })
	roundTrip(t, vs["payment-signature"], func(h string) (any, error) { return DecodePaymentPayload(h) })
	roundTrip(t, vs["payment-response-success"], func(h string) (any, error) { return DecodeSettleResponse(h) })
	roundTrip(t, vs["payment-response-failure"], func(h string) (any, error) { return DecodeSettleResponse(h) })
}

func TestSpecVectorFields(t *testing.T) {
	vs := loadSpecVectors(t)

	pr, err := DecodePaymentRequired(vs["payment-required"].Value)
	if err != nil {
		t.Fatal(err)
	}
	if pr.X402Version != 2 || pr.Error != "PAYMENT-SIGNATURE header is required" ||
		pr.Resource.URL != "https://api.example.com/premium-data" || len(pr.Accepts) != 1 {
		t.Errorf("PaymentRequired: %+v", pr)
	}
	req := pr.Accepts[0]
	if req.Scheme != SchemeExact || req.Network != "eip155:84532" || req.Amount != "10000" ||
		req.MaxTimeoutSeconds != 60 || req.Extra == nil || req.Extra.Name != "USDC" || req.Extra.Version != "2" {
		t.Errorf("Requirements: %+v extra=%+v", req, req.Extra)
	}
	if req.TransferMethod() != TransferEIP3009 {
		t.Errorf("transfer method defaults to eip3009, got %q", req.TransferMethod())
	}

	pp, err := DecodePaymentPayload(vs["payment-signature"].Value)
	if err != nil {
		t.Fatal(err)
	}
	if pp.Resource == nil || pp.Accepted.PayTo != "0x209693Bc6afc0C5328bA36FaF03C514EF312287C" {
		t.Errorf("PaymentPayload: %+v", pp)
	}
	e, err := pp.EIP3009()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.Signature, "0x2d6a7588") || e.Authorization.From != "0x857b06519E91e3A54538791bDbb0E22373e36b66" ||
		e.Authorization.Value != "10000" || e.Authorization.ValidBefore != "1740672154" ||
		e.Authorization.Nonce != "0xf3746613c2d920b5fdabc0856f2aeb2d4f88ee6037b8cc5d04a71a4462f13480" {
		t.Errorf("EIP3009: %+v", e)
	}

	ok, err := DecodeSettleResponse(vs["payment-response-success"].Value)
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Success || ok.Transaction == "" || ok.Network != "eip155:84532" || ok.Payer == "" {
		t.Errorf("SettleResponse success: %+v", ok)
	}
	bad, err := DecodeSettleResponse(vs["payment-response-failure"].Value)
	if err != nil {
		t.Fatal(err)
	}
	if bad.Success || bad.ErrorReason != ReasonInsufficientFunds || bad.Transaction != "" {
		t.Errorf("SettleResponse failure: %+v", bad)
	}
}

// The Radius vector: the shape the Radius facilitator settled on testnet, with the
// extra.assetTransferMethod echoed as the scheme spec asks.
func TestRadiusVector(t *testing.T) {
	b, err := os.ReadFile("testdata/radius_eip3009.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Signer        string        `json:"signer"`
		Header        string        `json:"header"`
		Signature     string        `json:"signature"`
		Authorization Authorization `json:"authorization"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	pp, err := DecodePaymentPayload(v.Header)
	if err != nil {
		t.Fatal(err)
	}
	if pp.Accepted.Network != "eip155:72344" || pp.Accepted.Asset != "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb" ||
		pp.Accepted.TransferMethod() != TransferEIP3009 || pp.Accepted.Extra.Name != "Stable Coin" || pp.Accepted.Extra.Version != "1" {
		t.Errorf("accepted: %+v extra=%+v", pp.Accepted, pp.Accepted.Extra)
	}
	e, err := pp.EIP3009()
	if err != nil {
		t.Fatal(err)
	}
	if e.Signature != v.Signature || e.Authorization != v.Authorization || e.Authorization.From != v.Signer {
		t.Errorf("payload: %+v", e)
	}
}

func TestDecodeHeaderTolerance(t *testing.T) {
	vs := loadSpecVectors(t)
	std := vs["payment-response-success"].Value
	raw, _ := base64.StdEncoding.DecodeString(std)
	cases := map[string]string{
		"standard padded":     std,
		"standard unpadded":   strings.TrimRight(std, "="),
		"url-safe padded":     base64.URLEncoding.EncodeToString(raw),
		"url-safe unpadded":   base64.RawURLEncoding.EncodeToString(raw),
		"surrounding spaces":  "  " + std + " ",
		"with a plus and url": base64.URLEncoding.EncodeToString([]byte(`{"success":true,"transaction":"0x>>??","network":"n"}`)),
	}
	for name, h := range cases {
		var s SettleResponse
		if err := DecodeHeader(h, &s); err != nil {
			t.Errorf("%s: %v", name, err)
		} else if !s.Success {
			t.Errorf("%s: decoded %+v", name, s)
		}
	}
}

func TestDecodeHeaderRejects(t *testing.T) {
	cases := map[string]struct {
		header string
		want   error
	}{
		"empty":            {"", ErrMalformed},
		"not base64":       {"%%%", ErrMalformed},
		"not json":         {base64.StdEncoding.EncodeToString([]byte("hello")), ErrMalformed},
		"trailing data":    {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2} {}`)), ErrMalformed},
		"too long":         {strings.Repeat("A", MaxHeaderBytes+1), ErrMalformed},
		"wrong type":       {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":"2"}`)), ErrMalformed},
		"version 1":        {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":1,"accepted":{"scheme":"exact","network":"n"},"payload":{}}`)), ErrVersion},
		"no scheme":        {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2,"accepted":{},"payload":{}}`)), ErrMalformed},
		"payload not obj":  {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2,"accepted":{"scheme":"exact","network":"n"},"payload":"x"}`)), ErrMalformed},
		"payload missing":  {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2,"accepted":{"scheme":"exact","network":"n"}}`)), ErrMalformed},
		"payload is array": {base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2,"accepted":{"scheme":"exact","network":"n"},"payload":[]}`)), ErrMalformed},
	}
	for name, c := range cases {
		_, err := DecodePaymentPayload(c.header)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
	}
	if _, err := DecodePaymentRequired(base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2,"resource":{"url":"u"},"accepts":[]}`))); !errors.Is(err, ErrMalformed) {
		t.Errorf("empty accepts: %v", err)
	}
}

func TestEIP3009RequiresEveryField(t *testing.T) {
	full := `{"signature":"0xs","authorization":{"from":"a","to":"b","value":"1","validAfter":"0","validBefore":"1","nonce":"0xn"}}`
	if _, err := (PaymentPayload{Payload: json.RawMessage(full)}).EIP3009(); err != nil {
		t.Fatal(err)
	}
	for _, drop := range []string{`"signature":"0xs",`, `"from":"a",`, `"to":"b",`, `"value":"1",`, `"validAfter":"0",`, `"validBefore":"1",`, `,"nonce":"0xn"`} {
		p := PaymentPayload{Payload: json.RawMessage(strings.Replace(full, drop, "", 1))}
		if _, err := p.EIP3009(); !errors.Is(err, ErrPayload) {
			t.Errorf("without %s: want ErrPayload, got %v", drop, err)
		}
	}
	if _, err := (PaymentPayload{Payload: json.RawMessage(`{"signature":1}`)}).EIP3009(); !errors.Is(err, ErrPayload) {
		t.Errorf("wrong type: %v", err)
	}
}

func TestSettleResponseHashAliases(t *testing.T) {
	for _, in := range []string{
		`{"success":true,"transaction":"0x1","network":"n"}`,
		`{"success":true,"txHash":"0x1","network":"n"}`,
		`{"success":true,"transactionHash":"0x1","network":"n"}`,
		`{"success":true,"hash":"0x1","network":"n"}`,
		`{"success":true,"transaction":"0x1","txHash":"0x2","network":"n"}`,
	} {
		var s SettleResponse
		if err := json.Unmarshal([]byte(in), &s); err != nil || s.Transaction != "0x1" {
			t.Errorf("%s: tx=%q err=%v", in, s.Transaction, err)
		}
	}
}

func TestPendingKeepsSpecShape(t *testing.T) {
	h, err := Pending("eip155:72344", "0xpayer", "pay_1").Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(h)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// What a spec client reads: success, an empty transaction, the network, the payer.
	if m["success"] != true || m["transaction"] != "" || m["network"] != "eip155:72344" || m["payer"] != "0xpayer" {
		t.Errorf("spec fields: %v", m)
	}
	if _, has := m["errorReason"]; has {
		t.Error("errorReason must be absent on success")
	}
	ext, _ := m["extensions"].(map[string]any)
	st, _ := ext[SettlementExtension].(map[string]any)
	if st["status"] != "pending" || st["paymentId"] != "pay_1" {
		t.Errorf("extension: %v", m["extensions"])
	}
	// And it reads back through our own decoder.
	back, err := DecodeSettleResponse(h)
	if err != nil || !back.Success || back.Transaction != "" {
		t.Errorf("decode back: %+v %v", back, err)
	}
}

func TestEncodeIsStandardPaddedBase64OfCompactJSON(t *testing.T) {
	h, err := (PaymentRequired{X402Version: 2, Resource: Resource{URL: "u"}, Accepts: []Requirements{{Scheme: "exact"}}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(raw), " \n\t") {
		t.Errorf("not compact: %s", raw)
	}
	// Optional fields stay out of the wire when empty, as in the spec's examples.
	for _, absent := range []string{"error", "extensions", "extra", "description"} {
		if strings.Contains(string(raw), `"`+absent+`"`) {
			t.Errorf("%q should be omitted when empty: %s", absent, raw)
		}
	}
}
