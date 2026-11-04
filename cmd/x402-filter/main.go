// Command x402-filter is the Envoy ext_proc filter that enforces x402 payments in the
// data plane. At M3 it answers priced routes with a 402 built from the price table
// (internal/extproc, internal/paywall), which it watches in one ConfigMap
// (internal/pricing).
//
// One gRPC port serves two services: ext_proc for Envoy, and the standard gRPC health
// service for the kubelet's probes. Liveness asks for the whole server (""), readiness
// for "readiness", which is NOT_SERVING until the first price table loads. Two names,
// so a missing ConfigMap leaves the pod alive and unready instead of restarting it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/extproc"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// readinessService is the gRPC health service name the readiness probe asks for.
const readinessService = "readiness"

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	listen := flag.String("listen", ":9002", "gRPC listen address (ext_proc and health)")
	drain := flag.Duration("drain", 20*time.Second, "how long to let in-flight streams finish on SIGTERM")
	delay := flag.Duration("delay", 0, "drills only: hold each request this long before answering")
	namespace := flag.String("prices-namespace", os.Getenv("POD_NAMESPACE"), "namespace of the price-table ConfigMap (default $POD_NAMESPACE)")
	configMap := flag.String("prices-configmap", "x402-prices", "name of the price-table ConfigMap")
	networks := flag.String("networks", "testnet", "comma-separated networks routes may charge on (testnet, mainnet)")
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
	if err := run(log, *listen, *drain, *delay, *namespace, *configMap, *networks); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen string, drain, delay time.Duration, namespace, configMap, networkList string) error {
	var nets []radius.Profile
	for _, n := range strings.Split(networkList, ",") {
		p, err := radius.Lookup(strings.TrimSpace(n))
		if err != nil {
			return err
		}
		nets = append(nets, p)
	}
	if namespace == "" {
		return fmt.Errorf("no namespace for the price table: set -prices-namespace or POD_NAMESPACE")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	client, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	prices := pricing.NewStore()
	watcher := &pricing.Watcher{Client: client, Namespace: namespace, Name: configMap, Networks: nets, Store: prices, Log: log}
	go func() {
		if err := watcher.Run(ctx); err != nil {
			log.Error("price table watch stopped", "err", err)
		}
	}()

	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, extproc.NewServer(log, delay, prices))
	hs := health.NewServer() // "" (the whole server) reports SERVING from the start
	hs.SetServingStatus(readinessService, healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(srv, hs)
	go func() {
		select {
		case <-prices.Loaded():
			hs.SetServingStatus(readinessService, healthpb.HealthCheckResponse_SERVING)
			log.Info("ready: price table loaded")
		case <-ctx.Done():
		}
	}()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	log.Info("listening", "addr", lis.Addr().String(), "prices", namespace+"/"+configMap, "networks", networkList)
	if delay > 0 {
		log.Warn("drill mode: every request is held before it is answered", "delay", delay.String())
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// SIGTERM: fail readiness so no new streams arrive, then let in-flight requests
	// finish, up to the drain limit. Each open stream is a request paused inside Envoy.
	log.Info("draining", "limit", drain.String())
	hs.Shutdown()
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(drain):
		srv.Stop()
	}
	return nil
}
