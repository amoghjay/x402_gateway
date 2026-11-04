// Package extproc implements Envoy's external processing (ext_proc) gRPC service.
//
// How the hop works: for every request on a route with an EnvoyExtensionPolicy, Envoy
// opens one bidirectional gRPC stream to this server. It sends one message per phase it
// was configured to send (request headers, then response headers), and waits for our
// answer to each before moving the request on. So each Recv below is a request paused
// inside Envoy, and the Send is what lets it continue. One stream is one HTTP request,
// so what the request phase decided is simply remembered for the response phase.
//
// Request phase, on a priced route: no payment → an ImmediateResponse 402 (the paywall)
// and the backend is never contacted; Envoy then closes the stream. A payment → verify
// the signature locally, read the payer's balance (cached), reserve (claim the nonce,
// add to the payer's in-flight sum), strip the payment header, tell the backend who
// paid, and CONTINUE. All of it under the filter's own deadline, well inside Envoy's
// per-message timeout, so a slow dependency gets a retryable 503 from us rather than
// Envoy's 504. Response phase: a 2xx is captured (the settlement job goes on the stream
// and the receipt header goes to the client); anything else releases the reservation.
// If the stream ends with a reservation neither captured nor released (the client went
// away, the upstream reset), it is released on the way out.
package extproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/amoghjay/x402_gateway/internal/balance"
	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/paywall"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
	"github.com/amoghjay/x402_gateway/internal/x402/verify"
)

// Where Envoy puts the attributes the policy asks for: a flat Struct under the ext_proc
// filter's name, one field per attribute, keyed by the attribute's full name. An
// attribute Envoy cannot evaluate is left out, not sent empty (Envoy 1.39,
// ext_proc/matching_utils.cc), so absence has to be handled.
const (
	attributesKey = "envoy.filters.http.ext_proc"
	routeNameAttr = "xds.route_name"
)

// Headers the filter sets for the backend on a paid request. Any client-sent copy is
// overwritten, so a backend can trust them: only the filter writes them.
const (
	HeaderPayer     = "x-x402-payer"
	HeaderPaymentID = "x-x402-payment-id"
)

// ReasonReplay is the 402 error for a signature that was already served. The spec's §9
// has no code for it (the chain refuses a used nonce at settle time; we refuse it at
// the door), and `error` is free-form.
const ReasonReplay = "payment_already_used"

// Defaults for Payments.
const (
	DefaultDeadline       = 250 * time.Millisecond // request phase, under Envoy's 300 ms (the controller's messageTimeout)
	responsePhaseDeadline = 50 * time.Millisecond  // one Redis call; the phase must stay trivial
	// reserveMargin is kept back from the balance step so the reserve script, one Redis
	// round trip, always has room: a balance read that runs to the deadline must not
	// starve the step after it (seen on GKE: "reserve: context deadline exceeded").
	reserveMargin = 25 * time.Millisecond
)

// Decision is what the filter did with a request: one per request phase, logged, and
// later the label of the request counter. A closed set, so "free because unpriced" and
// "free because something failed" can never be confused in the logs.
type Decision string

const (
	PaymentRequired          Decision = "payment_required"           // 402, no payment sent
	PaymentPresentUnverified Decision = "payment_present_unverified" // 402: a payment was sent, but this filter runs without payments wired
	PaymentInvalid           Decision = "payment_invalid"            // 402: the payment failed verification (reason in the 402)
	Replay                   Decision = "replay"                     // 402: this signature was already served
	InsufficientFunds        Decision = "insufficient_funds"         // 402: balance minus in-flight is below the price
	PaymentAccepted          Decision = "payment_accepted"           // continue: verified and reserved; the response phase decides the rest
	Paid                     Decision = "paid"                       // response 2xx: captured for settlement, receipt sent
	ReleasedNon2xx           Decision = "released_non_2xx"           // response not 2xx: nothing served, reservation released
	ReleasedStreamEnd        Decision = "released_stream_end"        // stream ended without a response phase: reservation released
	CaptureFailed            Decision = "capture_failed"             // response 2xx but the job could not be queued: served unpaid
	Deadline                 Decision = "deadline"                   // 503 + Retry-After: the balance read overran the filter's deadline
	DependencyUnavailable    Decision = "dependency_unavailable"     // 503 + Retry-After: Redis or the RPC failed
	Unpriced                 Decision = "unpriced"                   // continue: the route has no price (a policy being removed)
	NoRouteAttribute         Decision = "no_route_attribute"         // 500: the policy does not send xds.route_name
	TableNotLoaded           Decision = "table_not_loaded"           // 503: no price table yet
	PaywallError             Decision = "paywall_error"              // 500: the 402 could not be built
)

