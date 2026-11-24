// Package metrics is the gate's Prometheus instrumentation: one registry for the filter
// and the settler, the metric definitions, and small recording helpers so call sites stay
// one line.
//
// Names follow the Prometheus conventions: an `x402_` prefix, counters ending in `_total`,
// durations in seconds. Labels are bounded by configuration or by closed sets defined in
// code. A payer address, nonce, transaction hash, raw path or URL is never a label: each
// would add one series per request.
//
// The controller does not serve this registry: controller-runtime already serves its own,
// and the controller registers the two collectors it needs (BuildInfo, PlatformFetches)
// there.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Phases of one request through the filter: Envoy's request headers, its response
// headers, and the release on the way out when a stream ends without a response phase.
const (
	PhaseRequest   = "request"
	PhaseResponse  = "response"
	PhaseStreamEnd = "stream_end"
)

// Dependencies the gate calls.
const (
	DepRedis       = "redis"
	DepRPC         = "rpc"
	DepFacilitator = "facilitator"
)

// Results of a dependency call.
const (
	ResultOK      = "ok"
	ResultError   = "error"
	ResultTimeout = "timeout"
)

// durationBuckets run from 1 ms to 10 s: a request phase on an Async route answers in
// milliseconds, and a Sync route's held settle may take up to Envoy's 10 s message timeout.
var durationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

var (
	// BuildInfo is 1, labelled with the binary's version and component.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "x402_build_info",
		Help: "Always 1; labelled with the running binary's version and component.",
	}, []string{"version", "component"})

	filterDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "x402_filter_decisions_total",
		Help: "Decisions the filter made, one per request phase that decided something. reason is the 402 reason or the policy rule's code, bounded.",
	}, []string{"route", "phase", "decision", "reason"})

	filterPhase = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "x402_filter_phase_duration_seconds",
		Help:    "How long the filter held one request phase inside Envoy. A paid request is phase=request,decision=payment_accepted plus phase=response,decision=settled (Sync) or paid (Async).",
		Buckets: durationBuckets,
	}, []string{"route", "phase", "decision"})

	dependency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "x402_dependency_duration_seconds",
		Help:    "Calls to Redis, the chain RPC and facilitators: duration and result.",
		Buckets: durationBuckets,
	}, []string{"dependency", "operation", "result"})

	balanceReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "x402_balance_reads_total",
		Help: "Payer balance reads on the paid path, by where the balance came from: fresh or last_known (cache), fetched (RPC within the deadline), deadline or error.",
	}, []string{"source"})

	releaseFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "x402_filter_release_failures_total",
		Help: "Reservations the filter could not release; the payer's in-flight sum stays overcounted until its TTL.",
	})

	priceRoutes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "x402_price_table_routes",
		Help: "Routes in the price table in force, by kind: priced, platform, blocked (a broken policy; answers 503).",
	}, []string{"kind"})

	priceLoaded = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "x402_price_table_last_load_timestamp_seconds",
		Help: "Unix time the price table in force was loaded.",
	})

	priceRejected = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "x402_price_table_rejected_total",
		Help: "Price tables refused as unreadable; the last good table stays in force.",
	})

	settlerOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "x402_settler_outcomes_total",
		Help: "Settler outcomes: settlement jobs (Async) and held-payment records (Sync).",
	}, []string{"network", "outcome"})

	// PlatformFetches counts the controller's compiled-policy fetches from Radius Platform.
	PlatformFetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "x402_platform_fetches_total",
		Help: "Compiled-policy fetches from Radius Platform: new_version, unchanged, or error (the policy in force is kept).",
	}, []string{"result"})
)

// Registry holds every gate metric plus the Go runtime and process collectors. The filter
// and the settler serve it; a series appears once it has been recorded.
var Registry = func() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		BuildInfo, filterDecisions, filterPhase, dependency, balanceReads, releaseFailures,
		priceRoutes, priceLoaded, priceRejected, settlerOutcomes,
	)
	return r
}()

// SetBuildInfo records the running binary.
func SetBuildInfo(version, component string) { BuildInfo.WithLabelValues(version, component).Set(1) }

// Decision counts one filter decision.
func Decision(route, phase, decision, reason string) {
	filterDecisions.WithLabelValues(route, phase, decision, reason).Inc()
}

// Phase records how long the filter held one phase.
func Phase(route, phase, decision string, d time.Duration) {
	filterPhase.WithLabelValues(route, phase, decision).Observe(d.Seconds())
}

// Dependency records one call to a dependency. err decides the result: nil is ok, a
// deadline or a network timeout is timeout, anything else is error.
func Dependency(dep, operation string, err error, d time.Duration) {
	dependency.WithLabelValues(dep, operation, Result(err)).Observe(d.Seconds())
}

// Result classifies a dependency call's error.
func Result(err error) string {
	if err == nil {
		return ResultOK
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return ResultTimeout
	}
	return ResultError
}

// BalanceRead counts one balance read by its source.
func BalanceRead(source string) { balanceReads.WithLabelValues(source).Inc() }

// ReleaseFailed counts one reservation the filter could not release.
func ReleaseFailed() { releaseFailures.Inc() }

// PriceTable records the table now in force.
func PriceTable(priced, platform, blocked int, at time.Time) {
	priceRoutes.WithLabelValues("priced").Set(float64(priced))
	priceRoutes.WithLabelValues("platform").Set(float64(platform))
	priceRoutes.WithLabelValues("blocked").Set(float64(blocked))
	priceLoaded.Set(float64(at.UnixNano()) / 1e9)
}

// PriceTableRejected counts one refused table.
func PriceTableRejected() { priceRejected.Inc() }

// SettlerOutcome counts one settler outcome.
func SettlerOutcome(network, outcome string) {
	if network == "" {
		network = "unknown"
	}
	settlerOutcomes.WithLabelValues(network, outcome).Inc()
}

// Handler serves Registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// Serve answers /metrics on addr until ctx is done. It returns once listening, so a bad
// address fails the caller at startup rather than leaving the gate unobservable.
func Serve(ctx context.Context, addr string, log *slog.Logger) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server stopped", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Info("serving metrics", "addr", lis.Addr().String())
	return nil
}
