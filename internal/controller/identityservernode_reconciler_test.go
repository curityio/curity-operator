package controller_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/internal/controller"
)

var _ = Describe("IdentityServerNode Reconciler", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
	})

	AfterEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		_ = k8sClient.Delete(ctx, namespace)
	})

	Context("Happy path — Admin node", func() {
		It("should create Deployment and Service for admin node", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "admin-node", v1alpha1.NodeTypeAdmin, "cluster-1")
			testSimulateClusterConfigReady(ns, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "admin-node"), deploy)
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--admin"))

			svc := &corev1.Service{}
			eventuallyGetResource(ns, ownedName("cluster-1", "admin-node"), svc)

			// Verify node status is updated
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "admin-node", Namespace: ns}, node)
				return node.Status.DeploymentName
			}, timeout, interval).Should(Equal(ownedName("cluster-1", "admin-node")))

			Expect(node.Status.ServiceName).To(Equal(ownedName("cluster-1", "admin-node")))
		})
	})

	Context("Deployment creation gate — ClusterConfigReady", func() {
		It("should defer Deployment until ClusterConfigReady when admin exists", func() {
			testCreateCluster(ns, "gate-cluster")
			testCreateNode(ns, "gate-admin", v1alpha1.NodeTypeAdmin, "gate-cluster")
			testCreateNode(ns, "gate-runtime", v1alpha1.NodeTypeRuntime, "gate-cluster")

			// Both nodes should be gated — no Deployment yet
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "gate-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)
			}, timeout, interval).Should(Equal("WaitingForClusterConfig"))

			Expect(node.Status.ServiceName).To(Equal(ownedName("gate-cluster", "gate-admin")))

			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName("gate-cluster", "gate-admin"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName("gate-cluster", "gate-runtime"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())

			// Services must exist while Deployment is gated — the genclust Job
			// needs DNS resolution of the admin hostname.
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-admin"), &corev1.Service{})
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-runtime"), &corev1.Service{})

			// Unblock by setting ClusterConfigReady=True
			testSimulateClusterConfigReady(ns, "gate-cluster")

			// Both Deployments should now appear
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-admin"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-runtime"), &appsv1.Deployment{})
		})

		It("should create Deployment immediately for runtime-only cluster", func() {
			testCreateCluster(ns, "nonadmin-cluster")
			testCreateNode(ns, "nonadmin-runtime", v1alpha1.NodeTypeRuntime, "nonadmin-cluster")

			// No admin exists — Deployment should be created without waiting
			eventuallyGetResource(ns, ownedName("nonadmin-cluster", "nonadmin-runtime"), &appsv1.Deployment{})

			// Should NOT have WaitingForClusterConfig
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nonadmin-runtime", Namespace: ns}, node)).To(Succeed())
			Expect(conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)).NotTo(Equal("WaitingForClusterConfig"))
		})

		It("should not disrupt existing Deployment when admin is added to runtime-only cluster", func() {
			testCreateCluster(ns, "evolve-cluster")
			testCreateNode(ns, "evolve-runtime", v1alpha1.NodeTypeRuntime, "evolve-cluster")

			// Runtime Deployment should exist immediately (no admin)
			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("evolve-cluster", "evolve-runtime"), deploy)

			// Now add admin — ClusterConfigReady is NOT True
			testCreateNode(ns, "evolve-admin", v1alpha1.NodeTypeAdmin, "evolve-cluster")

			// Existing runtime Deployment should remain (gate bypassed for existing)
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("evolve-cluster", "evolve-runtime"), Namespace: ns}, deploy)
			}, 5*time.Second, 250*time.Millisecond).Should(Succeed())

			// Admin Deployment should be gated (no Deployment yet)
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "evolve-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)
			}, timeout, interval).Should(Equal("WaitingForClusterConfig"))

			// Unblock — admin Deployment should appear
			testSimulateClusterConfigReady(ns, "evolve-cluster")
			eventuallyGetResource(ns, ownedName("evolve-cluster", "evolve-admin"), &appsv1.Deployment{})
		})

		It("should defer Deployment updates while cluster config is regenerating", func() {
			testCreateCluster(ns, "hash-cluster")
			testCreateNode(ns, "hash-runtime", v1alpha1.NodeTypeRuntime, "hash-cluster")
			testCreateNode(ns, "hash-admin", v1alpha1.NodeTypeAdmin, "hash-cluster")
			testSimulateClusterConfigReady(ns, "hash-cluster")

			// Wait for the runtime Deployment to reach its initial steady state.
			deploy := &appsv1.Deployment{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hash-cluster", "hash-runtime"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]).NotTo(BeEmpty())
			}, timeout, interval).Should(Succeed())
			originalGeneration := deploy.Generation

			// Simulate config regeneration: reset Secret to placeholder, as
			// Branch A/C of the cluster reconciler does during key rotation
			// or a multi-input spec change.
			secretName := "hash-cluster-cluster-config"
			Eventually(func() error {
				var secret corev1.Secret
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, &secret); err != nil {
					return err
				}
				secret.Data = map[string][]byte{"cluster.xml": []byte("placeholder")}
				return k8sClient.Update(ctx, &secret)
			}, timeout, interval).Should(Succeed())

			// Wait for the cluster reconciler to flip ClusterConfigReady away
			// from True. Without this, the node reconciler could race ahead
			// and apply the Deployment update while the gate still reads True.
			Eventually(func() bool {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hash-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return !hasCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady, metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Patch the node spec while regen is in flight.
			Eventually(func() error {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hash-runtime", Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.Replicas = ptr.To(int32(2))
				return k8sClient.Update(ctx, node)
			}, timeout, interval).Should(Succeed())

			// Gate must defer the Deployment write — Generation stays at the
			// pre-edit value and Spec.Replicas does not change to the new value
			// while ClusterConfigReady != True. This prevents new pods from
			// mounting the placeholder cluster.xml via subPath.
			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hash-cluster", "hash-runtime"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(deploy.Generation).To(Equal(originalGeneration))
				if deploy.Spec.Replicas != nil {
					g.Expect(*deploy.Spec.Replicas).NotTo(Equal(int32(2)))
				}
			}, 5*time.Second, interval).Should(Succeed())

			// Restore the Secret and flip Cond=True. The deferred update must
			// land in a single Generation bump, with the new replicas applied.
			testSimulateClusterConfigReady(ns, "hash-cluster")
			Eventually(func(g Gomega) int32 {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hash-cluster", "hash-runtime"), Namespace: ns}, deploy)).To(Succeed())
				return *deploy.Spec.Replicas
			}, timeout, interval).Should(Equal(int32(2)))
		})
	})

	Context("Cluster label", func() {
		It("should set curity.io/cluster label on the node", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "labeled-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "labeled-node", Namespace: ns}, node)
				return node.Labels["curity.io/cluster"]
			}, timeout, interval).Should(Equal("cluster-1"))
		})

		It("should allow duplicate admin check to work via label-based filtering", func() {
			testCreateCluster(ns, "cluster-x")
			testCreateCluster(ns, "cluster-y")

			// Admin for cluster-x
			testCreateNode(ns, "admin-x", v1alpha1.NodeTypeAdmin, "cluster-x")
			testSimulateClusterConfigReady(ns, "cluster-x")
			eventuallyGetResource(ns, ownedName("cluster-x", "admin-x"), &appsv1.Deployment{})

			// Admin for cluster-y — different cluster, should NOT be blocked
			testCreateNode(ns, "admin-y", v1alpha1.NodeTypeAdmin, "cluster-y")
			testSimulateClusterConfigReady(ns, "cluster-y")
			eventuallyGetResource(ns, ownedName("cluster-y", "admin-y"), &appsv1.Deployment{})
		})
	})

	Context("Happy path — Runtime node", func() {
		It("should create Deployment with --no-admin args", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "runtime-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "runtime-node"), deploy)
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--no-admin"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("--admin"))
		})
	})

	Context("Error paths", func() {
		It("should set Degraded when cluster does not exist", func() {
			testCreateNode(ns, "orphan-node", v1alpha1.NodeTypeRuntime, "nonexistent-cluster")

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-node", Namespace: ns}, node); err != nil {
					return false
				}
				return hasCondition(node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue(), "expected Degraded condition to be set")
		})

		It("should flip ClusterReady=True and clear ClusterNotFound Degraded when the missing cluster is created", func() {
			// The reverse of the orphan test: once the referenced cluster
			// appears, the findNodesForCluster watch enqueues the node (the
			// node was labeled curity.io/cluster=<ref> on its first reconcile,
			// even while orphaned), and the reconciler must drop the
			// ClusterNotFound Degraded reason instead of leaving it stuck.
			testCreateNode(ns, "late-node", v1alpha1.NodeTypeRuntime, "late-cluster")

			By("waiting for orphan node to enter ClusterNotFound Degraded state")
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "late-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal(v1alpha1.ReasonClusterNotFound))

			By("creating the previously missing cluster")
			testCreateCluster(ns, "late-cluster")

			By("expecting ClusterReady to flip True with ReasonClusterFound")
			Eventually(func() bool {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "late-node", Namespace: ns}, node); err != nil {
					return false
				}
				return hasCondition(node.Status.Conditions, v1alpha1.ConditionClusterReady, metav1.ConditionTrue) &&
					conditionReason(node.Status.Conditions, v1alpha1.ConditionClusterReady) == v1alpha1.ReasonClusterFound
			}, timeout, interval).Should(BeTrue(), "expected ClusterReady=True/ClusterFound after cluster creation")

			By("expecting the stale ClusterNotFound Degraded reason to be gone")
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "late-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal(v1alpha1.ReasonClusterNotFound))

			By("expecting the Deployment to be created now that the cluster exists")
			eventuallyGetResource(ns, ownedName("late-cluster", "late-node"), &appsv1.Deployment{})
		})

		It("should block second admin node for same cluster", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "admin-1", v1alpha1.NodeTypeAdmin, "cluster-1")
			testSimulateClusterConfigReady(ns, "cluster-1")

			eventuallyGetResource(ns, ownedName("cluster-1", "admin-1"), &appsv1.Deployment{})

			// Create second admin
			testCreateNode(ns, "admin-2", v1alpha1.NodeTypeAdmin, "cluster-1")

			// Second admin should be Degraded with DuplicateAdmin reason
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "admin-2", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateAdmin"))

			// Verify no Deployment was created for admin-2
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-1", "admin-2"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		})

		It("should block node with duplicate role in same cluster", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "role-node-1", v1alpha1.NodeTypeRuntime, "cluster-1")

			eventuallyGetResource(ns, ownedName("cluster-1", "role-node-1"), &appsv1.Deployment{})

			// Create second node with explicit duplicate role
			dupNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "role-node-dup", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "role-node-1-role", // same role as role-node-1
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-1"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, dupNode)).To(Succeed())

			// Should get Degraded with DuplicateRole reason
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "role-node-dup", Namespace: ns}, dupNode); err != nil {
					return ""
				}
				return conditionReason(dupNode.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateRole"))

			Expect(hasCondition(dupNode.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)).To(BeTrue())

			// No Deployment for the duplicate
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-1", "role-node-dup"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		})

		It("should clear DuplicateRole on the originally-stuck node after the conflicting peer's role is changed", func() {
			// Both nodes end up Degraded once the duplicate exists (whichever
			// reconciles second sets itself Degraded; the cluster reconciler's
			// NodeCount bump then re-queues the first via the existing
			// clusterSpecOrNodeCountChangedPredicate, which also goes
			// Degraded). After the conflicting peer mutates its role, only
			// that peer auto-reconciles via its Generation bump; the
			// originally-stuck peer needs the validation-branch RequeueAfter
			// to recover, since a peer's spec mutation does not change
			// NodeCount.
			testCreateCluster(ns, "recovery-cluster")
			testCreateNode(ns, "stuck-node", v1alpha1.NodeTypeRuntime, "recovery-cluster")
			eventuallyGetResource(ns, ownedName("recovery-cluster", "stuck-node"), &appsv1.Deployment{})

			dupNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "peer-node", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "stuck-node-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "recovery-cluster"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, dupNode)).To(Succeed())

			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateRole"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "peer-node", Namespace: ns}, dupNode)).To(Succeed())
			dupNode.Spec.Role = "stuck-node-role-w"
			Expect(k8sClient.Update(ctx, dupNode)).To(Succeed())

			// 60s allows for one full RequeueAfter cycle (30s) plus reconcile time.
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 60*time.Second, interval).ShouldNot(Equal("DuplicateRole"))
		})

		It("should clear DuplicateAdmin on the originally-stuck admin after the conflicting peer's type is changed", func() {
			testCreateCluster(ns, "admin-recovery-cluster")
			testCreateNode(ns, "stuck-admin", v1alpha1.NodeTypeAdmin, "admin-recovery-cluster")
			testSimulateClusterConfigReady(ns, "admin-recovery-cluster")
			eventuallyGetResource(ns, ownedName("admin-recovery-cluster", "stuck-admin"), &appsv1.Deployment{})

			testCreateNode(ns, "peer-admin", v1alpha1.NodeTypeAdmin, "admin-recovery-cluster")

			// 60s (not the 30s default) to ride out variable CI scheduling:
			// the stuck-admin's Degraded=DuplicateAdmin is set only AFTER
			// the cluster-watch fires from peer-admin's creation event
			// (incrementing Status.NodeCount), which can lag under heavy
			// envtest reconciler load. Matches the 60s timeout on the
			// recovery Eventually below.
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 60*time.Second, interval).Should(Equal("DuplicateAdmin"))

			// Retry on conflict: the reconciler also writes the peer's
			// finalizer / labels concurrently with our spec change. Matches
			// the patchClusterRef helper added in clusterref_move_test.go.
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				peer := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "peer-admin", Namespace: ns}, peer); err != nil {
					return err
				}
				peer.Spec.Type = v1alpha1.NodeTypeRuntime
				peer.Spec.Role = "peer-admin-runtime-role"
				return k8sClient.Update(ctx, peer)
			})).To(Succeed())

			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 60*time.Second, interval).ShouldNot(Equal("DuplicateAdmin"))
		})

		It("should give two ambiguous-name nodes their own Deployments without collision", func() {
			// Regression guard for the cross-cluster ownedResourceName
			// ambiguity that the old code defended against with a runtime
			// collision check. With the SHA-256 suffix, ("foo-bar","baz")
			// and ("foo","bar-baz") produce distinct names — both nodes
			// run successfully, no Degraded condition, no events.
			testCreateCluster(ns, "foo-bar")
			testCreateCluster(ns, "foo")

			testCreateNode(ns, "baz", v1alpha1.NodeTypeRuntime, "foo-bar")
			testCreateNode(ns, "bar-baz", v1alpha1.NodeTypeRuntime, "foo")

			nameA := ownedName("foo-bar", "baz")
			nameB := ownedName("foo", "bar-baz")
			Expect(nameA).NotTo(Equal(nameB), "ambiguous pair must hash to distinct names")

			eventuallyGetResource(ns, nameA, &appsv1.Deployment{})
			eventuallyGetResource(ns, nameB, &appsv1.Deployment{})

			// Neither node ends up Degraded for collision. (DuplicateRole
			// is scoped per-cluster, so two nodes in different clusters
			// never collide on role regardless of the role string used.)
			nodeA := &v1alpha1.IdentityServerNode{}
			nodeB := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "baz", Namespace: ns}, nodeA)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "bar-baz", Namespace: ns}, nodeB)).To(Succeed())
			Expect(conditionReason(nodeA.Status.Conditions, v1alpha1.ConditionDegraded)).NotTo(Equal("OwnedResourceNameCollision"))
			Expect(conditionReason(nodeB.Status.Conditions, v1alpha1.ConditionDegraded)).NotTo(Equal("OwnedResourceNameCollision"))
		})

		It("should allow same role in different clusters", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")

			// Same role "shared-role" in two different clusters — should both work
			nodeA := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-a", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "shared-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-a"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())

			nodeB := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-b", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "shared-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-b"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())

			// Both should get Deployments
			eventuallyGetResource(ns, ownedName("cluster-a", "node-a"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("cluster-b", "node-b"), &appsv1.Deployment{})
		})
	})

	Context("Deletion", func() {
		It("should clean up via OwnerReference garbage collection", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "del-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "del-node"), deploy)

			Expect(deploy.OwnerReferences).To(HaveLen(1))
			Expect(deploy.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

			// Delete the node
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "del-node", Namespace: ns}, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			eventuallyDeleted(ns, "del-node", &v1alpha1.IdentityServerNode{})
		})
	})

	// Regression for the invalid-spec retry storm: a CR with podLabels that
	// the apiserver rejects must surface Degraded=InvalidSpec instead of
	// hot-looping silently. On `main` before the fix, Degraded stayed False
	// indefinitely and the operator logged 41 errors in 10 min for one CR.
	Context("InvalidSpec — permanent error classification", func() {
		It("should set Degraded=InvalidSpec when podLabels are syntactically invalid", func() {
			testCreateCluster(ns, "is-cluster")
			testCreateNode(ns, "is-admin", v1alpha1.NodeTypeAdmin, "is-cluster")
			testSimulateClusterConfigReady(ns, "is-cluster")
			eventuallyGetResource(ns, ownedName("is-cluster", "is-admin"), &appsv1.Deployment{})

			badNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "is-rt", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "is-rt-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "is-cluster"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					PodLabels:                map[string]string{"bad/label": "with spaces and !"},
				},
			}
			Expect(k8sClient.Create(ctx, badNode)).To(Succeed())

			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "is-rt", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal(v1alpha1.ReasonInvalidSpec))

			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "is-rt", Namespace: ns}, node)).To(Succeed())
			Expect(hasCondition(node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue)).To(BeTrue())
			Expect(hasCondition(node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)).To(BeTrue())
			Expect(conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)).To(Equal(v1alpha1.ReasonInvalidSpec))

			// No Deployment should have been created for the bad-spec runtime node.
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("is-cluster", "is-rt"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		})

		It("should clear Degraded=InvalidSpec after the spec is fixed", func() {
			testCreateCluster(ns, "isf-cluster")
			testCreateNode(ns, "isf-admin", v1alpha1.NodeTypeAdmin, "isf-cluster")
			testSimulateClusterConfigReady(ns, "isf-cluster")
			eventuallyGetResource(ns, ownedName("isf-cluster", "isf-admin"), &appsv1.Deployment{})

			badNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "isf-rt", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "isf-rt-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "isf-cluster"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					PodLabels:                map[string]string{"bad/label": "with spaces and !"},
				},
			}
			Expect(k8sClient.Create(ctx, badNode)).To(Succeed())

			By("waiting for Degraded=InvalidSpec on the bad spec")
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "isf-rt", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal(v1alpha1.ReasonInvalidSpec))

			By("patching to a valid podLabels value")
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "isf-rt", Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.PodLabels = map[string]string{"team": "identity"}
				return k8sClient.Update(ctx, node)
			})).To(Succeed())

			By("expecting Degraded to leave InvalidSpec and the Deployment to be created")
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "isf-rt", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal(v1alpha1.ReasonInvalidSpec))

			eventuallyGetResource(ns, ownedName("isf-cluster", "isf-rt"), &appsv1.Deployment{})
		})
	})

	Context("Updates", func() {
		It("should update Deployment when replicas change", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "update-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "update-node"), deploy)

			// Update replicas
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-node", Namespace: ns}, node)).To(Succeed())
			node.Spec.Replicas = ptr.To(int32(3))
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			// Verify Deployment replicas updated
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-1", "update-node"), Namespace: ns}, deploy)
				if deploy.Spec.Replicas == nil {
					return 0
				}
				return *deploy.Spec.Replicas
			}, timeout, interval).Should(Equal(int32(3)))
		})
	})

	Context("Label preservation", func() {
		It("preserves foreign labels on Service across reconciles", func() {
			testCreateCluster(ns, "lbl-cluster")
			testCreateNode(ns, "lbl-node", v1alpha1.NodeTypeRuntime, "lbl-cluster")

			svc := &corev1.Service{}
			eventuallyGetResource(ns, ownedName("lbl-cluster", "lbl-node"), svc)

			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("lbl-cluster", "lbl-node"), Namespace: ns}, current); err != nil {
					return err
				}
				if current.Labels == nil {
					current.Labels = map[string]string{}
				}
				current.Labels["monitoring"] = "prometheus"
				current.Labels["argocd.argoproj.io/instance"] = "my-app"
				current.Labels["app.kubernetes.io/version"] = "hijacked"
				return k8sClient.Update(ctx, current)
			})).To(Succeed())

			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lbl-node", Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.Replicas = ptr.To(int32(2))
				return k8sClient.Update(ctx, node)
			})).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("lbl-cluster", "lbl-node"), Namespace: ns}, svc)).To(Succeed())
				g.Expect(svc.Labels).To(HaveKeyWithValue("monitoring", "prometheus"))
				g.Expect(svc.Labels).To(HaveKeyWithValue("argocd.argoproj.io/instance", "my-app"))
				g.Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/version", "11.0"))
				g.Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "curity-operator"))
			}, timeout, interval).Should(Succeed())
		})

		It("preserves foreign labels on Deployment across reconciles", func() {
			testCreateCluster(ns, "lbl-cluster")
			testCreateNode(ns, "lbl-node", v1alpha1.NodeTypeRuntime, "lbl-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("lbl-cluster", "lbl-node"), deploy)

			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("lbl-cluster", "lbl-node"), Namespace: ns}, current); err != nil {
					return err
				}
				if current.Labels == nil {
					current.Labels = map[string]string{}
				}
				current.Labels["monitoring"] = "prometheus"
				current.Labels["argocd.argoproj.io/instance"] = "my-app"
				return k8sClient.Update(ctx, current)
			})).To(Succeed())

			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lbl-node", Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.Replicas = ptr.To(int32(2))
				return k8sClient.Update(ctx, node)
			})).To(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("lbl-cluster", "lbl-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Replicas).ToNot(BeNil())
				g.Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
				g.Expect(deploy.Labels).To(HaveKeyWithValue("monitoring", "prometheus"))
				g.Expect(deploy.Labels).To(HaveKeyWithValue("argocd.argoproj.io/instance", "my-app"))
				g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "curity-operator"))
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("Config discovery", func() {
		It("should mount validated base ConfigMap on Deployment", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			// Create managed ConfigMap
			testCreateManagedConfigMap(ns, "my-config", map[string]string{"init.xml": "<config/>"}, nil)

			// Verify config volume appears on Deployment
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-my-config")).To(BeTrue(), "should have cfg-cm-my-config volume")
				g.Expect(hasVolumeMount(deploy, "cfg-cm-my-config", "/opt/idsvr/etc/init/cm_my-config_init.xml")).To(BeTrue(), "should mount at init path")
			}, timeout, interval).Should(Succeed())

			// Verify config-hash annotation on pod template
			Expect(deploy.Spec.Template.Annotations).To(HaveKey("curity.io/managed-configs-hash"))
		})

		It("should route configs only to admin when admin exists", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-admin", v1alpha1.NodeTypeAdmin, "cfg-cluster")
			testSimulateClusterConfigReady(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-runtime", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			adminDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-admin"), adminDeploy)
			runtimeDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-runtime"), runtimeDeploy)

			testCreateManagedConfigMap(ns, "admin-only-config", map[string]string{"settings.xml": "<settings/>"}, nil)

			// Admin should get the config
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-admin"), Namespace: ns}, adminDeploy)).To(Succeed())
				g.Expect(hasVolumeName(adminDeploy, "cfg-cm-admin-only-config")).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			// Runtime should NOT get the config
			Consistently(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-runtime"), Namespace: ns}, runtimeDeploy)
				return countCfgVolumes(runtimeDeploy)
			}, 5*time.Second, interval).Should(Equal(0), "runtime should not get config when admin exists")
		})

		It("should mount all configs on runtime when no admin exists", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-runtime", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-runtime"), deploy)

			testCreateManagedConfigMap(ns, "runtime-config", map[string]string{"app.xml": "<app/>"}, nil)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-runtime"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-runtime-config")).To(BeTrue())
				g.Expect(hasVolumeMount(deploy, "cfg-cm-runtime-config", "/opt/idsvr/etc/init/cm_runtime-config_app.xml")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should mount license Secret at license path", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			testCreateManagedSecret(ns, "my-license",
				map[string][]byte{"license.json": []byte(`{"key":"value"}`)},
				map[string]string{"curity.io/config-type": "license"})

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-secret-my-license")).To(BeTrue())
				g.Expect(hasVolumeMount(deploy, "cfg-secret-my-license", "/opt/idsvr/etc/init/license/secret_my-license_license.json")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should skip a CM with unknown config type but still mount its valid neighbor", func() {
			// A single typo must NOT cascade into everyone — the valid
			// neighbor must still get mounted. End-to-end asserts the
			// skip-the-offender behavior all the way through: discovery
			// skips the bad CM, cluster validates the good one, node
			// mounts the validated one.
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			// Bad neighbor: invalid config-type value.
			testCreateManagedConfigMap(ns, "bad-type-config",
				map[string]string{"data.xml": "<data/>"},
				map[string]string{"curity.io/config-type": "invalid"})

			// Good neighbor: valid config-type, will be mounted.
			testCreateManagedConfigMap(ns, "good-neighbor",
				map[string]string{"init.xml": "<config/>"},
				map[string]string{"curity.io/config-type": "base"})

			By("expecting the valid neighbor to mount while the bad one is skipped")
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(countCfgVolumes(deploy)).To(Equal(1),
					"only the valid CM should be mounted; a typo in one CM must not block its neighbor")
				g.Expect(hasVolumeName(deploy, "cfg-cm-good-neighbor")).To(BeTrue(),
					"good-neighbor must be mounted")
				g.Expect(hasVolumeName(deploy, "cfg-cm-bad-type-config")).To(BeFalse(),
					"bad-type-config must be skipped")
			}, timeout, interval).Should(Succeed())

			By("expecting a Warning Event on the offending ConfigMap (not on the node)")
			Eventually(func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "UnknownConfigType" &&
						e.InvolvedObject.Kind == "ConfigMap" &&
						e.InvolvedObject.Name == "bad-type-config" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(),
				"expected Warning/UnknownConfigType event on ConfigMap/bad-type-config")

			By("expecting NO UnknownConfigType event on the node (regression guard for the duplicate emit)")
			Consistently(func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "UnknownConfigType" && e.InvolvedObject.Kind == "IdentityServerNode" {
						return false
					}
				}
				return true
			}, "2s", "200ms").Should(BeTrue(),
				"node-side UnknownConfigType emit was removed; only the CM should carry it")
		})

		It("should update config-hash annotation when config data changes", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			testCreateManagedConfigMap(ns, "mutable-config", map[string]string{"data.xml": "<v1/>"}, nil)

			// Wait for initial hash
			var initialHash string
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)
				initialHash = deploy.Spec.Template.Annotations["curity.io/managed-configs-hash"]
				return initialHash
			}, timeout, interval).ShouldNot(BeEmpty())

			// Update ConfigMap data
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mutable-config", Namespace: ns}, cm)).To(Succeed())
			cm.Data["data.xml"] = "<v2/>"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Verify hash changed
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)
				return deploy.Spec.Template.Annotations["curity.io/managed-configs-hash"]
			}, timeout, interval).ShouldNot(Equal(initialHash))
		})

		It("should remove config volumes when label is removed", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			testCreateManagedConfigMap(ns, "removable-config", map[string]string{"init.xml": "<init/>"}, nil)

			// Wait for volume to appear
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-removable-config")).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			// Remove the managed label
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "removable-config", Namespace: ns}, cm)).To(Succeed())
			delete(cm.Labels, "curity.io/managed")
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Verify volume removed
			Eventually(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)
				return countCfgVolumes(deploy)
			}, timeout, interval).Should(Equal(0), "config volume should be removed after label removal")
		})

		It("should populate AppliedManagedResources with discovered managed configs", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), &appsv1.Deployment{})

			testCreateManagedConfigMap(ns, "status-config", map[string]string{"cfg.xml": "<cfg/>"}, nil)

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, node)).To(Succeed())
				g.Expect(node.Status.AppliedManagedResources).To(HaveLen(1))
				g.Expect(node.Status.AppliedManagedResources[0].Name).To(Equal("status-config"))
				g.Expect(node.Status.AppliedManagedResources[0].Kind).To(Equal("ConfigMap"))
				g.Expect(node.Status.AppliedManagedResources[0].ConfigType).To(Equal("base"))
			}, timeout, interval).Should(Succeed())
		})

		It("should mount multiple configs in deterministic order", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			// Create configs with names that sort differently than creation order
			testCreateManagedConfigMap(ns, "zzz-config", map[string]string{"z.xml": "<z/>"}, nil)
			testCreateManagedConfigMap(ns, "aaa-config", map[string]string{"a.xml": "<a/>"}, nil)
			testCreateManagedSecret(ns, "mmm-secret",
				map[string][]byte{"m.xml": []byte("<m/>")}, nil)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(countCfgVolumes(deploy)).To(Equal(3))
				g.Expect(hasVolumeName(deploy, "cfg-cm-aaa-config")).To(BeTrue())
				g.Expect(hasVolumeName(deploy, "cfg-cm-zzz-config")).To(BeTrue())
				g.Expect(hasVolumeName(deploy, "cfg-secret-mmm-secret")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should re-scope a managed ConfigMap from cluster A to cluster B in one reconcile cycle", func() {
			// Feature's headline flow: editing curity.io/cluster from A to B
			// must make A lose the volume AND B gain it inside one Eventually.
			// The managedConfigHandler unions old+new scopes so both sides
			// get enqueued; this test verifies end-to-end convergence, not
			// just enqueue semantics.
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")
			testCreateNode(ns, "node-a", v1alpha1.NodeTypeRuntime, "cluster-a")
			testCreateNode(ns, "node-b", v1alpha1.NodeTypeRuntime, "cluster-b")

			deployA := &appsv1.Deployment{}
			deployB := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-a", "node-a"), deployA)
			eventuallyGetResource(ns, ownedName("cluster-b", "node-b"), deployB)

			// Managed ConfigMap initially scoped to cluster-a only.
			testCreateManagedConfigMap(ns, "rescope-cm",
				map[string]string{"init.xml": "<config/>"},
				map[string]string{"curity.io/cluster": "cluster-a"},
			)

			By("asserting A has the volume and B does not (initial scope)")
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-a", "node-a"), Namespace: ns}, deployA)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-b", "node-b"), Namespace: ns}, deployB)).To(Succeed())
				g.Expect(hasVolumeName(deployA, "cfg-cm-rescope-cm")).To(BeTrue(), "A should mount the CM")
				g.Expect(countCfgVolumes(deployB)).To(Equal(0), "B should have no cfg volume yet")
			}, timeout, interval).Should(Succeed())

			By("flipping the ConfigMap's curity.io/cluster annotation from A to B")
			Eventually(func() error {
				var cm corev1.ConfigMap
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rescope-cm", Namespace: ns}, &cm); err != nil {
					return err
				}
				cm.Annotations["curity.io/cluster"] = "cluster-b"
				return k8sClient.Update(ctx, &cm)
			}, timeout, interval).Should(Succeed())

			By("asserting A loses the volume AND B gains it (both sides converge)")
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-a", "node-a"), Namespace: ns}, deployA)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-b", "node-b"), Namespace: ns}, deployB)).To(Succeed())
				g.Expect(countCfgVolumes(deployA)).To(Equal(0), "A must drop the CM volume after re-scope")
				g.Expect(hasVolumeName(deployB, "cfg-cm-rescope-cm")).To(BeTrue(), "B must mount the CM after re-scope")
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("PodDisruptionBudget", func() {
		const timeout = 30 * time.Second
		const interval = 250 * time.Millisecond

		var ns string

		BeforeEach(func() {
			ns = nodeTestNamespace()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		})

		It("creates PDB when cluster sets minAvailable and node does not override", func() {
			clusterName := "cluster-pdb-1"
			nodeName := "node-pdb-1"
			min := intstr.FromInt32(2)

			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:             "11.0",
					PodDisruptionBudget: &v1alpha1.PDBSpec{MinAvailable: &min},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			// Inheritance is the whole point of this test — if a future
			// testCreateNode helper starts defaulting PodDisruptionBudget,
			// the test would silently pass for the wrong reason.
			var freshNode v1alpha1.IdentityServerNode
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &freshNode)).To(Succeed())
			Expect(freshNode.Spec.PodDisruptionBudget).To(BeNil(), "node must not set PDB itself")

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(2))
			Expect(pdb.OwnerReferences).To(HaveLen(1))
			Expect(pdb.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
			Expect(pdb.OwnerReferences[0].Name).To(Equal(nodeName))
		})

		It("creates PDB with percentage minAvailable when set on node", func() {
			clusterName := "cluster-pdb-pct"
			nodeName := "node-pdb-pct"
			testCreateCluster(ns, clusterName)
			min := intstr.FromString("50%")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(4)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.Type).To(Equal(intstr.String))
			Expect(pdb.Spec.MinAvailable.StrVal).To(Equal("50%"))
		})

		It("deletes PDB when field is removed from node spec", func() {
			clusterName := "cluster-pdb-del"
			nodeName := "node-pdb-del"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})

			// Clear the PDB spec.
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Spec.PodDisruptionBudget = nil
				return k8sClient.Update(ctx, &fresh)
			}, timeout, interval).Should(Succeed())

			eventuallyDeleted(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})
		})

		It("does not create PDB on admin node even when set", func() {
			clusterName := "cluster-pdb-admin"
			nodeName := "node-pdb-admin"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			// Wait a bit for reconcile, then confirm no PDB exists.
			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &policyv1.PodDisruptionBudget{}))
			}, 5*time.Second, 500*time.Millisecond).Should(BeTrue())
		})

		It("does not delete PDB that is not owned by the node", func() {
			clusterName := "cluster-pdb-notowned"
			nodeName := "node-pdb-notowned"
			testCreateCluster(ns, clusterName)

			// Pre-create a user PDB with the target name but no owner ref.
			min := intstr.FromInt32(7)
			userPDB := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, nodeName), Namespace: ns},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &min,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"user-pdb": "true"}},
				},
			}
			Expect(k8sClient.Create(ctx, userPDB)).To(Succeed())

			// Create a node with no PDB spec — cleanup path runs.
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			// The pre-existing PDB must still be there with its original values.
			Consistently(func() error {
				var pdb policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &pdb); err != nil {
					return err
				}
				if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 7 {
					return fmt.Errorf("user PDB was mutated")
				}
				return nil
			}, 5*time.Second, 500*time.Millisecond).Should(Succeed())

			// The node's Degraded condition should carry reason PDBNotOwned.
			Eventually(func() string {
				var node v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("PDBNotOwned"))
		})

		It("emits PDBUnsatisfiable only when PDB status is fresh", func() {
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-unsat"
			nodeName := "node-pdb-unsat"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctxLocal, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
			pdbGen := pdb.Generation

			hasUnsatisfiableEvent := func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctxLocal, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "PDBUnsatisfiable" && e.InvolvedObject.Name == nodeName {
						return true
					}
				}
				return false
			}

			By("writing a STALE status with ObservedGeneration=0 and zero healthy counts")
			Eventually(func() error {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Status = policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 0, // intentionally behind fresh.Generation
					DisruptionsAllowed: 0,
					CurrentHealthy:     0,
					DesiredHealthy:     0,
				}
				return k8sClient.Status().Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("nudging the node to trigger a reconcile")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				if fresh.Annotations == nil {
					fresh.Annotations = map[string]string{}
				}
				fresh.Annotations["smoke.curity.io/nudge"] = "stale"
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying NO PDBUnsatisfiable event fires while status is stale")
			Consistently(hasUnsatisfiableEvent, 3*time.Second, 250*time.Millisecond).Should(BeFalse())

			By("writing a FRESH unsatisfiable status with ObservedGeneration matching the PDB's Generation")
			Eventually(func() error {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Status = policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: pdbGen,
					DisruptionsAllowed: 0,
					CurrentHealthy:     2,
					DesiredHealthy:     5, // exceeds CurrentHealthy — unsatisfiable
				}
				return k8sClient.Status().Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("nudging again to trigger reconcile on fresh status")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Annotations["smoke.curity.io/nudge"] = "fresh"
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying PDBUnsatisfiable event fires once status is fresh")
			Eventually(hasUnsatisfiableEvent, timeout, interval).Should(BeTrue())
		})

		It("clears Degraded=PDBNotOwned when the foreign PDB is deleted", func() {
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-clear"
			nodeName := "node-pdb-clear"
			testCreateCluster(ns, clusterName)

			By("pre-creating an unowned PDB with the target node's name")
			foreignMin := intstr.FromInt32(7)
			foreignPDB := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, nodeName), Namespace: ns},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &foreignMin,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"user-pdb": "true"}},
				},
			}
			Expect(k8sClient.Create(ctxLocal, foreignPDB)).To(Succeed())

			By("creating the runtime node with no PDB spec — cleanup branch will see the collision")
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			By("waiting for Degraded=True with reason PDBNotOwned")
			Eventually(func() string {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return ""
				}
				return conditionReason(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("PDBNotOwned"))

			By("deleting the foreign PDB to resolve the collision")
			Expect(k8sClient.Delete(ctxLocal, foreignPDB)).To(Succeed())

			By("nudging the node to trigger a reconcile")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				if fresh.Annotations == nil {
					fresh.Annotations = map[string]string{}
				}
				fresh.Annotations["smoke.curity.io/nudge"] = "cleared"
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying Degraded's reason is no longer PDBNotOwned — computeNodeConditions takes over")
			Eventually(func() string {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return ""
				}
				return conditionReason(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal("PDBNotOwned"))
		})

		It("deletes operator-owned PDB and suppresses PDBDeleted when node type flips runtime→admin", func() {
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-flip"
			nodeName := "node-pdb-flip"
			testCreateCluster(ns, clusterName)

			By("creating a runtime node with PDB so the operator creates a PDB it owns")
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
				},
			}
			Expect(k8sClient.Create(ctxLocal, node)).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})

			By("flipping the node type to admin")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Spec.Type = v1alpha1.NodeTypeAdmin
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying the operator-owned PDB is deleted")
			eventuallyDeleted(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})

			hasEvent := func(reason string) bool {
				var events corev1.EventList
				if err := k8sClient.List(ctxLocal, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == reason && e.InvolvedObject.Name == nodeName {
						return true
					}
				}
				return false
			}

			By("verifying PDBIgnored event fires (admin guard)")
			Eventually(func() bool { return hasEvent("PDBIgnored") }, timeout, interval).Should(BeTrue())

			By("verifying PDBDeleted is suppressed on admin path — PDBIgnored already covers the signal")
			Consistently(func() bool { return hasEvent("PDBDeleted") }, 3*time.Second, 250*time.Millisecond).Should(BeFalse())
		})

		It("reverts unauthorized PDB spec mutations quickly (proves Owns watch is wired)", func() {
			// A PDB-only mutation wouldn't trigger reconcile from any of the
			// other Owns (Deployment/Service/HPA) or cluster watches, so a
			// sub-second revert proves Owns(&PodDisruptionBudget{}) in
			// SetupWithManager is firing. If Owns were removed, the mutated
			// spec would persist until some other unrelated watch event
			// (or the long default resync) triggered reconcile.
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-owns"
			nodeName := "node-pdb-owns"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctxLocal, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)

			By("mutating the PDB's MinAvailable directly")
			bogus := intstr.FromInt32(99)
			Eventually(func() error {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Spec.MinAvailable = &bogus
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("expecting fast revert to the desired value — this is the Owns watch firing")
			Eventually(func() int {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil || fresh.Spec.MinAvailable == nil {
					return -1
				}
				return fresh.Spec.MinAvailable.IntValue()
			}, 2*time.Second, 100*time.Millisecond).Should(Equal(1))
		})

		It("re-creates PDB if deleted manually", func() {
			clusterName := "cluster-pdb-recreate"
			nodeName := "node-pdb-recreate"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)

			// Delete the PDB and watch it come back.
			originalUID := pdb.UID
			Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())
			Eventually(func() bool {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return false
				}
				return fresh.UID != originalUID
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("HPA hardening", func() {
		const timeout = 30 * time.Second
		const interval = 250 * time.Millisecond

		var ns string

		BeforeEach(func() {
			ns = nodeTestNamespace()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		})

		// countEvents returns the total number of Recorder.Eventf calls made
		// for (objName, reason), summing the dedup Count field. K8s server-side
		// dedup merges emits with the same (InvolvedObject, Reason, Message)
		// into a single Event object with Count incremented — so to verify
		// "exactly one emit" or "exactly two emits" we must read Count, not
		// the number of Event objects.
		countEvents := func(objName, reason string) int {
			var events corev1.EventList
			if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
				return -1
			}
			n := 0
			for _, e := range events.Items {
				if e.InvolvedObject.Name == objName && e.Reason == reason {
					if e.Count > 0 {
						n += int(e.Count)
					} else {
						n++
					}
				}
			}
			return n
		}

		// getHPAGen fetches the current Generation of the operator-owned HPA.
		// envtest's apiserver leaves Generation at 0; real K8s gives Generation=1.
		// Returning the live value lets tests construct freshness assertions that
		// work in both environments.
		getHPAGen := func(clusterName, nodeName string) int64 {
			var h autoscalingv2.HorizontalPodAutoscaler
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: ownedName(clusterName, nodeName), Namespace: ns,
			}, &h)).To(Succeed())
			return h.Generation
		}

		// nudgeNode bumps a label on the node without touching spec, so
		// node.Generation does NOT change. Used to trigger a reconcile without
		// inducing a Generation-based gate transition.
		nudgeNode := func(nsName, name string) {
			Eventually(func() error {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, &n); err != nil {
					return err
				}
				if n.Labels == nil {
					n.Labels = map[string]string{}
				}
				n.Labels["hpa-test.nudge/seq"] = fmt.Sprintf("%d", time.Now().UnixNano())
				return k8sClient.Update(ctx, &n)
			}, timeout, interval).Should(Succeed())
		}

		// nudgeNodeAndWait: label-nudge + settle Sleep. ResourceVersion is
		// not a reliable completion signal — apiserver elides no-op Status
		// writes, so RV may not advance.
		const nudgeSettle = 1500 * time.Millisecond
		nudgeNodeAndWait := func(nsName, name string) {
			GinkgoHelper()
			Eventually(func() error {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, &n); err != nil {
					return err
				}
				if n.Labels == nil {
					n.Labels = map[string]string{}
				}
				n.Labels["hpa-test.nudge/seq"] = fmt.Sprintf("%d", time.Now().UnixNano())
				return k8sClient.Update(ctx, &n)
			}, timeout, interval).Should(Succeed())
			time.Sleep(nudgeSettle)
		}

		runtimeNodeWithAutoscaling := func(nsName, clusterName, nodeName string) *v1alpha1.IdentityServerNode {
			return &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: nsName},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(1)),
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled:                        true,
						MinReplicas:                    2,
						MaxReplicas:                    5,
						TargetCPUUtilizationPercentage: 80,
					},
					// Resources.Requests.cpu satisfies Gate D's pre-flight check.
					// Tests that exercise the missing-request path strip this
					// inline before Create.
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
					Service: defaultTestService(),
				},
			}
		}

		// U1 — Positive baseline: healthy runtime + autoscaling, no Warnings.
		It("emits HPAReconciled and no warnings on healthy runtime node with autoscaling", func() {
			clusterName := "hpa-u1"
			nodeName := "node-u1"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())

			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			Consistently(func() bool {
				return countEvents(nodeName, "AutoscalingIgnored") == 0 &&
					countEvents(nodeName, "HPANotOwned") == 0 &&
					countEvents(nodeName, "HPAUnsatisfiable") == 0
			}, 3*time.Second, 500*time.Millisecond).Should(BeTrue())
		})

		// U2a — Negative: foreign HPA pre-exists, Degraded=HPANotOwned set.
		It("sets Degraded=HPANotOwned when a foreign HPA exists with the operator's owned name", func() {
			clusterName := "hpa-u2a"
			nodeName := "node-u2a"
			testCreateCluster(ns, clusterName)

			foreignHPA := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, nodeName), Namespace: ns},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1", Kind: "Deployment", Name: "some-other-deployment",
					},
					MinReplicas: ptr.To(int32(1)),
					MaxReplicas: 3,
				},
			}
			Expect(k8sClient.Create(ctx, foreignHPA)).To(Succeed())

			// Create runtime node with NO autoscaling — cleanup branch runs.
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("HPANotOwned"))

			// Foreign HPA's spec must not be touched.
			Consistently(func() error {
				var hpa autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &hpa); err != nil {
					return err
				}
				if hpa.Spec.MaxReplicas != 3 {
					return fmt.Errorf("foreign HPA spec was mutated")
				}
				return nil
			}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
		})

		// U2b — Event emission: collision produces at least one HPANotOwned
		// Warning. Gate C matches the PDBNotOwned pattern (emit every
		// reconcile, rely on K8s server-side Event dedup to fold repeats into
		// a single Event with incrementing count) rather than maintaining an
		// in-memory transition cache.
		It("emits at least one HPANotOwned Warning while the collision persists", func() {
			clusterName := "hpa-u2b"
			nodeName := "node-u2b"
			testCreateCluster(ns, clusterName)

			foreignHPA := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, nodeName), Namespace: ns},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1", Kind: "Deployment", Name: "other",
					},
					MinReplicas: ptr.To(int32(1)), MaxReplicas: 3,
				},
			}
			Expect(k8sClient.Create(ctx, foreignHPA)).To(Succeed())
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			Eventually(func() int { return countEvents(nodeName, "HPANotOwned") },
				timeout, interval).Should(BeNumerically(">=", 1))
		})

		// U2c — Lifecycle: Degraded condition clears after the foreign HPA is
		// deleted, and re-asserts on re-introduction. Condition surfacing is
		// the user-visible contract; specific event counts are not.
		It("clears Degraded=HPANotOwned on resolve and re-asserts on re-introduction", func() {
			clusterName := "hpa-u2c"
			nodeName := "node-u2c"
			testCreateCluster(ns, clusterName)

			hpaName := ownedName(clusterName, nodeName)
			mkForeign := func() *autoscalingv2.HorizontalPodAutoscaler {
				return &autoscalingv2.HorizontalPodAutoscaler{
					ObjectMeta: metav1.ObjectMeta{Name: hpaName, Namespace: ns},
					Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							APIVersion: "apps/v1", Kind: "Deployment", Name: "other",
						},
						MinReplicas: ptr.To(int32(1)), MaxReplicas: 3,
					},
				}
			}
			Expect(k8sClient.Create(ctx, mkForeign())).To(Succeed())
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)
			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("HPANotOwned"))

			// Resolve: delete foreign HPA. The Owns watch only fires for
			// HPAs with an owner-ref to our node — the foreign HPA has no such
			// ref, so deletion does NOT wake our reconciler. Nudge the node
			// so the cleanup branch re-runs.
			Expect(k8sClient.Delete(ctx, mkForeign())).To(Succeed())
			nudgeNode(ns, nodeName)
			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal("HPANotOwned"))

			// Re-introduce. Same Owns-watch limitation; nudge again.
			Expect(k8sClient.Create(ctx, mkForeign())).To(Succeed())
			nudgeNode(ns, nodeName)
			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("HPANotOwned"))
		})

		// U3 — Lifecycle: Degraded clears when foreign HPA deleted.
		It("clears Degraded=HPANotOwned when the foreign HPA is deleted", func() {
			clusterName := "hpa-u3"
			nodeName := "node-u3"
			testCreateCluster(ns, clusterName)
			hpaName := ownedName(clusterName, nodeName)

			foreignHPA := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: hpaName, Namespace: ns},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1", Kind: "Deployment", Name: "other",
					},
					MinReplicas: ptr.To(int32(1)), MaxReplicas: 3,
				},
			}
			Expect(k8sClient.Create(ctx, foreignHPA)).To(Succeed())
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n)
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("HPANotOwned"))

			// Foreign HPA has no owner-ref to our node; deleting it does not
			// trigger a reconcile via Owns. Nudge the node so cleanup runs.
			Expect(k8sClient.Delete(ctx, foreignHPA)).To(Succeed())
			nudgeNode(ns, nodeName)

			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n)
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal("HPANotOwned"))
		})

		// U4a — Age-grace: HPAs younger than HPAHealthGracePeriod are exempt.
		// Suite default is 0; this spec opts back into a non-zero grace.
		It("suppresses HPAUnsatisfiable for HPAs younger than the grace window", func() {
			savedGrace := controller.HPAHealthGracePeriod
			controller.HPAHealthGracePeriod = 30 * time.Second
			defer func() { controller.HPAHealthGracePeriod = savedGrace }()

			clusterName := "hpa-u4a"
			nodeName := "node-u4a"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			hpaName := ownedName(clusterName, nodeName)

			// HPA is fresh (<30s). Even with a False condition, no event fires.
			Eventually(func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
					Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
						{
							Type: autoscalingv2.AbleToScale, Status: corev1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             "FailedGetScale", Message: "deployment not found (test)",
						},
					},
				}
				return k8sClient.Status().Update(ctx, &h)
			}, timeout, interval).Should(Succeed())
			nudgeNodeAndWait(ns, nodeName)
			Consistently(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				3*time.Second, 500*time.Millisecond).Should(Equal(0))
		})

		// U4b — Anti-spam: same LastTransitionTime + 5 nudges → exactly 1 event.
		It("emits HPAUnsatisfiable exactly once per condition transition", func() {
			clusterName := "hpa-u4b"
			nodeName := "node-u4b"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			hpaGen := getHPAGen(clusterName, nodeName)
			hpaName := ownedName(clusterName, nodeName)
			transitionTime := metav1.Now()

			Eventually(func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
					ObservedGeneration: ptr.To(hpaGen),
					DesiredReplicas:    2,
					Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
						{
							Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse,
							LastTransitionTime: transitionTime,
							Reason:             "FailedGetResourceMetric", Message: "no metrics (test)",
						},
					},
				}
				return k8sClient.Status().Update(ctx, &h)
			}, timeout, interval).Should(Succeed())
			nudgeNode(ns, nodeName)
			Eventually(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				timeout, interval).Should(Equal(1))

			// 5 nudges. Status (incl. LastTransitionTime) unchanged → exactly 1 event.
			// Wait for each reconcile to complete before the next nudge, so the
			// final Consistently window doesn't catch late emits from in-flight
			// reconciles.
			for i := 0; i < 5; i++ {
				nudgeNodeAndWait(ns, nodeName)
			}
			Consistently(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				3*time.Second, 500*time.Millisecond).Should(Equal(1))
		})

		// U4c — Lifecycle: flap re-emits.
		It("re-emits HPAUnsatisfiable when a condition transitions out and back in", func() {
			clusterName := "hpa-u4c"
			nodeName := "node-u4c"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			hpaGen := getHPAGen(clusterName, nodeName)
			hpaName := ownedName(clusterName, nodeName)

			writeStatus := func(condStatus corev1.ConditionStatus, transitionTime metav1.Time) {
				Eventually(func() error {
					var h autoscalingv2.HorizontalPodAutoscaler
					if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
						return err
					}
					h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
						ObservedGeneration: ptr.To(hpaGen),
						DesiredReplicas:    2,
						Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
							{Type: autoscalingv2.AbleToScale, Status: condStatus,
								LastTransitionTime: transitionTime, Reason: "Flap", Message: "(test)"},
						},
					}
					return k8sClient.Status().Update(ctx, &h)
				}, timeout, interval).Should(Succeed())
				nudgeNode(ns, nodeName)
			}

			// Fail #1.
			writeStatus(corev1.ConditionFalse, metav1.Now())
			Eventually(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				timeout, interval).Should(Equal(1))

			// Recover. reap() drops the cache entry.
			writeStatus(corev1.ConditionTrue, metav1.Time{Time: time.Now().Add(1 * time.Second)})
			time.Sleep(1 * time.Second)

			// Fail #2 with newer LastTransitionTime — must emit a 2nd event.
			writeStatus(corev1.ConditionFalse, metav1.Time{Time: time.Now().Add(2 * time.Second)})
			Eventually(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				timeout, interval).Should(Equal(2))
		})

		// U5 — Whitelist: ScalingDisabled is a legitimate "paused" state,
		// not a failure. The gate must not emit HPAUnsatisfiable for it even
		// when ScalingActive=False matches the outer filter.
		It("does not emit HPAUnsatisfiable for ScalingDisabled (legitimate paused state)", func() {
			clusterName := "hpa-u5"
			nodeName := "node-u5"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			hpaName := ownedName(clusterName, nodeName)

			Eventually(func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
					Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
						{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse,
							LastTransitionTime: metav1.Now(), Reason: "ScalingDisabled"},
					},
				}
				return k8sClient.Status().Update(ctx, &h)
			}, timeout, interval).Should(Succeed())
			nudgeNodeAndWait(ns, nodeName)
			Consistently(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				3*time.Second, 500*time.Millisecond).Should(Equal(0))
		})

		// D1 — Gate D positive: HPA enabled with no resources.requests.cpu
		// fires HPAMissingResourceRequest. Count is dedup'd server-side.
		It("emits at least one HPAMissingResourceRequest Warning when CPU request is unset", func() {
			clusterName := "hpa-d1"
			nodeName := "node-d1"
			testCreateCluster(ns, clusterName)
			node := runtimeNodeWithAutoscaling(ns, clusterName, nodeName)
			node.Spec.Resources = nil // trip Gate D
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			Eventually(func() int { return countEvents(nodeName, "HPAMissingResourceRequest") },
				timeout, interval).Should(BeNumerically(">=", 1))
		})

		// D2 — Gate D negative: HPA enabled with CPU request set → no event.
		It("does not emit HPAMissingResourceRequest when CPU request is set", func() {
			clusterName := "hpa-d2"
			nodeName := "node-d2"
			testCreateCluster(ns, clusterName)
			// Factory already provides a CPU request — use as-is.
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})

			Consistently(func() int { return countEvents(nodeName, "HPAMissingResourceRequest") },
				3*time.Second, 500*time.Millisecond).Should(Equal(0))
		})

		// D3 — Degraded reason transitions correctly through
		// missing → present → missing-again.
		It("clears and re-asserts Degraded=HPAMissingResourceRequest as resources change", func() {
			clusterName := "hpa-d3"
			nodeName := "node-d3"
			testCreateCluster(ns, clusterName)
			node := runtimeNodeWithAutoscaling(ns, clusterName, nodeName)
			node.Spec.Resources = nil
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			degradedReason := func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}
			Eventually(degradedReason, timeout, interval).Should(Equal("HPAMissingResourceRequest"))

			// Add the request → Degraded transitions away.
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return err
				}
				n.Spec.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}
				return k8sClient.Update(ctx, &n)
			})).To(Succeed())
			Eventually(degradedReason, timeout, interval).ShouldNot(Equal("HPAMissingResourceRequest"))

			// Remove the request → Degraded re-asserts.
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return err
				}
				n.Spec.Resources = nil
				return k8sClient.Update(ctx, &n)
			})).To(Succeed())
			Eventually(degradedReason, timeout, interval).Should(Equal("HPAMissingResourceRequest"))
		})

		// D4 — Degraded=HPAMissingResourceRequest set and clears on recovery.
		It("sets Degraded=HPAMissingResourceRequest and clears when request is added", func() {
			clusterName := "hpa-d4"
			nodeName := "node-d4"
			testCreateCluster(ns, clusterName)
			node := runtimeNodeWithAutoscaling(ns, clusterName, nodeName)
			node.Spec.Resources = nil
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("HPAMissingResourceRequest"))

			// Recovery: add the request → Degraded reason transitions away.
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return err
				}
				n.Spec.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}
				return k8sClient.Update(ctx, &n)
			})).To(Succeed())
			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal("HPAMissingResourceRequest"))
		})

		// D5 — Gate B suppresses redundant FailedGetResourceMetric when Gate
		// D fired this reconcile; other Gate B reasons still surface.
		It("suppresses HPAUnsatisfiable for FailedGetResourceMetric when Gate D fired", func() {
			clusterName := "hpa-d5"
			nodeName := "node-d5"
			testCreateCluster(ns, clusterName)
			node := runtimeNodeWithAutoscaling(ns, clusterName, nodeName)
			node.Spec.Resources = nil // trip Gate D
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})

			// Gate D fires first — confirm the marker is set.
			Eventually(func() int { return countEvents(nodeName, "HPAMissingResourceRequest") },
				timeout, interval).Should(BeNumerically(">=", 1))

			// Simulate KCM writing ScalingActive=False with the matching reason.
			hpaName := ownedName(clusterName, nodeName)
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
					ObservedGeneration: ptr.To(h.Generation),
					Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
						{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse,
							LastTransitionTime: metav1.Now(), Reason: "FailedGetResourceMetric"},
					},
				}
				return k8sClient.Status().Update(ctx, &h)
			})).To(Succeed())
			nudgeNodeAndWait(ns, nodeName)

			// Gate B must NOT fire for FailedGetResourceMetric while Gate D is active.
			Consistently(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				3*time.Second, 500*time.Millisecond).Should(Equal(0))

			// Sanity: unrelated AbleToScale=False still surfaces.
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status.Conditions = append(h.Status.Conditions,
					autoscalingv2.HorizontalPodAutoscalerCondition{
						Type: autoscalingv2.AbleToScale, Status: corev1.ConditionFalse,
						LastTransitionTime: metav1.Now(), Reason: "FailedGetScale",
					})
				return k8sClient.Status().Update(ctx, &h)
			})).To(Succeed())
			nudgeNode(ns, nodeName)
			Eventually(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				timeout, interval).Should(BeNumerically(">=", 1))
		})

		// U6 — Edge: two failing conditions on same reconcile → two events.
		It("emits one HPAUnsatisfiable per failing condition", func() {
			clusterName := "hpa-u6"
			nodeName := "node-u6"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			hpaGen := getHPAGen(clusterName, nodeName)
			hpaName := ownedName(clusterName, nodeName)
			t1 := metav1.Now()
			t2 := metav1.Time{Time: t1.Add(1 * time.Second)}

			Eventually(func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
					ObservedGeneration: ptr.To(hpaGen),
					DesiredReplicas:    2,
					Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
						{Type: autoscalingv2.AbleToScale, Status: corev1.ConditionFalse,
							LastTransitionTime: t1, Reason: "FailedGetScale"},
						{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse,
							LastTransitionTime: t2, Reason: "FailedGetResourceMetric"},
					},
				}
				return k8sClient.Status().Update(ctx, &h)
			}, timeout, interval).Should(Succeed())
			nudgeNode(ns, nodeName)

			Eventually(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				timeout, interval).Should(Equal(2))
		})

		// U7 — Edge: ScalingLimited=True is NOT a failure.
		It("does not emit HPAUnsatisfiable for ScalingLimited=True", func() {
			clusterName := "hpa-u7"
			nodeName := "node-u7"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})
			hpaGen := getHPAGen(clusterName, nodeName)
			hpaName := ownedName(clusterName, nodeName)

			Eventually(func() error {
				var h autoscalingv2.HorizontalPodAutoscaler
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: ns}, &h); err != nil {
					return err
				}
				h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
					ObservedGeneration: ptr.To(hpaGen),
					DesiredReplicas:    5,
					Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
						{Type: autoscalingv2.ScalingLimited, Status: corev1.ConditionTrue,
							LastTransitionTime: metav1.Now(), Reason: "TooFewReplicas"},
					},
				}
				return k8sClient.Status().Update(ctx, &h)
			}, timeout, interval).Should(Succeed())
			nudgeNodeAndWait(ns, nodeName)

			Consistently(func() int { return countEvents(nodeName, "HPAUnsatisfiable") },
				3*time.Second, 500*time.Millisecond).Should(Equal(0))
		})

		// U8 — Edge: PDB+HPA both collide → PDB wins in Degraded.
		It("applies PDBNotOwned overlay over HPANotOwned when both collide", func() {
			clusterName := "hpa-u8"
			nodeName := "node-u8"
			testCreateCluster(ns, clusterName)

			ownName := ownedName(clusterName, nodeName)
			foreignHPA := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: ownName, Namespace: ns},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1", Kind: "Deployment", Name: "other",
					},
					MinReplicas: ptr.To(int32(1)), MaxReplicas: 3,
				},
			}
			Expect(k8sClient.Create(ctx, foreignHPA)).To(Succeed())

			foreignMin := intstr.FromInt32(7)
			foreignPDB := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: ownName, Namespace: ns},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &foreignMin,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"user-pdb": "true"}},
				},
			}
			Expect(k8sClient.Create(ctx, foreignPDB)).To(Succeed())

			// Runtime node with NEITHER autoscaling nor PDB set — both cleanup
			// branches fire and both record collisions.
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			Eventually(func() string {
				var n v1alpha1.IdentityServerNode
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n)
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("PDBNotOwned"))

			// Both Warning events should have been emitted once.
			Expect(countEvents(nodeName, "HPANotOwned")).To(BeNumerically(">=", 1))
			Expect(countEvents(nodeName, "PDBNotOwned")).To(BeNumerically(">=", 1))
		})

		// U9a — Pending: CEL rejects admin+autoscaling.enabled=true at
		// admission, so the reconciler-side guard is unreachable from
		// envtest. Covered by E2E + U9b/c.
		PIt("emits AutoscalingIgnored on first reconcile of stored admin node with autoscaling.enabled=true", func() {})

		// U9b — Steady-state has no event spam (runtime path; CEL blocks admin path).
		It("does not bomb events during steady-state reconciles", func() {
			clusterName := "hpa-u9b"
			nodeName := "node-u9b"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})

			// Several nudges; no Warnings should accumulate. Wait for each
			// reconcile to finish so a late emit can't slip into the window.
			for i := 0; i < 5; i++ {
				nudgeNodeAndWait(ns, nodeName)
			}
			Consistently(func() int {
				return countEvents(nodeName, "AutoscalingIgnored") +
					countEvents(nodeName, "HPANotOwned") +
					countEvents(nodeName, "HPAUnsatisfiable")
			}, 3*time.Second, 500*time.Millisecond).Should(Equal(0))
		})

		// U9c — Lifecycle: AutoscalingIgnored gate's Generation comparison.
		// Direct unit test of the gate via reconciler-internal cache transition
		// is not exposed; this test exercises the broader anti-spam guarantee
		// across a spec-change boundary.
		It("does not re-emit Warning events when spec changes from autoscaling-on to autoscaling-off", func() {
			clusterName := "hpa-u9c"
			nodeName := "node-u9c"
			testCreateCluster(ns, clusterName)
			Expect(k8sClient.Create(ctx, runtimeNodeWithAutoscaling(ns, clusterName, nodeName))).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &autoscalingv2.HorizontalPodAutoscaler{})

			// Toggle off — cleanup branch fires, emits Normal HPADeleted, no Warnings.
			Eventually(func() error {
				var n v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
					return err
				}
				n.Spec.Autoscaling.Enabled = false
				return k8sClient.Update(ctx, &n)
			}, timeout, interval).Should(Succeed())

			Eventually(func() int { return countEvents(nodeName, "HPADeleted") },
				timeout, interval).Should(BeNumerically(">=", 1))
			Expect(countEvents(nodeName, "AutoscalingIgnored")).To(Equal(0))
			Expect(countEvents(nodeName, "HPANotOwned")).To(Equal(0))
		})
	})
})