// redacted headers are logged as "[redacted]": credentials have no place in logs.
var redacted = map[string]bool{
	"authorization":     true,
	"cookie":            true,
	"set-cookie":        true,
	"payment-signature": true,
}

// Payments is what the paid path needs. Nil on a Server means payments are not wired
// (the M3 shape: every priced request gets the 402).
type Payments struct {
	Balances map[string]*balance.Cache // by CAIP-2 network
	Reserve  *reserve.Store
	Deadline time.Duration    // request-phase budget; default DefaultDeadline
	Now      func() time.Time // tests
}

// SBCDomain is SBC's EIP-712 domain on a network, which the verifier recovers
// signatures against.
func SBCDomain(p radius.Profile) eip3009.Domain {
	return eip3009.Domain{Name: radius.SBCName, Version: radius.SBCVersion, ChainID: p.ChainID, VerifyingContract: radius.SBCAddress}
}

// Server is the ext_proc server.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	log    *slog.Logger
	delay  time.Duration
	prices *pricing.Store
	pay    *Payments
}

// NewServer returns a Server that prices requests from prices and logs one decision per
// request. delay holds each request phase that long before answering; it exists for the
// timeout drill and is 0 otherwise.
func NewServer(log *slog.Logger, delay time.Duration, prices *pricing.Store) *Server {
	return &Server{log: log, delay: delay, prices: prices}
}

// WithPayments wires the paid path.
func (s *Server) WithPayments(p *Payments) *Server {
	s.pay = p
	return s
}

// request is one stream's state: what the request phase decided, for the response
// phase and for the release on the way out.
type request struct {
	log      *slog.Logger
	entry    *pricing.Entry
	payment  string // the PAYMENT-SIGNATURE value, as received, for the settlement job
	res      verify.Result
	rsv      *reserve.Reservation
	captured bool // the settler owns the in-flight amount now
	released bool
}

// Process handles one HTTP request's stream: one message in, one answer out, per phase.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	st := &request{log: s.log}
	defer s.finish(st)
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil // Envoy closed the stream: the request is done
		}
		if err != nil {
			return err
		}

		resp, err := s.handle(stream.Context(), req, st)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// finish releases a reservation the response phase never settled: the client went
// away, the upstream reset, Envoy gave up. Nothing was served, so nothing is charged.
func (s *Server) finish(st *request) {
	if st.rsv != nil && !st.captured && !st.released {
		s.release(st, ReleasedStreamEnd)
	}
}

// handle answers one phase.
func (s *Server) handle(ctx context.Context, req *extprocv3.ProcessingRequest, st *request) (*extprocv3.ProcessingResponse, error) {
	switch r := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		if err := wait(ctx, s.delay); err != nil {
			return nil, err
		}
		return s.requestHeaders(ctx, req, headerMap(r.RequestHeaders.GetHeaders()), st), nil

	case *extprocv3.ProcessingRequest_ResponseHeaders:
		h := headerMap(r.ResponseHeaders.GetHeaders())
		if st.log.Enabled(context.Background(), slog.LevelDebug) {
			st.log.Debug("response headers", "headers", redact(h))
		}
		return s.responseHeaders(ctx, h, st), nil

	default:
		// Bodies and trailers are never requested (processingMode sends headers only).
		// Failing loudly beats a silent hang if the policy is changed to send them.
		return nil, status.Errorf(codes.Unimplemented, "ext_proc phase %T not handled", r)
	}
}

