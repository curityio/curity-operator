package controller_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/internal/controller"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	// Disable the post-creation grace period in tests — every test HPA is
	// freshly created, and we cannot meaningfully wait the production 1min.
	// Individual specs that need the grace can opt back in.
	controller.HPAHealthGracePeriod = 0

	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		// config/crd/bases holds our CRDs; test/crds holds the third-party
		// ServiceMonitor CRD so the cluster controller's observability path runs.
		CRDDirectoryPaths: []string{"../../config/crd/bases", "../../test/crds"},
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	Expect(batchv1.AddToScheme(scheme)).To(Succeed())
	Expect(autoscalingv2.AddToScheme(scheme)).To(Succeed())
	Expect(policyv1.AddToScheme(scheme)).To(Succeed())
	Expect(networkingv1.AddToScheme(scheme)).To(Succeed())
	Expect(monitoringv1.AddToScheme(scheme)).To(Succeed())
	Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Start a controller-runtime manager with both reconcilers
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics in tests
		},
	})
	Expect(err).NotTo(HaveOccurred())

	//nolint:staticcheck // TODO: migrate to events.EventRecorder
	nodeRec := mgr.GetEventRecorderFor("identityservernode-controller")
	err = (&controller.IdentityServerNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: nodeRec,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	//nolint:staticcheck // TODO: migrate to events.EventRecorder
	clusterRec := mgr.GetEventRecorderFor("identityservercluster-controller")
	err = (&controller.IdentityServerClusterReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   clusterRec,
		RestConfig: cfg,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})
