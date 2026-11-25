// Package metricstest reads the gate's metrics in tests. Metrics are process-wide, so a
// test compares a value before and after what it does, never an absolute number.
package metricstest

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/amoghjay/x402_gateway/internal/metrics"
)

// Value is a counter's or gauge's value, or a histogram's sample count, for the series
// with exactly these labels; 0 if the series does not exist yet.
func Value(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !matches(m, labels) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// WaitFor polls until the series reaches want or the wait ends, then fails the test if
// it has not. The filter records a phase after Envoy (here, the test) has its answer, so
// a test that reads right after a round trip has to wait.
func WaitFor(t testing.TB, name string, labels map[string]string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for Value(t, name, labels) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := Value(t, name, labels); got != want {
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}

func matches(m *dto.Metric, labels map[string]string) bool {
	if len(m.GetLabel()) != len(labels) {
		return false
	}
	for _, lp := range m.GetLabel() {
		if v, ok := labels[lp.GetName()]; !ok || v != lp.GetValue() {
			return false
		}
	}
	return true
}
