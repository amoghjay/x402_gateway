package extproc

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// dial starts the server on an in-memory listener and opens one stream, as Envoy would
// for one HTTP request. It returns the stream and everything the server logged.
func dial(t *testing.T) (extprocv3.ExternalProcessor_ProcessClient, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, NewServer(slog.New(slog.NewJSONHandler(&logs, nil))))
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
	for i := 0; i < len(kv); i += 2 {
		// RawValue, as current Envoy sends it.
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{Key: kv[i], RawValue: []byte(kv[i+1])})
	}
	return hm
}

func TestBothPhasesContinueUnchanged(t *testing.T) {
	stream, logs := dial(t)

	err := stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{Headers: headers(":method", "GET", ":path", "/get", "authorization", "Bearer s3cret")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetRequestHeaders().GetResponse(); got == nil || got.GetHeaderMutation() != nil || got.GetStatus() != extprocv3.CommonResponse_CONTINUE {
		t.Fatalf("request phase: want an unchanged CONTINUE, got %v", resp)
	}

	err = stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &extprocv3.HttpHeaders{Headers: headers(":status", "200")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResponseHeaders().GetResponse() == nil {
		t.Fatalf("response phase: want a ResponseHeaders answer, got %v", resp)
	}

	if !strings.Contains(logs.String(), `"path":"/get"`) {
		t.Errorf("request path not logged: %s", logs)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Errorf("authorization header leaked into logs: %s", logs)
	}
}

func TestBodyPhaseFailsLoudly(t *testing.T) {
	stream, _ := dial(t)

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
