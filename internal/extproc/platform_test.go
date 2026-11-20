package extproc

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/amoghjay/x402_gateway/internal/counter"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// The project in ../policy/testdata/three_options.json and the route that follows it.
const (
	platformProject = "00000000-0000-4000-8000-0000000000aa"
	platformRoute   = "httproute/shop/metered/rule/0/match/0"
)

// platformStore loads a table with one platform route, and the project's document unless
// doc is empty, the way the watcher does.
func platformStore(t *testing.T, noPolicy, doc string) *pricing.Store {
	t.Helper()
	p, err := pricing.Parse([]byte(`{"schemaVersion":1,"routes":{},"platform":{"httproute/shop/metered":{"project":"`+platformProject+`","noPolicy":"`+noPolicy+`"}}}`), radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]string{}
	if doc != "" {
		data[pricing.PolicyKey(platformProject)] = doc
	}
	p.LoadPolicies(data, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := pricing.NewStore()
	s.Set(p)
	return s
}

func compiledDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../policy/testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type platformTest struct {
	t     *testing.T
	srv   *Server
	logs  *logBuf
	redis *miniredis.Miniredis
	conn  func() extprocv3.ExternalProcessor_ProcessClient
}

func newPlatformTest(t *testing.T, store *pricing.Store) *platformTest {
	t.Helper()
	logs := &logBuf{}
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rc.Close() })
	srv := NewServer(slog.New(slog.NewJSONHandler(logs, nil)), 0, store).WithCounters(counter.New(rc))
	srv.now = func() time.Time { return time.Unix(1_760_000_010, 0) } // 30 s left in the minute
	conn := serve(t, srv)
	return &platformTest{t: t, srv: srv, logs: logs, redis: mr, conn: func() extprocv3.ExternalProcessor_ProcessClient { return open(t, conn) }}
}

// request sends one request phase from a client at addr ("" for no source.address).
func (pt *platformTest) request(stream extprocv3.ExternalProcessor_ProcessClient, method, path, addr string, kv ...string) *extprocv3.ProcessingResponse {
	pt.t.Helper()
	req := requestHeaders(platformRoute, append([]string{":method", method, ":path", path}, kv...)...)
	if addr != "" {
		req.Attributes[attributesKey].Fields[sourceAddressAttr] = structpb.NewStringValue(addr)
	}
	return roundTrip(pt.t, stream, req)
}

func (pt *platformTest) respond(stream extprocv3.ExternalProcessor_ProcessClient) map[string]string {
	pt.t.Helper()
	resp := roundTrip(pt.t, stream, &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &extprocv3.HttpHeaders{Headers: headers(":status", "200", "ratelimit-limit", "999")},
	}})
	out := map[string]string{}
	for _, o := range resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders() {
		out[o.GetHeader().GetKey()] = string(o.GetHeader().GetRawValue())
	}
	return out
}

func continued(t *testing.T, resp *extprocv3.ProcessingResponse) {
	t.Helper()
	if r := resp.GetRequestHeaders().GetResponse(); r == nil || r.GetStatus() != extprocv3.CommonResponse_CONTINUE {
		t.Fatalf("want CONTINUE, got %v", resp)
	}
}

// denied checks an immediate JSON denial and returns its headers.
func denied(t *testing.T, resp *extprocv3.ProcessingResponse, status int, code string) map[string]string {
	t.Helper()
	ir := resp.GetImmediateResponse()
	if ir == nil || int(ir.GetStatus().GetCode()) != status {
		t.Fatalf("want an immediate %d, got %v", status, resp)
	}
	var body map[string]string
	if err := json.Unmarshal(ir.GetBody(), &body); err != nil || body["code"] != code || len(body) != 1 {
		t.Fatalf("body %q, want {\"code\":%q}", ir.GetBody(), code)
	}
	h := immediateHeaders(t, ir)
	if h["content-type"] != "application/json" || h["cache-control"] != "no-store" {
		t.Errorf("headers %v", h)
	}
	return h
}

func TestPlatformAllowAndDeny(t *testing.T) {
	pt := newPlatformTest(t, platformStore(t, "deny", compiledDoc(t)))

	// The free rule: forwarded, without the client's payment headers, and no limit headers.
	s := pt.conn()
	resp := pt.request(s, "GET", "/anything/platform/free?x=1", "203.0.113.5:4711", "payment-signature", "abc")
	continued(t, resp)
	removed := resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders()
	if strings.Join(removed, ",") != "payment-signature,x-payment" {
		t.Errorf("removed %v, want the payment headers", removed)
	}
	if h := pt.respond(s); len(h) != 0 {
		t.Errorf("response headers on a rule without limits: %v", h)
	}

	// The deny rule: its status and code, never forwarded.
	denied(t, pt.request(pt.conn(), "GET", "/anything/platform/blocked", "203.0.113.5:4711"), 403, "not_for_sale")

	// No rule matches; the policy's default is allow.
	continued(t, pt.request(pt.conn(), "GET", "/somewhere/else", "203.0.113.5:4711"))
	// HEAD does not match a GET rule (the reference evaluator's rule; the SDK differs): the
	// blocked endpoint falls to the default.
	continued(t, pt.request(pt.conn(), "HEAD", "/anything/platform/blocked", "203.0.113.5:4711"))

	for _, want := range []string{`"decision":"rule_allowed"`, `"decision":"rule_denied"`, `"rule":"get-anything-platform-blocked/deny"`, `"policyVersion":2`, `"project":"` + platformProject + `"`} {
		if !strings.Contains(pt.logs.String(), want) {
			t.Errorf("log lacks %s", want)
		}
	}
}

