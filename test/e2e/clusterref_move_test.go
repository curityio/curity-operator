package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

var _ = Describe("ClusterRef change", func() {

	Describe("admin move a→b", Ordered, func() {
		const (
			ns        = "e2e-move-admin"
			clusterA  = "move-a"
			clusterB  = "move-b"
			adminName = "move-admin"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("cleans cluster-a orphan resources and flips ClusterConfigReady=False without mutating Secret", func() {
			ctx := context.Background()

			By("creating two clusters and an admin on cluster-a")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterA, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterB, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminName, "namespace": ns, "clusterName": clusterA})
			utils.SimulateClusterConfigReady(ns, clusterA, e2eTimeout, e2eInterval)

			By("waiting for cluster-a's admin Deployment + Service to exist")
			deployA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, adminName), Namespace: ns}}
			utils.WaitForResource(deployA, e2eTimeout, e2eInterval)
			svcA := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, adminName), Namespace: ns}}
			utils.WaitForResource(svcA, e2eTimeout, e2eInterval)

			By("snapshot — initial state on cluster-a")
			node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: adminName, Namespace: ns}}
			utils.WaitForConditions(node, e2eTimeout, e2eInterval)
			utils.MatchCRDResource(node, "01-admin-on-a")
			clusterAObj := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterA, Namespace: ns}}
			utils.WaitForConditions(clusterAObj, e2eTimeout, e2eInterval)
			utils.MatchCRDResource(clusterAObj, "01-cluster-a-with-admin")

			By("patching admin's identityServerClusterRef from cluster-a to cluster-b")
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: adminName, Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: clusterB}
				return k().Update(ctx, node)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("capturing cluster-a Secret state before the move")
			secBefore := &corev1.Secret{}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterA + "-cluster-config", Namespace: ns}, secBefore)).To(Succeed())
			beforeXML := string(secBefore.Data["cluster.xml"])
			beforeAdminAnno := secBefore.Annotations["curity.io/admin-node"]
			Expect(beforeXML).NotTo(Equal("placeholder"))
			Expect(beforeAdminAnno).To(Equal(adminName))

			By("waiting for cluster-a's admin Deployment to be cleaned up")
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, adminName), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, adminName), Namespace: ns}, &corev1.Service{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			By("waiting for cluster-a ClusterConfigReady=False, reason=WaitingForAdmin")
			Eventually(func() string {
				if err := k().Get(ctx, client.ObjectKey{Name: clusterA, Namespace: ns}, clusterAObj); err != nil {
					return ""
				}
				for _, c := range clusterAObj.Status.Conditions {
					if c.Type == v1alpha1.ConditionClusterConfigReady && c.Status == metav1.ConditionFalse {
						return c.Reason
					}
				}
				return ""
			}, e2eTimeout, e2eInterval).Should(Equal("WaitingForAdmin"))

			By("verifying cluster-a's Secret data + admin-node annotation were NOT mutated")
			// Critical: keeping the Secret intact lets surviving runtime
			// pods (mounted via subPath) keep their valid in-memory config.
			// Mutating it would force a rolling restart that crashloops on
			// any new pod reading the placeholder data.
			Consistently(func(g Gomega) {
				s := &corev1.Secret{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterA + "-cluster-config", Namespace: ns}, s)).To(Succeed())
				g.Expect(string(s.Data["cluster.xml"])).To(Equal(beforeXML))
				g.Expect(s.Annotations["curity.io/admin-node"]).To(Equal(beforeAdminAnno))
			}, "3s", e2eInterval).Should(Succeed())

			By("simulating cluster-b's config readiness so the new admin Deployment can be created")
			utils.SimulateClusterConfigReady(ns, clusterB, e2eTimeout, e2eInterval)

			By("waiting for the new admin Deployment + Service on cluster-b")
			deployB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterB, adminName), Namespace: ns}}
			utils.WaitForResource(deployB, e2eTimeout, e2eInterval)
			svcB := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterB, adminName), Namespace: ns}}
			utils.WaitForResource(svcB, e2eTimeout, e2eInterval)

			By("snapshot — post-move state")
			Expect(k().Get(ctx, client.ObjectKey{Name: adminName, Namespace: ns}, node)).To(Succeed())
			utils.MatchCRDResource(node, "02-admin-on-b")
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterA, Namespace: ns}, clusterAObj)).To(Succeed())
			utils.MatchCRDResource(clusterAObj, "02-cluster-a-abandoned")
			clusterBObj := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterB, Namespace: ns}}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterB, Namespace: ns}, clusterBObj)).To(Succeed())
			utils.MatchCRDResource(clusterBObj, "02-cluster-b-with-admin")
		})
	})

	Describe("runtime move a→b cleans HPA and PDB", Ordered, func() {
		const (
			ns       = "e2e-move-runtime"
			clusterA = "rt-move-a"
			clusterB = "rt-move-b"
			rtName   = "rt-mover"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("removes HPA + PDB from cluster-a along with Deployment + Service", func() {
			ctx := context.Background()

			By("creating two clusters with autoscaling+PDB enabled and a runtime node on cluster-a")
			minOne := int32(1)
			clusterAObj := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterA, Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled:                        true,
						MinReplicas:                    minOne,
						MaxReplicas:                    3,
						TargetCPUUtilizationPercentage: 70,
					},
				},
			}
			Expect(k().Create(ctx, clusterAObj)).To(Succeed())
			Expect(k().Create(ctx, &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterB, Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
			})).To(Succeed())

			pdbMin := intstr.FromInt32(1)
			rt := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: rtName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "runtime-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterA},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &pdbMin},
					Service:                  defaultTestService(),
				},
			}
			Expect(k().Create(ctx, rt)).To(Succeed())

			By("waiting for all four child resources on cluster-a")
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)
			utils.WaitForResource(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)
			utils.WaitForResource(&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)
			utils.WaitForResource(&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("patching runtime's clusterRef to cluster-b")
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: rtName, Namespace: ns}, rt); err != nil {
					return err
				}
				rt.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: clusterB}
				return k().Update(ctx, rt)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("waiting for all four cluster-a child kinds to be cleaned up")
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, rtName), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, rtName), Namespace: ns}, &corev1.Service{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, rtName), Namespace: ns}, &autoscalingv2.HorizontalPodAutoscaler{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, rtName), Namespace: ns}, &policyv1.PodDisruptionBudget{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			By("waiting for new Deployment + Service on cluster-b (runtime-only, no admin gate)")
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterB, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)
			utils.WaitForResource(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterB, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)
		})
	})

	Describe("multi-cluster isolation", Ordered, func() {
		const (
			ns       = "e2e-move-multi"
			clusterA = "iso-a"
			clusterB = "iso-b"
			clusterC = "iso-c"
			adminA   = "iso-admin-a"
			adminB   = "iso-admin-b"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("does not touch peer cluster's children when one node moves", func() {
			ctx := context.Background()

			By("creating three clusters and two admins, one in cluster-a and one in cluster-b")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterA, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterB, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterC, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminA, "namespace": ns, "clusterName": clusterA})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminB, "namespace": ns, "clusterName": clusterB})
			utils.SimulateClusterConfigReady(ns, clusterA, e2eTimeout, e2eInterval)
			utils.SimulateClusterConfigReady(ns, clusterB, e2eTimeout, e2eInterval)

			By("waiting for both admins' Deployments")
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, adminA), Namespace: ns}}, e2eTimeout, e2eInterval)
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterB, adminB), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("moving adminA from cluster-a to cluster-c")
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: adminA, Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: clusterC}
				return k().Update(ctx, node)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("waiting for cluster-a's adminA Deployment to be cleaned")
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, adminA), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			By("verifying cluster-b's adminB Deployment + Service are intact (UID isolation)")
			Expect(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterB, adminB), Namespace: ns}, &appsv1.Deployment{})).To(Succeed())
			Expect(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterB, adminB), Namespace: ns}, &corev1.Service{})).To(Succeed())

			By("verifying cluster-b's Secret was NOT reset to placeholder")
			secB := &corev1.Secret{}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterB + "-cluster-config", Namespace: ns}, secB)).To(Succeed())
			Expect(string(secB.Data["cluster.xml"])).NotTo(Equal("placeholder"))
		})
	})

	Describe("in-cluster admin rename preserves Secret", Ordered, func() {
		// Regression guard: when an admin is "renamed" within the same
		// cluster (delete-old then create-new), there is a brief window
		// when the cluster has no admin. The abandoned-Secret reset must
		// NOT fire in this window — Branch B handles the host rewrite
		// cheaply once the new admin appears, and an eager reset would
		// force an unnecessary full regen.
		const (
			ns       = "e2e-rename-admin"
			cluster  = "rename-cluster"
			adminOld = "rename-old"
			adminNew = "rename-new"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("does not reset Secret when admin CR is deleted (rename-in-progress)", func() {
			ctx := context.Background()

			By("creating cluster + first admin and waiting for Secret to populate")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": cluster, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminOld, "namespace": ns, "clusterName": cluster})
			utils.SimulateClusterConfigReady(ns, cluster, e2eTimeout, e2eInterval)

			By("deleting old admin CR (no replacement yet)")
			old := &v1alpha1.IdentityServerNode{}
			Expect(k().Get(ctx, client.ObjectKey{Name: adminOld, Namespace: ns}, old)).To(Succeed())
			Expect(k().Delete(ctx, old)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: adminOld, Namespace: ns}, &v1alpha1.IdentityServerNode{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			By("verifying the cluster-config Secret is NOT reset to placeholder")
			// Hold a few reconcile cycles to make sure the reset path stays a no-op.
			Consistently(func() string {
				s := &corev1.Secret{}
				if err := k().Get(ctx, client.ObjectKey{Name: cluster + "-cluster-config", Namespace: ns}, s); err != nil {
					return "<error>"
				}
				return string(s.Data["cluster.xml"])
			}, "5s", e2eInterval).ShouldNot(Equal("placeholder"))

			By("creating new admin CR with a different name (the rename)")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminNew, "namespace": ns, "clusterName": cluster})

			By("waiting for the Secret's curity.io/admin-node annotation to advance to the new admin")
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k().Get(ctx, client.ObjectKey{Name: cluster + "-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return s.Annotations["curity.io/admin-node"]
			}, e2eTimeout, e2eInterval).Should(Equal(adminNew))
		})
	})

	Describe("successive a→b→c moves", Ordered, func() {
		// Smoke test S6: a node bouncing through several clusters in
		// rapid succession leaves no orphans. Cleanup uses List+filter
		// keyed on UID so any number of stale ownedResource names get
		// caught in one reconcile sweep.
		const (
			ns       = "e2e-multi-move"
			clusterA = "multi-a"
			clusterB = "multi-b"
			clusterC = "multi-c"
			rtName   = "multi-hopper"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("cleans both prior generations of orphans", func() {
			ctx := context.Background()

			By("creating three clusters and a runtime node on cluster-a")
			for _, c := range []string{clusterA, clusterB, clusterC} {
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": c, "namespace": ns})
			}
			rt := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: rtName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "hopper-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterA},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k().Create(ctx, rt)).To(Succeed())

			By("waiting for the initial Deployment on cluster-a")
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterA, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("first move a→b")
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: rtName, Namespace: ns}, rt); err != nil {
					return err
				}
				rt.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: clusterB}
				return k().Update(ctx, rt)
			}, e2eTimeout, e2eInterval).Should(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, rtName), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterB, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("second move b→c")
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: rtName, Namespace: ns}, rt); err != nil {
					return err
				}
				rt.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: clusterC}
				return k().Update(ctx, rt)
			}, e2eTimeout, e2eInterval).Should(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(clusterB, rtName), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterC, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("verifying both prior generations are gone")
			Consistently(func() error {
				return k().Get(ctx, client.ObjectKey{Name: ownedName(clusterA, rtName), Namespace: ns}, &appsv1.Deployment{})
			}, "2s", e2eInterval).ShouldNot(Succeed())
			Consistently(func() error {
				return k().Get(ctx, client.ObjectKey{Name: ownedName(clusterB, rtName), Namespace: ns}, &appsv1.Deployment{})
			}, "2s", e2eInterval).ShouldNot(Succeed())
		})
	})

	Describe("move to non-existent cluster", Ordered, func() {
		// Smoke test S7: cleanup placement is "before destination fetch"
		// so source orphans are removed even when the new cluster CR
		// doesn't exist and the node ends up Degraded with ClusterNotFound.
		const (
			ns          = "e2e-move-missing"
			realCluster = "real-cluster"
			ghostName   = "ghost-cluster"
			rtName      = "missing-rt"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("cleans source orphan even when destination is missing", func() {
			ctx := context.Background()

			By("creating one real cluster and a runtime node on it")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": realCluster, "namespace": ns})
			rt := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: rtName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "rt-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: realCluster},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k().Create(ctx, rt)).To(Succeed())

			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(realCluster, rtName), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("patching clusterRef to a non-existent cluster")
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: rtName, Namespace: ns}, rt); err != nil {
					return err
				}
				rt.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: ghostName}
				return k().Update(ctx, rt)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("source-cluster orphan must be cleaned even though destination is missing")
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(realCluster, rtName), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			By("node must be Degraded with ClusterNotFound")
			Eventually(func() string {
				n := &v1alpha1.IdentityServerNode{}
				if err := k().Get(ctx, client.ObjectKey{Name: rtName, Namespace: ns}, n); err != nil {
					return ""
				}
				for _, c := range n.Status.Conditions {
					if c.Type == v1alpha1.ConditionClusterReady {
						return c.Reason
					}
				}
				return ""
			}, e2eTimeout, e2eInterval).Should(Equal(v1alpha1.ReasonClusterNotFound))
		})
	})

	Describe("move into cluster with existing admin", Ordered, func() {
		// Smoke test S8: cleanup runs before the duplicate-admin
		// validation, so the source-cluster's stale children get cleaned
		// even when the move itself is rejected by DuplicateAdmin.
		const (
			ns        = "e2e-dup-admin-move"
			sourceA   = "dup-src-a"
			targetB   = "dup-tgt-b"
			incumbent = "dup-incumbent"
			mover     = "dup-mover"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("cleans source orphan even when DuplicateAdmin rejects the move", func() {
			ctx := context.Background()

			By("creating two clusters with one admin each")
			for _, c := range []string{sourceA, targetB} {
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": c, "namespace": ns})
			}
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": incumbent, "namespace": ns, "clusterName": targetB})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": mover, "namespace": ns, "clusterName": sourceA})
			utils.SimulateClusterConfigReady(ns, sourceA, e2eTimeout, e2eInterval)
			utils.SimulateClusterConfigReady(ns, targetB, e2eTimeout, e2eInterval)

			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(sourceA, mover), Namespace: ns}}, e2eTimeout, e2eInterval)
			utils.WaitForResource(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName(targetB, incumbent), Namespace: ns}}, e2eTimeout, e2eInterval)

			By("moving mover into target which already has incumbent")
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: mover, Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: targetB}
				return k().Update(ctx, node)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("source orphan goes")
			Eventually(func() bool {
				return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName(sourceA, mover), Namespace: ns}, &appsv1.Deployment{}))
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			By("mover is Degraded with DuplicateAdmin")
			Eventually(func() string {
				n := &v1alpha1.IdentityServerNode{}
				if err := k().Get(ctx, client.ObjectKey{Name: mover, Namespace: ns}, n); err != nil {
					return ""
				}
				for _, c := range n.Status.Conditions {
					if c.Type == v1alpha1.ConditionDegraded {
						return c.Reason
					}
				}
				return ""
			}, e2eTimeout, e2eInterval).Should(Equal("DuplicateAdmin"))

			By("incumbent's Deployment is intact")
			Expect(k().Get(ctx, client.ObjectKey{Name: ownedName(targetB, incumbent), Namespace: ns}, &appsv1.Deployment{})).To(Succeed())
		})
	})
})
