package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// ownedName mirrors internal/controller.ownedResourceName — the operator names
// node-owned Deployments/Services/HPAs/PDBs as {clusterName}-{nodeName}.
func ownedName(clusterName, nodeName string) string {
	return clusterName + "-" + nodeName
}

// e2eHasCfgVolume checks if a Deployment has a volume with the given prefix.
func e2eHasCfgVolume(deploy *appsv1.Deployment, volumeName string) bool {
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == volumeName {
			return true
		}
	}
	return false
}

// e2eCountCfgVolumes counts volumes with "cfg-" prefix.
func e2eCountCfgVolumes(deploy *appsv1.Deployment) int {
	count := 0
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if strings.HasPrefix(v.Name, "cfg-") {
			count++
		}
	}
	return count
}

// e2eHasVolumeMount checks if the first container has a mount with the given name and path.
func e2eHasVolumeMount(deploy *appsv1.Deployment, name, mountPath string) bool {
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return false
	}
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == name && m.MountPath == mountPath {
			return true
		}
	}
	return false
}

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
				utils.SimulateClusterConfigReady(ns, "deploy-cluster", e2eTimeout, e2eInterval)

				adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("deploy-cluster", "deploy-admin"), Namespace: ns}}
				utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(adminDeploy, "[admin-deployment] deploy-admin")

				adminSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ownedName("deploy-cluster", "deploy-admin"), Namespace: ns}}
				utils.WaitForResource(adminSvc, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(adminSvc, "[admin-service] deploy-admin")

				By("deploying runtime node and verifying resources")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "deploy-runtime", "namespace": ns, "clusterName": "deploy-cluster"})

				runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("deploy-cluster", "deploy-runtime"), Namespace: ns}}
				utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)
				utils.MatchYAMLResource(runtimeDeploy, "[runtime-deployment] deploy-runtime")

				runtimeSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ownedName("deploy-cluster", "deploy-runtime"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("del-cluster", "del-node"), Namespace: ns}}
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
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName("del-cluster", "del-node"), Namespace: ns}, &appsv1.Deployment{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName("del-cluster", "del-node"), Namespace: ns}, &corev1.Service{}))
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("fin-cluster", "fin-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("upd-cluster", "upd-node"), Namespace: ns}}
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
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("upd-cluster", "upd-node"), Namespace: ns}, deploy)).To(Succeed())
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("idem-cluster", "idem-node"), Namespace: ns}}
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
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("idem-cluster", "idem-node"), Namespace: ns}, deploy)).To(Succeed())
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("status-cluster", "status-node"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				node := &v1alpha1.IdentityServerNode{ObjectMeta: metav1.ObjectMeta{Name: "status-node", Namespace: ns}}
				utils.WaitForConditions(node, e2eTimeout, e2eInterval)
				Expect(node.Status.ObservedGeneration).To(BeNumerically(">", 0))
				Expect(node.Status.DeploymentName).To(Equal(ownedName("status-cluster", "status-node")))
				Expect(node.Status.ServiceName).To(Equal(ownedName("status-cluster", "status-node")))
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
				utils.SimulateClusterConfigReady(ns, "dup-cluster", e2eTimeout, e2eInterval)

				deploy1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("dup-cluster", "admin-1"), Namespace: ns}}
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
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName("dup-cluster", "admin-2"), Namespace: ns}, &appsv1.Deployment{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())

				// admin-1's post-duplicate state is non-deterministic (flips between
				// Ready and Degraded=DuplicateAdmin across runs); separate story
				// created to fix this.

				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "dup-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				utils.MatchCRDResource(cluster, "dup-cluster")
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
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName("does-not-exist", "orphan-node"), Namespace: ns}, &appsv1.Deployment{}))
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
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName("recovery-cluster", "recovery-node"), Namespace: ns}, &appsv1.Deployment{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())

				By("creating missing cluster to trigger recovery")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "recovery-cluster", "namespace": ns})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("recovery-cluster", "recovery-node"), Namespace: ns}}
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

				deploy1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("role-cluster", "role-node-1"), Namespace: ns}}
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
					return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: ownedName("role-cluster", "role-node-2"), Namespace: ns}, &appsv1.Deployment{}))
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

				deployA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("role-cluster-a", "cross-a"), Namespace: ns}}
				utils.WaitForResource(deployA, e2eTimeout, e2eInterval)
				deployB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("role-cluster-b", "cross-b"), Namespace: ns}}
				utils.WaitForResource(deployB, e2eTimeout, e2eInterval)

				utils.WaitForConditions(nodeA, e2eTimeout, e2eInterval)
				utils.WaitForConditions(nodeB, e2eTimeout, e2eInterval)

				Expect(k().Get(ctx, client.ObjectKey{Name: "cross-a", Namespace: ns}, nodeA)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "cross-b", Namespace: ns}, nodeB)).To(Succeed())
				utils.MatchCRDResource(nodeA, "cross-a")
				utils.MatchCRDResource(nodeB, "cross-b")
			})
		})

		Describe("Owned resource name collision", Ordered, func() {
			const ns = "e2e-name-collision"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should block newer node when ownedResourceName collides across clusters", func() {
				// ownedResourceName = clusterName + "-" + nodeName. Separator
				// is ambiguous: ("foo-bar","baz") and ("foo","bar-baz") both
				// yield "foo-bar-baz". Without the guard, two nodes would
				// fight over one Deployment/Service via CreateOrUpdate.
				ctx := context.Background()

				By("creating winner cluster and node")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "foo-bar", "namespace": ns})
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "foo", "namespace": ns})

				winner := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "baz", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "winner-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "foo-bar"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, winner)).To(Succeed())

				winnerDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("foo-bar", "baz"), Namespace: ns}}
				utils.WaitForResource(winnerDeploy, e2eTimeout, e2eInterval)

				// metav1.Time has 1-second resolution; sleep to guarantee
				// winner and loser get distinct CreationTimestamps so the
				// older-wins rule fires deterministically (otherwise the
				// reconciler falls back to UID tie-break).
				time.Sleep(1100 * time.Millisecond)

				By("creating loser node whose ownedResourceName collides")
				loser := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "bar-baz", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "loser-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "foo"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, loser)).To(Succeed())

				Eventually(func(g Gomega) string {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: "bar-baz", Namespace: ns}, loser)).To(Succeed())
					for _, c := range loser.Status.Conditions {
						if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
							return c.Reason
						}
					}
					return ""
				}, e2eTimeout, e2eInterval).Should(Equal(v1alpha1.ReasonOwnedNameCollision))

				By("verifying winner's Deployment is still owned by the winner")
				Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("foo-bar", "baz"), Namespace: ns}, winnerDeploy)).To(Succeed())
				controller := metav1.GetControllerOf(winnerDeploy)
				Expect(controller).NotTo(BeNil())
				Expect(controller.Name).To(Equal("baz"))

				By("snapshotting loser status")
				Expect(k().Get(ctx, client.ObjectKey{Name: "baz", Namespace: ns}, winner)).To(Succeed())
				utils.WaitForConditions(winner, e2eTimeout, e2eInterval)
				Expect(k().Get(ctx, client.ObjectKey{Name: "baz", Namespace: ns}, winner)).To(Succeed())
				Expect(k().Get(ctx, client.ObjectKey{Name: "bar-baz", Namespace: ns}, loser)).To(Succeed())
				utils.MatchCRDResource(winner, "collision-winner")
				utils.MatchCRDResource(loser, "collision-loser")
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
				utils.SimulateClusterConfigReady(ns, "rep-cluster", e2eTimeout, e2eInterval)

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("rep-cluster", "admin-5"), Namespace: ns}}
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
					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("multi-cluster", name), Namespace: ns}}
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
				utils.SimulateClusterConfigReady(ns, "default-creds", e2eTimeout, e2eInterval)

				deploy := &appsv1.Deployment{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: ownedName("default-creds", "default-admin"), Namespace: ns}, deploy)
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-cluster", "log-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("off-cluster", "off-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("off-sidecar-cluster", "off-sidecar-node"), Namespace: ns}}
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
				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("off-sidecar-cluster", "off-sidecar-node"), Namespace: ns}}
				Eventually(func(g Gomega) int {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("off-sidecar-cluster", "off-sidecar-node"), Namespace: ns}, deploy)).To(Succeed())
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
				adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cc-cluster", "cc-admin"), Namespace: ns}}
				utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)
				runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cc-cluster", "cc-runtime"), Namespace: ns}}
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
				Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cc-cluster", "cc-admin"), Namespace: ns}, adminDeploy)).To(Succeed())
				hash, ok := adminDeploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]
				Expect(ok).To(BeTrue(), "admin Deployment should have cluster-config-hash annotation")
				Expect(hash).To(Equal(expectedHashStr))

				runtimeDeploy := &appsv1.Deployment{}
				Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cc-cluster", "cc-runtime"), Namespace: ns}, runtimeDeploy)).To(Succeed())
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
						return e.Name == "CONFIG_SERVICE_HOST" && e.Value == "job-cluster-job-admin"
					}),
				), "Job should have CONFIG_SERVICE_HOST=job-cluster-job-admin (the admin Service name)")

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

		Describe("Label-based config discovery", func() {

			Describe("Admin routing — config mounts only on admin", Ordered, func() {
				const ns = "e2e-cfg-admin-routing"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should mount base ConfigMap only on admin when admin exists", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-ar-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "cfg-ar-admin", "namespace": ns, "clusterName": "cfg-ar-cluster"})
					utils.SimulateClusterConfigReady(ns, "cfg-ar-cluster", e2eTimeout, e2eInterval)
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "cfg-ar-runtime", "namespace": ns, "clusterName": "cfg-ar-cluster"})

					adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-ar-cluster", "cfg-ar-admin"), Namespace: ns}}
					utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)
					runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-ar-cluster", "cfg-ar-runtime"), Namespace: ns}}
					utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)

					By("creating labeled ConfigMap")
					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "routing-config", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
						},
						Data: map[string]string{"settings.xml": "<settings/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					By("verifying admin gets config volume")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-ar-cluster", "cfg-ar-admin"), Namespace: ns}, adminDeploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(adminDeploy, "cfg-cm-routing-config")).To(BeTrue())
						g.Expect(e2eHasVolumeMount(adminDeploy, "cfg-cm-routing-config", "/opt/idsvr/etc/init/cm_routing-config_settings.xml")).To(BeTrue())
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("verifying runtime does NOT get config volume")
					Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-ar-cluster", "cfg-ar-runtime"), Namespace: ns}, runtimeDeploy)).To(Succeed())
					Expect(e2eCountCfgVolumes(runtimeDeploy)).To(Equal(0), "runtime should not have config volumes when admin exists")

					By("snapshotting")
					utils.MatchYAMLResource(adminDeploy, "[deployment] cfg-ar-admin")
					utils.MatchYAMLResource(runtimeDeploy, "[deployment] cfg-ar-runtime")

					adminNode := &v1alpha1.IdentityServerNode{}
					Expect(k().Get(ctx, client.ObjectKey{Name: "cfg-ar-admin", Namespace: ns}, adminNode)).To(Succeed())
					utils.MatchCRDResource(adminNode, "cfg-ar-admin")

					runtimeNode := &v1alpha1.IdentityServerNode{}
					Expect(k().Get(ctx, client.ObjectKey{Name: "cfg-ar-runtime", Namespace: ns}, runtimeNode)).To(Succeed())
					utils.MatchCRDResource(runtimeNode, "cfg-ar-runtime")
				})
			})

			Describe("No admin — config mounts on runtime", Ordered, func() {
				const ns = "e2e-cfg-no-admin"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should mount base ConfigMap on runtime when no admin exists", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-na-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "cfg-na-runtime", "namespace": ns, "clusterName": "cfg-na-cluster"})

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-na-cluster", "cfg-na-runtime"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "runtime-cfg", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
						},
						Data: map[string]string{"app.xml": "<app/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-na-cluster", "cfg-na-runtime"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-runtime-cfg")).To(BeTrue())
						g.Expect(e2eHasVolumeMount(deploy, "cfg-cm-runtime-cfg", "/opt/idsvr/etc/init/cm_runtime-cfg_app.xml")).To(BeTrue())
					}, e2eTimeout, e2eInterval).Should(Succeed())

					utils.MatchYAMLResource(deploy, "[deployment] cfg-na-runtime")
				})
			})

			Describe("License Secret mount path", Ordered, func() {
				const ns = "e2e-cfg-license"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should mount license Secret at license path", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-lic-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "cfg-lic-admin", "namespace": ns, "clusterName": "cfg-lic-cluster"})
					utils.SimulateClusterConfigReady(ns, "cfg-lic-cluster", e2eTimeout, e2eInterval)

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-lic-cluster", "cfg-lic-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name: "my-license", Namespace: ns,
							Labels:      map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{"curity.io/config-type": "license"},
						},
						Data: map[string][]byte{"license.json": []byte(`{"key":"val"}`)},
					}
					Expect(k().Create(ctx, secret)).To(Succeed())

					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-lic-cluster", "cfg-lic-admin"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-secret-my-license")).To(BeTrue())
						g.Expect(e2eHasVolumeMount(deploy, "cfg-secret-my-license", "/opt/idsvr/etc/init/license/secret_my-license_license.json")).To(BeTrue())
					}, e2eTimeout, e2eInterval).Should(Succeed())

					utils.MatchYAMLResource(deploy, "[deployment] cfg-lic-admin")
				})
			})

			Describe("Multiple discovered configs", Ordered, func() {
				const ns = "e2e-cfg-multi"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should mount multiple discovered configs", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-multi-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "cfg-multi-admin", "namespace": ns, "clusterName": "cfg-multi-cluster"})
					utils.SimulateClusterConfigReady(ns, "cfg-multi-cluster", e2eTimeout, e2eInterval)

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-multi-cluster", "cfg-multi-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					By("creating 2 ConfigMaps + 1 Secret")
					cm1 := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "base-config-a", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string]string{"a.xml": "<a/>"},
					}
					cm2 := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "base-config-b", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string]string{"b.xml": "<b/>"},
					}
					sec := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "secret-config", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string][]byte{"s.xml": []byte("<s/>")},
					}
					Expect(k().Create(ctx, cm1)).To(Succeed())
					Expect(k().Create(ctx, cm2)).To(Succeed())
					Expect(k().Create(ctx, sec)).To(Succeed())

					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-multi-cluster", "cfg-multi-admin"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eCountCfgVolumes(deploy)).To(Equal(3))
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-base-config-a")).To(BeTrue())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-base-config-b")).To(BeTrue())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-secret-secret-config")).To(BeTrue())
					}, e2eTimeout, e2eInterval).Should(Succeed())

					utils.MatchYAMLResource(deploy, "[deployment] cfg-multi-admin")
				})
			})

			Describe("Rolling restart on config change", Ordered, func() {
				const ns = "e2e-cfg-rolling"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should trigger rolling restart on config data change", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-roll-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "cfg-roll-runtime", "namespace": ns, "clusterName": "cfg-roll-cluster"})

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-roll-cluster", "cfg-roll-runtime"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "rolling-config", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string]string{"init.xml": "<v1/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					By("recording initial config-hash annotation")
					var initialHash string
					Eventually(func() string {
						Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-roll-cluster", "cfg-roll-runtime"), Namespace: ns}, deploy)).To(Succeed())
						initialHash = deploy.Spec.Template.Annotations["curity.io/managed-configs-hash"]
						return initialHash
					}, e2eTimeout, e2eInterval).ShouldNot(BeEmpty())

					By("updating ConfigMap data")
					Expect(k().Get(ctx, client.ObjectKey{Name: "rolling-config", Namespace: ns}, cm)).To(Succeed())
					cm.Data["init.xml"] = "<v2/>"
					Expect(k().Update(ctx, cm)).To(Succeed())

					By("verifying config-hash annotation changed")
					Eventually(func() string {
						Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-roll-cluster", "cfg-roll-runtime"), Namespace: ns}, deploy)).To(Succeed())
						return deploy.Spec.Template.Annotations["curity.io/managed-configs-hash"]
					}, e2eTimeout, e2eInterval).ShouldNot(Equal(initialHash))
				})
			})

			Describe("Unlabeled ConfigMap not mounted", Ordered, func() {
				const ns = "e2e-cfg-unlabeled"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should not mount unlabeled ConfigMap", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-ul-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "cfg-ul-runtime", "namespace": ns, "clusterName": "cfg-ul-cluster"})

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-ul-cluster", "cfg-ul-runtime"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					// Create ConfigMap WITHOUT managed label
					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "unlabeled-config", Namespace: ns},
						Data:       map[string]string{"data.xml": "<data/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					// Deployment should not gain any config volumes
					Consistently(func() int {
						Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-ul-cluster", "cfg-ul-runtime"), Namespace: ns}, deploy)).To(Succeed())
						return e2eCountCfgVolumes(deploy)
					}, 5*time.Second, e2eInterval).Should(Equal(0), "unlabeled ConfigMap should not be mounted")

					utils.MatchYAMLResource(deploy, "[deployment] cfg-ul-runtime")
				})
			})

			Describe("Coexistence with cluster-config", Ordered, func() {
				const ns = "e2e-cfg-coexist"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should coexist with cluster-config volume", func() {
					ctx := context.Background()

					By("pre-creating populated cluster-config Secret")
					configSecret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name: "cfg-co-cluster-cluster-config", Namespace: ns,
							Labels: map[string]string{
								"curity.io/cluster":   "cfg-co-cluster",
								"curity.io/component": "cluster-config",
							},
							Annotations: map[string]string{
								"curity.io/admin-node":               "cfg-co-admin",
								"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
							},
						},
						Data: map[string][]byte{"cluster.xml": []byte("<config>coexist-test</config>")},
					}
					Expect(k().Create(ctx, configSecret)).To(Succeed())

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-co-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "cfg-co-admin", "namespace": ns, "clusterName": "cfg-co-cluster"})

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-co-cluster", "cfg-co-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "coexist-config", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string]string{"extra.xml": "<extra/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-co-cluster", "cfg-co-admin"), Namespace: ns}, deploy)).To(Succeed())
						// Both volumes should exist
						g.Expect(e2eHasCfgVolume(deploy, "cluster-config")).To(BeTrue(), "should have cluster-config volume")
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-coexist-config")).To(BeTrue(), "should have discovered config volume")
					}, e2eTimeout, e2eInterval).Should(Succeed())

					utils.MatchYAMLResource(deploy, "[deployment] cfg-co-admin")
				})
			})

			Describe("Config type defaulting (read-time only — operator does not write annotation)", Ordered, func() {
				const ns = "e2e-cfg-type-default"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("treats absent annotation as base without writing it back", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-td-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "cfg-td-runtime", "namespace": ns, "clusterName": "cfg-td-cluster"})

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-td-cluster", "cfg-td-runtime"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "no-type-config", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string]string{"x.xml": "<x/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					By("waiting for the operator to discover the unannotated CM and mount it as base")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-td-cluster", "cfg-td-runtime"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-no-type-config")).To(BeTrue(), "unannotated CM should mount as base via read-time defaulting")
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("asserting the operator never writes curity.io/config-type back onto the source CM")
					Consistently(func() string {
						updated := &corev1.ConfigMap{}
						if err := k().Get(ctx, client.ObjectKey{Name: "no-type-config", Namespace: ns}, updated); err != nil {
							return ""
						}
						return updated.Annotations["curity.io/config-type"]
					}, 10*time.Second, e2eInterval).Should(BeEmpty(), "operator must not write the curity.io/config-type annotation")
				})
			})

			Describe("Logging config-type", Ordered, func() {
				const ns = "e2e-cfg-logging"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				const loggingMountPath = "/opt/idsvr/etc/log4j2.xml"

				countIssuesFor := func(g Gomega, clusterName, resourceName, reason string) int {
					cluster := &v1alpha1.IdentityServerCluster{}
					g.Expect(k().Get(context.Background(),
						client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
					n := 0
					for _, issue := range cluster.Status.ManagedResourceIssues {
						if issue.Name == resourceName && issue.Reason == reason {
							n++
						}
					}
					return n
				}

				countWarningEvents := func(g Gomega, resourceName, reason string) int {
					var events corev1.EventList
					g.Expect(k().List(context.Background(), &events, client.InNamespace(ns))).To(Succeed())
					n := 0
					for _, e := range events.Items {
						if e.InvolvedObject.Name == resourceName &&
							e.Reason == reason &&
							e.Type == corev1.EventTypeWarning {
							n++
						}
					}
					return n
				}

				It("mounts log4j2.xml at the leaf path with no filename mangling", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "log-ok-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "log-ok-admin", "namespace": ns, "clusterName": "log-ok-cluster"})
					utils.SimulateClusterConfigReady(ns, "log-ok-cluster", e2eTimeout, e2eInterval)

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-ok-cluster", "log-ok-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "my-log4j2", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{
								"curity.io/config-type": "logging",
								"curity.io/cluster":     "log-ok-cluster",
							},
						},
						Data: map[string]string{
							"log4j2.xml": `<?xml version="1.0" encoding="UTF-8"?>
<Configuration><Appenders><Console name="stdout" target="SYSTEM_OUT"><PatternLayout pattern="%level %msg%n"/></Console></Appenders><Loggers><Root level="INFO"><AppenderRef ref="stdout"/></Root></Loggers></Configuration>`,
						},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("log-ok-cluster", "log-ok-admin"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-my-log4j2")).To(BeTrue())
						g.Expect(e2eHasVolumeMount(deploy, "cfg-cm-my-log4j2", loggingMountPath)).To(BeTrue())
						g.Expect(e2eHasVolumeMount(deploy, "cfg-cm-my-log4j2", "/opt/idsvr/etc/init/cm_my-log4j2_log4j2.xml")).To(BeFalse(),
							"logging type must use leaf path, not mangled prefix")
					}, e2eTimeout, e2eInterval).Should(Succeed())

					utils.MatchYAMLResource(deploy, "[deployment] log-ok-admin")
				})

				It("surfaces LoggingConfigInvalid when the data shape is wrong", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "log-bad-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "log-bad-admin", "namespace": ns, "clusterName": "log-bad-cluster"})
					utils.SimulateClusterConfigReady(ns, "log-bad-cluster", e2eTimeout, e2eInterval)

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-bad-cluster", "log-bad-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					// Logging-typed CM with the wrong key name.
					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "bad-log", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{
								"curity.io/config-type": "logging",
								"curity.io/cluster":     "log-bad-cluster",
							},
						},
						Data: map[string]string{"config.xml": "<wrong/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					By("expecting LoggingConfigInvalid issue on the cluster status")
					Eventually(func(g Gomega) {
						g.Expect(countIssuesFor(g, "log-bad-cluster", "bad-log", "LoggingConfigInvalid")).
							To(Equal(1))
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("expecting a Warning event on the offending CM")
					Eventually(func(g Gomega) {
						g.Expect(countWarningEvents(g, "bad-log", "LoggingConfigInvalid")).
							To(BeNumerically(">=", 1))
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("expecting the resource to NOT be mounted")
					Consistently(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("log-bad-cluster", "log-bad-admin"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-bad-log")).To(BeFalse(),
							"invalid logging resource must not mount")
					}, 3*time.Second, time.Second).Should(Succeed())
				})

				It("blocks both resources on DuplicateLoggingConfig", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "log-dup-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "log-dup-admin", "namespace": ns, "clusterName": "log-dup-cluster"})
					utils.SimulateClusterConfigReady(ns, "log-dup-cluster", e2eTimeout, e2eInterval)

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-dup-cluster", "log-dup-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					// Two valid logging-typed resources, both scoped to this cluster.
					cmA := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "log-dup-a", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{
								"curity.io/config-type": "logging",
								"curity.io/cluster":     "log-dup-cluster",
							},
						},
						Data: map[string]string{"log4j2.xml": "<a/>"},
					}
					cmB := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "log-dup-b", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{
								"curity.io/config-type": "logging",
								"curity.io/cluster":     "log-dup-cluster",
							},
						},
						Data: map[string]string{"log4j2.xml": "<b/>"},
					}
					Expect(k().Create(ctx, cmA)).To(Succeed())
					Expect(k().Create(ctx, cmB)).To(Succeed())

					By("expecting both resources to surface DuplicateLoggingConfig")
					Eventually(func(g Gomega) {
						g.Expect(countIssuesFor(g, "log-dup-cluster", "log-dup-a", "DuplicateLoggingConfig")).
							To(Equal(1))
						g.Expect(countIssuesFor(g, "log-dup-cluster", "log-dup-b", "DuplicateLoggingConfig")).
							To(Equal(1))
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("expecting a Warning event on each conflicting CM")
					Eventually(func(g Gomega) {
						g.Expect(countWarningEvents(g, "log-dup-a", "DuplicateLoggingConfig")).
							To(BeNumerically(">=", 1))
						g.Expect(countWarningEvents(g, "log-dup-b", "DuplicateLoggingConfig")).
							To(BeNumerically(">=", 1))
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("expecting no DuplicateConfigKey events on either CM (no double-reporting)")
					Consistently(func(g Gomega) {
						g.Expect(countWarningEvents(g, "log-dup-a", "DuplicateConfigKey")).To(Equal(0))
						g.Expect(countWarningEvents(g, "log-dup-b", "DuplicateConfigKey")).To(Equal(0))
					}, 3*time.Second, time.Second).Should(Succeed())

					By("expecting NEITHER resource to be mounted (predictable failure)")
					Consistently(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("log-dup-cluster", "log-dup-admin"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-log-dup-a")).To(BeFalse())
						g.Expect(e2eHasCfgVolume(deploy, "cfg-cm-log-dup-b")).To(BeFalse())
					}, 3*time.Second, time.Second).Should(Succeed())
				})

				It("does NOT inspect non-logging resources for log4j2.xml keys (lock-in)", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "log-wt-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "log-wt-admin", "namespace": ns, "clusterName": "log-wt-cluster"})
					utils.SimulateClusterConfigReady(ns, "log-wt-cluster", e2eTimeout, e2eInterval)

					deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-wt-cluster", "log-wt-admin"), Namespace: ns}}
					utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

					// Base CM with key log4j2.xml — user mistake, but operator
					// must trust the declared type and stay silent.
					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "wrong-type-base", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{
								// Annotation defaults to "base" via cluster reconciler;
								// scope binds to this test's cluster so it cannot leak
								// into other tests sharing this namespace.
								"curity.io/cluster": "log-wt-cluster",
							},
						},
						Data: map[string]string{"log4j2.xml": "<wrong/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					By("expecting the base CM to mount at the mangled path")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("log-wt-cluster", "log-wt-admin"), Namespace: ns}, deploy)).To(Succeed())
						g.Expect(e2eHasVolumeMount(deploy, "cfg-cm-wrong-type-base",
							"/opt/idsvr/etc/init/cm_wrong-type-base_log4j2.xml")).To(BeTrue(),
							"base CM with log4j2.xml key must mount at the mangled base path")
						g.Expect(e2eHasVolumeMount(deploy, "cfg-cm-wrong-type-base", loggingMountPath)).To(BeFalse(),
							"base CM must NOT mount at the leaf logging path")
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("expecting NO LoggingConfigInvalid, DuplicateLoggingConfig, or hint-style issue on this CM")
					Consistently(func(g Gomega) {
						cluster := &v1alpha1.IdentityServerCluster{}
						g.Expect(k().Get(ctx, client.ObjectKey{Name: "log-wt-cluster", Namespace: ns}, cluster)).To(Succeed())
						for _, issue := range cluster.Status.ManagedResourceIssues {
							if issue.Name == "wrong-type-base" {
								g.Expect(issue.Reason).NotTo(Or(
									Equal("LoggingConfigInvalid"),
									Equal("DuplicateLoggingConfig"),
									Equal("LoggingKeyInWrongType"),
								), "operator must not inspect data keys of non-logging resources")
							}
						}
					}, 3*time.Second, time.Second).Should(Succeed())
				})

				It("mounts logging on runtime even when admin exists (per-pod routing)", func() {
					ctx := context.Background()

					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "log-rt-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "log-rt-admin", "namespace": ns, "clusterName": "log-rt-cluster"})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "log-rt-runtime", "namespace": ns, "clusterName": "log-rt-cluster"})
					utils.SimulateClusterConfigReady(ns, "log-rt-cluster", e2eTimeout, e2eInterval)

					adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-rt-cluster", "log-rt-admin"), Namespace: ns}}
					runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("log-rt-cluster", "log-rt-runtime"), Namespace: ns}}
					utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)
					utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)

					// One logging CM + one base CM, both scoped to this cluster.
					logCM := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "log-rt", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{
								"curity.io/config-type": "logging",
								"curity.io/cluster":     "log-rt-cluster",
							},
						},
						Data: map[string]string{"log4j2.xml": "<Configuration/>"},
					}
					baseCM := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name: "base-rt", Namespace: ns,
							Labels:      map[string]string{"curity.io/managed": "true"},
							Annotations: map[string]string{"curity.io/cluster": "log-rt-cluster"},
						},
						Data: map[string]string{"server.xml": "<server/>"},
					}
					Expect(k().Create(ctx, logCM)).To(Succeed())
					Expect(k().Create(ctx, baseCM)).To(Succeed())

					By("expecting admin to mount BOTH base and logging")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("log-rt-cluster", "log-rt-admin"), Namespace: ns}, adminDeploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(adminDeploy, "cfg-cm-log-rt")).To(BeTrue())
						g.Expect(e2eHasCfgVolume(adminDeploy, "cfg-cm-base-rt")).To(BeTrue())
						g.Expect(e2eHasVolumeMount(adminDeploy, "cfg-cm-log-rt", loggingMountPath)).To(BeTrue())
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("expecting runtime to mount ONLY logging (base goes to admin)")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("log-rt-cluster", "log-rt-runtime"), Namespace: ns}, runtimeDeploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(runtimeDeploy, "cfg-cm-log-rt")).To(BeTrue(),
							"logging must mount on runtime even when admin exists")
						g.Expect(e2eHasVolumeMount(runtimeDeploy, "cfg-cm-log-rt", loggingMountPath)).To(BeTrue())
						g.Expect(e2eHasCfgVolume(runtimeDeploy, "cfg-cm-base-rt")).To(BeFalse(),
							"base must not mount on runtime when admin exists (existing routing rule unchanged)")
					}, e2eTimeout, e2eInterval).Should(Succeed())
				})
			})

			Describe("Admin node added re-routes configs", Ordered, func() {
				const ns = "e2e-cfg-admin-added"
				BeforeAll(func() { createNS(ns) })
				AfterAll(func() { deleteNS(ns) })

				It("should re-route configs when admin node is added", func() {
					ctx := context.Background()

					By("setting up cluster + runtime (no admin) + managed config")
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
						map[string]interface{}{"name": "cfg-aa-cluster", "namespace": ns})
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
						map[string]interface{}{"name": "cfg-aa-runtime", "namespace": ns, "clusterName": "cfg-aa-cluster"})

					runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-aa-cluster", "cfg-aa-runtime"), Namespace: ns}}
					utils.WaitForResource(runtimeDeploy, e2eTimeout, e2eInterval)

					cm := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "reroute-config", Namespace: ns,
							Labels: map[string]string{"curity.io/managed": "true"}},
						Data: map[string]string{"reroute.xml": "<reroute/>"},
					}
					Expect(k().Create(ctx, cm)).To(Succeed())

					By("verifying runtime gets config (no admin exists)")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-aa-cluster", "cfg-aa-runtime"), Namespace: ns}, runtimeDeploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(runtimeDeploy, "cfg-cm-reroute-config")).To(BeTrue())
					}, e2eTimeout, e2eInterval).Should(Succeed())

					By("adding admin node")
					utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
						map[string]interface{}{"name": "cfg-aa-admin", "namespace": ns, "clusterName": "cfg-aa-cluster"})
					utils.SimulateClusterConfigReady(ns, "cfg-aa-cluster", e2eTimeout, e2eInterval)

					adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("cfg-aa-cluster", "cfg-aa-admin"), Namespace: ns}}
					utils.WaitForResource(adminDeploy, e2eTimeout, e2eInterval)

					By("verifying admin gets config and runtime loses it")
					Eventually(func(g Gomega) {
						g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-aa-cluster", "cfg-aa-admin"), Namespace: ns}, adminDeploy)).To(Succeed())
						g.Expect(e2eHasCfgVolume(adminDeploy, "cfg-cm-reroute-config")).To(BeTrue(), "admin should get config")
					}, e2eTimeout, e2eInterval).Should(Succeed())

					Eventually(func() int {
						Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("cfg-aa-cluster", "cfg-aa-runtime"), Namespace: ns}, runtimeDeploy)).To(Succeed())
						return e2eCountCfgVolumes(runtimeDeploy)
					}, e2eTimeout, e2eInterval).Should(Equal(0), "runtime should lose config when admin added")
				})
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
				utils.SimulateClusterConfigReady(ns, "ui-cluster", e2eTimeout, e2eInterval)

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("ui-cluster", "ui-admin"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				Expect(deploy.Spec.Template.Spec.Containers[0].Ports).To(ContainElement(
					Satisfy(func(p corev1.ContainerPort) bool { return p.Name == "admin-ui" && p.ContainerPort == 6749 }),
				), "admin-ui port 6749 should exist")

				svc := &corev1.Service{}
				Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("ui-cluster", "ui-admin"), Namespace: ns}, svc)).To(Succeed())
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("custom-cluster", "custom-node"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))

				Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
					Satisfy(func(e corev1.EnvVar) bool { return e.Name == "CUSTOM_ENV" && e.Value == "hello-e2e" }),
				))

				svc := &corev1.Service{}
				Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("custom-cluster", "custom-node"), Namespace: ns}, svc)).To(Succeed())
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
				utils.SimulateClusterConfigReady(ns, "args-cluster", e2eTimeout, e2eInterval)

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("args-cluster", "args-admin"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("rtargs-cluster", "args-runtime"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("probe-cluster", "probe-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("img-cluster", "img-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("ver-cluster", "ver-node"), Namespace: ns}}
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
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("ver-cluster", "ver-node"), Namespace: ns}, deploy)).To(Succeed())
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("lbl-cluster", "lbl-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("sec-cluster", "sec-node"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("sched-cluster", "sched-runtime"), Namespace: ns}}
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

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("ovr-cluster", "ovr-runtime"), Namespace: ns}}
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

	// =================================================================
	// autoscaling
	// =================================================================
	Context("autoscaling", func() {

		Describe("HPA created for runtime with autoscaling enabled", Label("smoke"), Ordered, func() {
			const ns = "e2e-hpa"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create HPA for runtime node with autoscaling enabled", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node with autoscaling")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-runtime", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
						},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying HPA is created")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-cluster", "hpa-runtime"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)

				Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
				Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))
				Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(ownedName("hpa-cluster", "hpa-runtime")))
				Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
				Expect(hpa.Spec.Metrics).To(HaveLen(1))
				Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(80)))

				By("verifying HPA owner reference")
				Expect(hpa.OwnerReferences).To(HaveLen(1))
				Expect(hpa.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

				utils.MatchYAMLResource(hpa, "hpa-runtime")
			})

			It("should set Deployment replicas to minReplicas when autoscaling enabled", func() {
				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-cluster", "hpa-runtime"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
			})
		})

		Describe("HPA not created for admin node", Label("smoke"), Ordered, func() {
			const ns = "e2e-hpa-admin"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should not create HPA for admin node even with autoscaling enabled", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-adm-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-adm-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating admin node with autoscaling enabled")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-admin", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeAdmin, Role: "admin-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-adm-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
						},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())
				utils.SimulateClusterConfigReady(ns, "hpa-adm-cluster", e2eTimeout, e2eInterval)

				By("verifying Deployment exists")
				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-adm-cluster", "hpa-admin"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

				By("verifying HPA never created")
				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx,
						client.ObjectKey{Name: ownedName("hpa-adm-cluster", "hpa-admin"), Namespace: ns},
						&autoscalingv2.HorizontalPodAutoscaler{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())
			})
		})

		Describe("HPA deleted when autoscaling disabled", Label("smoke"), Ordered, func() {
			const ns = "e2e-hpa-toggle"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should delete HPA when autoscaling is disabled", func() {
				ctx := context.Background()

				By("creating cluster and runtime node with autoscaling")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-tog-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-tog-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-toggle", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-tog-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
						},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("waiting for HPA to be created")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-tog-cluster", "hpa-toggle"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)

				By("disabling autoscaling")
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "hpa-toggle", Namespace: ns}, node); err != nil {
						return err
					}
					node.Spec.Autoscaling.Enabled = false
					return k().Update(ctx, node)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying HPA is deleted")
				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx,
						client.ObjectKey{Name: ownedName("hpa-tog-cluster", "hpa-toggle"), Namespace: ns},
						&autoscalingv2.HorizontalPodAutoscaler{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
			})
		})

		Describe("HPA updated when autoscaling spec changes", Ordered, func() {
			const ns = "e2e-hpa-update"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should update HPA when maxReplicas and custom metrics change", func() {
				ctx := context.Background()

				By("creating cluster and runtime node with CPU-only autoscaling")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-upd-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-upd-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-update", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-upd-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
						},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying initial HPA: maxReplicas=10, 1 metric (CPU)")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-upd-cluster", "hpa-update"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)
				Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))
				Expect(hpa.Spec.Metrics).To(HaveLen(1))

				By("updating node: maxReplicas=20 and adding a custom metric")
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "hpa-update", Namespace: ns}, node); err != nil {
						return err
					}
					node.Spec.Autoscaling.MaxReplicas = 20
					node.Spec.Autoscaling.CustomMetrics = []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.PodsMetricSourceType,
							Pods: &autoscalingv2.PodsMetricSource{
								Metric: autoscalingv2.MetricIdentifier{Name: "http_requests_per_second"},
								Target: autoscalingv2.MetricTarget{
									Type:         autoscalingv2.AverageValueMetricType,
									AverageValue: ptr.To(resource.MustParse("1000")),
								},
							},
						},
					}
					return k().Update(ctx, node)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying HPA updated: maxReplicas=20, 2 metrics")
				Eventually(func(g Gomega) {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("hpa-upd-cluster", "hpa-update"), Namespace: ns}, hpa)).To(Succeed())
					g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(20)))
					g.Expect(hpa.Spec.Metrics).To(HaveLen(2))
				}, e2eTimeout, e2eInterval).Should(Succeed())

				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ResourceMetricSourceType))
				Expect(hpa.Spec.Metrics[1].Type).To(Equal(autoscalingv2.PodsMetricSourceType))

				By("removing custom metrics")
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "hpa-update", Namespace: ns}, node); err != nil {
						return err
					}
					node.Spec.Autoscaling.CustomMetrics = nil
					return k().Update(ctx, node)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying HPA reverted to CPU-only")
				Eventually(func(g Gomega) int {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("hpa-upd-cluster", "hpa-update"), Namespace: ns}, hpa)).To(Succeed())
					return len(hpa.Spec.Metrics)
				}, e2eTimeout, e2eInterval).Should(Equal(1))
			})
		})

		Describe("Cluster-level autoscaling defaults inherited", Ordered, func() {
			const ns = "e2e-hpa-cluster"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should inherit cluster-level autoscaling defaults", func() {
				ctx := context.Background()

				By("creating cluster with autoscaling enabled")
				cluster := &v1alpha1.IdentityServerCluster{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-cl-cluster", Namespace: ns},
					Spec: v1alpha1.IdentityServerClusterSpec{
						Version: "11.0",
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    3,
							MaxReplicas:                    15,
							TargetCPUUtilizationPercentage: 70,
						},
					},
				}
				Expect(k().Create(ctx, cluster)).To(Succeed())
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node WITHOUT node-level autoscaling")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-inherit", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-cl-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying HPA is created with cluster defaults")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-cl-cluster", "hpa-inherit"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)

				Expect(*hpa.Spec.MinReplicas).To(Equal(int32(3)))
				Expect(hpa.Spec.MaxReplicas).To(Equal(int32(15)))
				Expect(*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(70)))
			})
		})

		Describe("Cluster-level custom metrics inherited", Ordered, func() {
			const ns = "e2e-hpa-clcm"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should inherit cluster-level custom metrics into HPA", func() {
				ctx := context.Background()

				By("creating cluster with custom metrics")
				cluster := &v1alpha1.IdentityServerCluster{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-clcm-cluster", Namespace: ns},
					Spec: v1alpha1.IdentityServerClusterSpec{
						Version: "11.0",
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
							CustomMetrics: []autoscalingv2.MetricSpec{
								{
									Type: autoscalingv2.PodsMetricSourceType,
									Pods: &autoscalingv2.PodsMetricSource{
										Metric: autoscalingv2.MetricIdentifier{Name: "http_requests_per_second"},
										Target: autoscalingv2.MetricTarget{
											Type:         autoscalingv2.AverageValueMetricType,
											AverageValue: ptr.To(resource.MustParse("500")),
										},
									},
								},
							},
						},
					},
				}
				Expect(k().Create(ctx, cluster)).To(Succeed())
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node WITHOUT node-level autoscaling")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-clcm-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-clcm-cluster"},
						Replicas:                 ptr.To(int32(1)),
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying HPA has CPU + cluster-level custom metric")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-clcm-cluster", "hpa-clcm-node"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)

				Expect(hpa.Spec.Metrics).To(HaveLen(2))
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ResourceMetricSourceType))
				Expect(hpa.Spec.Metrics[1].Type).To(Equal(autoscalingv2.PodsMetricSourceType))
				Expect(hpa.Spec.Metrics[1].Pods.Metric.Name).To(Equal("http_requests_per_second"))
			})
		})

		Describe("HPA with custom metrics", Ordered, func() {
			const ns = "e2e-hpa-custom"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should support custom metrics in HPA", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-cust-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-cust-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node with custom metrics")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-custom", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-cust-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
							CustomMetrics: []autoscalingv2.MetricSpec{
								{
									Type: autoscalingv2.PodsMetricSourceType,
									Pods: &autoscalingv2.PodsMetricSource{
										Metric: autoscalingv2.MetricIdentifier{Name: "http_requests_per_second"},
										Target: autoscalingv2.MetricTarget{
											Type:         autoscalingv2.AverageValueMetricType,
											AverageValue: ptr.To(resource.MustParse("1000")),
										},
									},
								},
							},
						},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying HPA has CPU + custom metrics")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-cust-cluster", "hpa-custom"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)

				Expect(hpa.Spec.Metrics).To(HaveLen(2))
				Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ResourceMetricSourceType))
				Expect(hpa.Spec.Metrics[1].Type).To(Equal(autoscalingv2.PodsMetricSourceType))
				utils.MatchYAMLResource(hpa, "hpa-custom-metrics")
			})
		})

		Describe("No HPA when autoscaling nil", Ordered, func() {
			const ns = "e2e-hpa-nil"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should not create HPA when autoscaling is nil", func() {
				ctx := context.Background()

				By("creating cluster and runtime node with no autoscaling")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-nil-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-nil-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
					map[string]interface{}{"name": "hpa-nil-runtime", "namespace": ns, "clusterName": "hpa-nil-cluster"})

				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-nil-cluster", "hpa-nil-runtime"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				By("verifying no HPA created")
				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx,
						client.ObjectKey{Name: ownedName("hpa-nil-cluster", "hpa-nil-runtime"), Namespace: ns},
						&autoscalingv2.HorizontalPodAutoscaler{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())
			})
		})

		Describe("Replica preservation on re-reconcile", Ordered, func() {
			const ns = "e2e-hpa-noreset"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should not reset replicas on re-reconcile when HPA is active", func() {
				ctx := context.Background()

				By("creating cluster and runtime node with autoscaling")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "hpa-nr-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "hpa-nr-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "hpa-noreset", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-nr-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    10,
							TargetCPUUtilizationPercentage: 80,
						},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("waiting for Deployment and HPA")
				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-nr-cluster", "hpa-noreset"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("hpa-nr-cluster", "hpa-noreset"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)

				By("simulating HPA scaling: patch Deployment replicas to 5")
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: ownedName("hpa-nr-cluster", "hpa-noreset"), Namespace: ns}, deploy); err != nil {
						return err
					}
					deploy.Spec.Replicas = ptr.To(int32(5))
					return k().Update(ctx, deploy)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("triggering re-reconcile via node annotation change")
				Eventually(func() error {
					if err := k().Get(ctx, client.ObjectKey{Name: "hpa-noreset", Namespace: ns}, node); err != nil {
						return err
					}
					if node.Spec.PodAnnotations == nil {
						node.Spec.PodAnnotations = make(map[string]string)
					}
					node.Spec.PodAnnotations["trigger-reconcile"] = "true"
					return k().Update(ctx, node)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying replicas preserved at 5 (not reset to minReplicas=2)")
				Consistently(func(g Gomega) int32 {
					g.Expect(k().Get(ctx, client.ObjectKey{Name: ownedName("hpa-nr-cluster", "hpa-noreset"), Namespace: ns}, deploy)).To(Succeed())
					if deploy.Spec.Replicas == nil {
						return 0
					}
					return *deploy.Spec.Replicas
				}, 5*time.Second, e2eInterval).Should(Equal(int32(5)))
			})
		})
	})

	// =================================================================
	// podDisruptionBudget
	// =================================================================
	Context("podDisruptionBudget", func() {

		Describe("PDB created for runtime with integer minAvailable", Label("smoke"), Ordered, func() {
			const ns = "e2e-pdb"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create PDB for runtime node with minAvailable", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "pdb-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "pdb-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node with minAvailable=2")
				min := intstr.FromInt32(2)
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "pdb-runtime", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "pdb-cluster"},
						Replicas:                 ptr.To(int32(3)),
						PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying PDB is created")
				pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: ownedName("pdb-cluster", "pdb-runtime"), Namespace: ns}}
				utils.WaitForResource(pdb, e2eTimeout, e2eInterval)

				Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
				Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(2))
				Expect(pdb.Spec.MaxUnavailable).To(BeNil())
				Expect(pdb.Spec.Selector).NotTo(BeNil())
				Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", ownedName("pdb-cluster", "pdb-runtime")))

				By("verifying PDB owner reference")
				Expect(pdb.OwnerReferences).To(HaveLen(1))
				Expect(pdb.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
				Expect(pdb.OwnerReferences[0].Name).To(Equal("pdb-runtime"))

				utils.MatchYAMLResource(pdb, "pdb-runtime-min-int")
			})
		})

		Describe("PDB with percentage minAvailable", Label("smoke"), Ordered, func() {
			const ns = "e2e-pdb-pct"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create PDB with percentage minAvailable", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "pdb-pct-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "pdb-pct-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node with minAvailable=\"50%\"")
				min := intstr.FromString("50%")
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "pdb-pct-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "pdb-pct-cluster"},
						Replicas:                 ptr.To(int32(4)),
						PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying PDB is created with percentage value")
				pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: ownedName("pdb-pct-cluster", "pdb-pct-node"), Namespace: ns}}
				utils.WaitForResource(pdb, e2eTimeout, e2eInterval)

				Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
				Expect(pdb.Spec.MinAvailable.Type).To(Equal(intstr.String))
				Expect(pdb.Spec.MinAvailable.StrVal).To(Equal("50%"))

				utils.MatchYAMLResource(pdb, "pdb-runtime-min-pct")
			})
		})

		Describe("PDB deleted when field removed", Label("smoke"), Ordered, func() {
			const ns = "e2e-pdb-del"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should delete PDB when podDisruptionBudget is removed from spec", func() {
				ctx := context.Background()

				By("creating cluster and runtime node with PDB")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "pdb-del-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "pdb-del-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				min := intstr.FromInt32(1)
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "pdb-del-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "pdb-del-cluster"},
						Replicas:                 ptr.To(int32(2)),
						PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("waiting for PDB to be created")
				pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: ownedName("pdb-del-cluster", "pdb-del-node"), Namespace: ns}}
				utils.WaitForResource(pdb, e2eTimeout, e2eInterval)

				By("removing PDB from node spec")
				Eventually(func() error {
					var fresh v1alpha1.IdentityServerNode
					if err := k().Get(ctx, client.ObjectKey{Name: "pdb-del-node", Namespace: ns}, &fresh); err != nil {
						return err
					}
					fresh.Spec.PodDisruptionBudget = nil
					return k().Update(ctx, &fresh)
				}, e2eTimeout, e2eInterval).Should(Succeed())

				By("verifying PDB is deleted")
				Eventually(func() bool {
					return apierrors.IsNotFound(k().Get(ctx,
						client.ObjectKey{Name: ownedName("pdb-del-cluster", "pdb-del-node"), Namespace: ns},
						&policyv1.PodDisruptionBudget{}))
				}, e2eTimeout, e2eInterval).Should(BeTrue())
			})
		})

		Describe("PDB not created for admin node", Label("smoke"), Ordered, func() {
			const ns = "e2e-pdb-admin"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should ignore PDB spec on admin node", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "pdb-adm-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "pdb-adm-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating admin node with PDB spec set")
				min := intstr.FromInt32(1)
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "pdb-admin", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeAdmin, Role: "admin-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "pdb-adm-cluster"},
						Replicas:                 ptr.To(int32(1)),
						PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())
				utils.SimulateClusterConfigReady(ns, "pdb-adm-cluster", e2eTimeout, e2eInterval)

				By("verifying Deployment exists")
				deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ownedName("pdb-adm-cluster", "pdb-admin"), Namespace: ns}}
				utils.WaitForResource(deploy, e2eTimeout, e2eInterval)

				By("verifying PDB never created for admin")
				Consistently(func() bool {
					return apierrors.IsNotFound(k().Get(ctx,
						client.ObjectKey{Name: ownedName("pdb-adm-cluster", "pdb-admin"), Namespace: ns},
						&policyv1.PodDisruptionBudget{}))
				}, 5*time.Second, e2eInterval).Should(BeTrue())
			})
		})

		Describe("PDB coexists with HPA on the same node", Label("smoke"), Ordered, func() {
			const ns = "e2e-pdb-hpa"
			BeforeAll(func() { createNS(ns) })
			AfterAll(func() { deleteNS(ns) })

			It("should create both HPA and PDB for a runtime node that sets both", func() {
				ctx := context.Background()

				By("creating cluster")
				utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
					map[string]interface{}{"name": "pdb-hpa-cluster", "namespace": ns})
				cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "pdb-hpa-cluster", Namespace: ns}}
				utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)

				By("creating runtime node with both HPA and PDB")
				min := intstr.FromInt32(1)
				node := &v1alpha1.IdentityServerNode{
					ObjectMeta: metav1.ObjectMeta{Name: "pdb-hpa-node", Namespace: ns},
					Spec: v1alpha1.IdentityServerNodeSpec{
						Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role",
						IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "pdb-hpa-cluster"},
						Replicas:                 ptr.To(int32(1)),
						Autoscaling: &v1alpha1.AutoscalingSpec{
							Enabled:                        true,
							MinReplicas:                    2,
							MaxReplicas:                    5,
							TargetCPUUtilizationPercentage: 80,
						},
						PodDisruptionBudget: &v1alpha1.PDBSpec{MinAvailable: &min},
					},
				}
				Expect(k().Create(ctx, node)).To(Succeed())

				By("verifying both HPA and PDB are created and owned by the node")
				hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: ownedName("pdb-hpa-cluster", "pdb-hpa-node"), Namespace: ns}}
				utils.WaitForResource(hpa, e2eTimeout, e2eInterval)
				Expect(hpa.OwnerReferences).To(HaveLen(1))
				Expect(hpa.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

				pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: ownedName("pdb-hpa-cluster", "pdb-hpa-node"), Namespace: ns}}
				utils.WaitForResource(pdb, e2eTimeout, e2eInterval)
				Expect(pdb.OwnerReferences).To(HaveLen(1))
				Expect(pdb.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
				Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(1))
			})
		})
	})

})
