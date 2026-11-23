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
// Envoy's 504. Response phase: a response that delivers nothing (1xx, 4xx, 5xx, 304) releases the
// reservation; a 2xx or a redirect is charged (chargeable). One on an
// Async route is captured (the settlement job goes on the stream and the receipt header
// goes to the client, before the money has moved). One on a Sync route is settled
// through the facilitator while Envoy holds the response: Envoy sends nothing to the
// client until this phase is answered, so the response is released only once the
// transfer is on chain, and replaced with a 402 or a 503 otherwise (sync.go).
// If the stream ends with a reservation neither captured nor released (the client went
// away, the upstream reset), it is released on the way out.
package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/amoghjay/x402_gateway/internal/balance"
	"github.com/amoghjay/x402_gateway/internal/counter"
	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/paywall"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/x402"
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
	Replay                   Decision = "replay"                     // 402: this signature was already served (Redis), or already spent on chain (Sync)
	InsufficientFunds        Decision = "insufficient_funds"         // 402: balance minus in-flight is below the price
	PaymentAccepted          Decision = "payment_accepted"           // continue: verified and reserved; the response phase decides the rest
	Paid                     Decision = "paid"                       // Async, response chargeable: captured for settlement, receipt sent
	ReleasedNotServed        Decision = "released_not_served"        // response delivers nothing (1xx, 4xx, 5xx, 304): reservation released
	ReleasedStreamEnd        Decision = "released_stream_end"        // stream ended without a response phase: reservation released
	CaptureFailed            Decision = "capture_failed"             // Async, response chargeable but the job could not be queued: served unpaid
	Settled                  Decision = "settled"                    // Sync, response chargeable: settled through the facilitator, then released
	SettledOnChain           Decision = "settled_on_chain"           // Sync: /settle failed or was unclear, but the chain says the transfer happened; released
	SettleRefused            Decision = "settle_refused"             // Sync: 402 in place of the response; the settlement was refused, nothing charged
	SettlementUnknown        Decision = "settlement_unknown"         // Sync: 503 in place of the response; the settlement's outcome is not known
	Deadline                 Decision = "deadline"                   // 503 + Retry-After: the balance read overran the filter's deadline
	DependencyUnavailable    Decision = "dependency_unavailable"     // 503 + Retry-After: Redis or the RPC failed
	Unpriced                 Decision = "unpriced"                   // continue: the route has no price (a policy being removed)
	PolicyInvalid            Decision = "policy_invalid"             // 503: the route's policy or price entry is invalid; refused, never served free
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

	// Sync routes settle on the response phase, through the route's network's
	// facilitator. A Sync route on a network without one is refused at the door: it
	// could be forwarded, but never released.
	Facilitators map[string]*facilitator.Client // by CAIP-2 network
	// RPCs answer authorizationState: before a settle (a nonce already used is a replay),
	// and after one that gave no clear answer. Required for Sync routes, like Facilitators.
	RPCs          map[string]*radius.RPC // by CAIP-2 network
	SettleTimeout time.Duration          // per held response; default DefaultSettleTimeout
	ConfirmWait   time.Duration          // after an unclear settle; default DefaultConfirmWait
	// FacilitatorVerify: on a Sync route, also ask the facilitator's /verify on the
	// request, after the local checks and the reservation. The gate standard requires
	// it; it costs one facilitator round trip before the backend is called.
	FacilitatorVerify bool
	VerifyTimeout     time.Duration // default DefaultVerifyTimeout

	// FacilitatorHTTP is the client for facilitators a Radius Platform compiled policy
	// names (each payment option carries its own). One facilitator.Client is kept per URL.
	FacilitatorHTTP *http.Client
	byURL           sync.Map // facilitator URL → *facilitator.Client
}

