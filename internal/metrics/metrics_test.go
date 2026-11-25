package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestResult(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ResultOK},
		{context.DeadlineExceeded, ResultTimeout},
		{fmt.Errorf("balanceOf: %w", context.DeadlineExceeded), ResultTimeout},
		{timeoutErr{}, ResultTimeout},
		{errors.New("connection refused"), ResultError},
	} {
		if got := Result(c.err); got != c.want {
			t.Errorf("Result(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

func TestHandlerServesTheGateMetrics(t *testing.T) {
	SetBuildInfo("test", "filter")
	Decision("httproute/demo/httpbin", PhaseRequest, "payment_required", "")
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`x402_build_info{component="filter",version="test"} 1`,
		`x402_filter_decisions_total{decision="payment_required",phase="request",reason="",route="httproute/demo/httpbin"}`,
		"go_goroutines",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}