func (s *Server) requestHeaders(ctx context.Context, req *extprocv3.ProcessingRequest, h map[string]string, st *request) *extprocv3.ProcessingResponse {
	route := req.GetAttributes()[attributesKey].GetFields()[routeNameAttr].GetStringValue()
	log := s.log.With("method", h[":method"], "authority", h[":authority"], "path", h[":path"], "route", route)
	st.log = log
	if log.Enabled(context.Background(), slog.LevelDebug) {
		log.Debug("request headers", "headers", redact(h))
	}

	// Fail closed on anything that means the filter cannot tell what the route costs.
	// Envoy's failOpen stays false, so these answers reach the client as they are.
	key, ok := pricing.RouteKey(route)
	if !ok {
		log.Error("no usable xds.route_name; check the EnvoyExtensionPolicy's processingMode.request.attributes", "decision", NoRouteAttribute)
		return plain(http.StatusInternalServerError, "x402 filter: route not identified\n", NoRouteAttribute)
	}
	log = log.With("routeKey", key)
	st.log = log
	table := s.prices.Load()
	if table == nil {
		log.Error("no price table loaded", "decision", TableNotLoaded)
		return plain(http.StatusServiceUnavailable, "x402 filter: price table not loaded\n", TableNotLoaded)
	}
	entry, ok := table.Lookup(key)
	if !ok {
		// A policy attached the route to the filter, but the table has no price for it:
		// the window while a PaymentPolicy is deleted (M4 removes the extension policy
		// first, then the table entry). Nothing to charge, so let it through.
		log.Warn("route is not in the price table; serving it free", "decision", Unpriced)
		return continueWith(nil)
	}
	st.entry = entry

	pw := paywall.Request{Method: h[":method"], URL: publicURL(h), Accept: h["accept"], UserAgent: h["user-agent"]}
	hdr := h[strings.ToLower(x402.HeaderPaymentSignature)]
	switch {
	case hdr == "":
		return s.paywall(st, pw, PaymentRequired)
	case s.pay == nil:
		// Payments are not wired into this filter: a payment is not enough to be served,
		// nothing goes through unpaid-in-fact.
		return s.paywall(st, pw, PaymentPresentUnverified)
	}
	return s.accept(ctx, st, pw, hdr)
}

// accept is the paid path: verify, balance, reserve, continue. Every step runs under
// the filter's deadline. A refusal is a 402 with the reason; our own slowness is a 503
// with Retry-After, never a charge.
func (s *Server) accept(ctx context.Context, st *request, pw paywall.Request, hdr string) *extprocv3.ProcessingResponse {
	ctx, cancel := context.WithTimeout(ctx, s.pay.deadline())
	defer cancel()
	entry, log := st.entry, st.log

	pp, err := x402.DecodePaymentPayload(hdr)
	if err != nil {
		return s.refuse(st, pw, x402.ReasonInvalidPayload, PaymentInvalid, err)
	}
	res, err := verify.Verify(s.pay.now(), entry.Requirements, SBCDomain(entry.Network), pp)
	if err != nil {
		return s.refuse(st, pw, verify.Reason(err), PaymentInvalid, err)
	}
	log = log.With("payer", res.Payer.String())
	st.log = log

	cache := s.pay.Balances[entry.Network.Network()]
	if cache == nil {
		log.Error("no balance cache for the route's network", "decision", DependencyUnavailable, "network", entry.Network.Network())
		return retryLater(DependencyUnavailable)
	}
	t0 := time.Now()
	bctx, bcancel := context.WithTimeout(ctx, max(s.pay.deadline()-reserveMargin, reserveMargin))
	bal, err := cache.Get(bctx, res.Payer)
	bcancel()
	switch {
	case errors.Is(err, balance.ErrDeadline):
		// The read goes on and fills the cache; the client's retry is a hit.
		log.Warn("balance read overran the deadline; not charged", "decision", Deadline, "ms", time.Since(t0).Milliseconds())
		return retryLater(Deadline)
	case err != nil:
		log.Error("balance read failed; not charged", "decision", DependencyUnavailable, "err", err)
		return retryLater(DependencyUnavailable)
	}

	out, rsv, err := s.pay.Reserve.Reserve(ctx, res.Payer, res.Authorization.Nonce, entry.Price, bal.Value, res.Expires)
	if err != nil {
		log.Error("reserve failed; not charged", "decision", DependencyUnavailable, "err", err)
		return retryLater(DependencyUnavailable)
	}
	switch out {
	case reserve.Replay:
		return s.refuse(st, pw, ReasonReplay, Replay, fmt.Errorf("nonce %s was already served", evm.Hex(res.Authorization.Nonce[:])))
	case reserve.Insufficient:
		return s.refuse(st, pw, x402.ReasonInsufficientFunds, InsufficientFunds,
			fmt.Errorf("balance %s (%s, %s old) minus in-flight is below the price %s", bal.Value, bal.Source, bal.Age.Round(time.Millisecond), entry.Price))
	}
	st.res, st.rsv, st.payment = res, rsv, hdr

	log.Info("payment accepted", "decision", PaymentAccepted, "paymentId", paymentID(res), "amount", entry.Price.String(),
		"balance", bal.Value.String(), "balanceSource", bal.Source, "inflight", rsv.Inflight.String(), "ms", time.Since(t0).Milliseconds())
	// The signature never reaches the backend; the payer's identity does, in headers
	// only the filter writes (a client-sent copy is overwritten).
	return continueWith(&extprocv3.HeaderMutation{
		RemoveHeaders: []string{strings.ToLower(x402.HeaderPaymentSignature)},
		SetHeaders:    setHeaders(HeaderPayer, res.Payer.String(), HeaderPaymentID, paymentID(res)),
	})
}