// facilitatorAt is the client for a facilitator a compiled policy names.
func (p *Payments) facilitatorAt(url string) *facilitator.Client {
	if c, ok := p.byURL.Load(url); ok {
		return c.(*facilitator.Client)
	}
	hc := p.FacilitatorHTTP
	if hc == nil {
		hc = radius.NewFacilitatorHTTPClient()
	}
	c, _ := p.byURL.LoadOrStore(url, &facilitator.Client{URL: url, HTTP: hc})
	return c.(*facilitator.Client)
}

// facilitator is the request's facilitator: the one its payment option names (a platform
// route), else its network's.
func (s *Server) facilitator(st *request) *facilitator.Client {
	if st.fac != nil {
		return st.fac
	}
	return s.pay.Facilitators[st.entry.Network.Network()]
}

// DefaultVerifyTimeout bounds the facilitator's /verify on a Sync route's request. Envoy
// allows a Sync route 10 s per message, so it has room; a slow facilitator gets a 503.
const DefaultVerifyTimeout = 2 * time.Second

func (p *Payments) verifyTimeout() time.Duration {
	if p.VerifyTimeout > 0 {
		return p.VerifyTimeout
	}
	return DefaultVerifyTimeout
}

// Server is the ext_proc server.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	log      *slog.Logger
	delay    time.Duration
	prices   *pricing.Store
	pay      *Payments
	counters *counter.Store   // platform routes' limits; nil reads every counter as failed
	now      func() time.Time // the policy's clock: counter windows; tests set it
}

// NewServer returns a Server that prices requests from prices and logs one decision per
// request. delay holds each request phase that long before answering; it exists for the
// timeout drill and is 0 otherwise.
func NewServer(log *slog.Logger, delay time.Duration, prices *pricing.Store) *Server {
	return &Server{log: log, delay: delay, prices: prices, now: time.Now}
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
	payment  string              // the PAYMENT-SIGNATURE value, as received, for the settlement job
	pp       x402.PaymentPayload // the same, decoded, for a Sync route's /settle
	pw       paywall.Request     // for a 402 sent in place of the response (Sync, settle refused)
	res      verify.Result
	rsv      *reserve.Reservation
	captured bool // the settler owns the in-flight amount now
	released bool
	settled  bool // Sync: the transfer is on chain
	unknown  bool // Sync: the settle's outcome is not known; the in-flight amount stays until its TTL
	held     bool // Sync: the payment's record (reserve.Held) is written and not yet cleared

	ruleHeaders map[string]string   // platform route: the decision's limit headers, for the response
	fac         *facilitator.Client // platform route: the facilitator the paid option names
	remote      *remotePayment      // platform route: a payment on the facilitator-only path
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
			if st.settled {
				// Paid for and not delivered: Envoy (or the client) went away while the
				// response was held. The record stays, so the settler lists it for a refund.
				st.log.Error("settled, but the response could not be handed back to Envoy", "paymentId", st.paymentID(), "err", err)
			}
			return err
		}
		if st.settled {
			// Envoy has the paid response: delivered, so the record can go. Only now, so
			// a crash between the settle and this point leaves it for the settler.
			s.unhold(st)
		}
	}
}

