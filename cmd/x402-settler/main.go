// Command x402-settler drains the settle stream the filter captures into and settles
// each payment through the Radius facilitator, after the response has gone out
// (internal/settle). It is the only process that talks to the facilitator; the request
// path never does.
//
// One replica is enough for now (the facilitator, not this, is the settle ceiling), but
// nothing here assumes it: every replica is a consumer in one group, and an idle job
// is claimed by whichever runs. Liveness and readiness are plain HTTP; readiness is
// Redis answering, never the facilitator, which the settler retries instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/rdb"
	"github.com/amoghjay/x402_gateway/internal/reserve"
	"github.com/amoghjay/x402_gateway/internal/settle"
)

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	redisAddr := flag.String("redis", "", "Redis address (required): the settle stream and the in-flight sums")
	networks := flag.String("networks", "testnet", "comma-separated networks to settle on (testnet, mainnet)")
	consumer := flag.String("consumer", os.Getenv("POD_NAME"), "this replica's consumer name (default $POD_NAME, then the hostname)")
	healthAddr := flag.String("health-addr", ":8081", "HTTP address for /healthz and /readyz")
	facilitatorURL := flag.String("facilitator-url", os.Getenv("RADIUS_FACILITATOR_URL"), "facilitator base URL (default $RADIUS_FACILITATOR_URL, then the network's)")
	rpcURL := flag.String("rpc-url", os.Getenv("RADIUS_RPC_URL"), "JSON-RPC URL for authorizationState (default $RADIUS_RPC_URL, then the network's public endpoint); a keyed URL is a secret and is never logged")
	settleTimeout := flag.Duration("settle-timeout", settle.DefaultSettleTimeout, "per /settle call")
	minIdle := flag.Duration("min-idle", settle.DefaultMinIdle, "a pending job idle this long is claimed and retried")
	debug := flag.Bool("debug", false, "debug logging")
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
	if err := run(log, *redisAddr, *networks, *consumer, *healthAddr, *facilitatorURL, *rpcURL, *settleTimeout, *minIdle); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, redisAddr, networks, consumer, healthAddr, facilitatorURL, rpcURL string, settleTimeout, minIdle time.Duration) error {
	if redisAddr == "" {
		return errors.New("-redis is required")
	}
	if consumer == "" {
		consumer, _ = os.Hostname()
	}
	var nets []radius.Profile
	for _, n := range strings.Split(networks, ",") {
		p, err := radius.Lookup(strings.TrimSpace(n))
		if err != nil {
			return err
		}
		nets = append(nets, p)
	}
	if (facilitatorURL != "" || rpcURL != "") && len(nets) != 1 {
		return fmt.Errorf("-facilitator-url and -rpc-url name one endpoint, but %d networks are configured", len(nets))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	client := rdb.Settler(redisAddr, settle.DefaultBlock)
	defer func() { _ = client.Close() }()
	httpClient := radius.NewHTTPClient()
	facs := map[string]*facilitator.Client{}
	rpcs := map[string]*radius.RPC{}
	for _, p := range nets {
		fu, ru := p.Facilitator, p.RPC
		if facilitatorURL != "" {
			fu = facilitatorURL
		}
		if rpcURL != "" {
			ru = rpcURL
		}
		facs[p.Network()] = &facilitator.Client{URL: fu, HTTP: httpClient}
		rpcs[p.Network()] = &radius.RPC{URL: ru, HTTP: httpClient}
	}
	s := &settle.Settler{
		Redis: client, Facilitators: facs, RPCs: rpcs, Reserve: &reserve.Store{Redis: client},
		Log: log, Consumer: consumer, SettleTimeout: settleTimeout, MinIdle: minIdle,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context(), client, 500*time.Millisecond); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	health := &http.Server{Addr: healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
		}
	}()
	defer func() { _ = health.Close() }()

	log.Info("settling", "redis", redisAddr, "consumer", consumer, "networks", networks, "group", settle.DefaultGroup)
	return s.Run(ctx)
}
