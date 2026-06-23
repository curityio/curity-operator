package e2e

import (
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

func TestMain(m *testing.M) {
	setup()

	code := m.Run()

	teardown(m)

	os.Exit(code)
}

func setup() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	utils.TestEnvironment = utils.NewE2ETestEnv("curity")

	err := v1alpha1.AddToScheme(utils.TestEnvironment.Scheme)
	if err != nil {
		fmt.Printf("Test setup failed: %v\n", err)
		os.Exit(1)
	}

	// ServiceMonitor types so the e2e client can read the observability test's
	// ServiceMonitor objects. The CRD itself is installed only by that test.
	if err = monitoringv1.AddToScheme(utils.TestEnvironment.Scheme); err != nil {
		fmt.Printf("Test setup failed: %v\n", err)
		os.Exit(1)
	}

	err = utils.TestEnvironment.Setup()
	if err != nil {
		fmt.Printf("Test setup failed: %v\n", err)
		os.Exit(1)
	}
}

func teardown(_ *testing.M) {
	// NOTE: snaps.Clean() is NOT called here because all snapshots are
	// created via snaps.WithConfig() (per-file instances) which the global
	// registry does not track. Calling Clean() would empty all snapshot
	// files. Obsolete snapshots should be removed manually or via git.
	err := utils.TestEnvironment.Teardown()
	if err != nil {
		GinkgoWriter.Println(fmt.Sprintf("Test teardown failed: %v", err.Error()))
		os.Exit(1)
	}
}

// TestE2E runs the e2e test suite.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	GinkgoWriter.Println("Starting curity-operator e2e suite")
	RunSpecs(t, "e2e suite")
}
