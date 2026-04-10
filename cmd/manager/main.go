package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/internal/controller"
)

var (
	scheme = runtime.NewScheme()

	healthAddr  string
	metricsAddr string
	leaderElect bool
)

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
	utilruntime.Must(autoscalingv2.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "curity-operator",
		Short: "Kubernetes operator for Curity Identity Server",
		RunE:  run,
	}

	cmd.Flags().StringVar(&healthAddr, "health-addr", ":8081", "address for health probe endpoints")
	cmd.Flags().StringVar(&metricsAddr, "metrics-addr", "0", "address for metrics endpoint (0 to disable)")
	cmd.Flags().BoolVar(&leaderElect, "leader-elect", false, "enable leader election for HA deployments")

	return cmd
}

func run(cmd *cobra.Command, _ []string) error {
	zapLogger := zap.New(zap.UseDevMode(false))
	ctrl.SetLogger(zapLogger)
	log := ctrl.Log.WithName("setup")

	log.Info("starting curity-operator")

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: healthAddr,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		LeaderElection:   leaderElect,
		LeaderElectionID: "curity-operator-leader",
	})
	if err != nil {
		return fmt.Errorf("failed to create manager: %w", err)
	}

	//nolint:staticcheck // TODO: migrate to events.EventRecorder
	nodeRecorder := mgr.GetEventRecorderFor("identityservernode-controller")
	if err := (&controller.IdentityServerNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: nodeRecorder,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to setup IdentityServerNode controller: %w", err)
	}

	//nolint:staticcheck // TODO: migrate to events.EventRecorder
	clusterRecorder := mgr.GetEventRecorderFor("identityservercluster-controller")
	if err := (&controller.IdentityServerClusterReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   clusterRecorder,
		RestConfig: cfg,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to setup IdentityServerCluster controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("failed to setup health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("failed to setup ready check: %w", err)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}

	return nil
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