// The rate limit is one count in Redis: the sixth request in the minute is refused with 429
// and Retry-After, and every allowed response carries the limit headers, ours replacing the
// backend's.
func TestPlatformRateLimit(t *testing.T) {
	pt := newPlatformTest(t, platformStore(t, "deny", compiledDoc(t)))
	for i := 1; i <= 5; i++ {
		s := pt.conn()
		continued(t, pt.request(s, "GET", "/anything/platform/limited", "198.51.100.1:1"))
		h := pt.respond(s)
		want := map[string]string{"ratelimit-limit": "5", "ratelimit-remaining": string(rune('0' + 5 - i)), "ratelimit-reset": "30"}
		for k, v := range want {
			if h[k] != v {
				t.Errorf("request %d: %s = %q, want %q (all: %v)", i, k, h[k], v, h)
			}
		}
	}
	h := denied(t, pt.request(pt.conn(), "GET", "/anything/platform/limited", "198.51.100.2:1"), 429, "rate_limited")
	if h["retry-after"] != "30" || h["ratelimit-remaining"] != "0" {
		t.Errorf("429 headers %v", h)
	}
}

// The free quota counts per client address, from Envoy's source.address. Past it the charge
// applies: a 402.
func TestPlatformFreeQuotaPerClient(t *testing.T) {
	pt := newPlatformTest(t, platformStore(t, "deny", compiledDoc(t)))
	for i := 0; i < 3; i++ {
		continued(t, pt.request(pt.conn(), "GET", "/anything/platform/metered", "203.0.113.5:4711"))
	}
	resp := pt.request(pt.conn(), "GET", "/anything/platform/metered", "203.0.113.5:4712")
	ir := resp.GetImmediateResponse()
	if ir == nil || ir.GetStatus().GetCode() != 402 {
		t.Fatalf("past the free quota: %v", resp)
	}
	// The 402 carries the free quota's headers (§4.5): nothing left this window.
	if h := immediateHeaders(t, ir); h["ratelimit-limit"] != "3" || h["ratelimit-remaining"] != "0" {
		t.Errorf("402 limit headers %v", h)
	}
	// Another client has its own quota.
	continued(t, pt.request(pt.conn(), "GET", "/anything/platform/metered", "[2001:db8::1]:443"))
	// No address at all counts on ip:unknown, shared by every such request.
	continued(t, pt.request(pt.conn(), "GET", "/anything/platform/metered", ""))
	keys := strings.Join(pt.redis.Keys(), " ")
	for _, want := range []string{":ip:203.0.113.5", ":ip:2001:db8::/64", ":ip:unknown"} {
		if !strings.Contains(keys, want) {
			t.Errorf("no counter key with %s: %s", want, keys)
		}
	}
}

// With Redis down every counter reads as failed: counter_error is within in this document, so
// the limited rule allows, and without a working count no limit headers are sent.
func TestPlatformCounterFailure(t *testing.T) {
	pt := newPlatformTest(t, platformStore(t, "deny", compiledDoc(t)))
	pt.redis.Close()
	s := pt.conn()
	continued(t, pt.request(s, "GET", "/anything/platform/limited", "198.51.100.1:1"))
	if h := pt.respond(s); len(h) != 0 {
		t.Errorf("limit headers from a failed counter: %v", h)
	}
	if !strings.Contains(pt.logs.String(), "counter store failed") {
		t.Error("the failure is not logged")
	}

	// counter_error over: the same failure denies.
	over := strings.Replace(compiledDoc(t), `"counter_error": "within"`, `"counter_error": "over"`, 1)
	pt = newPlatformTest(t, platformStore(t, "deny", over))
	pt.redis.Close()
	h := denied(t, pt.request(pt.conn(), "GET", "/anything/platform/limited", "198.51.100.1:1"), 429, "rate_limited")
	if _, ok := h["retry-after"]; ok {
		t.Error("Retry-After from a failed counter")
	}
}

func TestPlatformNoPolicy(t *testing.T) {
	pt := newPlatformTest(t, platformStore(t, "deny", ""))
	denied(t, pt.request(pt.conn(), "GET", "/anything/platform/free", "203.0.113.5:1"), 503, "policy_unavailable")
	if !strings.Contains(pt.logs.String(), `"decision":"policy_unavailable"`) {
		t.Error("decision not logged")
	}

	pt = newPlatformTest(t, platformStore(t, "allow", ""))
	continued(t, pt.request(pt.conn(), "GET", "/anything/platform/paid/1", "203.0.113.5:1"))
}
