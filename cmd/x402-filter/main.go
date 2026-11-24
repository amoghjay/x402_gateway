// Command x402-filter is the Envoy ext_proc filter that enforces x402 payments in the
// data plane. It prices requests from the table it watches in one ConfigMap
// (internal/pricing), answers unpaid ones with the paywall (internal/paywall), and
// serves paid ones after verifying the signature locally, reading the payer's balance
// through a Redis cache (internal/balance) and reserving in Redis (internal/reserve).
// An Async route's settlement is queued for the settler; a Sync route's is made here,
// through the facilitator, while Envoy holds the response (internal/extproc).
//
// One gRPC port serves two services: ext_proc for Envoy, and the standard gRPC health
// service for the kubelet's probes. Liveness asks for the whole server (""), readiness
// for "readiness", which is SERVING only while the price table is loaded and Redis
// answers. Two names, so a missing ConfigMap or a Redis outage leaves the pod alive and
// unready instead of restarting it. Never the facilitator or the RPC: only Sync routes
// call the first, and the second has a cache in front of it.
//
// Prometheus metrics are served on a separate HTTP port, -metrics-addr (:9090), so a
// scrape never shares a connection with Envoy's streams.
//
// Without -redis the paid path is off and every priced request gets the 402 (the M3
// shape), which is also the safe answer if the flag is dropped by mistake.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/amoghjay/x402_gateway/internal/balance"
	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/counter"
	"github.com/amoghjay/x402_gateway/internal/extproc"
	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/metrics"
	"github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/rdb"
	"github.com/amoghjay/x402_gateway/internal/reserve"
)

// readinessService is the gRPC health service name the readiness probe asks for.
const readinessService = "readiness"

type config struct {
	listen, namespace, configMap, networks string
	metricsAddr                            string // "" = no metrics listener
	drain, delay                           time.Duration
	redis                                  string // "" = payments off
	rpcURL                                 string // "" = the network's public endpoint
	facilitatorURL                         string // "" = the network's facilitator
	deadline, fresh, lastKnown, keepalive  time.Duration
	settleTimeout                          time.Duration
	facilitatorVerify                      bool
	verifyTimeout                          time.Duration
}

