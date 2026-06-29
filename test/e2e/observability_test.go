package e2e

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

// The operator discovers the ServiceMonitor CRD once at startup, and the
// default e2e env is deployed without it (so other tests stay CRD-absent and
// their cluster-status snapshots don't gain serviceMonitorName). This suite
// therefore installs the CRD and restarts the operator in BeforeAll, then
// restores the CRD-absent env in AfterAll.
const (
	obsOperatorNS = "curity-operator"
	smCRDPath     = "./test/crds/monitoring.coreos.com_servicemonitors.yaml"
)

func restartOperator() {
	out, err := utils.Run("kubectl", "get", "deploy", "-l", "control-plane=controller-manager",
		"-n", obsOperatorNS, "-o", "jsonpath={.items[0].metadata.name}")
	Expect(err).NotTo(HaveOccurred())
	deploy := strings.TrimSpace(out)
	Expect(deploy).NotTo(BeEmpty(), "operator deployment not found")
	_, err = utils.Run("kubectl", "rollout", "restart", "deployment/"+deploy, "-n", obsOperatorNS)
	Expect(err).NotTo(HaveOccurred())
	_, err = utils.Run("kubectl", "rollout", "status", "deployment/"+deploy, "-n", obsOperatorNS, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("IdentityServerCluster observability", Label("smoke"), Ordered, func() {
	const ns = "e2e-observability"

	statusSMName := func(cluster string) string {
		c := &v1alpha1.IdentityServerCluster{}
		_ = k().Get(context.Background(), client.ObjectKey{Name: cluster, Namespace: ns}, c)
		return c.Status.ServiceMonitorName
	}

	BeforeAll(func() {
		By("installing the ServiceMonitor CRD and restarting the operator so discovery sees it")
		_, err := utils.Run("kubectl", "apply", "-f", smCRDPath)
		Expect(err).NotTo(HaveOccurred())
		restartOperator()
		createNS(ns)
	})

	AfterAll(func() {
		deleteNS(ns)
		By("removing the ServiceMonitor CRD and restoring the CRD-absent operator state")
		_, _ = utils.Run("kubectl", "delete", "-f", smCRDPath, "--ignore-not-found")
		restartOperator()
	})

	It("creates a ServiceMonitor by default and records its name in status", func() {
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "obs-default", "namespace": ns})

		sm := &monitoringv1.ServiceMonitor{ObjectMeta: metav1.ObjectMeta{Name: ownedName("obs-default", "metrics"), Namespace: ns}}
		utils.WaitForResource(sm, e2eTimeout, e2eInterval)

		By("snapshotting the rendered ServiceMonitor")
		utils.MatchYAMLResource(sm, "obs-default-servicemonitor")

		By("selecting the cluster's node Services and scraping /metrics over http")
		Expect(sm.Spec.Selector.MatchLabels).To(Equal(map[string]string{
			"curity.io/cluster":            "obs-default",
			"app.kubernetes.io/managed-by": "curity-operator",
		}))
		Expect(sm.Spec.NamespaceSelector.MatchNames).To(Equal([]string{ns}))
		Expect(sm.Spec.Endpoints).To(HaveLen(1))
		Expect(sm.Spec.Endpoints[0].Port).To(Equal("metrics"))
		Expect(sm.Spec.Endpoints[0].Path).To(Equal("/metrics"))
		Expect(sm.Spec.Endpoints[0].Scheme).To(BeNil(), "http (nil scheme)")

		By("owning the ServiceMonitor for garbage collection")
		Expect(sm.OwnerReferences).To(HaveLen(1))
		Expect(sm.OwnerReferences[0].Kind).To(Equal("IdentityServerCluster"))
		Expect(sm.OwnerReferences[0].Name).To(Equal("obs-default"))
		Expect(sm.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))

		By("surfacing the name in status")
		Eventually(func() string {
			return statusSMName("obs-default")
		}, e2eTimeout, e2eInterval).Should(Equal(ownedName("obs-default", "metrics")))
	})

	It("applies custom labels and interval", func() {
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-observability.yaml", ns,
			map[string]interface{}{"name": "obs-custom", "namespace": ns})

		sm := &monitoringv1.ServiceMonitor{ObjectMeta: metav1.ObjectMeta{Name: ownedName("obs-custom", "metrics"), Namespace: ns}}
		utils.WaitForResource(sm, e2eTimeout, e2eInterval)

		Expect(sm.Labels).To(HaveKeyWithValue("release", "kube-prometheus-stack"))
		Expect(sm.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "curity-operator"))
		Expect(sm.Labels).To(HaveKeyWithValue("curity.io/cluster", "obs-custom"))
		Expect(string(sm.Spec.Endpoints[0].Interval)).To(Equal("15s"))

		utils.MatchYAMLResource(sm, "obs-custom-servicemonitor")
	})

	It("deletes the ServiceMonitor and clears status when disabled", func() {
		_, err := utils.Run("kubectl", "patch", "isc", "obs-default", "-n", ns, "--type=merge",
			"-p", `{"spec":{"observability":{"serviceMonitor":{"enabled":false}}}}`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			sm := &monitoringv1.ServiceMonitor{}
			err := k().Get(context.Background(), client.ObjectKey{Name: ownedName("obs-default", "metrics"), Namespace: ns}, sm)
			return apierrors.IsNotFound(err)
		}, e2eTimeout, e2eInterval).Should(BeTrue(), "ServiceMonitor should be deleted")

		Eventually(func() string {
			return statusSMName("obs-default")
		}, e2eTimeout, e2eInterval).Should(BeEmpty())
	})
})
