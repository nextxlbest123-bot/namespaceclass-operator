package main

import (
	"flag"
	"os"

	v1 "github.com/lixu/namespaceclass-operator/api/v1"
	"github.com/lixu/namespaceclass-operator/controllers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = corev1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
}

func main() {
	var enableLeaderElection bool
	var probeAddr string

	var concurrentNsReconciles int
	var concurrentNsClassReconciles int

	flag.StringVar(&probeAddr, "health-probe-addr", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election to ensure high availability.")

	//concurrentNsReconciles and concurrentNsClassReconciles are used to set the MaxConcurrentReconciles.
	flag.IntVar(&concurrentNsReconciles, "concurrent-ns-reconciles", 10, "The max number of concurrent Reconciles for Namespace objects.")
	flag.IntVar(&concurrentNsClassReconciles, "concurrent-nsclass-reconciles", 2, "The max number of concurrent Reconciles for NamespaceClass objects.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	cfg.QPS = 20
	cfg.Burst = 50

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "namespaceclass-operator-lock.core.akuity.io",
		HealthProbeBindAddress: probeAddr,
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.NamespaceReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		MaxConcurrentReconciles: concurrentNsClassReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ns controller", "controller", "Namespace")
		os.Exit(1)
	}

	if err = (&controllers.NamespaceClassReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		MaxConcurrentReconciles: concurrentNsClassReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ns class controller", "controller", "Namespace")
		os.Exit(1)
	}

	setupLog.Info("starting NamespaceClass controller")

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
