package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

const (
	e2eTimeout  = 60 * time.Second
	e2eInterval = time.Second
)

func createNS(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := utils.TestEnvironment.K8sClient.Create(context.Background(), ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Fail(fmt.Sprintf("failed to create namespace %s: %v", name, err))
	}
}

func deleteNS(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_ = utils.TestEnvironment.K8sClient.Delete(context.Background(), ns)
}

func k() client.Client { return utils.TestEnvironment.K8sClient }

var _ = Describe("IdentityServerNode", func() {

	// =================================================================
	// deployment lifecycle
	// =================================================================
	Context("deployment lifecycle", func() {

		Describe("Deploy admin + runtime nodes", Label("smoke"), Ordered, func() {
			const ns = "e2e-deploy"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create Deployments and Services for both node types", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "deploy-cluster", "namespace": ns})

				By("waiting for cluster to reconcile")
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "deploy-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "deploy-cluster pre-deployment")

				By("deploying admin node and verifying resources")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
					map[string]interface{}{"name": "deploy-admin", "namespace": ns, "clusterName": "deploy-cluster"})

				adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy-admin", Namespace: ns}}
				utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(adminDeploy, "[admin-deployment] deploy-admin")

				adminSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "deploy-admin", Namespace: ns}}
				utils.WaitForResource(adminSvc, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(adminSvc, "[admin-service] deploy-admin")

				By("deploying runtime node and verifying resources")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "deploy-runtime", "namespace": ns, "clusterName": "deploy-cluster"})

				runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy-runtime", Namespace: ns}}
				utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(runtimeDeploy, "[runtime-deployment] deploy-runtime")

				runtimeSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "deploy-runtime", Namespace: ns}}
				utils.WaitForResource(runtimeSvc, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(runtimeSvc, "[runtime-service] deploy-runtime")

				By("snapshotting final state with both nodes")
				Eventually(func(g Gomega) int32 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "deploy-cluster", Namespace: ns}, cluster)).To(Succeed())
					return cluster.Status.NodeCount
				}, e2eTimeout, e2eInterval).Should(BeNumerically(">=", 2))

				adminNode := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "deploy-admin", Namespace: ns}}
				utils.WaitForConditions(adminNode, e2eTimeout, e2eInterval)

				runtimeNode := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "deploy-runtime", Namespace: ns}}
				utils.WaitForConditions(runtimeNode, e2eTimeout, e2eInterval)

				Expect(k().Get(ctx, client.ObjectKey{Name: "deploy-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "deploy-cluster post-deployment")
				utils.MatchCRDResource(adminNode, "deploy-admin post-deployment")
				utils.MatchCRDResource(runtimeNode, "deploy-runtime post-deployment")
			})
		})

		Describe("Delete node cleans up resources", Label("smoke"), Ordered, func() {
			const ns = "e2e-delete"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should garbage collect Deployment and Service via OwnerRef", func() {
				ctx := context.Background()

				By("creating cluster and node")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "del-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "del-node", "namespace": ns, "clusterName": "del-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "del-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(deploy.OwnerReferences).To(HaveLen(1))
				Expect(deploy.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "del-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "del-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("snapshotting before delete")
				utils.MatchCRDResource(cluster, "del-cluster before-delete")
				Expect(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "del-node before-delete")

				By("deleting node and verifying garbage collection")
				Expect(k().Delete(ctx, node)).To(Succeed())

				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, &v1alpha1.IdentityServerNode{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, &appsv1.Deployment{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, &corev1.Service{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
			})
		})

		Describe("Cluster deletion blocked by nodes", Ordered, func() {
			const ns = "e2e-finalizer"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should block cluster deletion until all nodes removed", func() {
				ctx := context.Background()

				By("creating cluster and node")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "fin-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "fin-node", "namespace": ns, "clusterName": "fin-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "fin-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				By("verifying finalizer is set on cluster")
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "fin-cluster", Namespace: ns}}
				Eventually(func(g Gomega) bool {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, cluster)).To(Succeed())
					for _, f := range cluster.Finalizers {
						if f == v1alpha1.ClusterFinalizer {
							return true
						}
					}
					return false
				}, e2eTimeout, e2eInterval).Should(BeTrue())

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "fin-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)

				utils.MatchCRDResource(cluster, "fin-cluster before-delete")
				Expect(k().Get(ctx, client.ObjectKey{Name: "fin-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "fin-node before-delete")

				By("attempting cluster deletion while node exists")
				Expect(k().Delete(ctx, cluster)).To(Succeed())
				Consistently(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, &v1alpha1.IdentityServerCluster{})
				}, 5*time.Second, e2eInterval).Should(Succeed())

				By("removing node to unblock cluster deletion")
				Expect(k().Get(ctx, client.ObjectKey{Name: "fin-node", Namespace: ns}, node)).To(Succeed())
				Expect(k().Delete(ctx, node)).To(Succeed())

				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, &v1alpha1.IdentityServerCluster{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
			})
		})

		Describe("Update replicas", Ordered, func() {
			const ns = "e2e-update"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should update Deployment when replicas change", func() {
				ctx := context.Background()

				By("creating cluster and node")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "upd-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "upd-node", "namespace": ns, "clusterName": "upd-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "upd-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

				By("snapshotting pre-update state")
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "upd-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "upd-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "upd-cluster pre-update")
				utils.MatchCRDResource(node, "upd-node pre-update")

				By("updating replicas to 3")
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, node); err != nil {
						return err
					}
					node.Spec.Replicas = ptr.To(int32(3))
					return k().Update(ctx, node)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				Eventually(func(g Gomega) int32 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, deploy)).To(Succeed())
					if deploy.Spec.Replicas == nil {
						return 0
					}
					return *deploy.Spec.Replicas
				}, e2eTimeout, e2eInterval).Should(Equal(int32(3)))

				By("snapshotting post-update state")
				Eventually(func(g Gomega) int32 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, node)).To(Succeed())
					return node.Status.Replicas
				}, e2eTimeout, e2eInterval).Should(Equal(int32(3)))

				Expect(k().Get(ctx, client.ObjectKey{Name: "upd-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "upd-cluster post-update")
				utils.MatchCRDResource(node, "upd-node post-update")
			})
		})

		Describe("Idempotency", Ordered, func() {
			const ns = "e2e-idempotent"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should not update Deployment when spec unchanged", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "idem-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "idem-node", "namespace": ns, "clusterName": "idem-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "idem-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				gen := deploy.Generation

				By("triggering a reconcile by adding a label to the node")
				Eventually(func() error {
					node := &v1alpha1.IdentityServerNode{}
					if err := k().Get(ctx, client.ObjectKey{Name: "idem-node", Namespace: ns}, node); err != nil {
						return err
					}
					if node.Labels == nil {
						node.Labels = map[string]string{}
					}
					node.Labels["trigger"] = "reconcile"
					return k().Update(ctx, node)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying Deployment generation did not change")
				Consistently(func(g Gomega) {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "idem-node", Namespace: ns}, deploy)).To(Succeed())
					g.Expect(deploy.Generation).To(Equal(gen), "Deployment should not be updated when spec unchanged")
				}, 3*time.Second, e2eInterval).Should(Succeed())

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "idem-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "idem-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "idem-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(node, "idem-node")
			})
		})
	})

	// =================================================================
	// status and conditions
	// =================================================================
	Context("status and conditions", func() {

		Describe("Cluster status reflects node health", Label("smoke"), Ordered, func() {
			const ns = "e2e-status"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set conditions and status fields", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "status-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "status-node", "namespace": ns, "clusterName": "status-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "status-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "status-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(node.Status.ObservedGeneration).To(BeNumerically(">", 0))
				Expect(node.Status.DeploymentName).To(Equal("status-node"))
				Expect(node.Status.ServiceName).To(Equal("status-node"))
				Expect(node.Status.Replicas).To(Equal(int32(1)))
				Expect(node.Status.Conditions).To(ContainElement(
					Satisfy(func(c metav1.Condition) bool {
						return c.Type == v1alpha1.ConditionReady
					}),
				), "node should have Ready condition")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "status-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				Expect(cluster.Status.NodeCount).To(Equal(int32(1)))
				Expect(cluster.Status.Version).To(Equal("11.0"))
				Expect(cluster.Status.ObservedGeneration).To(BeNumerically(">", 0))

				Expect(k().Get(ctx, client.ObjectKey{Name: "status-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "status-cluster")
				Expect(k().Get(ctx, client.ObjectKey{Name: "status-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "status-node")
			})
		})

		Describe("Cluster with no nodes", Ordered, func() {
			const ns = "e2e-empty"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set nodeCount=0 and Ready=False", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "empty-cluster", "namespace": ns})

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "empty-cluster", Namespace: ns}}
				Eventually(func(g Gomega) int64 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "empty-cluster", Namespace: ns}, cluster)).To(Succeed())
					return cluster.Status.ObservedGeneration
				}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

				Expect(cluster.Status.NodeCount).To(Equal(int32(0)))
				Expect(cluster.Status.Conditions).To(ContainElement(
					Satisfy(func(c metav1.Condition) bool {
						return c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionFalse
					}),
				), "Ready should be False for empty cluster")

				Expect(k().Get(ctx, client.ObjectKey{Name: "empty-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "empty-cluster")
			})
		})
	})

	// =================================================================
	// validation and rejection
	// =================================================================
	Context("validation and rejection", func() {

		Describe("Duplicate admin node rejection", Ordered, func() {
			const ns = "e2e-dup-admin"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set Degraded on second admin and not create its Deployment", func() {
				ctx := context.Background()

				By("creating cluster and first admin")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "dup-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
					map[string]interface{}{"name": "admin-1", "namespace": ns, "clusterName": "dup-cluster"})

				deploy1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "admin-1", Namespace: ns}}
				utils.WaitForResource(deploy1, e2eTimeout, e2eInterval)

				By("creating second admin node")
				node2 := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "admin-2", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeAdmin, Role: "admin-role-2",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "dup-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node2)).To(Succeed())

				By("verifying second admin is degraded")
				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "admin-2", Namespace: ns}, node2)).To(Succeed())
					for _, c := range node2.Status.Conditions {
						if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
							return c.Reason
						}
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal("DuplicateAdmin"))

				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "admin-2", Namespace: ns}, &appsv1.Deployment{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())

				By("verifying first admin also detects the duplicate")
				admin1 := &v1alpha1.IdentityServerNode{}
				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "admin-1", Namespace: ns}, admin1)).To(Succeed())
					for _, c := range admin1.Status.Conditions {
						if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
							return c.Reason
						}
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal("DuplicateAdmin"))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "dup-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				utils.MatchCRDResource(cluster, "dup-cluster")
				Expect(k().Get(ctx, client.ObjectKey{Name: "admin-1", Namespace: ns}, admin1)).To(Succeed())
				utils.MatchCRDResource(admin1, "admin-1")
				Expect(k().Get(ctx, client.ObjectKey{Name: "admin-2", Namespace: ns}, node2)).To(Succeed())
				utils.MatchCRDResource(node2, "admin-2 degraded")
			})
		})

		Describe("Orphan node", Ordered, func() {
			const ns = "e2e-orphan"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set Degraded when cluster does not exist", func() {
				ctx := context.Background()
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "orphan-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "orphan-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "does-not-exist"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "orphan-node", Namespace: ns}, node)).To(Succeed())
					for _, c := range node.Status.Conditions {
						if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
							return c.Reason
						}
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal("ClusterNotFound"))

				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "orphan-node", Namespace: ns}, &appsv1.Deployment{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())

				Expect(k().Get(ctx, client.ObjectKey{Name: "orphan-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "orphan-node degraded")
			})
		})

		Describe("Orphan node recovery", Ordered, func() {
			const ns = "e2e-recovery"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should recover when missing cluster is created", func() {
				ctx := context.Background()

				By("creating node referencing non-existent cluster")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "recovery-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "recovery-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "recovery-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying node is degraded")
				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, node)).To(Succeed())
					for _, c := range node.Status.Conditions {
						if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
							return c.Reason
						}
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal("ClusterNotFound"))

				Expect(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "recovery-node degraded")

				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, &appsv1.Deployment{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())

				By("creating missing cluster to trigger recovery")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "recovery-cluster", "namespace": ns})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "recovery-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				By("snapshotting recovered state")
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "recovery-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				Expect(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(cluster, "recovery-cluster")
				utils.MatchCRDResource(node, "recovery-node recovered")
			})
		})

		Describe("Duplicate role rejection", Ordered, func() {
			const ns = "e2e-dup-role"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should block node with duplicate role in same cluster", func() {
				ctx := context.Background()

				By("creating cluster and first node")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "role-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "role-node-1", "namespace": ns, "clusterName": "role-cluster"})

				deploy1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "role-node-1", Namespace: ns}}
				utils.WaitForResource(deploy1, e2eTimeout, e2eInterval)

				By("creating second node with same role")
				node2 := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "role-node-2", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "role-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node2)).To(Succeed())

				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "role-node-2", Namespace: ns}, node2)).To(Succeed())
					for _, c := range node2.Status.Conditions {
						if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
							return c.Reason
						}
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal("DuplicateRole"))

				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "role-node-2", Namespace: ns}, &appsv1.Deployment{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())

				node1 := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "role-node-1", Namespace: ns}}
				utils.WaitForConditions(node1, e2eTimeout, e2eInterval)

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "role-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				utils.MatchCRDResource(cluster, "role-cluster")
				utils.MatchCRDResource(node1, "role-node-1")
				Expect(k().Get(ctx, client.ObjectKey{Name: "role-node-2", Namespace: ns}, node2)).To(Succeed())
				utils.MatchCRDResource(node2, "role-node-2 degraded")
			})

			It("should allow same role in different clusters", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "role-cluster-a", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "role-cluster-b", "namespace": ns})

				nodeA := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "cross-a", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "shared-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "role-cluster-a"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, nodeA)).To(Succeed())

				nodeB := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "cross-b", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "shared-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "role-cluster-b"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, nodeB)).To(Succeed())

				deployA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cross-a", Namespace: ns}}
				utils.WaitForResource(deployA, e2eTimeout, e2eInterval)
				deployB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cross-b", Namespace: ns}}
				utils.WaitForResource(deployB, e2eTimeout, e2eInterval)

				utils.WaitForConditions(nodeA, e2eTimeout, e2eInterval)
				utils.WaitForConditions(nodeB, e2eTimeout, e2eInterval)

				Expect(k().Get(ctx, client.ObjectKey{Name: "cross-a", Namespace: ns}, nodeA)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "cross-b", Namespace: ns}, nodeB)).To(Succeed())
				utils.MatchCRDResource(nodeA, "cross-a")
				utils.MatchCRDResource(nodeB, "cross-b")
			})
		})

		Describe("Admin replicas forced to 1", Ordered, func() {
			const ns = "e2e-admin-rep"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should force admin Deployment to 1 replica", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "rep-cluster", "namespace": ns})

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "admin-5", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeAdmin, Role: "admin-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "rep-cluster"},
						Replicas:                 ptr.To(int32(5)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "admin-5", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "rep-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)

				Expect(k().Get(ctx, client.ObjectKey{Name: "rep-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "rep-cluster")
				Expect(k().Get(ctx, client.ObjectKey{Name: "admin-5", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "admin-5")
			})
		})

		Describe("Multiple runtime nodes", Label("slow"), Ordered, func() {
			const ns = "e2e-multi"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create separate Deployments for each node", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "multi-cluster", "namespace": ns})

				for i := 1; i <= 3; i++ {
					name := fmt.Sprintf("runtime-%d", i)
					node := &v1alpha1.IdentityServerNode{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
						Spec: v1alpha1.IdentityServerNodeSpec{
							Type: v1alpha1.NodeTypeRuntime, Role: fmt.Sprintf("role-%d", i),
							IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "multi-cluster"},
							Replicas:                 ptr.To(int32(1)),
						},
					}
					Expect(k().Create(ctx, node)).To(Succeed())
				}

				for i := 1; i <= 3; i++ {
					name := fmt.Sprintf("runtime-%d", i)
					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				}

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "multi-cluster", Namespace: ns}}
				Eventually(func(g Gomega) int32 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "multi-cluster", Namespace: ns}, cluster)).To(Succeed())
					return cluster.Status.NodeCount
				}, e2eTimeout, e2eInterval).Should(Equal(int32(3)))
				utils.MatchCRDResource(cluster, "multi-cluster")

				for i := 1; i <= 3; i++ {
					name := fmt.Sprintf("runtime-%d", i)
					node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
					utils.WaitForConditions(node, e2eTimeout, e2eInterval)
					Expect(k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, node)).To(Succeed())
					utils.MatchCRDResource(node, name)
				}
			})
		})
	})

	// =================================================================
	// credentials
	// =================================================================
	Context("credentials", func() {

		Describe("Admin credentials auto-generation", Ordered, func() {
			const ns = "e2e-creds"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create secret with random values when it does not exist", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
					map[string]interface{}{"name": "creds-cluster", "namespace": ns, "secretName": "auto-gen-secret"})

				secret := &corev1.Secret{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "auto-gen-secret", Namespace: ns}, secret)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
				Expect(secret.Data).To(HaveKey("CONFIG_ENCRYPTION_KEY"))
				Expect(secret.Data).To(HaveKey("KEYSTORE_PASSWORD"))
				Expect(len(secret.Data["ADMIN_PASSWORD"])).To(BeNumerically(">", 0))
				Expect(secret.OwnerReferences).To(BeEmpty())
				Expect(secret.Labels["app.kubernetes.io/managed-by"]).To(Equal("curity-operator"))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "creds-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "creds-cluster")
			})
		})

		Describe("Existing secret not overwritten", Ordered, func() {
			const ns = "e2e-creds-exist"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should not overwrite pre-existing secret", func() {
				ctx := context.Background()
				preExisting := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "pre-existing-secret", Namespace: ns},
					Data: map[string][]byte{
						"ADMIN_PASSWORD":        []byte("my-known-password"),
						"CONFIG_ENCRYPTION_KEY": []byte("my-known-key"),
						"KEYSTORE_PASSWORD":     []byte("my-known-ks"),
					},
				}
				Expect(k().Create(ctx, preExisting)).To(Succeed())

				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
					map[string]interface{}{"name": "creds-cluster-2", "namespace": ns, "secretName": "pre-existing-secret"})

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "creds-cluster-2", Namespace: ns}}
				Eventually(func(g Gomega) int64 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "creds-cluster-2", Namespace: ns}, cluster)).To(Succeed())
					return cluster.Status.ObservedGeneration
				}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

				secret := &corev1.Secret{}
				Expect(k().Get(ctx, client.ObjectKey{Name: "pre-existing-secret", Namespace: ns}, secret)).To(Succeed())
				Expect(string(secret.Data["ADMIN_PASSWORD"])).To(Equal("my-known-password"))

				Expect(k().Get(ctx, client.ObjectKey{Name: "creds-cluster-2", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "creds-cluster-2")
			})
		})

		Describe("Secret survives cluster deletion", Ordered, func() {
			const ns = "e2e-secret-survive"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should keep secret after cluster is deleted", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
					map[string]interface{}{"name": "surv-cluster", "namespace": ns, "secretName": "surv-secret"})

				secret := &corev1.Secret{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "surv-secret", Namespace: ns}, secret)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "surv-cluster", Namespace: ns}}
				Eventually(func(g Gomega) bool {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "surv-cluster", Namespace: ns}, cluster)).To(Succeed())
					for _, f := range cluster.Finalizers {
						if f == v1alpha1.ClusterFinalizer {
							return true
						}
					}
					return false
				}, e2eTimeout, e2eInterval).Should(BeTrue())

				utils.MatchCRDResource(cluster, "surv-cluster before-delete")

				Expect(k().Delete(ctx, cluster)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "surv-cluster", Namespace: ns}, &v1alpha1.IdentityServerCluster{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())

				Expect(k().Get(ctx, client.ObjectKey{Name: "surv-secret", Namespace: ns}, secret)).To(Succeed())
				Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
			})
		})

		Describe("Default admin credentials", Ordered, func() {
			const ns = "e2e-default-creds"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should auto-create credentials secret with conventional name", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "default-creds", "namespace": ns})

				secret := &corev1.Secret{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "default-creds-admin-creds", Namespace: ns}, secret)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
				Expect(secret.Data).To(HaveKey("CONFIG_ENCRYPTION_KEY"))
				Expect(secret.Data).To(HaveKey("KEYSTORE_PASSWORD"))
				Expect(len(secret.Data["ADMIN_PASSWORD"])).To(BeNumerically(">", 0))
				Expect(secret.Labels["app.kubernetes.io/managed-by"]).To(Equal("curity-operator"))
				Expect(secret.Labels["curity.io/cluster"]).To(Equal("default-creds"))
				Expect(secret.OwnerReferences).To(BeEmpty())
			})

			It("should inject defaulted credentials into admin node deployment", func() {
				ctx := context.Background()

				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin-ui.yaml", ns,
					map[string]interface{}{"name": "default-admin", "namespace": ns,
						"clusterName": "default-creds", "uiEnabled": true, "uiSecure": false})

				deploy := &appsv1.Deployment{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "default-admin", Namespace: ns}, deploy)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				envVars := deploy.Spec.Template.Spec.Containers[0].Env
				Expect(envVars).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "PASSWORD" && e.ValueFrom != nil &&
							e.ValueFrom.SecretKeyRef != nil &&
							e.ValueFrom.SecretKeyRef.Name == "default-creds-admin-creds"
					}),
				), "expected PASSWORD env var from default-creds-admin-creds")
				Expect(envVars).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "CONFIG_ENCRYPTION_KEY" && e.ValueFrom != nil &&
							e.ValueFrom.SecretKeyRef != nil &&
							e.ValueFrom.SecretKeyRef.Name == "default-creds-admin-creds"
					}),
				), "expected CONFIG_ENCRYPTION_KEY env var from default-creds-admin-creds")
				Expect(envVars).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "KEYSTORE_PASSWORD" && e.ValueFrom != nil &&
							e.ValueFrom.SecretKeyRef != nil &&
							e.ValueFrom.SecretKeyRef.Name == "default-creds-admin-creds"
					}),
				), "expected KEYSTORE_PASSWORD env var from default-creds-admin-creds")

				utils.MatchYAMLResource(deploy, "[deployment] default-admin")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "default-creds", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "default-creds")
			})
		})
	})

	// =================================================================
	// configuration
	// =================================================================
	Context("configuration", func() {

		Describe("Logging sidecars", Ordered, func() {
			const ns = "e2e-logging"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create sidecar containers when stdout logging enabled", func() {
				ctx := context.Background()

				cluster := &v1alpha1.IdentityServerCluster{
					ObjectMeta: metav1.ObjectMeta{Name: "log-cluster", Namespace: ns},
					Spec: v1alpha1.IdentityServerClusterSpec{
						Version: "11.0",
						Logging: &v1alpha1.LoggingSpec{
							Level:  "DEBUG",
							Stdout: true,
							Logs:   []string{"audit", "request"},
						},
					},
				}
				Expect(k().Create(ctx, cluster)).To(Succeed())

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "log-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "log-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "log-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "log-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				containers := deploy.Spec.Template.Spec.Containers
				Expect(containers).To(HaveLen(3), "expected 1 main + 2 sidecar containers")

				sidecarNames := make([]string, 0)
				for _, c := range containers[1:] {
					sidecarNames = append(sidecarNames, c.Name)
					Expect(c.Image).To(Equal("busybox:latest"))
					Expect(c.Command[0]).To(Equal("tail"))
				}
				Expect(sidecarNames).To(ConsistOf("audit", "request"))

				Expect(deploy.Spec.Template.Spec.Volumes).To(ContainElement(
					Satisfy(func(v corev1.Volume) bool {
						return v.Name == "log-volume" && v.EmptyDir != nil
					}),
				), "log-volume EmptyDir should exist")

				Expect(containers[0].Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "LOGGING_LEVEL" && e.Value == "DEBUG"
					}),
				), "LOGGING_LEVEL should be DEBUG")

				Expect(k().Get(ctx, client.ObjectKey{Name: "log-cluster", Namespace: ns}, cluster)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "log-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(cluster, "log-cluster")
				utils.MatchCRDResource(node, "log-node")
			})
		})

		Describe("Logging level OFF", Ordered, func() {
			const ns = "e2e-logging-off"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set LOGGING_LEVEL to OFF", func() {
				ctx := context.Background()

				cluster := &v1alpha1.IdentityServerCluster{
					ObjectMeta: metav1.ObjectMeta{Name: "off-cluster", Namespace: ns},
					Spec: v1alpha1.IdentityServerClusterSpec{
						Version: "11.0",
						Logging: &v1alpha1.LoggingSpec{Level: "OFF"},
					},
				}
				Expect(k().Create(ctx, cluster)).To(Succeed())

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "off-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "off-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "off-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "off-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "LOGGING_LEVEL" && e.Value == "OFF"
					}),
				), "LOGGING_LEVEL should be OFF")

				Expect(k().Get(ctx, client.ObjectKey{Name: "off-cluster", Namespace: ns}, cluster)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "off-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(cluster, "off-cluster")
				utils.MatchCRDResource(node, "off-node")
			})

			It("should suppress sidecars when level is OFF", func() {
				ctx := context.Background()

				cluster := &v1alpha1.IdentityServerCluster{
					ObjectMeta: metav1.ObjectMeta{Name: "off-sidecar-cluster", Namespace: ns},
					Spec: v1alpha1.IdentityServerClusterSpec{
						Version: "11.0",
						Logging: &v1alpha1.LoggingSpec{
							Level:  "OFF",
							Stdout: true,
							Logs:   []string{"audit"},
						},
					},
				}
				Expect(k().Create(ctx, cluster)).To(Succeed())

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "off-sidecar-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "off-sidecar-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "off-sidecar-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "off-sidecar-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1),
					"expected 1 container (no sidecars when level is OFF)")

				for _, v := range deploy.Spec.Template.Spec.Volumes {
					Expect(v.Name).NotTo(Equal("log-volume"),
						"log-volume should not exist when level is OFF")
				}

				Expect(k().Get(ctx, client.ObjectKey{Name: "off-sidecar-cluster", Namespace: ns}, cluster)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "off-sidecar-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(cluster, "off-sidecar-cluster")
				utils.MatchCRDResource(node, "off-sidecar-node")
			})

			It("should restore sidecars when level changes from OFF to DEBUG", func() {
				ctx := context.Background()

				By("updating cluster logging level from OFF to DEBUG")
				cluster := &v1alpha1.IdentityServerCluster{}
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "off-sidecar-cluster", Namespace: ns}, cluster); err != nil {
						return err
					}
					cluster.Spec.Logging.Level = "DEBUG"
					return k().Update(ctx, cluster)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("waiting for sidecars to appear on Deployment")
				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "off-sidecar-node", Namespace: ns}}
				Eventually(func(g Gomega) int {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "off-sidecar-node", Namespace: ns}, deploy)).To(Succeed())
					return len(deploy.Spec.Template.Spec.Containers)
				}, e2eTimeout, e2eInterval).Should(Equal(2), "expected 1 main + 1 sidecar after level change to DEBUG")

				Expect(deploy.Spec.Template.Spec.Containers).To(ContainElement(
					Satisfy(func(c corev1.Container) bool {
						return c.Name == "audit"
					}),
				), "expected audit sidecar container")

				Expect(deploy.Spec.Template.Spec.Volumes).To(ContainElement(
					Satisfy(func(v corev1.Volume) bool {
						return v.Name == "log-volume" && v.EmptyDir != nil
					}),
				), "log-volume should exist after level change to DEBUG")

				Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "LOGGING_LEVEL" && e.Value == "DEBUG"
					}),
				), "LOGGING_LEVEL should be DEBUG after update")
			})
		})

		Describe("Cluster config generation", Label("slow"), Ordered, func() {
			const ns = "e2e-clusterconfig"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should mount cluster config volume on all node Deployments", func() {
				ctx := context.Background()

				By("pre-creating populated cluster config Secret")
				configSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cc-cluster-cluster-config",
						Namespace: ns,
						Labels: map[string]string{
							"curity.io/cluster":   "cc-cluster",
							"curity.io/component": "cluster-config",
						},
						Annotations: map[string]string{
							"curity.io/admin-node":               "cc-admin",
							"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
						},
					},
					Data: map[string][]byte{"cluster.xml": []byte("<config>test-cluster-xml</config>")},
				}
				Expect(k().Create(ctx, configSecret)).To(Succeed())

				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "cc-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
					map[string]interface{}{"name": "cc-admin", "namespace": ns, "clusterName": "cc-cluster"})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "cc-runtime", "namespace": ns, "clusterName": "cc-cluster"})

				By("verifying cluster-config volume on both Deployments")
				adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cc-admin", Namespace: ns}}
				utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)
				runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cc-runtime", Namespace: ns}}
				utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)

				for _, d := range []*appsv1.Deployment{adminDeploy, runtimeDeploy} {
					Expect(d.Spec.Template.Spec.Volumes).To(ContainElement(
						Satisfy(func(v corev1.Volume) bool {
							return v.Name == "cluster-config" && v.Secret != nil && v.Secret.SecretName == "cc-cluster-cluster-config"
						}),
					), fmt.Sprintf("%s should have cluster-config volume", d.Name))
					Expect(d.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(
						Satisfy(func(m corev1.VolumeMount) bool {
							return m.Name == "cluster-config" && m.MountPath == "/opt/idsvr/etc/init/cluster.xml" && m.SubPath == "cluster.xml" && m.ReadOnly
						}),
					), fmt.Sprintf("%s should mount cluster.xml", d.Name))
				}

				utils.MatchYAMLResource(adminDeploy, "[deployment] cc-admin")
				utils.MatchYAMLResource(runtimeDeploy, "[deployment] cc-runtime")
			})

			It("should include cluster-config-hash annotation on pods", func() {
				ctx := context.Background()

				expectedHash := sha256.Sum256([]byte("<config>test-cluster-xml</config>"))
				expectedHashStr := hex.EncodeToString(expectedHash[:])

				adminDeploy := &appsv1.Deployment{}
				Expect(k().Get(ctx, client.ObjectKey{Name: "cc-admin", Namespace: ns}, adminDeploy)).To(Succeed())
				hash, ok := adminDeploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]
				Expect(ok).To(BeTrue(), "admin Deployment should have cluster-config-hash annotation")
				Expect(hash).To(Equal(expectedHashStr))

				runtimeDeploy := &appsv1.Deployment{}
				Expect(k().Get(ctx, client.ObjectKey{Name: "cc-runtime", Namespace: ns}, runtimeDeploy)).To(Succeed())
				hash, ok = runtimeDeploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]
				Expect(ok).To(BeTrue(), "runtime Deployment should have cluster-config-hash annotation")
				Expect(hash).To(Equal(expectedHashStr))
			})

			It("should set ClusterConfigReady condition on cluster", func() {
				ctx := context.Background()

				cluster := &v1alpha1.IdentityServerCluster{}
				Eventually(func(g Gomega) bool {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "cc-cluster", Namespace: ns}, cluster)).To(Succeed())
					for _, c := range cluster.Status.Conditions {
						if c.Type == "ClusterConfigReady" && c.Status == metav1.ConditionTrue {
							return true
						}
					}
					return false
				}, e2eTimeout, e2eInterval).Should(BeTrue())

				Expect(cluster.Status.ClusterConfigSecretName).To(Equal("cc-cluster-cluster-config"))
				utils.MatchCRDResource(cluster, "cc-cluster with-config")
			})
		})

		Describe("Cluster config Job creation", Ordered, func() {
			const ns = "e2e-ccjob"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create placeholder Secret and genclust Job when admin exists", func() {
				ctx := context.Background()

				By("creating cluster and admin node")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "job-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
					map[string]interface{}{"name": "job-admin", "namespace": ns, "clusterName": "job-cluster"})

				By("verifying placeholder Secret")
				secret := &corev1.Secret{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "job-cluster-cluster-config", Namespace: ns}, secret)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				Expect(string(secret.Data["cluster.xml"])).To(Equal("placeholder"))
				Expect(secret.Annotations).To(HaveKeyWithValue("argocd.argoproj.io/compare-options", "IgnoreExtraneous"))
				Expect(secret.Annotations).To(HaveKey("curity.io/admin-node"))
				Expect(secret.Labels).To(HaveKeyWithValue("curity.io/cluster", "job-cluster"))
				Expect(secret.Labels).To(HaveKeyWithValue("curity.io/component", "cluster-config"))
				Expect(secret.OwnerReferences).To(BeEmpty())

				utils.MatchYAMLResource(secret, "[secret] job-cluster-cluster-config")

				By("verifying genclust Job")
				job := &batchv1.Job{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: "job-cluster-cluster-config-job", Namespace: ns}, job)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				Expect(job.Labels["curity.io/cluster"]).To(Equal("job-cluster"))
				Expect(job.Labels["curity.io/component"]).To(Equal("cluster-config"))
				Expect(*job.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())

				container := job.Spec.Template.Spec.Containers[0]
				Expect(container.Name).To(Equal("genclust"))
				Expect(container.Image).To(ContainSubstring("curity"))
				Expect(container.Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool {
						return e.Name == "CONFIG_SERVICE_HOST" && e.Value == "job-admin"
					}),
				), "Job should have CONFIG_SERVICE_HOST=job-admin")

				utils.MatchResource(job, "job", "job-cluster-cluster-config-job")

				By("snapshotting cluster status")
				cluster := &v1alpha1.IdentityServerCluster{}
				Eventually(func(g Gomega) bool {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "job-cluster", Namespace: ns}, cluster)).To(Succeed())
					for _, c := range cluster.Status.Conditions {
						if c.Type == "ClusterConfigReady" {
							return true
						}
					}
					return false
				}, e2eTimeout, e2eInterval).Should(BeTrue())

				utils.MatchCRDResource(cluster, "job-cluster with-job")
			})
		})
	})

	// =================================================================
	// admin UI and custom ports
	// =================================================================
	Context("admin UI and custom ports", func() {

		Describe("Admin UI port configuration", Ordered, func() {
			const ns = "e2e-admin-ui"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should expose admin-ui port and set ADMIN_UI_HTTP_MODE", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "ui-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin-ui.yaml", ns,
					map[string]interface{}{"name": "ui-admin", "namespace": ns, "clusterName": "ui-cluster",
						"uiEnabled": true, "uiSecure": false})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ui-admin", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				Expect(deploy.Spec.Template.Spec.Containers[0].Ports).To(ContainElement(
					Satisfy(func(p corev1.ContainerPort) bool { return p.Name == "admin-ui" && p.ContainerPort == 6749 }),
				), "admin-ui port 6749 should exist")

				svc := &corev1.Service{}
				Expect(k().Get(ctx, client.ObjectKey{Name: "ui-admin", Namespace: ns}, svc)).To(Succeed())
				Expect(svc.Spec.Ports).To(ContainElement(
					Satisfy(func(p corev1.ServicePort) bool { return p.Name == "admin-ui" && p.Port == 6749 }),
				), "admin-ui port on Service")

				Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool { return e.Name == "ADMIN_UI_HTTP_MODE" && e.Value == "true" }),
				), "ADMIN_UI_HTTP_MODE should be true")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "ui-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "ui-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "ui-admin", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(node, "ui-admin")
			})
		})

		Describe("Custom service port and env vars", Ordered, func() {
			const ns = "e2e-custom"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should use custom port and pass env vars", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "custom-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime-custom.yaml", ns,
					map[string]interface{}{
						"name": "custom-node", "namespace": ns, "clusterName": "custom-cluster",
						"role": "custom-role", "replicas": 2, "serviceType": "ClusterIP",
						"servicePort": 9443, "customEnvValue": "hello-e2e",
					})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "custom-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))

				Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool { return e.Name == "CUSTOM_ENV" && e.Value == "hello-e2e" }),
				))

				svc := &corev1.Service{}
				Expect(k().Get(ctx, client.ObjectKey{Name: "custom-node", Namespace: ns}, svc)).To(Succeed())
				Expect(svc.Spec.Ports).To(ContainElement(
					Satisfy(func(p corev1.ServicePort) bool { return p.Name == "http" && p.Port == 9443 }),
				), "Service should have http port 9443")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "custom-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "custom-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "custom-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(node, "custom-node")
			})
		})

		Describe("Admin container args", Ordered, func() {
			const ns = "e2e-admin-args"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should have correct admin args format", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "args-cluster", "namespace": ns})

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "args-admin", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeAdmin, Role: "my-admin-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "args-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "args-admin", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				args := deploy.Spec.Template.Spec.Containers[0].Args
				Expect(args).To(Equal([]string{"/opt/idsvr/bin/idsvr", "-s", "my-admin-role", "-N", "args-admin", "--admin"}))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "args-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "args-cluster")
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(node, "args-admin")
			})
		})

		Describe("Runtime container args", Ordered, func() {
			const ns = "e2e-rt-args"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should have correct runtime args format", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "rtargs-cluster", "namespace": ns})

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "args-runtime", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "my-runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "rtargs-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "args-runtime", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				args := deploy.Spec.Template.Spec.Containers[0].Args
				Expect(args).To(Equal([]string{"/opt/idsvr/bin/idsvr", "-s", "my-runtime-role", "--no-admin"}))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "rtargs-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "rtargs-cluster")
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(node, "args-runtime")
			})
		})

		Describe("Probe defaults", Ordered, func() {
			const ns = "e2e-probes"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set correct default probe values", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "probe-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "probe-node", "namespace": ns, "clusterName": "probe-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "probe-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				lp := deploy.Spec.Template.Spec.Containers[0].LivenessProbe
				Expect(lp.HTTPGet.Path).To(Equal("/"))
				Expect(lp.HTTPGet.Port.IntValue()).To(Equal(4465))
				Expect(lp.InitialDelaySeconds).To(Equal(int32(30)))
				Expect(lp.PeriodSeconds).To(Equal(int32(10)))
				Expect(lp.TimeoutSeconds).To(Equal(int32(1)))
				Expect(lp.FailureThreshold).To(Equal(int32(3)))

				rp := deploy.Spec.Template.Spec.Containers[0].ReadinessProbe
				Expect(rp.HTTPGet.Path).To(Equal("/"))
				Expect(rp.SuccessThreshold).To(Equal(int32(3)))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "probe-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "probe-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "probe-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "probe-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "probe-node")
			})
		})
	})

	// =================================================================
	// image and version
	// =================================================================
	Context("image and version", func() {

		Describe("Image override", Ordered, func() {
			const ns = "e2e-image"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should use custom image from cluster spec", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-image.yaml", ns,
					map[string]interface{}{"name": "img-cluster", "namespace": ns, "image": "custom-registry/idsvr:custom"})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "img-node", "namespace": ns, "clusterName": "img-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "img-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("custom-registry/idsvr:custom"))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "img-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "img-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "img-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "img-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "img-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "img-node")
			})
		})

		Describe("Cluster version change updates nodes", Ordered, func() {
			const ns = "e2e-version"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should update Deployment image when cluster version changes", func() {
				ctx := context.Background()

				By("creating cluster and node with initial version")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "ver-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "ver-node", "namespace": ns, "clusterName": "ver-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ver-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("curity.azurecr.io/curity/idsvr:11.0"))

				By("updating cluster version")
				cluster := &v1alpha1.IdentityServerCluster{}
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "ver-cluster", Namespace: ns}, cluster); err != nil {
						return err
					}
					cluster.Spec.Version = "12.0"
					return k().Update(ctx, cluster)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying Deployment image updated")
				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "ver-node", Namespace: ns}, deploy)).To(Succeed())
					if len(deploy.Spec.Template.Spec.Containers) > 0 {
						return deploy.Spec.Template.Spec.Containers[0].Image
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal("curity.azurecr.io/curity/idsvr:12.0"))

				node := &v1alpha1.IdentityServerNode{}
				Expect(k().Get(ctx, client.ObjectKey{Name: "ver-node", Namespace: ns}, node)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "ver-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "ver-cluster after-version-change")
				utils.MatchCRDResource(node, "ver-node after-version-change")
			})
		})
	})

	// =================================================================
	// labels and security
	// =================================================================
	Context("labels and security", func() {

		Describe("Labels and annotations merge", Ordered, func() {
			const ns = "e2e-labels"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should merge cluster and node labels/annotations", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-labels.yaml", ns,
					map[string]interface{}{"name": "lbl-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime-labels.yaml", ns,
					map[string]interface{}{"name": "lbl-node", "namespace": ns, "clusterName": "lbl-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "lbl-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				podLabels := deploy.Spec.Template.Labels
				Expect(podLabels["team"]).To(Equal("identity"), "node label wins on conflict")

				podAnnotations := deploy.Spec.Template.Annotations
				Expect(podAnnotations["prometheus.io/scrape"]).To(Equal("true"), "cluster annotation preserved")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "lbl-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "lbl-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "lbl-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "lbl-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "lbl-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "lbl-node")
			})
		})

		Describe("Security context", Ordered, func() {
			const ns = "e2e-security"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should set pod security context matching Helm chart", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "sec-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "sec-node", "namespace": ns, "clusterName": "sec-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "sec-node", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				podSC := deploy.Spec.Template.Spec.SecurityContext
				Expect(*podSC.RunAsUser).To(Equal(int64(10001)))
				Expect(*podSC.RunAsGroup).To(Equal(int64(10000)))
				Expect(*podSC.FSGroup).To(Equal(int64(10000)))

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "sec-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "sec-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "sec-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "sec-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "sec-node", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "sec-node")
			})
		})
	})

	// =================================================================
	// scheduling
	// =================================================================
	Context("scheduling", func() {

		Describe("Scheduling from cluster to Deployment", Ordered, func() {
			const ns = "e2e-scheduling"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should apply cluster nodeSelector to Deployment pods", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-scheduling.yaml", ns,
					map[string]interface{}{"name": "sched-cluster", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "sched-runtime", "namespace": ns, "clusterName": "sched-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "sched-runtime", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("node-pool", "curity"))
				utils.MatchYAMLResource(deploy, "[deployment] sched-runtime")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "sched-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				utils.MatchCRDResource(cluster, "sched-cluster")

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "sched-runtime", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "sched-runtime", Namespace: ns}, node)).To(Succeed())
				utils.MatchCRDResource(node, "sched-runtime")
			})
		})

		Describe("Node scheduling overrides cluster", Ordered, func() {
			const ns = "e2e-sched-override"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should merge node nodeSelector over cluster defaults", func() {
				ctx := context.Background()
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-scheduling.yaml", ns,
					map[string]interface{}{"name": "ovr-cluster", "namespace": ns})

				nodeYAML := fmt.Sprintf(`apiVersion: curity.io/v1alpha1
kind: IdentityServerNode
metadata:
  name: ovr-runtime
  namespace: %s
spec:
  type: runtime
  role: override-role
  identityServerClusterRef:
    name: ovr-cluster
  replicas: 1
  nodeSelector:
    node-pool: gpu
    disk: ssd`, ns)
				utils.ApplyRawYAML(nodeYAML, ns)

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ovr-runtime", Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("node-pool", "gpu"))
				Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("disk", "ssd"))
				utils.MatchYAMLResource(deploy, "[deployment] ovr-runtime")

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "ovr-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "ovr-cluster", Namespace: ns}, cluster)).To(Succeed())
				utils.MatchCRDResource(cluster, "ovr-cluster")
			})
		})
	})

})
