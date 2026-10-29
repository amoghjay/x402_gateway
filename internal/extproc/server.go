// Package extproc implements Envoy's external processing (ext_proc) gRPC service.
//
// How the hop works: for every request on a route with an EnvoyExtensionPolicy, Envoy
// opens one bidirectional gRPC stream to this server. It sends one message per phase it
// was configured to send (request headers, then response headers), and waits for our
// answer to each before moving the request on. So each Recv below is a request paused
// inside Envoy, and the Send is what lets it continue.
//
// Gate Zero: the server is a no-op. It logs both phases and lets everything through,
// which proves the wiring (policy → Envoy → this pod → back) before any payment logic.
package extproc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// redacted headers are logged as "[redacted]": credentials have no place in logs.
var redacted = map[string]bool{
	"authorization": true,
	"cookie":        true,
	"set-cookie":    true,
}

// Server is the no-op ext_proc server.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	log   *slog.Logger
	delay time.Duration
}

// NewServer returns a Server that logs every phase to log. delay holds each request
// phase that long before answering; it exists for the timeout drill and is 0 otherwise.
func NewServer(log *slog.Logger, delay time.Duration) *Server {
	return &Server{log: log, delay: delay}
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

// handle answers one phase. An empty CommonResponse means "continue unchanged".
func (s *Server) handle(ctx context.Context, req *extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, error) {
	switch r := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		h := headerMap(r.RequestHeaders.GetHeaders())
		s.log.Info("request headers",
			"method", h[":method"], "path", h[":path"], "authority", h[":authority"], "headers", h)
		if err := wait(ctx, s.delay); err != nil {
			return nil, err
		}
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}},
			},
		}, nil

	case *extprocv3.ProcessingRequest_ResponseHeaders:
		h := headerMap(r.ResponseHeaders.GetHeaders())
		s.log.Info("response headers", "status", h[":status"], "headers", h)
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

// headerMap flattens Envoy's header list for logging. Envoy may send a value as bytes
// (RawValue) rather than a string (Value), so both are read.
func headerMap(hm *corev3.HeaderMap) map[string]string {
	out := make(map[string]string, len(hm.GetHeaders()))
	for _, hv := range hm.GetHeaders() {
		k := strings.ToLower(hv.GetKey())
		v := hv.GetValue()
		if v == "" {
			v = string(hv.GetRawValue())
		}
		if redacted[k] {
			v = "[redacted]"
		}
		out[k] = v
	}
	return out
}
