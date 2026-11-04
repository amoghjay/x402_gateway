package extproc

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Route names as Envoy Gateway v1.9.2 builds them.
const (
	priced   = "httproute/demo/httpbin/rule/0/match/0"
	unpriced = "httproute/demo/other/rule/0/match/0"
)

// loaded returns a store holding the example price table (httproute/demo/httpbin).
func loaded(t *testing.T) *pricing.Store {
	t.Helper()
	b, err := os.ReadFile("../pricing/testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pricing.Parse(b, radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	s := pricing.NewStore()
	s.Set(p)
	return s
}

// dial starts the server on an in-memory listener and opens one stream, as Envoy would
// for one HTTP request. It returns the stream and everything the server logged.
func dial(t *testing.T, delay time.Duration, prices *pricing.Store, level slog.Level) (extprocv3.ExternalProcessor_ProcessClient, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: level}))
	extprocv3.RegisterExternalProcessorServer(srv, NewServer(log, delay, prices))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := extprocv3.NewExternalProcessorClient(conn).Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stream, &logs
}

func headers(kv ...string) *corev3.HeaderMap {
	hm := &corev3.HeaderMap{}
	for i := 0; i+1 < len(kv); i += 2 {
		// RawValue, as current Envoy sends it.
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{Key: kv[i], RawValue: []byte(kv[i+1])})
	}
	return hm
}

// requestHeaders is the first message of a stream as Envoy sends it: the headers, and
// the attributes the policy asked for. route "" sends no attributes at all.
func requestHeaders(route string, kv ...string) *extprocv3.ProcessingRequest {
	base := []string{":method", "GET", ":scheme", "http", ":authority", "34.173.224.114", ":path", "/get?x=1"}
	req := &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{Headers: headers(append(base, kv...)...)},
	}}
	if route != "" {
		req.Attributes = map[string]*structpb.Struct{attributesKey: {Fields: map[string]*structpb.Value{
			routeNameAttr: structpb.NewStringValue(route),
		}}}
	}
	return req
}

func roundTrip(t *testing.T, stream extprocv3.ExternalProcessor_ProcessClient, req *extprocv3.ProcessingRequest) *extprocv3.ProcessingResponse {
	t.Helper()
	if err := stream.Send(req); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// immediateHeaders checks how every header is set (RawValue, overwrite) and returns them.
func immediateHeaders(t *testing.T, ir *extprocv3.ImmediateResponse) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, o := range ir.GetHeaders().GetSetHeaders() {
		// Envoy reads raw_value and ignores value on this path.
		if o.GetHeader().GetValue() != "" || len(o.GetHeader().GetRawValue()) == 0 {
			t.Errorf("header %s: value in Value, not RawValue", o.GetHeader().GetKey())
		}
		if o.GetAppendAction() != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
			t.Errorf("header %s: append action %v", o.GetHeader().GetKey(), o.GetAppendAction())
		}
		out[o.GetHeader().GetKey()] = string(o.GetHeader().GetRawValue())
	}
	return out
}

func TestPricedRouteGets402(t *testing.T) {
	stream, logs := dial(t, 0, loaded(t), slog.LevelInfo)
	resp := roundTrip(t, stream, requestHeaders(priced, "accept", "*/*", "user-agent", "node"))
	ir := resp.GetImmediateResponse()
	if ir == nil || ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_payment_required" {
		t.Fatalf("want an immediate 402, got %v", resp)
	}
	h := immediateHeaders(t, ir)
	for k, want := range map[string]string{
		"content-type":                  "application/json",
		"cache-control":                 "no-store",
		"link":                          `<https://docs.radiustech.xyz/build/x402/>; rel="help"`,
		"access-control-expose-headers": "PAYMENT-REQUIRED",
	} {
		if h[k] != want {
			t.Errorf("%s: %q, want %q", k, h[k], want)
		}
	}
	pr, err := x402.DecodePaymentRequired(h["payment-required"])
	if err != nil {
		t.Fatal(err)
	}
	want := radius.Testnet.ExactRequirements("1000", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", 300)
	if pr.Resource.URL != "http://34.173.224.114/get?x=1" || len(pr.Accepts) != 1 || !reflect.DeepEqual(pr.Accepts[0], want) {
		t.Errorf("payment required %+v", pr)
	}
	if !json.Valid(ir.GetBody()) {
		t.Errorf("body is not JSON: %s", ir.GetBody())
	}
	if !strings.Contains(logs.String(), `"decision":"payment_required"`) || !strings.Contains(logs.String(), `"routeKey":"httproute/demo/httpbin"`) {
		t.Errorf("decision not logged: %s", logs)
	}
}

func TestBrowserGetsTheHTMLPage(t *testing.T) {
	stream, _ := dial(t, 0, loaded(t), slog.LevelInfo)
	resp := roundTrip(t, stream, requestHeaders(priced, "accept", "text/html,*/*;q=0.8", "user-agent", "Mozilla/5.0"))
	h := immediateHeaders(t, resp.GetImmediateResponse())
	if h["content-type"] != "text/html; charset=utf-8" || h["payment-required"] == "" {
		t.Fatalf("headers %v", h)
	}
}

