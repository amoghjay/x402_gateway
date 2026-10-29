// Command x402-filter is the Envoy ext_proc filter that enforces x402 payments in the
// data plane. At Gate Zero it is a no-op: it logs both phases of every request and lets
// it through (internal/extproc).
//
// One gRPC port serves two services: ext_proc for Envoy, and the standard gRPC health
// service for the kubelet's readiness and liveness probes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/extproc"
)

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	listen := flag.String("listen", ":9002", "gRPC listen address (ext_proc and health)")
	drain := flag.Duration("drain", 20*time.Second, "how long to let in-flight streams finish on SIGTERM")
	delay := flag.Duration("delay", 0, "drills only: hold each request this long before answering")
	flag.Parse()

	if *version {
		fmt.Println(buildinfo.Version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("version", buildinfo.Version)
	if err := run(log, *listen, *drain, *delay); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen string, drain, delay time.Duration) error {
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, extproc.NewServer(log, delay))
	hs := health.NewServer() // "" (the whole server) reports SERVING from the start
	healthpb.RegisterHealthServer(srv, hs)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	log.Info("listening", "addr", lis.Addr().String())
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