// responseHeaders settles the request phase's reservation: a 2xx is captured, anything
// else released. It is kept to one Redis call so it cannot approach Envoy's per-message
// timeout, which after a served response would turn a 200 into a 504.
func (s *Server) responseHeaders(ctx context.Context, h map[string]string, st *request) *extprocv3.ProcessingResponse {
	if st.rsv == nil {
		return continueResponse(nil) // unpriced, or nothing was reserved
	}
	code, _ := strconv.Atoi(h[":status"])
	if code < 200 || code > 299 {
		// Nothing was served (a backend error, or Envoy's own 502/503/504 after we
		// continued): nobody is charged.
		s.release(st, ReleasedNon2xx)
		st.log.Info("not served; released", "decision", ReleasedNon2xx, "status", code)
		return continueResponse(nil)
	}

	ctx, cancel := context.WithTimeout(ctx, responsePhaseDeadline)
	defer cancel()
	id := paymentID(st.res)
	entry := st.entry
	streamID, err := s.pay.Reserve.Capture(ctx, reserve.Job{
		PaymentID:    id,
		Payer:        st.res.Payer.String(),
		Route:        entry.Key,
		Amount:       entry.Price.String(),
		Network:      entry.Network.Network(),
		ValidBefore:  st.res.Expires.Unix(),
		Payment:      st.payment,
		Requirements: entry.Requirements,
		ServedAt:     s.pay.now(),
	})
	if err != nil {
		// The job is the only copy of the signed authorization; without it there is
		// nothing to settle. Served, unpaid, and said so: the first correctness SLI.
		s.release(st, CaptureFailed)
		st.log.Error("served but not captured: unpaid", "decision", CaptureFailed, "paymentId", id, "err", err)
		return continueResponse(&extprocv3.HeaderMutation{SetHeaders: setHeaders("cache-control", "private, no-store")})
	}
	st.captured = true

	receipt, err := x402.SettleResponse{
		Success:     true,
		Transaction: "", // settled after this response; a receipts endpoint is backlog
		Network:     entry.Network.Network(),
		Payer:       st.res.Payer.String(),
		Extensions:  map[string]any{x402.SettlementExtension: map[string]string{"status": "pending", "paymentId": id}},
	}.Encode()
	if err != nil {
		st.log.Error("receipt header could not be built", "err", err) // served and captured; only the header is missing
		return continueResponse(&extprocv3.HeaderMutation{SetHeaders: setHeaders("cache-control", "private, no-store")})
	}
	st.log.Info("paid", "decision", Paid, "paymentId", id, "stream", streamID, "status", code)
	// no-store overrides whatever the backend said: a shared cache must never resell a
	// paid response to the next caller.
	return continueResponse(&extprocv3.HeaderMutation{SetHeaders: setHeaders(
		"cache-control", "private, no-store",
		strings.ToLower(x402.HeaderPaymentResponse), receipt,
		"access-control-expose-headers", x402.HeaderPaymentResponse,
	)})
}

// release gives the reservation back. It runs on its own short context: the stream's
// may already be cancelled, and a release must not be lost to that.
func (s *Server) release(st *request, why Decision) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), responsePhaseDeadline)
	defer cancel()
	st.released = true
	if err := s.pay.Reserve.Release(ctx, st.rsv.Payer, st.rsv.Amount); err != nil {
		// The in-flight TTL (30 s) is the backstop: the payer is overcounted until then.
		st.log.Warn("release failed; the in-flight TTL will clear it", "decision", why, "err", err)
		return
	}
	if why == ReleasedStreamEnd {
		st.log.Info("stream ended before a response; released", "decision", why)
	}
}

// paymentID identifies a payment by its nonce: unique per signature (the replay guard
// sees to that), and the same value the chain's AuthorizationUsed event carries.
func paymentID(res verify.Result) string { return evm.Hex(res.Authorization.Nonce[:]) }

// paywall answers 402 with how to pay.
func (s *Server) paywall(st *request, pw paywall.Request, d Decision) *extprocv3.ProcessingResponse {
	resp, err := paywall.Render(st.entry, pw)
	if err != nil {
		st.log.Error("could not build the 402", "decision", PaywallError, "err", err)
		return plain(http.StatusInternalServerError, "x402 filter: could not build the payment request\n", PaywallError)
	}
	st.log.Info("payment required", "decision", d, "price", st.entry.Requirements.Amount, "network", st.entry.Requirements.Network)
	return immediate(resp.Status, resp.Headers, resp.Body, d)
}