func main() {
	var c config
	version := flag.Bool("version", false, "print the version and exit")
	flag.StringVar(&c.listen, "listen", ":9002", "gRPC listen address (ext_proc and health)")
	flag.StringVar(&c.metricsAddr, "metrics-addr", ":9090", "HTTP listen address for Prometheus metrics at /metrics; empty turns it off")
	flag.DurationVar(&c.drain, "drain", 20*time.Second, "how long to let in-flight streams finish on SIGTERM")
	flag.DurationVar(&c.delay, "delay", 0, "drills only: hold each request this long before answering")
	flag.StringVar(&c.namespace, "prices-namespace", os.Getenv("POD_NAMESPACE"), "namespace of the price-table ConfigMap (default $POD_NAMESPACE)")
	flag.StringVar(&c.configMap, "prices-configmap", "x402-prices", "name of the price-table ConfigMap")
	flag.StringVar(&c.networks, "networks", "testnet", "comma-separated networks routes may charge on (testnet, mainnet)")
	flag.StringVar(&c.redis, "redis", "", "Redis address for the replay guard, in-flight sums, balance cache and settle stream; empty = payments off, every priced request gets the 402")
	flag.StringVar(&c.rpcURL, "rpc-url", os.Getenv("RADIUS_RPC_URL"), "JSON-RPC URL for balance reads (default $RADIUS_RPC_URL, then the network's public endpoint); a keyed URL is a secret and is never logged")
	flag.StringVar(&c.facilitatorURL, "facilitator-url", os.Getenv("X402_FACILITATOR_URL"), "x402 facilitator base URL for Sync routes (default $X402_FACILITATOR_URL, then the network's); with several networks, name=URL pairs separated by commas, e.g. testnet=http://f:8080")
	flag.DurationVar(&c.settleTimeout, "settle-timeout", extproc.DefaultSettleTimeout, "Sync routes: how long a response is held for the facilitator's /settle; with the chain reads it must stay under the route's Envoy messageTimeout")
	flag.BoolVar(&c.facilitatorVerify, "facilitator-verify", true, "Sync routes: also ask the facilitator's /verify on the request (the gate standard requires it; one facilitator round trip before the backend is called)")
	flag.DurationVar(&c.verifyTimeout, "verify-timeout", extproc.DefaultVerifyTimeout, "Sync routes: limit for the facilitator's /verify on the request")
	flag.DurationVar(&c.deadline, "deadline", extproc.DefaultDeadline, "request-phase budget; past it a valid payer gets 503 + Retry-After, not a charge")
	flag.DurationVar(&c.fresh, "balance-fresh", balance.DefaultFresh, "a cached balance younger than this is served as is")
	flag.DurationVar(&c.lastKnown, "balance-last-known", balance.DefaultLastKnown, "a cached balance up to this old is served when a refresh overruns the deadline")
	flag.DurationVar(&c.keepalive, "rpc-keepalive", 20*time.Second, "how often the RPC connection is exercised so a balance read never pays for a new connection")
	debug := flag.Bool("debug", false, "log every request's headers (credentials redacted)")
	flag.Parse()

	if *version {
		fmt.Println(buildinfo.Version)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("version", buildinfo.Version)
	if err := run(log, c); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, c config) error {
	var nets []radius.Profile
	for _, n := range strings.Split(c.networks, ",") {
		p, err := radius.Lookup(strings.TrimSpace(n))
		if err != nil {
			return err
		}
		nets = append(nets, p)
	}
	if c.namespace == "" {
		return fmt.Errorf("no namespace for the price table: set -prices-namespace or POD_NAMESPACE")
	}
	if c.rpcURL != "" && len(nets) != 1 {
		return fmt.Errorf("-rpc-url names one endpoint, but %d networks are configured", len(nets))
	}
	facURLs, err := facilitatorURLs(c.facilitatorURL, nets)
	if err != nil {
		return err
	}
	if c.verifyTimeout+c.deadline >= pricing.SyncMessageTimeout {
		return fmt.Errorf("-verify-timeout %s plus -deadline %s must be under Envoy's messageTimeout for Sync routes (%s)", c.verifyTimeout, c.deadline, pricing.SyncMessageTimeout)
	}
	if c.settleTimeout >= pricing.SyncMessageTimeout {
		return fmt.Errorf("-settle-timeout %s must be under Envoy's messageTimeout for Sync routes (%s)", c.settleTimeout, pricing.SyncMessageTimeout)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	client, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", c.listen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	metrics.SetBuildInfo(buildinfo.Version, "filter")
	if c.metricsAddr != "" {
		if err := metrics.Serve(ctx, c.metricsAddr, log); err != nil {
			return fmt.Errorf("metrics listener: %w", err)
		}
	}

	prices := pricing.NewStore()
	watcher := &pricing.Watcher{Client: client, Namespace: c.namespace, Name: c.configMap, Networks: nets, Store: prices, Log: log}
	go func() {
		if err := watcher.Run(ctx); err != nil {
			log.Error("price table watch stopped", "err", err)
		}
	}()

	server := extproc.NewServer(log, c.delay, prices)
	var redisClient *redis.Client
	if c.redis != "" {
		redisClient = rdb.RequestPath(c.redis)
		httpClient := radius.NewHTTPClient()
		facilitatorHTTP := radius.NewFacilitatorHTTPClient()
		balances := map[string]*balance.Cache{}
		facs := map[string]*facilitator.Client{}
		rpcs := map[string]*radius.RPC{}
		for _, p := range nets {
			url := p.RPC
			if c.rpcURL != "" {
				url = c.rpcURL
			}
			rpc := &radius.RPC{URL: url, HTTP: httpClient}
			go rpc.KeepWarm(ctx, c.keepalive, log.With("network", p.Name))
			balances[p.Network()] = &balance.Cache{
				Redis: redisClient, RPC: rpc, Token: p.TokenAddress(), Log: log,
				Fresh: c.fresh, LastKnown: c.lastKnown,
			}
			fu := p.Facilitator
			if u, ok := facURLs[p.Name]; ok {
				fu = u
				log.Warn("facilitator overridden", "network", p.Name, "url", u)
			}
			facs[p.Network()] = &facilitator.Client{URL: fu, HTTP: facilitatorHTTP}
			rpcs[p.Network()] = rpc
		}
		server.WithPayments(&extproc.Payments{
			Balances:     balances,
			Reserve:      &reserve.Store{Redis: redisClient},
			Deadline:     c.deadline,
			Facilitators: facs, RPCs: rpcs, SettleTimeout: c.settleTimeout,
			FacilitatorVerify: c.facilitatorVerify, VerifyTimeout: c.verifyTimeout,
			// Facilitators that Radius Platform payment options name, by URL.
			FacilitatorHTTP: facilitatorHTTP,
		})
		// Radius Platform rules' rate limits and free quotas: one shared count in Redis.
		server.WithCounters(counter.New(redisClient))
	} else {
		log.Warn("payments are off (-redis is empty): every priced request gets the 402, and platform rules' counters read as failed")
	}
	log.Info("Radius Platform compiled policies accepted", "schemas", policy.Schemas)

	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, server)
	hs := health.NewServer() // "" (the whole server) reports SERVING from the start
	hs.SetServingStatus(readinessService, healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(srv, hs)
	go readiness(ctx, log, hs, prices, redisClient)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	log.Info("listening", "addr", lis.Addr().String(), "prices", c.namespace+"/"+c.configMap, "networks", c.networks,
		"redis", c.redis, "deadline", c.deadline.String(), "facilitatorVerify", c.facilitatorVerify)
	if c.delay > 0 {
		log.Warn("drill mode: every request is held before it is answered", "delay", c.delay.String())
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// SIGTERM: fail readiness so no new streams arrive, then let in-flight requests
	// finish, up to the drain limit. Each open stream is a request paused inside Envoy.
	log.Info("draining", "limit", c.drain.String())
	hs.Shutdown()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(c.drain):
		srv.Stop()
	}
	return nil
}

// readiness keeps the "readiness" health status in step with what the filter needs to
// answer a request: the price table, and Redis when payments are on. It flips, and
// logs, only on a change.
func readiness(ctx context.Context, log *slog.Logger, hs *health.Server, prices *pricing.Store, redisClient *redis.Client) {
	select {
	case <-prices.Loaded():
	case <-ctx.Done():
		return
	}
	ready := false
	check := func() {
		ok, why := true, "price table loaded"
		if redisClient != nil {
			if err := rdb.Ping(ctx, redisClient, 200*time.Millisecond); err != nil {
				ok, why = false, "redis: "+err.Error()
			}
		}
		if ok == ready {
			return
		}
		ready = ok
		if ok {
			hs.SetServingStatus(readinessService, healthpb.HealthCheckResponse_SERVING)
			log.Info("ready", "why", why)
		} else {
			hs.SetServingStatus(readinessService, healthpb.HealthCheckResponse_NOT_SERVING)
			log.Warn("not ready", "why", why)
		}
	}
	check()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// facilitatorURLs reads -facilitator-url: one bare URL when one network is configured,
// or name=URL pairs that override only the networks they name (a drill points Radius
// testnet at a slow proxy while Base Sepolia keeps its own facilitator). A name that is
// not a configured network is an error, so a typo cannot leave the default in place.
func facilitatorURLs(v string, nets []radius.Profile) (map[string]string, error) {
	out := map[string]string{}
	if v == "" {
		return out, nil
	}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		name, u, pair := strings.Cut(part, "=")
		if !pair || strings.Contains(name, "/") || strings.Contains(name, ":") { // a bare URL (it may hold a '=' in its query)
			if len(nets) != 1 || strings.Contains(v, ",") {
				return nil, fmt.Errorf("-facilitator-url: a bare URL needs exactly one network (%d configured); use name=URL pairs", len(nets))
			}
			out[nets[0].Name] = part
			continue
		}
		if !slices.ContainsFunc(nets, func(p radius.Profile) bool { return p.Name == name }) {
			return nil, fmt.Errorf("-facilitator-url: %q is not one of the configured networks", name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("-facilitator-url: %q is given twice", name)
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("-facilitator-url: %s=%q is not an http(s) URL", name, u)
		}
		out[name] = u
	}
	return out, nil
}
