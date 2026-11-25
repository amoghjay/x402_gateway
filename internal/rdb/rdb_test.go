package rdb

import (
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/amoghjay/x402_gateway/internal/metrics/metricstest"
)

// Every command is a dependency call labelled with its name; a miss (redis.Nil) is an
// answer, not a failure.
func TestCommandsAreRecorded(t *testing.T) {
	mr := miniredis.RunT(t)
	c := RequestPath(mr.Addr())
	t.Cleanup(func() { _ = c.Close() })
	ok := map[string]string{"dependency": "redis", "operation": "get", "result": "ok"}
	before := metricstest.Value(t, "x402_dependency_duration_seconds", ok)
	if err := c.Get(t.Context(), "missing").Err(); err == nil {
		t.Fatal("want redis.Nil for a missing key")
	}
	if got := metricstest.Value(t, "x402_dependency_duration_seconds", ok); got != before+1 {
		t.Errorf("get ok = %v, want %v", got, before+1)
	}
	mr.Close()
	failed := map[string]string{"dependency": "redis", "operation": "get", "result": "error"}
	f0 := metricstest.Value(t, "x402_dependency_duration_seconds", failed)
	_ = c.Get(t.Context(), "k").Err()
	if got := metricstest.Value(t, "x402_dependency_duration_seconds", failed); got <= f0 {
		t.Errorf("a failed get was not recorded as an error (%v)", got)
	}
}
