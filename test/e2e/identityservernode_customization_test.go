package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

var _ = Describe("IdentityServerNode customization + NetworkPolicy", Label("smoke"), Ordered, func() {
	const ns = "e2e-customization"

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("renders an admin NetworkPolicy and pod-customization fields on the runtime Deployment", func() {
		const (
			clusterName = "custom-cluster"
			adminName   = "custom-admin"
			runtimeName = "custom-runtime"
		)

		// Cluster with NetworkPolicy enabled (gateway namespace set so the
		// admin-UI ingress rule is emitted once the admin node enables its UI).
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-networkpolicy.yaml", ns,
			map[string]interface{}{
				"name":                clusterName,
				"namespace":           ns,
				"apiGatewayNamespace": "edge-gateway",
			})

		// Admin node with UI enabled — triggers the cluster-scoped NetworkPolicy
		// (admin-only).
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin-ui.yaml", ns,
			map[string]interface{}{
				"name":        adminName,
				"namespace":   ns,
				"clusterName": clusterName,
				"uiEnabled":   true,
				"uiSecure":    true,
			})

		// Unblock the admin Deployment gate (genclust does not complete in e2e).
		utils.SimulateClusterConfigReady(ns, clusterName, e2eTimeout, e2eInterval)

		adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, adminName), Namespace: ns}}
		utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)

		// NetworkPolicy is created for the admin node, cluster-scoped config.
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, adminName), Namespace: ns}}
		utils.WaitForResource(np, e2eTimeout, e2eInterval)
		Expect(np.Spec.PolicyTypes).To(Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}))
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("curity.io/owned-by", ownedName(clusterName, adminName)))
		Expect(np.Spec.Ingress).To(HaveLen(3), "runtime→admin + genclust→admin-config + gateway→admin-UI rules")
		utils.MatchYAMLResource(np, "admin-networkpolicy")

		// Runtime node exercising every pod-customization field.
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime-podcustomization.yaml", ns,
			map[string]interface{}{
				"name":        runtimeName,
				"namespace":   ns,
				"role":        "default",
				"clusterName": clusterName,
			})

		runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, runtimeName), Namespace: ns}}
		utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)

		podSpec := runtimeDeploy.Spec.Template.Spec
		Expect(podSpec.TerminationGracePeriodSeconds).NotTo(BeNil())
		Expect(*podSpec.TerminationGracePeriodSeconds).To(Equal(int64(120)))

		// Main container: pull-policy override + container securityContext.
		main := podSpec.Containers[0]
		Expect(main.Name).To(Equal("curity"))
		Expect(main.ImagePullPolicy).To(Equal(corev1.PullAlways))
		Expect(main.SecurityContext).NotTo(BeNil())
		Expect(main.SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
		Expect(*main.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())

		// Pod securityContext merge preserved the required UID/GID.
		Expect(podSpec.SecurityContext).NotTo(BeNil())
		Expect(*podSpec.SecurityContext.RunAsUser).To(Equal(int64(10001)))
		Expect(podSpec.SecurityContext.FSGroupChangePolicy).NotTo(BeNil())

		// User sidecar appended after the operator's containers; init appended too.
		Expect(containerNames(podSpec.Containers)).To(ContainElement("audit-shipper"))
		Expect(containerNames(podSpec.InitContainers)).To(ContainElement("wait-for-db"))

		utils.MatchYAMLResource(runtimeDeploy, "runtime-deployment")

		// CRD status snapshots at the post-deployment lifecycle stage.
		adminNode := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: adminName, Namespace: ns}}
		utils.WaitForConditions(adminNode, e2eTimeout, e2eInterval)
		utils.MatchCRDResource(adminNode, "admin-node post-deployment")

		runtimeNode := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: runtimeName, Namespace: ns}}
		utils.WaitForConditions(runtimeNode, e2eTimeout, e2eInterval)
		utils.MatchCRDResource(runtimeNode, "runtime-node post-deployment")
	})
})

func containerNames(cs []corev1.Container) []string {
	names := make([]string, 0, len(cs))
	for i := range cs {
		names = append(names, cs[i].Name)
	}
	return names
}