// refuse answers a 402 whose error line leads with the reason, so the client knows why
// its payment was turned down and how to pay again.
func (s *Server) refuse(st *request, pw paywall.Request, reason string, d Decision, cause error) *extprocv3.ProcessingResponse {
	pw.Refused = reason
	resp, err := paywall.Render(st.entry, pw)
	if err != nil {
		st.log.Error("could not build the 402", "decision", PaywallError, "err", err)
		return plain(http.StatusInternalServerError, "x402 filter: could not build the payment request\n", PaywallError)
	}
	st.log.Info("payment refused", "decision", d, "reason", reason, "cause", cause.Error())
	return immediate(resp.Status, resp.Headers, resp.Body, d)
}

// retryLater is the answer when our side, not the buyer, failed: 503 with Retry-After,
// nothing charged. The next attempt usually hits the cache the first one filled.
func retryLater(d Decision) *extprocv3.ProcessingResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "1")
	return immediate(http.StatusServiceUnavailable, h, []byte("x402 filter: temporarily unable to check the payment; retry in 1s\n"), d)
}

// continueWith lets the request through, with header mutations if any.
func continueWith(m *extprocv3.HeaderMutation) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{HeaderMutation: m}},
		},
	}
}

func continueResponse(m *extprocv3.HeaderMutation) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{HeaderMutation: m}},
		},
	}
}

// setHeaders builds overwrite mutations from key/value pairs. Overwrite, always: the
// zero append action appends, which would comma-join Cache-Control with the upstream's
// and let a client-sent copy of our headers through.
func setHeaders(kv ...string) []*corev3.HeaderValueOption {
	out := make([]*corev3.HeaderValueOption, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: strings.ToLower(kv[i]), RawValue: []byte(kv[i+1])},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return out
}

// publicURL rebuilds the URL the caller asked for, for resource.url (radius-cli sell check
// compares the two). X-Forwarded-Proto wins when a TLS-terminating proxy sits in front;
// Envoy sets it from the downstream connection otherwise.
func publicURL(h map[string]string) string {
	scheme := h["x-forwarded-proto"]
	if scheme != "http" && scheme != "https" {
		scheme = h[":scheme"]
	}
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + h[":authority"] + h[":path"]
}

// immediate answers the request from the filter. Header values go in RawValue: Envoy's
// ext_proc reads raw_value and ignores value, and a header it rejects is only logged
// while the response goes out without it. Every header overwrites, so our Content-Type
// replaces the text/plain Envoy gives a local reply with a body.
func immediate(code int, headers http.Header, body []byte, d Decision) *extprocv3.ProcessingResponse {
	status := typev3.StatusCode_InternalServerError
	if code >= 200 && code <= 599 { // Envoy refuses anything below 200 and uses its default
		status = typev3.StatusCode(int32(code))
	}
	var kv []string
	for k, vs := range headers {
		for _, v := range vs {
			kv = append(kv, k, v)
		}
	}
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: status},
				Headers: &extprocv3.HeaderMutation{SetHeaders: setHeaders(kv...)},
				Body:    body,
				Details: "x402_" + string(d), // shows up as %RESPONSE_CODE_DETAILS% in access logs
			},
		},
	}
}

func plain(code int, body string, d Decision) *extprocv3.ProcessingResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return immediate(code, h, []byte(body), d)
}

// wait sleeps for d, unless Envoy gives up on the stream first (its message timeout, or
// the client went away). Then the context is cancelled and holding on would only keep a
// dead stream open, and delay the drain on SIGTERM.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// headerMap flattens Envoy's header list, keys lower-cased. Envoy may send a value as
// bytes (RawValue) rather than a string (Value), so both are read.
func headerMap(hm *corev3.HeaderMap) map[string]string {
	out := make(map[string]string, len(hm.GetHeaders()))
	for _, hv := range hm.GetHeaders() {
		v := hv.GetValue()
		if v == "" {
			v = string(hv.GetRawValue())
		}
		out[strings.ToLower(hv.GetKey())] = v
	}
	return out
}

// redact copies h for logging, with credentials and payments replaced.
func redact(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if redacted[k] {
			v = "[redacted]"
		}
		out[k] = v
	}
	return out
}

func (p *Payments) deadline() time.Duration {
	if p.Deadline > 0 {
		return p.Deadline
	}
	return DefaultDeadline
}

func (p *Payments) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
