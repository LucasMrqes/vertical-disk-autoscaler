package main

import (
	"flag"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
	"github.com/padoa/vertical-disk-autoscaler/internal/azure"
	"github.com/padoa/vertical-disk-autoscaler/internal/controller"
	"github.com/padoa/vertical-disk-autoscaler/internal/registry"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr           string
		probeAddr             string
		scalingInterval       time.Duration
		maxAzureWritesPerHour int
		debug                 bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.DurationVar(&scalingInterval, "scaling-interval", 30*time.Second, "How often the scaling loop evaluates disks.")
	flag.IntVar(&maxAzureWritesPerHour, "max-azure-writes-per-hour", 600, "Global rate limit for Azure API write operations per hour.")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging.")
	flag.Parse()

	// Configure zerolog for structured JSON logging.
	zl := zerolog.New(os.Stdout).With().Timestamp().Logger()
	if debug {
		zl = zl.Level(zerolog.DebugLevel)
	} else {
		zl = zl.Level(zerolog.InfoLevel)
	}

	log := zerologr.New(&zl)
	ctrl.SetLogger(log)

	setupLog := log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Azure credential — uses Workload Identity by default, falls back to env vars.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		setupLog.Error(err, "unable to create Azure credential")
		os.Exit(1)
	}

	reg := registry.New()
	provider := azure.NewProvider(log.WithName("azure"), cred, maxAzureWritesPerHour)

	// Register the policy controller (event-driven).
	policyCtrl := controller.NewPolicyController(mgr.GetClient(), log, reg, provider)
	if err := policyCtrl.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create policy controller")
		os.Exit(1)
	}

	// Register the scaling loop (periodic).
	scalingLoop := controller.NewScalingLoop(mgr.GetClient(), log, reg, provider, scalingInterval)
	if err := mgr.Add(scalingLoop); err != nil {
		setupLog.Error(err, "unable to add scaling loop")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager",
		"scalingInterval", scalingInterval,
		"maxAzureWritesPerHour", maxAzureWritesPerHour,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