// finish releases a reservation the response phase never settled: the client went
// away, the upstream reset, Envoy gave up. Nothing was served, so nothing is charged.
func (s *Server) finish(st *request) {
	if st.rsv != nil && !st.captured && !st.released && !st.unknown {
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
	if why, blocked := table.Blocked(key); blocked {
		// The route has a policy, but it is broken. Serving it would give it away and a
		// 402 would ask for a payment that cannot be right: refuse until it is fixed.
		log.Error("route's payment policy is invalid; refused", "decision", PolicyInvalid, "reason", why)
		return plain(http.StatusServiceUnavailable, "x402 filter: this route's payment policy is invalid\n", PolicyInvalid)
	}
	if pr, ok := table.Platform(key); ok {
		return s.platformRequest(ctx, req, h, st, key, table, pr)
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

	pw := paywall.Request{Method: h[":method"], URL: publicURL(h), Accept: h["accept"]}
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
	stream := ctx // the facilitator's /verify has its own budget, not the request deadline
	ctx, cancel := context.WithTimeout(ctx, s.pay.deadline())
	defer cancel()
	entry, log := st.entry, st.log

	pp, err := x402.DecodePaymentPayload(hdr)
	if err != nil {
		return s.refuse(st, pw, x402.ReasonInvalidPayload, PaymentInvalid, err)
	}
	res, err := verify.Verify(s.pay.now(), entry.Requirements, entry.Domain, pp)
	if err != nil {
		return s.refuse(st, pw, verify.Reason(err), PaymentInvalid, err)
	}
	log = log.With("payer", res.Payer.String())
	st.log = log
	if n := entry.Network.Network(); entry.Sync() && (s.facilitator(st) == nil || s.pay.RPCs[n] == nil) {
		// Checked before the backend is called: without both the response could never be
		// released, so the request would only cost the seller the work.
		log.Error("the route settles before release, but its network has no facilitator or no RPC configured", "decision", DependencyUnavailable, "network", n)
		return retryLater(DependencyUnavailable)
	}

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
	st.res, st.rsv, st.payment, st.pp, st.pw = res, rsv, hdr, pp, pw

	if entry.Sync() && s.pay.FacilitatorVerify {
		if refused := s.facilitatorVerify(stream, st, pw); refused != nil {
			return refused
		}
	}

	log.Info("payment accepted", "decision", PaymentAccepted, "paymentId", paymentID(res), "amount", entry.Price.String(),
		"balance", bal.Value.String(), "balanceSource", bal.Source, "inflight", rsv.Inflight.String(), "ms", time.Since(t0).Milliseconds())
	// The signature never reaches the backend; the payer's identity does, in headers
	// only the filter writes (a client-sent copy is overwritten).
	return continueWith(&extprocv3.HeaderMutation{
		RemoveHeaders: []string{strings.ToLower(x402.HeaderPaymentSignature)},
		SetHeaders:    setHeaders(HeaderPayer, res.Payer.String(), HeaderPaymentID, paymentID(res)),
	})
}

// chargeable reports whether a response delivers what was paid for. A 2xx does, and so
// does a redirect: its Location can be the content itself (a presigned download URL),
// so releasing a 3xx unpaid would give the content away. 304 Not Modified does not: it
// delivers nothing the client does not already hold. 1xx, 4xx and 5xx serve nothing.
func chargeable(code int) bool { return code >= 200 && code <= 399 && code != http.StatusNotModified }

// responseHeaders settles the request phase's reservation: a response that is not
// chargeable is released; one that is gets captured (Async) or settled while the
// response is held (Sync). The
// Async path is kept to one Redis call so it cannot approach Envoy's per-message
// timeout, which after a served response would turn a 200 into a 504.
func (s *Server) responseHeaders(ctx context.Context, h map[string]string, st *request) *extprocv3.ProcessingResponse {
	// A platform rule's decision may carry limit headers; they go on whatever the client
	// gets (§4.5).
	return withRuleHeaders(s.respond(ctx, h, st), st.ruleHeaders)
}

func (s *Server) respond(ctx context.Context, h map[string]string, st *request) *extprocv3.ProcessingResponse {
	if st.remote != nil {
		code, _ := strconv.Atoi(h[":status"])
		return s.settleRemote(ctx, code, st)
	}
	if st.rsv == nil {
		return continueResponse(nil) // unpriced, or nothing was reserved
	}
	code, _ := strconv.Atoi(h[":status"])
	if !chargeable(code) {
		// Nothing was served (a backend error, or Envoy's own 502/503/504 after we
		// continued): nobody is charged.
		s.release(st, ReleasedNotServed)
		st.log.Info("not served; released", "decision", ReleasedNotServed, "status", code)
		return continueResponse(nil)
	}
	if st.entry.Sync() {
		return s.settleHeld(ctx, code, st)
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

// paymentID is the request's payment id on either paid path.
func (st *request) paymentID() string {
	if st.remote != nil {
		return st.remote.id
	}
	return paymentID(st.res)
}

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

// facilitatorVerify asks the route's facilitator whether the payment is valid. A no is
// a 402 with its reason; no answer is a 503. Either way the reservation is released
// (nothing was charged) and the nonce stays claimed, as for any refused payment.
func (s *Server) facilitatorVerify(ctx context.Context, st *request, pw paywall.Request) *extprocv3.ProcessingResponse {
	fac := s.facilitator(st) // not nil: checked for Sync routes above
	ctx, cancel := context.WithTimeout(ctx, s.pay.verifyTimeout())
	defer cancel()
	t0 := time.Now()
	vr, err := fac.Verify(ctx, st.pp, st.entry.Requirements)
	ms := time.Since(t0).Milliseconds()
	var fs *facilitator.Status
	switch {
	case err == nil && vr.IsValid && (vr.Payer == "" || strings.EqualFold(vr.Payer, st.res.Payer.String())):
		st.log.Debug("facilitator verified the payment", "ms", ms)
		return nil
	case err == nil && vr.IsValid:
		s.release(st, DependencyUnavailable)
		st.log.Error("facilitator verified a different payer; not charged", "decision", DependencyUnavailable, "facilitatorPayer", vr.Payer, "ms", ms)
		return retryLater(DependencyUnavailable)
	case err == nil, errors.As(err, &fs) && fs.Definite():
		reason := vr.InvalidReason
		if reason == "" && fs != nil {
			var body x402.VerifyResponse
			if json.Unmarshal([]byte(fs.Body), &body) == nil {
				reason = body.InvalidReason
			}
		}
		if reason == "" {
			reason = x402.ReasonUnexpectedVerify
		}
		s.release(st, PaymentInvalid)
		return s.refuse(st, pw, reason, PaymentInvalid, fmt.Errorf("facilitator /verify said no in %d ms: %s", ms, reason))
	default:
		s.release(st, DependencyUnavailable)
		st.log.Error("facilitator /verify gave no answer; not charged", "decision", DependencyUnavailable, "err", err, "ms", ms)
		return retryLater(DependencyUnavailable)
	}
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
	return requestScheme(h) + "://" + h[":authority"] + h[":path"]
}

// requestScheme is the scheme the caller used: X-Forwarded-Proto when a TLS-terminating proxy
// set it, else the downstream connection's.
func requestScheme(h map[string]string) string {
	scheme := h["x-forwarded-proto"]
	if scheme != "http" && scheme != "https" {
		scheme = h[":scheme"]
	}
	if scheme == "" {
		scheme = "http"
	}
	return scheme
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
// bytes (RawValue) rather than a string (Value), so both are read. A field sent on several
// lines is joined with ", " (RFC 9110 §5.3), which is also how a signature covers it
// (RFC 9421 §2.1); a repeated PAYMENT-SIGNATURE so becomes one value that does not parse,
// and is refused rather than one line silently winning.
func headerMap(hm *corev3.HeaderMap) map[string]string {
	out := make(map[string]string, len(hm.GetHeaders()))
	for _, hv := range hm.GetHeaders() {
		v := hv.GetValue()
		if v == "" {
			v = string(hv.GetRawValue())
		}
		k := strings.ToLower(hv.GetKey())
		if prev, ok := out[k]; ok {
			v = prev + ", " + v
		}
		out[k] = v
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

func (p *Payments) settleTimeout() time.Duration {
	if p.SettleTimeout > 0 {
		return p.SettleTimeout
	}
	return DefaultSettleTimeout
}

func (p *Payments) confirmWait() time.Duration {
	if p.ConfirmWait > 0 {
		return p.ConfirmWait
	}
	return DefaultConfirmWait
}

func (p *Payments) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
