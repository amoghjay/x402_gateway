package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/controller"
	"github.com/amoghjay/x402_gateway/internal/metrics"
	"github.com/amoghjay/x402_gateway/internal/platform"
)

// routeFlags collects repeated --route values.
type routeFlags []string

func (r *routeFlags) String() string { return strings.Join(*r, ",") }
func (r *routeFlags) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// runSync is `x402-controller sync`: one Radius Platform project kept in a directory for a
// filter running with -prices-dir, with no Kubernetes. It returns the exit code.
func runSync(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		s        controller.Standalone
		routes   routeFlags
		instance string
	)
	fs.StringVar(&s.Dir, "out", "", "the directory the filter reads with -prices-dir")
	fs.StringVar(&s.OrganizationID, "organization", "", "the Radius Platform organization ID")
	fs.StringVar(&s.ProjectID, "project", "", "the Radius Platform project ID")
	fs.StringVar(&s.KeyFile, "key-file", "", "a file holding the project's Platform API key; re-read on every fetch")
	fs.Var(&routes, "route", "an Envoy route name the project governs; repeat for each route")
	fs.StringVar(&s.NoPolicy, "no-policy", "deny", "what the routes answer before any policy was loaded: deny (503 policy_unavailable) or allow")
	interval := fs.Duration("interval", controller.DefaultPollInterval, "how often the compiled policy is fetched (±10 %); at least 1s")
	platformURL := fs.String("platform-api-url", platform.DefaultBaseURL, "the Radius Platform API")
	fs.StringVar(&instance, "instance", "", "sent to the Platform as Radius-Gate-Instance (default: the hostname)")
	metricsAddr := fs.String("metrics-addr", "", "HTTP address for Prometheus /metrics; empty turns it off")
	debug := fs.Bool("debug", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "sync: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if *interval < time.Second {
		_, _ = fmt.Fprintf(stderr, "sync: --interval %v: the gate standard allows at most one fetch a second\n", *interval)
		return 2
	}
	if instance == "" {
		instance, _ = os.Hostname()
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("version", buildinfo.Version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	metrics.SetBuildInfo(buildinfo.Version, "sync")
	if *metricsAddr != "" {
		metrics.Registry.MustRegister(metrics.PlatformFetches)
		if err := metrics.Serve(ctx, *metricsAddr, log); err != nil {
			log.Error("metrics listener", "err", err)
			return 1
		}
	}

	p := controller.NewPoller()
	p.BaseURL, p.Interval, p.Log = *platformURL, *interval, log
	p.Fetcher = &platform.Client{
		HTTP:      &http.Client{Timeout: platform.DefaultTimeout},
		UserAgent: "x402-envoy-gate/" + buildinfo.Version,
		Instance:  instance,
	}
	s.Routes, s.Poller, s.Log = routes, p, log
	log.Info("syncing a Radius Platform project to a directory", "dir", s.Dir, "project", s.ProjectID, "routes", []string(routes), "noPolicy", s.NoPolicy, "instance", instance)
	if err := s.Run(ctx); err != nil {
		log.Error("exit", "err", err)
		return 1
	}
	return 0
}
