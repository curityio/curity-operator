// Package e2escoped runs the operator in namespace-scoped mode
// (watchScope=namespace) and checks that it only acts in its own namespace.
//
// It lives outside test/e2e on purpose: `go test ./test/e2e/...` would run it in
// parallel with the main suite against the same cluster. The Makefile runs it
// after the main suite, whose teardown removes the cluster-wide operator. The
// suite refuses to start if a cluster-wide curity-operator is still present,
// because that operator would also reconcile the "ignored" namespace and make
// the results meaningless.
package e2escoped

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

const (
	releaseName    = "curity-operator"
	deploymentName = releaseName + "-controller-manager"
	chartDir       = "charts/curity-operator"

	// watchedNS is where the operator is installed and what it may manage;
	// otherNS must be left alone.
	watchedNS = "curity-e2e-scoped"
	otherNS   = "curity-e2e-scoped-other"

	timeout  = 3 * time.Minute
	interval = 2 * time.Second
)

var crdNames = []string{
	"identityserverclusters.curity.io",
	"identityservernodes.curity.io",
	"identityserverdatabases.curity.io",
}

var k8s client.Client

// created records what this suite put on the cluster, so AfterSuite removes
// only that. Ginkgo runs AfterSuite even when BeforeSuite fails, and the
// cluster is shared: deleting a CRD the suite did not create would delete
// every resource of that kind in every namespace.
var created struct {
	// safetyChecksPassed is set once no other curity-operator was found.
	// Until then AfterSuite touches nothing.
	safetyChecksPassed bool
	crds               []string
	namespaces         []string
	chart              bool
}

func TestScopedE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	GinkgoWriter.Println("Starting curity-operator namespace-scoped e2e suite")
	RunSpecs(t, "namespace-scoped e2e suite")
}

var _ = BeforeSuite(func() {
	// Fixtures, the chart and `make` targets are addressed from the repo root.
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	Expect(os.Chdir(dir)).To(Succeed())

	s := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(s)).To(Succeed())
	Expect(v1alpha1.AddToScheme(s)).To(Succeed())
	cfg, err := config.GetConfig()
	Expect(err).NotTo(HaveOccurred())
	k8s, err = client.New(cfg, client.Options{Scheme: s})
	Expect(err).NotTo(HaveOccurred())

	img := os.Getenv("E2E_IMG")
	Expect(img).NotTo(BeEmpty(), "E2E_IMG must name the operator image under test (the Makefile target sets it)")
	repo, tag := splitImage(img)

	By("checking that no cluster-wide curity-operator is left on the cluster")
	expectNoClusterWideOperator()

	By("waiting for CRDs still being deleted by an earlier teardown")
	waitForTerminatingCRDs()
	created.safetyChecksPassed = true

	By("installing the CRDs separately, as a cluster admin would")
	for _, crd := range crdNames {
		if !crdExists(crd) {
			created.crds = append(created.crds, crd)
		}
	}
	_, err = utils.Run("make", "install")
	Expect(err).NotTo(HaveOccurred())
	waitArgs := []string{"wait", "--for=condition=Established", "--timeout=60s"}
	for _, crd := range crdNames {
		waitArgs = append(waitArgs, "crd/"+crd)
	}
	_, err = utils.Run("kubectl", waitArgs...)
	Expect(err).NotTo(HaveOccurred())

	createNamespace(watchedNS)
	createNamespace(otherNS)

	By("installing the chart with watchScope=namespace")
	args := []string{
		"upgrade", "--install", releaseName, chartDir,
		"--namespace", watchedNS,
		"--set", "watchScope=namespace",
		"--set", "crds.install=false",
		"--set", "controllerManager.kubeRbacProxy.enabled=false",
		"--set", "controllerManager.manager.image.repository=" + repo,
		"--set", "controllerManager.manager.image.tag=" + tag,
		"--wait", "--timeout", "3m",
	}
	if secret := os.Getenv("E2E_PULL_SECRET"); secret != "" {
		args = append(args, "--set", "imagePullSecrets[0].name="+secret)
	}
	// Set before running: a failed install can still leave a release behind.
	created.chart = true
	_, err = utils.Run("helm", args...)
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if !created.safetyChecksPassed {
		GinkgoWriter.Println("setup stopped before its safety checks passed; leaving the cluster untouched")
		return
	}

	// Best effort: every step runs even if an earlier one fails, so a failed
	// spec still leaves the cluster as the main suite's teardown left it.
	warn := func(step string, err error) {
		if err != nil {
			GinkgoWriter.Printf("warning: %s: %v\n", step, err)
		}
	}
	crTypes := strings.Join(crdNames, ",")

	// Both namespaces are this suite's own. The operator is still running,
	// so it can remove its finalizers in watchedNS.
	_, err := utils.Run("kubectl", "delete", crTypes, "--all", "-n", watchedNS, "--ignore-not-found", "--timeout=60s")
	warn("delete resources in "+watchedNS, err)

	// Nothing should have put a finalizer here; if a regression did, strip it
	// so the CRD deletion below cannot hang.
	_, err = utils.Run("kubectl", "delete", crTypes, "--all", "-n", otherNS, "--ignore-not-found", "--wait=false")
	warn("delete resources in "+otherNS, err)
	stripFinalizers(otherNS)

	if created.chart {
		_, err = utils.Run("helm", "uninstall", releaseName, "--namespace", watchedNS, "--ignore-not-found", "--wait")
		warn("helm uninstall", err)
	}

	if len(created.crds) > 0 {
		_, err = utils.Run("kubectl", append([]string{"delete", "crd", "--ignore-not-found", "--timeout=120s"}, created.crds...)...)
		warn("delete CRDs", err)
	}

	if len(created.namespaces) > 0 {
		_, err = utils.Run("kubectl", append([]string{"delete", "namespace", "--ignore-not-found", "--timeout=120s"}, created.namespaces...)...)
		warn("delete namespaces", err)
	}
})

