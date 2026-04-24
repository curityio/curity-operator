package e2e

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

var _ = Describe("Multi-cluster config scoping", Ordered, func() {
	const (
		ns           = "e2e-multi-scope"
		primary      = "primary"
		staging      = "staging"
		primaryAdmin = "pri-admin"
		stagingAdmin = "stg-admin"
		cmScopedPri  = "base-primary"
		cmScopedStg  = "base-staging"
		cmShared     = "shared-base"
		secretShared = "shared-license"
	)
	ctx := context.Background()

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("applies scoped configs only to the cluster listed in curity.io/cluster", func() {
		By("creating two clusters in the same namespace")
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": primary, "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": staging, "namespace": ns})

		By("creating an admin node in each cluster (same node name across clusters is fine)")
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
			map[string]interface{}{"name": primaryAdmin, "namespace": ns, "clusterName": primary})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
			map[string]interface{}{"name": stagingAdmin, "namespace": ns, "clusterName": staging})

		utils.SimulateClusterConfigReady(ns, primary, e2eTimeout, e2eInterval)
		utils.SimulateClusterConfigReady(ns, staging, e2eTimeout, e2eInterval)

		By("waiting for both Deployments to appear under the renamed {cluster}-{node} scheme")
		primaryDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: ownedName(primary, primaryAdmin), Namespace: ns,
		}}
		utils.WaitForResource(primaryDeploy, e2eTimeout, e2eInterval)
		stagingDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: ownedName(staging, stagingAdmin), Namespace: ns,
		}}
		utils.WaitForResource(stagingDeploy, e2eTimeout, e2eInterval)

		By("confirming node-owned Services exist under the same renamed scheme")
		primarySvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: ownedName(primary, primaryAdmin), Namespace: ns,
		}}
		utils.WaitForResource(primarySvc, e2eTimeout, e2eInterval)
		stagingSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: ownedName(staging, stagingAdmin), Namespace: ns,
		}}
		utils.WaitForResource(stagingSvc, e2eTimeout, e2eInterval)

		By("creating three managed configs with different scope annotations")
		scopedPri := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: cmScopedPri, Namespace: ns,
				Labels:      map[string]string{"curity.io/managed": "true"},
				Annotations: map[string]string{"curity.io/cluster": primary},
			},
			Data: map[string]string{"primary.xml": "<primary/>"},
		}
		scopedStg := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: cmScopedStg, Namespace: ns,
				Labels:      map[string]string{"curity.io/managed": "true"},
				Annotations: map[string]string{"curity.io/cluster": staging},
			},
			Data: map[string]string{"staging.xml": "<staging/>"},
		}
		sharedCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: cmShared, Namespace: ns,
				Labels: map[string]string{"curity.io/managed": "true"},
				// No curity.io/cluster annotation → applies to both clusters.
			},
			Data: map[string]string{"shared.xml": "<shared/>"},
		}
		sharedSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secretShared, Namespace: ns,
				Labels:      map[string]string{"curity.io/managed": "true"},
				Annotations: map[string]string{"curity.io/config-type": "license"},
			},
			Data: map[string][]byte{"license.json": []byte(`{"k":"v"}`)},
		}
		Expect(k().Create(ctx, scopedPri)).To(Succeed())
		Expect(k().Create(ctx, scopedStg)).To(Succeed())
		Expect(k().Create(ctx, sharedCM)).To(Succeed())
		Expect(k().Create(ctx, sharedSecret)).To(Succeed())

		By("simulating validation for the primary cluster's scoped set")
		primaryHash := e2eComputeConfigHash([]e2eConfigEntry{
			{Name: cmScopedPri, IsSecret: false, ConfigType: "base", Data: map[string][]byte{"primary.xml": []byte("<primary/>")}},
			{Name: cmShared, IsSecret: false, ConfigType: "base", Data: map[string][]byte{"shared.xml": []byte("<shared/>")}},
			{Name: secretShared, IsSecret: true, ConfigType: "license", Data: map[string][]byte{"license.json": []byte(`{"k":"v"}`)}},
		})
		e2eSimulateValidation(ns, primary, primaryHash)

		By("simulating validation for the staging cluster's scoped set")
		stagingHash := e2eComputeConfigHash([]e2eConfigEntry{
			{Name: cmScopedStg, IsSecret: false, ConfigType: "base", Data: map[string][]byte{"staging.xml": []byte("<staging/>")}},
			{Name: cmShared, IsSecret: false, ConfigType: "base", Data: map[string][]byte{"shared.xml": []byte("<shared/>")}},
			{Name: secretShared, IsSecret: true, ConfigType: "license", Data: map[string][]byte{"license.json": []byte(`{"k":"v"}`)}},
		})
		e2eSimulateValidation(ns, staging, stagingHash)

		By("verifying primary admin mounts its scoped CM + shared, NOT the staging one")
		Eventually(func(g Gomega) {
			g.Expect(k().Get(ctx, client.ObjectKeyFromObject(primaryDeploy), primaryDeploy)).To(Succeed())
			g.Expect(e2eCountCfgVolumes(primaryDeploy)).To(Equal(3), "primary admin should see cm-scoped + cm-shared + secret-shared")
			g.Expect(e2eHasCfgVolume(primaryDeploy, "cfg-cm-"+cmScopedPri)).To(BeTrue())
			g.Expect(e2eHasCfgVolume(primaryDeploy, "cfg-cm-"+cmShared)).To(BeTrue())
			g.Expect(e2eHasCfgVolume(primaryDeploy, "cfg-secret-"+secretShared)).To(BeTrue())
			g.Expect(e2eHasCfgVolume(primaryDeploy, "cfg-cm-"+cmScopedStg)).To(BeFalse(), "primary must NOT mount staging's scoped config")
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("verifying staging admin mounts its scoped CM + shared, NOT the primary one")
		Eventually(func(g Gomega) {
			g.Expect(k().Get(ctx, client.ObjectKeyFromObject(stagingDeploy), stagingDeploy)).To(Succeed())
			g.Expect(e2eCountCfgVolumes(stagingDeploy)).To(Equal(3))
			g.Expect(e2eHasCfgVolume(stagingDeploy, "cfg-cm-"+cmScopedStg)).To(BeTrue())
			g.Expect(e2eHasCfgVolume(stagingDeploy, "cfg-cm-"+cmShared)).To(BeTrue())
			g.Expect(e2eHasCfgVolume(stagingDeploy, "cfg-secret-"+secretShared)).To(BeTrue())
			g.Expect(e2eHasCfgVolume(stagingDeploy, "cfg-cm-"+cmScopedPri)).To(BeFalse(), "staging must NOT mount primary's scoped config")
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("verifying each cluster's own cluster-config Secret exists independently")
		for _, name := range []string{primary + "-cluster-config", staging + "-cluster-config"} {
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
			utils.WaitForResource(sec, e2eTimeout, e2eInterval)
		}

		By("verifying admin-creds Secrets are per-cluster")
		for _, name := range []string{primary + "-admin-creds", staging + "-admin-creds"} {
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
			utils.WaitForResource(sec, e2eTimeout, e2eInterval)
		}

		By("verifying controller OwnerReference from node → cluster is set for both clusters")
		for _, tc := range []struct{ node, cluster string }{
			{primaryAdmin, primary},
			{stagingAdmin, staging},
		} {
			Eventually(func(g Gomega) {
				n := &v1alpha1.IdentityServerNode{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: tc.node, Namespace: ns}, n)).To(Succeed())
				found := false
				for _, or := range n.OwnerReferences {
					if or.Kind == "IdentityServerCluster" && or.Name == tc.cluster {
						g.Expect(or.Controller).NotTo(BeNil())
						g.Expect(*or.Controller).To(BeTrue())
						g.Expect(or.BlockOwnerDeletion).NotTo(BeNil())
						g.Expect(*or.BlockOwnerDeletion).To(BeTrue())
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), fmt.Sprintf("node %q missing OwnerReference to %q", tc.node, tc.cluster))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		}

		By("verifying ClusterReady condition is True on each node")
		for _, name := range []string{primaryAdmin, stagingAdmin} {
			Eventually(func(g Gomega) {
				n := &v1alpha1.IdentityServerNode{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, n)).To(Succeed())
				var ready *metav1.Condition
				for i := range n.Status.Conditions {
					if n.Status.Conditions[i].Type == v1alpha1.ConditionClusterReady {
						ready = &n.Status.Conditions[i]
						break
					}
				}
				g.Expect(ready).NotTo(BeNil(), "ClusterReady condition missing")
				g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(ready.Reason).To(Equal(v1alpha1.ReasonClusterFound))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		}

		By("verifying status.appliedConfigs is per-cluster truthful")
		Eventually(func(g Gomega) {
			pri := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: primaryAdmin, Namespace: ns}, pri)).To(Succeed())
			names := namesOf(pri.Status.AppliedConfigs)
			g.Expect(names).To(ConsistOf(cmScopedPri, cmShared, secretShared))
			g.Expect(names).NotTo(ContainElement(cmScopedStg))
		}, e2eTimeout, e2eInterval).Should(Succeed())
		Eventually(func(g Gomega) {
			stg := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: stagingAdmin, Namespace: ns}, stg)).To(Succeed())
			names := namesOf(stg.Status.AppliedConfigs)
			g.Expect(names).To(ConsistOf(cmScopedStg, cmShared, secretShared))
			g.Expect(names).NotTo(ContainElement(cmScopedPri))
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("verifying ConfigScopeIssues condition is False/NoIssues on both clusters")
		for _, name := range []string{primary, staging} {
			Eventually(func(g Gomega) {
				c := &v1alpha1.IdentityServerCluster{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, c)).To(Succeed())
				var cond *metav1.Condition
				for i := range c.Status.Conditions {
					if c.Status.Conditions[i].Type == v1alpha1.ConditionConfigScopeIssues {
						cond = &c.Status.Conditions[i]
						break
					}
				}
				g.Expect(cond).NotTo(BeNil(), "ConfigScopeIssues condition missing on cluster %q", name)
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonNoIssues))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		}

		By("snapshotting per-cluster Deployments and CRDs")
		utils.MatchYAMLResource(primaryDeploy, "[multi-scope deployment] primary-admin")
		utils.MatchYAMLResource(stagingDeploy, "[multi-scope deployment] staging-admin")
		priNode := &v1alpha1.IdentityServerNode{}
		Expect(k().Get(ctx, client.ObjectKey{Name: primaryAdmin, Namespace: ns}, priNode)).To(Succeed())
		utils.MatchCRDResource(priNode, "multi-scope-primary-admin")
		stgNode := &v1alpha1.IdentityServerNode{}
		Expect(k().Get(ctx, client.ObjectKey{Name: stagingAdmin, Namespace: ns}, stgNode)).To(Succeed())
		utils.MatchCRDResource(stgNode, "multi-scope-staging-admin")
	})

	It("surfaces ConfigScopeIssues when the annotation names a non-existent cluster", func() {
		By("adding a config scoped to a non-existent cluster")
		bad := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "scoped-to-missing", Namespace: ns,
				Labels:      map[string]string{"curity.io/managed": "true"},
				Annotations: map[string]string{"curity.io/cluster": "does-not-exist"},
			},
			Data: map[string]string{"x.xml": "<x/>"},
		}
		Expect(k().Create(ctx, bad)).To(Succeed())

		By("verifying each cluster reports UnknownClusters in ConfigScopeIssues")
		for _, name := range []string{primary, staging} {
			Eventually(func(g Gomega) {
				c := &v1alpha1.IdentityServerCluster{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, c)).To(Succeed())
				var cond *metav1.Condition
				for i := range c.Status.Conditions {
					if c.Status.Conditions[i].Type == v1alpha1.ConditionConfigScopeIssues {
						cond = &c.Status.Conditions[i]
						break
					}
				}
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonUnknownClusters))
				g.Expect(cond.Message).To(ContainSubstring("ConfigMap/scoped-to-missing"))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		}
	})

	It("treats an empty curity.io/cluster annotation as applies-to-none and flags it", func() {
		const cmEmpty = "scoped-to-empty"
		By("adding a managed config with an empty curity.io/cluster annotation")
		empty := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: cmEmpty, Namespace: ns,
				Labels:      map[string]string{"curity.io/managed": "true"},
				Annotations: map[string]string{"curity.io/cluster": ""},
			},
			Data: map[string]string{"empty.xml": "<empty/>"},
		}
		Expect(k().Create(ctx, empty)).To(Succeed())

		By("verifying each cluster surfaces ConfigScopeIssues with EmptyScope and does not mount the config on any node")
		for _, name := range []string{primary, staging} {
			Eventually(func(g Gomega) {
				c := &v1alpha1.IdentityServerCluster{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, c)).To(Succeed())
				var cond *metav1.Condition
				for i := range c.Status.Conditions {
					if c.Status.Conditions[i].Type == v1alpha1.ConditionConfigScopeIssues {
						cond = &c.Status.Conditions[i]
						break
					}
				}
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Message).To(ContainSubstring("ConfigMap/" + cmEmpty))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		}

		// Empty-scope resource must not appear in any node's AppliedConfigs —
		// applies-to-none means mount nowhere.
		for _, adminName := range []string{primaryAdmin, stagingAdmin} {
			Eventually(func(g Gomega) {
				n := &v1alpha1.IdentityServerNode{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: adminName, Namespace: ns}, n)).To(Succeed())
				g.Expect(namesOf(n.Status.AppliedConfigs)).NotTo(ContainElement(cmEmpty))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		}
	})
})

// namesOf extracts the Name field of each AppliedConfigStatus.
func namesOf(status []v1alpha1.AppliedConfigStatus) []string {
	out := make([]string, 0, len(status))
	for _, s := range status {
		out = append(out, s.Name)
	}
	return out
}
