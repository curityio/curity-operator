package utils

import (
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/curityio/curity-operator/api/v1alpha1"
)

// E2ETestEnv holds the test environment configuration and clients.
type E2ETestEnv struct {
	K8sClient     client.Client
	DynamicClient *dynamic.DynamicClient
	Environment   *envtest.Environment
	Scheme        *runtime.Scheme

	restConfig        *rest.Config
	operatorNamespace string
}

// TestEnvironment is the global test environment instance.
var TestEnvironment *E2ETestEnv

// NewE2ETestEnv creates a new test environment for the given operator.
func NewE2ETestEnv(operatorName string) *E2ETestEnv {
	s := scheme.Scheme
	_ = v1alpha1.AddToScheme(s)
	return &E2ETestEnv{
		Scheme:            s,
		operatorNamespace: fmt.Sprintf("%s-operator", operatorName),
	}
}

// Setup initializes the test environment using an existing Kind cluster.
func (env *E2ETestEnv) Setup() error {
	env.Environment = &envtest.Environment{
		UseExistingCluster: &[]bool{true}[0],
	}

	var err error
	env.restConfig, err = env.Environment.Start()
	if err != nil || env.restConfig == nil {
		return fmt.Errorf("failed to setup rest-config: %w", err)
	}

	env.K8sClient, err = client.New(env.restConfig, client.Options{Scheme: env.Scheme})
	if err != nil || env.K8sClient == nil {
		return fmt.Errorf("failed to setup K8sClient: %s", err.Error())
	}

	env.DynamicClient = dynamic.NewForConfigOrDie(env.restConfig)

	ginkgo.GinkgoWriter.Println("installing CRDs")
	_, err = Run("make", "install")
	if err != nil {
		return fmt.Errorf("failed to install operator CRDs: %s", err.Error())
	}

	ginkgo.GinkgoWriter.Println("creating operator namespace")
	_, _ = Run("kubectl", "create", "namespace", env.operatorNamespace)

	ginkgo.GinkgoWriter.Println("deploying the operator")
	_, err = Run("make", "kustomize-deploy")
	if err != nil {
		return fmt.Errorf("failed to deploy operator: %s", err.Error())
	}

	ginkgo.GinkgoWriter.Println("validating that the controller-manager pod is running")
	err = env.verifyControllerUp()
	if err != nil {
		return fmt.Errorf("controller verification failed: %s", err.Error())
	}

	return nil
}

func (env *E2ETestEnv) verifyControllerUp() error {
	deadline := time.Now().Add(time.Minute)
	var lastErr error

	for time.Now().Before(deadline) {
		podOutput, err := Run("kubectl", "get",
			"pods", "-l", "control-plane=controller-manager",
			"-o", "go-template={{ range .items }}"+
				"{{ if not .metadata.deletionTimestamp }}"+
				"{{ .metadata.name }}"+
				"{{ \"\\n\" }}{{ end }}{{ end }}",
			"-n", env.operatorNamespace,
		)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}

		podNames := GetNonEmptyLines(podOutput)
		if len(podNames) != 1 {
			lastErr = fmt.Errorf("expect 1 controller pod running, but got %d", len(podNames))
			time.Sleep(time.Second)
			continue
		}

		status, err := Run("kubectl", "get",
			"pods", podNames[0], "-o", "jsonpath={.status.phase}",
			"-n", env.operatorNamespace,
		)
		if err != nil || status != "Running" {
			lastErr = fmt.Errorf("controller pod in %s status", status)
			time.Sleep(time.Second)
			continue
		}

		return nil
	}

	return fmt.Errorf("timed out after 1m: %w", lastErr)
}

// Teardown cleans up the test environment.
// It deletes all custom resources first (while the operator is still running
// to process finalizers), then undeploys the operator and CRDs.
func (env *E2ETestEnv) Teardown() error {
	// Delete all custom resources first so the operator can process finalizers
	// before the operator pod is removed. Without this, CRD deletion blocks
	// forever because finalizers can never be removed.
	ginkgo.GinkgoWriter.Println("deleting all IdentityServerNodes across namespaces")
	_, _ = Run("kubectl", "delete", "identityservernodes.curity.io", "--all", "--all-namespaces", "--timeout=60s")

	ginkgo.GinkgoWriter.Println("deleting all IdentityServerClusters across namespaces")
	_, _ = Run("kubectl", "delete", "identityserverclusters.curity.io", "--all", "--all-namespaces", "--timeout=60s")

	ginkgo.GinkgoWriter.Println("undeploying the operator")
	_, _ = Run("make", "undeploy")

	ginkgo.GinkgoWriter.Println("deleting operator namespace")
	_, _ = Run("kubectl", "delete", "namespace", env.operatorNamespace, "--ignore-not-found")

	return env.Environment.Stop()
}