// splitImage splits "registry/repo:tag" into repository and tag. A colon
// before the last slash is a registry port, not a tag.
func splitImage(img string) (string, string) {
	slash := strings.LastIndex(img, "/")
	colon := strings.LastIndex(img, ":")
	if colon > slash {
		return img[:colon], img[colon+1:]
	}
	return img, "latest"
}

// expectNoClusterWideOperator fails the suite if a curity-operator Deployment
// or manager ClusterRoleBinding exists anywhere. Matches on curity-specific
// names, not the generic kubebuilder control-plane label, because the shared
// cluster can run other operators.
func expectNoClusterWideOperator() {
	bindings, err := utils.Run("kubectl", "get", "clusterrolebindings",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.roleRef.name}{"\n"}{end}`)
	Expect(err).NotTo(HaveOccurred())
	var leftovers []string
	for _, line := range utils.GetNonEmptyLines(bindings) {
		fields := strings.Split(line, "\t")
		role := fields[len(fields)-1]
		if strings.Contains(role, "curity-operator") && strings.HasSuffix(role, "manager-role") {
			leftovers = append(leftovers, "ClusterRoleBinding "+fields[0])
		}
	}

	deployments, err := utils.Run("kubectl", "get", "deployments", "--all-namespaces",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.template.spec.containers[*].command}{"\n"}{end}`)
	Expect(err).NotTo(HaveOccurred())
	for _, line := range utils.GetNonEmptyLines(deployments) {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) == 2 && strings.Contains(fields[1], "curity-operator") {
			leftovers = append(leftovers, "Deployment "+fields[0])
		}
	}

	Expect(leftovers).To(BeEmpty(),
		"a curity-operator is still installed (did the main e2e teardown fail?); "+
			"it would also reconcile %s and invalidate this suite", otherNS)
}

// waitForTerminatingCRDs waits for curity CRDs that are being deleted to go
// away. If a custom resource kept a finalizer after its operator was removed,
// the CRD never finishes deleting; fail with the stuck resources named.
func waitForTerminatingCRDs() {
	terminating := func() []string {
		var names []string
		for _, crd := range crdNames {
			ts, err := utils.Run("kubectl", "get", "crd", crd, "--ignore-not-found",
				"-o", "jsonpath={.metadata.deletionTimestamp}")
			if err == nil && ts != "" {
				names = append(names, crd)
			}
		}
		return names
	}
	if len(terminating()) == 0 {
		return
	}
	Eventually(terminating, 2*time.Minute, interval).Should(BeEmpty(), func() string {
		stuck, _ := utils.Run("kubectl", "get", strings.Join(crdNames, ","), "--all-namespaces")
		return "curity CRDs are stuck terminating, most likely because these resources still carry finalizers:\n" + stuck
	})
}

func crdExists(name string) bool {
	out, err := utils.Run("kubectl", "get", "crd", name, "--ignore-not-found", "-o", "name")
	Expect(err).NotTo(HaveOccurred())
	return out != ""
}

// createNamespace creates a namespace and records it for cleanup. One that
// already exists (CI pre-creates watchedNS with a pull secret) is left to
// whoever created it.
func createNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8s.Create(context.Background(), ns)
	switch {
	case err == nil:
		created.namespaces = append(created.namespaces, name)
	case !apierrors.IsAlreadyExists(err):
		Fail(fmt.Sprintf("failed to create namespace %s: %v", name, err))
	}
}

func stripFinalizers(ns string) {
	for _, crd := range crdNames {
		out, err := utils.Run("kubectl", "get", crd, "-n", ns, "-o", "name", "--ignore-not-found")
		if err != nil {
			continue
		}
		for _, name := range utils.GetNonEmptyLines(out) {
			_, _ = utils.Run("kubectl", "patch", name, "-n", ns, "--type=merge",
				"-p", `{"metadata":{"finalizers":null}}`)
		}
	}
}

// canI asks the API server whether the operator's service account may perform
// an action. kubectl exits non-zero for "no", so stdout is read regardless.
func canI(serviceAccount string, args ...string) string {
	cmdArgs := append([]string{"auth", "can-i",
		"--as=system:serviceaccount:" + watchedNS + ":" + serviceAccount}, args...)
	out, _ := exec.Command("kubectl", cmdArgs...).Output()
	return strings.TrimSpace(string(out))
}
