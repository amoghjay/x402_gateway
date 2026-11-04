// Package extproc implements Envoy's external processing (ext_proc) gRPC service.
//
// How the hop works: for every request on a route with an EnvoyExtensionPolicy, Envoy
// opens one bidirectional gRPC stream to this server. It sends one message per phase it
// was configured to send (request headers, then response headers), and waits for our
// answer to each before moving the request on. So each Recv below is a request paused
// inside Envoy, and the Send is what lets it continue.
//
// M3: on a priced route the request phase answers with an ImmediateResponse 402 (the
// paywall), and the backend is never contacted. Envoy then closes the stream: no response
// phase follows a request it answered itself. Which route was matched comes from Envoy,
// as the xds.route_name attribute the policy asks it to send; the filter never matches
// paths itself.
package extproc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/amoghjay/x402_gateway/internal/paywall"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Where Envoy puts the attributes the policy asks for: a flat Struct under the ext_proc
// filter's name, one field per attribute, keyed by the attribute's full name. An
// attribute Envoy cannot evaluate is left out, not sent empty (Envoy 1.39,
// ext_proc/matching_utils.cc), so absence has to be handled.
const (
	attributesKey = "envoy.filters.http.ext_proc"
	routeNameAttr = "xds.route_name"
)

// Decision is what the filter did with a request: one per request, logged, and later
// the label of the request counter. A closed set, so "free because unpriced" and "free
// because something failed" can never be confused in the logs.
type Decision string

const (
	PaymentRequired          Decision = "payment_required"           // 402, no payment sent
	PaymentPresentUnverified Decision = "payment_present_unverified" // 402: a payment was sent; nothing verifies it before M5
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

// Server is the ext_proc server.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	log    *slog.Logger
	delay  time.Duration
	prices *pricing.Store
}

// NewServer returns a Server that prices requests from prices and logs one decision per
// request. delay holds each request phase that long before answering; it exists for the
// timeout drill and is 0 otherwise.
func NewServer(log *slog.Logger, delay time.Duration, prices *pricing.Store) *Server {
	return &Server{log: log, delay: delay, prices: prices}
}

// Process handles one HTTP request's stream: one message in, one answer out, per phase.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil // Envoy closed the stream: the request is done
		}
		if err != nil {
			return err
		}

		resp, err := s.handle(stream.Context(), req)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// handle answers one phase.
func (s *Server) handle(ctx context.Context, req *extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, error) {
	switch r := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		if err := wait(ctx, s.delay); err != nil {
			return nil, err
		}
		return s.requestHeaders(req, headerMap(r.RequestHeaders.GetHeaders())), nil

	case *extprocv3.ProcessingRequest_ResponseHeaders:
		if s.log.Enabled(context.Background(), slog.LevelDebug) {
			s.log.Debug("response headers", "headers", redact(headerMap(r.ResponseHeaders.GetHeaders())))
		}
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}},
			},
		}, nil

	default:
		// Bodies and trailers are never requested (processingMode sends headers only).
		// Failing loudly beats a silent hang if the policy is changed to send them.
		return nil, status.Errorf(codes.Unimplemented, "ext_proc phase %T not handled", r)
	}
}

func (s *Server) requestHeaders(req *extprocv3.ProcessingRequest, h map[string]string) *extprocv3.ProcessingResponse {
	route := req.GetAttributes()[attributesKey].GetFields()[routeNameAttr].GetStringValue()
	log := s.log.With("method", h[":method"], "authority", h[":authority"], "path", h[":path"], "route", route)
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
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}},
			},
		}
	}

	decision := PaymentRequired
	if h[strings.ToLower(x402.HeaderPaymentSignature)] != "" {
		// Until M5 wires the verifier, the balance check and the replay guard, a payment
		// is not enough to be served: nothing goes through unpaid-in-fact.
		decision = PaymentPresentUnverified
	}
	resp, err := paywall.Render(entry, paywall.Request{
		Method:    h[":method"],
		URL:       publicURL(h),
		Accept:    h["accept"],
		UserAgent: h["user-agent"],
	})
	if err != nil {
		log.Error("could not build the 402", "decision", PaywallError, "err", err)
		return plain(http.StatusInternalServerError, "x402 filter: could not build the payment request\n", PaywallError)
	}
	log.Info("payment required", "decision", decision, "price", entry.Requirements.Amount, "network", entry.Requirements.Network)
	return immediate(resp.Status, resp.Headers, resp.Body, decision)
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
	set := make([]*corev3.HeaderValueOption, 0, len(headers))
	for k, vs := range headers {
		for _, v := range vs {
			set = append(set, &corev3.HeaderValueOption{
				Header:       &corev3.HeaderValue{Key: strings.ToLower(k), RawValue: []byte(v)},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			})
		}
	}
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: status},
				Headers: &extprocv3.HeaderMutation{SetHeaders: set},
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