func TestForwardedProtoSetsTheScheme(t *testing.T) {
	stream, _ := dial(t, 0, loaded(t), slog.LevelInfo)
	resp := roundTrip(t, stream, requestHeaders(priced, "x-forwarded-proto", "https"))
	pr, _ := x402.DecodePaymentRequired(immediateHeaders(t, resp.GetImmediateResponse())["payment-required"])
	if pr.Resource.URL != "https://34.173.224.114/get?x=1" {
		t.Errorf("resource.url %q", pr.Resource.URL)
	}
}

// Before M5 nothing can verify a payment, so carrying one changes nothing but the label.
// The signature itself never reaches the logs, not even at debug level.
func TestPaymentPresentStillGets402(t *testing.T) {
	stream, logs := dial(t, 0, loaded(t), slog.LevelDebug)
	resp := roundTrip(t, stream, requestHeaders(priced, "payment-signature", "c2VjcmV0LXNpZ25hdHVyZQ==", "authorization", "Bearer s3cret"))
	if resp.GetImmediateResponse().GetStatus().GetCode() != 402 {
		t.Fatalf("want 402, got %v", resp)
	}
	if !strings.Contains(logs.String(), `"decision":"payment_present_unverified"`) {
		t.Errorf("decision not logged: %s", logs)
	}
	if strings.Contains(logs.String(), "c2VjcmV0") || strings.Contains(logs.String(), "s3cret") {
		t.Errorf("a credential leaked into the logs: %s", logs)
	}
}

func TestUnpricedRouteContinues(t *testing.T) {
	stream, logs := dial(t, 0, loaded(t), slog.LevelInfo)
	resp := roundTrip(t, stream, requestHeaders(unpriced))
	if got := resp.GetRequestHeaders().GetResponse(); got == nil || got.GetHeaderMutation() != nil || got.GetStatus() != extprocv3.CommonResponse_CONTINUE {
		t.Fatalf("want an unchanged CONTINUE, got %v", resp)
	}
	resp = roundTrip(t, stream, &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &extprocv3.HttpHeaders{Headers: headers(":status", "200")},
	}})
	if resp.GetResponseHeaders().GetResponse() == nil {
		t.Fatalf("response phase: want a ResponseHeaders answer, got %v", resp)
	}
	if !strings.Contains(logs.String(), `"decision":"unpriced"`) {
		t.Errorf("decision not logged: %s", logs)
	}
}

// Anything that means the filter cannot tell what a route costs fails closed.
func TestFailsClosed(t *testing.T) {
	for name, c := range map[string]struct {
		prices  *pricing.Store
		route   string
		code    int32
		details string
	}{
		"no attribute":        {loaded(t), "", 500, "x402_no_route_attribute"},
		"not an HTTPRoute":    {loaded(t), "grpcroute/demo/svc/rule/0/match/0", 500, "x402_no_route_attribute"},
		"route without parts": {loaded(t), "httproute/demo", 500, "x402_no_route_attribute"},
		"no table yet":        {pricing.NewStore(), priced, 503, "x402_table_not_loaded"},
		"no table, unpriced":  {pricing.NewStore(), unpriced, 503, "x402_table_not_loaded"},
	} {
		stream, _ := dial(t, 0, c.prices, slog.LevelInfo)
		ir := roundTrip(t, stream, requestHeaders(c.route)).GetImmediateResponse()
		if ir.GetStatus().GetCode() != typev3.StatusCode(c.code) || ir.GetDetails() != c.details {
			t.Errorf("%s: status %v details %q, want %d %q", name, ir.GetStatus().GetCode(), ir.GetDetails(), c.code, c.details)
		}
	}
}

func TestBodyPhaseFailsLoudly(t *testing.T) {
	stream, _ := dial(t, 0, loaded(t), slog.LevelInfo)

	err := stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
		RequestBody: &extprocv3.HttpBody{Body: []byte("x")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented for a body phase, got %v", err)
	}
}

func TestDelayHoldsTheRequestPhase(t *testing.T) {
	const delay = 100 * time.Millisecond
	stream, _ := dial(t, delay, loaded(t), slog.LevelInfo)

	start := time.Now()
	roundTrip(t, stream, requestHeaders(unpriced))
	if got := time.Since(start); got < delay {
		t.Fatalf("answered after %v, want at least %v", got, delay)
	}
}

// Envoy cancels the stream when its message timeout fires. The server must stop
// waiting then, not hold a dead stream for the rest of the delay.
func TestDelayEndsWhenEnvoyGivesUp(t *testing.T) {
	s := NewServer(slog.New(slog.DiscardHandler), time.Hour, loaded(t))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := s.handle(ctx, requestHeaders(priced))
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if got := time.Since(start); got > time.Second {
		t.Fatalf("returned after %v: the wait ignored cancellation", got)
	}
}
