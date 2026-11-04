// Command x402-controller reconciles PaymentPolicy objects (api/v1alpha1) into the
// objects the data plane reads: one EnvoyExtensionPolicy per priced route, the
// ReferenceGrant that lets those policies reach the filter's Service, and the
// price-table ConfigMap the filter watches (internal/controller).
//
// It runs one replica with leader election on, so a rolling update, where two pods
// overlap for a moment, never has two writers. Liveness and readiness are plain HTTP on
// the health address. The filter does not depend on this process: if it is down, the
// last price table and the existing EnvoyExtensionPolicies keep working as they are.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/amoghjay/x402_gateway/api/v1alpha1"
	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/controller"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	healthAddr := flag.String("health-addr", ":8081", "HTTP address for /healthz and /readyz")
	metricsAddr := flag.String("metrics-addr", "0", "HTTP address for Prometheus metrics (0 disables)")
	leaderElect := flag.Bool("leader-elect", true, "hold a Lease so only one replica writes")
	pricesNamespace := flag.String("prices-namespace", os.Getenv("POD_NAMESPACE"), "namespace of the price-table ConfigMap, the ReferenceGrant and the Lease (default $POD_NAMESPACE)")
	pricesName := flag.String("prices-configmap", "x402-prices", "name of the price-table ConfigMap the filter watches")
	grantName := flag.String("reference-grant", "extension-policies-to-filter", "name of the ReferenceGrant written next to the filter's Service")
	filterNamespace := flag.String("filter-namespace", "", "namespace of the filter's Service (default: the prices namespace)")
	filterName := flag.String("filter-service", "x402-filter", "name of the filter's Service")
	filterPort := flag.Int64("filter-port", 9002, "ext_proc port of the filter's Service")
	networks := flag.String("networks", "testnet", "comma-separated networks a policy may charge on (testnet, mainnet)")
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
	// controller-runtime logs through logr; route it to the same JSON stream.
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	if err := run(log, *healthAddr, *metricsAddr, *leaderElect, *pricesNamespace, *pricesName, *grantName, *filterNamespace, *filterName, *filterPort, *networks); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, healthAddr, metricsAddr string, leaderElect bool, pricesNamespace, pricesName, grantName, filterNamespace, filterName string, filterPort int64, networks string) error {
	if pricesNamespace == "" {
		return fmt.Errorf("-prices-namespace is empty and POD_NAMESPACE is unset")
	}
	if filterNamespace == "" {
		filterNamespace = pricesNamespace
	}
	var profiles []radius.Profile
	for _, n := range strings.Split(networks, ",") {
		p, err := radius.Lookup(strings.TrimSpace(n))
		if err != nil {
			return err
		}
		profiles = append(profiles, p)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "x402-controller",
		LeaderElectionNamespace: pricesNamespace,
		HealthProbeBindAddress:  healthAddr,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	r := &controller.Reconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Log:             log,
		Filter:          controller.Backend{Namespace: filterNamespace, Name: filterName, Port: filterPort},
		PricesNamespace: pricesNamespace,
		PricesName:      pricesName,
		GrantName:       grantName,
		Networks:        profiles,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	log.Info("starting", "prices", pricesNamespace+"/"+pricesName, "filter", filterNamespace+"/"+filterName, "networks", networks, "leaderElection", leaderElect)
	return mgr.Start(ctrl.SetupSignalHandler())
}
