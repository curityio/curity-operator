package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
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

			// 120s timeout: stuck-admin's Degraded=DuplicateAdmin lands
			// only after cluster.Status.NodeCount increments via cluster-watch,
			// which can lag past 60s under heavy CI envtest load.
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 120*time.Second, interval).Should(Equal("DuplicateAdmin"))

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
			}, 120*time.Second, interval).ShouldNot(Equal("DuplicateAdmin"))
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

})
