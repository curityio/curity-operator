package controller_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// Tests for the orphan-child cleanup helper and the abandoned-cluster Secret
// reset. The two are split across the IdentityServerNode and Cluster
// reconcilers but share the same triggering scenario (clusterRef change), so
// they live in one file for readability.
var _ = Describe("ClusterRef change cleanup", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	patchClusterRef := func(name, newCluster string) {
		var node v1alpha1.IdentityServerNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &node)).To(Succeed())
		node.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: newCluster}
		Expect(k8sClient.Update(ctx, &node)).To(Succeed())
	}

	// simulateConfigReady mirrors the pattern used by existing rename tests:
	// the cluster reconciler creates a placeholder Secret when an admin node
	// appears, but envtest doesn't run a real genclust Job to populate it.
	// Stamp the Secret with non-placeholder cluster.xml plus a host tag so
	// downstream gate logic at the node reconciler considers config "ready"
	// and creates the admin/runtime Deployment.
	simulateConfigReady := func(cluster, adminNode string) {
		secret := &corev1.Secret{}
		Eventually(func() string {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: cluster + "-cluster-config", Namespace: ns}, secret); err != nil {
				return ""
			}
			return secret.Annotations["curity.io/admin-node"]
		}, timeout, interval).Should(Equal(adminNode))
		secret.Data = map[string][]byte{
			"cluster.xml": []byte("<config><host>" + ownedName(cluster, adminNode) + "</host></config>"),
		}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
	}

	Context("admin move a→b", func() {
		It("deletes <a>-admin Deployment + Service and creates <b>-admin", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")
			testCreateNode(ns, "admin", v1alpha1.NodeTypeAdmin, "cluster-a")
			simulateConfigReady("cluster-a", "admin")

			// Original Deployment+Service in cluster-a.
			eventuallyGetResource(ns, ownedName("cluster-a", "admin"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("cluster-a", "admin"), &corev1.Service{})

			patchClusterRef("admin", "cluster-b")
			// New cluster-b needs its config ready before its admin Deployment is created.
			simulateConfigReady("cluster-b", "admin")

			eventuallyDeleted(ns, ownedName("cluster-a", "admin"), &appsv1.Deployment{})
			eventuallyDeleted(ns, ownedName("cluster-a", "admin"), &corev1.Service{})
			eventuallyGetResource(ns, ownedName("cluster-b", "admin"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("cluster-b", "admin"), &corev1.Service{})
		})

		It("flips ClusterConfigReady=False and preserves Secret data so surviving runtime pods stay healthy", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")
			testCreateNode(ns, "admin", v1alpha1.NodeTypeAdmin, "cluster-a")
			simulateConfigReady("cluster-a", "admin")

			// Capture the Secret state before the move so we can prove
			// the operator does not mutate it on admin departure.
			before := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a-cluster-config", Namespace: ns}, before)).To(Succeed())
			beforeData := string(before.Data["cluster.xml"])
			beforeAdmin := before.Annotations["curity.io/admin-node"]
			beforeHash := before.Annotations["curity.io/cluster-config-hash"]
			Expect(beforeData).NotTo(Equal("placeholder"))
			Expect(beforeAdmin).To(Equal("admin"))

			patchClusterRef("admin", "cluster-b")

			// Cluster-a's ClusterConfigReady condition flips to False —
			// our user-visible signal that the cluster has lost its admin.
			Eventually(func() string {
				var c v1alpha1.IdentityServerCluster
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a", Namespace: ns}, &c); err != nil {
					return ""
				}
				return conditionReason(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			}, timeout, interval).Should(Equal("WaitingForAdmin"))

			// CRITICAL: Secret data + annotations must NOT change. Mutating
			// them would cause the operator's config-hash recompute at
			// identityservernode_reconciler.go:401 to flip the value on
			// every surviving runtime node's Deployment template, forcing
			// a rolling restart whose new pod boots with stale/placeholder
			// data and crashloops. Holding the Secret keeps subPath-mounted
			// runtime pods healthy until the user resolves the move.
			Consistently(func(g Gomega) {
				s := &corev1.Secret{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a-cluster-config", Namespace: ns}, s)).To(Succeed())
				g.Expect(string(s.Data["cluster.xml"])).To(Equal(beforeData))
				g.Expect(s.Annotations["curity.io/admin-node"]).To(Equal(beforeAdmin))
				g.Expect(s.Annotations["curity.io/cluster-config-hash"]).To(Equal(beforeHash))
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("runtime move a→b with HPA and PDB", func() {
		It("deletes orphan HPA and PDB along with Deployment and Service", func() {
			min := intstr.FromInt32(1)
			clusterA := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "rt-a", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:             "11.0",
					PodDisruptionBudget: &v1alpha1.PDBSpec{MinAvailable: &min},
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled:                        true,
						MinReplicas:                    1,
						MaxReplicas:                    3,
						TargetCPUUtilizationPercentage: 70,
					},
				},
			}
			Expect(k8sClient.Create(ctx, clusterA)).To(Succeed())
			testCreateCluster(ns, "rt-b")

			testCreateNode(ns, "rt-node", v1alpha1.NodeTypeRuntime, "rt-a")

			eventuallyGetResource(ns, ownedName("rt-a", "rt-node"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("rt-a", "rt-node"), &corev1.Service{})
			eventuallyGetResource(ns, ownedName("rt-a", "rt-node"), &autoscalingv2.HorizontalPodAutoscaler{})
			eventuallyGetResource(ns, ownedName("rt-a", "rt-node"), &policyv1.PodDisruptionBudget{})

			patchClusterRef("rt-node", "rt-b")

			eventuallyDeleted(ns, ownedName("rt-a", "rt-node"), &appsv1.Deployment{})
			eventuallyDeleted(ns, ownedName("rt-a", "rt-node"), &corev1.Service{})
			eventuallyDeleted(ns, ownedName("rt-a", "rt-node"), &autoscalingv2.HorizontalPodAutoscaler{})
			eventuallyDeleted(ns, ownedName("rt-a", "rt-node"), &policyv1.PodDisruptionBudget{})
		})
	})

	Context("multi-cluster isolation", func() {
		It("does not touch peer cluster's children when one node moves", func() {
			testCreateCluster(ns, "iso-a")
			testCreateCluster(ns, "iso-b")
			testCreateCluster(ns, "iso-c")
			testCreateNode(ns, "iso-node-a", v1alpha1.NodeTypeAdmin, "iso-a")
			testCreateNode(ns, "iso-node-b", v1alpha1.NodeTypeAdmin, "iso-b")
			simulateConfigReady("iso-a", "iso-node-a")
			simulateConfigReady("iso-b", "iso-node-b")

			// Both nodes' children present.
			eventuallyGetResource(ns, ownedName("iso-a", "iso-node-a"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("iso-b", "iso-node-b"), &appsv1.Deployment{})

			patchClusterRef("iso-node-a", "iso-c")
			simulateConfigReady("iso-c", "iso-node-a")

			// iso-node-a's old children gone; new children at iso-c-iso-node-a.
			eventuallyDeleted(ns, ownedName("iso-a", "iso-node-a"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("iso-c", "iso-node-a"), &appsv1.Deployment{})

			// iso-node-b's children must be untouched (UID-match in IsControlledBy
			// ensures cleanup never reaches across nodes).
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("iso-b", "iso-node-b"), Namespace: ns}, &appsv1.Deployment{})
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("iso-b", "iso-node-b"), Namespace: ns}, &corev1.Service{})
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("foreign-owned resource", func() {
		It("does not delete a managed-by-labeled resource owned by a different controller", func() {
			testCreateCluster(ns, "fo-a")
			testCreateCluster(ns, "fo-b")
			testCreateNode(ns, "fo-node", v1alpha1.NodeTypeAdmin, "fo-a")
			simulateConfigReady("fo-a", "fo-node")

			eventuallyGetResource(ns, ownedName("fo-a", "fo-node"), &appsv1.Deployment{})

			// Hand-create a Deployment with our label but no controllerRef
			// pointing at any IdentityServerNode. Cleanup must skip it.
			foreign := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foreign-deploy",
					Namespace: ns,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "curity-operator",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(0)),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"foo": "bar"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"foo": "bar"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "scratch"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			patchClusterRef("fo-node", "fo-b")

			eventuallyDeleted(ns, ownedName("fo-a", "fo-node"), &appsv1.Deployment{})

			// Foreign Deployment must still exist after the move.
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "foreign-deploy", Namespace: ns}, &appsv1.Deployment{})
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("successive moves a→b→c", func() {
		It("cleans multi-generation orphans across two consecutive moves", func() {
			// Smoke test S6: a node bouncing through several clusters in
			// quick succession should leave no orphans behind, regardless
			// of how fast the patches arrive. Cleanup uses a List+filter
			// keyed on the node's UID, so any number of stale ownedResource
			// names get caught in one reconcile sweep.
			testCreateCluster(ns, "s6-a")
			testCreateCluster(ns, "s6-b")
			testCreateCluster(ns, "s6-c")
			testCreateNode(ns, "hopper", v1alpha1.NodeTypeRuntime, "s6-a")

			eventuallyGetResource(ns, ownedName("s6-a", "hopper"), &appsv1.Deployment{})

			patchClusterRef("hopper", "s6-b")
			eventuallyDeleted(ns, ownedName("s6-a", "hopper"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("s6-b", "hopper"), &appsv1.Deployment{})

			patchClusterRef("hopper", "s6-c")
			eventuallyDeleted(ns, ownedName("s6-b", "hopper"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("s6-c", "hopper"), &appsv1.Deployment{})

			// Final state: only the s6-c-named children exist; both prior
			// generations cleaned.
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("s6-a", "hopper"), Namespace: ns}, &appsv1.Deployment{})
			}, 2*time.Second, 250*time.Millisecond).ShouldNot(Succeed())
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("s6-b", "hopper"), Namespace: ns}, &appsv1.Deployment{})
			}, 2*time.Second, 250*time.Millisecond).ShouldNot(Succeed())
		})
	})

	Context("move to non-existent cluster", func() {
		It("cleans source cluster's orphans even when destination CR is missing", func() {
			// Smoke test S7: cleanup placement is "before destination
			// fetch" so orphans get removed in every code path including
			// the ClusterNotFound early return at lines 117-138.
			testCreateCluster(ns, "s7-real")
			testCreateNode(ns, "rt", v1alpha1.NodeTypeRuntime, "s7-real")
			eventuallyGetResource(ns, ownedName("s7-real", "rt"), &appsv1.Deployment{})

			patchClusterRef("rt", "s7-ghost")

			// Orphan from s7-real should be cleaned despite the new
			// cluster not existing.
			eventuallyDeleted(ns, ownedName("s7-real", "rt"), &appsv1.Deployment{})

			// Node should be marked Degraded with ClusterNotFound.
			Eventually(func() string {
				n := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rt", Namespace: ns}, n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionClusterReady)
			}, timeout, interval).Should(Equal(v1alpha1.ReasonClusterNotFound))
		})
	})

	Context("move into cluster with existing admin", func() {
		It("cleans source orphans even when destination's DuplicateAdmin check rejects", func() {
			// Smoke test S8: cleanup runs before the duplicate-admin
			// validation at identityservernode_reconciler.go:174-192,
			// so the source-cluster's stale children go even though the
			// move itself is rejected by the validation.
			testCreateCluster(ns, "s8-a")
			testCreateCluster(ns, "s8-b")
			testCreateNode(ns, "incumbent", v1alpha1.NodeTypeAdmin, "s8-b")
			testCreateNode(ns, "mover", v1alpha1.NodeTypeAdmin, "s8-a")
			simulateConfigReady("s8-a", "mover")
			simulateConfigReady("s8-b", "incumbent")

			eventuallyGetResource(ns, ownedName("s8-a", "mover"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("s8-b", "incumbent"), &appsv1.Deployment{})

			patchClusterRef("mover", "s8-b")

			// Source orphan must be gone.
			eventuallyDeleted(ns, ownedName("s8-a", "mover"), &appsv1.Deployment{})

			// Mover must be Degraded with DuplicateAdmin.
			Eventually(func() string {
				n := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mover", Namespace: ns}, n); err != nil {
					return ""
				}
				return conditionReason(n.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateAdmin"))

			// Incumbent's children must be untouched (UID isolation).
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("s8-b", "incumbent"), Namespace: ns}, &appsv1.Deployment{})
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("a→b→a bounce", func() {
		It("flips ClusterConfigReady False on departure and back to True on return without touching Secret data", func() {
			// Smoke test S9: when admin returns to its source cluster
			// under the same name, the steady-state branch in
			// ensureClusterConfig recognizes storedConfigHash ==
			// currentConfigHash and flips the condition back to True
			// without any genclust run. Critical: the Secret data is
			// never mutated across the bounce, so any surviving runtime
			// pods on s9-a stay healthy throughout.
			testCreateCluster(ns, "s9-a")
			testCreateCluster(ns, "s9-b")
			testCreateNode(ns, "bouncer", v1alpha1.NodeTypeAdmin, "s9-a")
			simulateConfigReady("s9-a", "bouncer")

			eventuallyGetResource(ns, ownedName("s9-a", "bouncer"), &appsv1.Deployment{})

			before := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "s9-a-cluster-config", Namespace: ns}, before)).To(Succeed())
			beforeData := string(before.Data["cluster.xml"])
			beforeAdmin := before.Annotations["curity.io/admin-node"]

			// First bounce: a→b. cluster-a's ClusterConfigReady flips to
			// False but Secret data must stay intact.
			patchClusterRef("bouncer", "s9-b")
			Eventually(func() string {
				var c v1alpha1.IdentityServerCluster
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "s9-a", Namespace: ns}, &c); err != nil {
					return ""
				}
				return conditionReason(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			}, timeout, interval).Should(Equal("WaitingForAdmin"))

			Consistently(func(g Gomega) {
				s := &corev1.Secret{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "s9-a-cluster-config", Namespace: ns}, s)).To(Succeed())
				g.Expect(string(s.Data["cluster.xml"])).To(Equal(beforeData))
				g.Expect(s.Annotations["curity.io/admin-node"]).To(Equal(beforeAdmin))
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())

			// Second bounce: b→a. cluster-a sees the admin back under the
			// same name and hash; steady-state branch flips condition to
			// True without any Secret mutation or Job creation.
			simulateConfigReady("s9-b", "bouncer")
			patchClusterRef("bouncer", "s9-a")

			Eventually(func() string {
				var c v1alpha1.IdentityServerCluster
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "s9-a", Namespace: ns}, &c); err != nil {
					return ""
				}
				return conditionReason(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			}, timeout, interval).Should(Equal("SecretReady"))

			s := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "s9-a-cluster-config", Namespace: ns}, s)).To(Succeed())
			Expect(string(s.Data["cluster.xml"])).To(Equal(beforeData))
			Expect(s.Annotations["curity.io/admin-node"]).To(Equal(beforeAdmin))
		})
	})

	Context("source cluster has surviving runtime nodes", func() {
		It("preserves the runtime's Deployment when admin departs the cluster", func() {
			// Smoke test S10: when an admin moves away, runtime nodes
			// remaining in the source cluster keep their existing
			// Deployment intact (the gate at identityservernode_reconciler.go:340-369
			// bypasses for nodes whose Deployment already exists).
			// Validates UID isolation between sibling nodes in the same
			// cluster: cleanup of admin's orphan does not touch runtime's
			// children.
			testCreateCluster(ns, "s10-a")
			testCreateCluster(ns, "s10-b")
			testCreateNode(ns, "admin", v1alpha1.NodeTypeAdmin, "s10-a")
			testCreateNode(ns, "rt-stays", v1alpha1.NodeTypeRuntime, "s10-a")
			simulateConfigReady("s10-a", "admin")

			rtDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("s10-a", "rt-stays"), rtDeploy)
			rtUIDBefore := rtDeploy.UID

			eventuallyGetResource(ns, ownedName("s10-a", "admin"), &appsv1.Deployment{})

			patchClusterRef("admin", "s10-b")

			// Admin's orphan goes; runtime's Deployment stays.
			eventuallyDeleted(ns, ownedName("s10-a", "admin"), &appsv1.Deployment{})

			// Runtime's Deployment must still exist with the same UID.
			Consistently(func() (string, error) {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("s10-a", "rt-stays"), Namespace: ns}, d); err != nil {
					return "", err
				}
				return string(d.UID), nil
			}, 2*time.Second, 250*time.Millisecond).Should(Equal(string(rtUIDBefore)))
		})
	})

	Context("selector-immutability guard on clusterRef move", func() {
		It("delete-and-recreates Deployment and PDB (rather than Update) so immutable selectors aren't violated", func() {
			// Deployment and PDB have spec.selector.matchLabels that include
			// app.kubernetes.io/instance: OwnedResourceName(...). When the
			// node's clusterRef changes, ownedResourceName changes, so the
			// selector value would have to change too. K8s rejects spec.selector
			// updates on Deployment and PDB with "selector is immutable", so
			// cleanupOrphanChildren MUST Delete the old resource (new UID on
			// the recreated one) rather than Update in place.
			testCreateCluster(ns, "imm-a")
			testCreateCluster(ns, "imm-b")
			testCreateNode(ns, "imm-node", v1alpha1.NodeTypeRuntime, "imm-a")

			oldDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("imm-a", "imm-node"), oldDeploy)
			oldDeployUID := oldDeploy.UID

			// PDB only created when MinAvailable is set; force it.
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "imm-node", Namespace: ns}, node)).To(Succeed())
			node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{
				MinAvailable: ptr.To(intstr.FromInt(1)),
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			oldPDB := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName("imm-a", "imm-node"), oldPDB)
			oldPDBUID := oldPDB.UID

			// Move the node to a different cluster — ownedResourceName changes,
			// so the selector value on Deployment and PDB must change.
			patchClusterRef("imm-node", "imm-b")

			// New-named resources appear; old ones go away.
			eventuallyGetResource(ns, ownedName("imm-b", "imm-node"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("imm-b", "imm-node"), &policyv1.PodDisruptionBudget{})
			eventuallyDeleted(ns, ownedName("imm-a", "imm-node"), &appsv1.Deployment{})
			eventuallyDeleted(ns, ownedName("imm-a", "imm-node"), &policyv1.PodDisruptionBudget{})

			// New resources must have NEW UIDs — proves the operator went
			// through Delete+Create, not Update. If it had tried Update,
			// the K8s API would have rejected it with "selector is immutable"
			// and the move would have stuck.
			newDeploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("imm-b", "imm-node"), Namespace: ns}, newDeploy)).To(Succeed())
			Expect(newDeploy.UID).NotTo(Equal(oldDeployUID), "Deployment must be recreated, not updated, to honor immutable selector")

			newPDB := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("imm-b", "imm-node"), Namespace: ns}, newPDB)).To(Succeed())
			Expect(newPDB.UID).NotTo(Equal(oldPDBUID), "PDB must be recreated, not updated, to honor immutable selector")

			// And the new resources' selectors must reference the new ownedResourceName.
			Expect(newDeploy.Spec.Selector.MatchLabels["app.kubernetes.io/instance"]).To(Equal(ownedName("imm-b", "imm-node")))
			Expect(newPDB.Spec.Selector.MatchLabels["app.kubernetes.io/instance"]).To(Equal(ownedName("imm-b", "imm-node")))
		})
	})

	Context("combined replicas + clusterRef change in one patch", func() {
		It("applies both changes atomically — orphan cleaned, new replica count", func() {
			// Smoke test S15: a single patch that changes BOTH spec.replicas
			// and spec.identityServerClusterRef should converge with
			// orphan cleanup and the new replica count visible on the
			// new Deployment.
			testCreateCluster(ns, "s15-a")
			testCreateCluster(ns, "s15-b")
			testCreateNode(ns, "combo", v1alpha1.NodeTypeRuntime, "s15-a")
			eventuallyGetResource(ns, ownedName("s15-a", "combo"), &appsv1.Deployment{})

			// Single patch: clusterRef a→b AND replicas 1→3.
			n := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "combo", Namespace: ns}, n)).To(Succeed())
			n.Spec.IdentityServerClusterRef = v1alpha1.ObjectReference{Name: "s15-b"}
			n.Spec.Replicas = ptr.To(int32(3))
			Expect(k8sClient.Update(ctx, n)).To(Succeed())

			eventuallyDeleted(ns, ownedName("s15-a", "combo"), &appsv1.Deployment{})

			Eventually(func() int32 {
				d := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("s15-b", "combo"), Namespace: ns}, d); err != nil {
					return 0
				}
				if d.Spec.Replicas == nil {
					return 0
				}
				return *d.Spec.Replicas
			}, timeout, interval).Should(Equal(int32(3)))
		})
	})

	// Cascade-deletion race: deleting cluster A while concurrently re-parenting
	// a child node N to cluster B must not let K8s GC delete N via stale
	// ownerRef.UID. The cluster reconciler's deletion gate counts children by
	// spec OR controller-ownerRef.UID, so A's finalizer holds until the node
	// reconciler migrates N's ownerRef to B.
	Context("cluster deletion during clusterRef move", func() {
		controllerOwnerUID := func(name string) types.UID {
			n := &v1alpha1.IdentityServerNode{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, n); err != nil {
				return ""
			}
			if ref := metav1.GetControllerOf(n); ref != nil {
				return ref.UID
			}
			return ""
		}

		It("blocks A's finalizer until the moved node's ownerRef migrates to B, and N survives", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")
			testCreateNode(ns, "rt-node", v1alpha1.NodeTypeRuntime, "cluster-a")

			var clusterA v1alpha1.IdentityServerCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a", Namespace: ns}, &clusterA)).To(Succeed())
			var clusterB v1alpha1.IdentityServerCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-b", Namespace: ns}, &clusterB)).To(Succeed())

			Eventually(func() types.UID {
				return controllerOwnerUID("rt-node")
			}, timeout, interval).Should(Equal(clusterA.UID), "node should be controller-owned by cluster-a before the race")

			// Trigger the race: delete A, then patch N's clusterRef. The
			// finalizer is in place, so A enters Terminating; the patch
			// must not let GC cascade-delete N via stale ownerRef.UID.
			Expect(k8sClient.Delete(ctx, &clusterA)).To(Succeed())
			patchClusterRef("rt-node", "cluster-b")

			// A's finalizer must hold through the migration window.
			Eventually(func() types.UID {
				return controllerOwnerUID("rt-node")
			}, timeout, interval).Should(Equal(clusterB.UID), "node ownerRef should migrate to cluster-b")

			// After migration, A is no longer counted as owning N (neither
			// spec nor UID matches), so the finalizer releases.
			eventuallyDeleted(ns, "cluster-a", &v1alpha1.IdentityServerCluster{})

			// N must survive the cascade-GC window.
			survivor := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rt-node", Namespace: ns}, survivor)).To(Succeed())
			Expect(survivor.DeletionTimestamp).To(BeNil(), "rt-node must not be cascade-deleted by GC")
		})

		It("holds A's finalizer when node moves to a missing cluster, until N is removed", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateNode(ns, "rt-node", v1alpha1.NodeTypeRuntime, "cluster-a")

			var clusterA v1alpha1.IdentityServerCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a", Namespace: ns}, &clusterA)).To(Succeed())

			Eventually(func() types.UID {
				return controllerOwnerUID("rt-node")
			}, timeout, interval).Should(Equal(clusterA.UID))

			Expect(k8sClient.Delete(ctx, &clusterA)).To(Succeed())
			patchClusterRef("rt-node", "missing-cluster")

			// A stays in Terminating because UID leg keeps N counted (the
			// node reconciler can't migrate ownerRef when destination is
			// missing). N persists because spec.clusterRef points at a
			// missing cluster — orphan, but not deleted by GC.
			Consistently(func() bool {
				c := &v1alpha1.IdentityServerCluster{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a", Namespace: ns}, c)
				return err == nil && c.DeletionTimestamp != nil
			}, 3*time.Second, interval).Should(BeTrue(), "cluster-a should stay in Terminating while N retains ownerRef.UID")

			// Once user removes N, A's finalizer releases.
			n := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rt-node", Namespace: ns}, n)).To(Succeed())
			Expect(k8sClient.Delete(ctx, n)).To(Succeed())
			eventuallyDeleted(ns, "rt-node", &v1alpha1.IdentityServerNode{})
			eventuallyDeleted(ns, "cluster-a", &v1alpha1.IdentityServerCluster{})
		})

		It("regression: delete cluster with no concurrent move blocks until node is removed", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateNode(ns, "rt-node", v1alpha1.NodeTypeRuntime, "cluster-a")

			Eventually(func() types.UID {
				return controllerOwnerUID("rt-node")
			}, timeout, interval).ShouldNot(BeEmpty())

			c := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a", Namespace: ns}, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())

			Consistently(func() bool {
				cur := &v1alpha1.IdentityServerCluster{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-a", Namespace: ns}, cur)
				return err == nil && cur.DeletionTimestamp != nil
			}, 2*time.Second, interval).Should(BeTrue(), "cluster-a should remain Terminating while the node still references it")

			n := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rt-node", Namespace: ns}, n)).To(Succeed())
			Expect(k8sClient.Delete(ctx, n)).To(Succeed())
			eventuallyDeleted(ns, "rt-node", &v1alpha1.IdentityServerNode{})
			eventuallyDeleted(ns, "cluster-a", &v1alpha1.IdentityServerCluster{})
		})
	})

	Context("admin deleted within cluster (no move)", func() {
		It("preserves cluster-config Secret when admin CR is deleted permanently", func() {
			testCreateCluster(ns, "del-cluster")
			testCreateNode(ns, "del-admin", v1alpha1.NodeTypeAdmin, "del-cluster")

			// Pre-populate Secret with real XML.
			secret := &corev1.Secret{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "del-cluster-cluster-config", Namespace: ns}, secret); err != nil {
					return ""
				}
				return secret.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("del-admin"))
			secret.Data = map[string][]byte{
				"cluster.xml": []byte(
					fmt.Sprintf("<config><host>%s</host></config>", ownedName("del-cluster", "del-admin"))),
			}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Delete admin permanently (no replacement).
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "del-admin", Namespace: ns}, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			eventuallyDeleted(ns, "del-admin", &v1alpha1.IdentityServerNode{})

			// Secret data must NOT be reset to placeholder, because the prior
			// admin CR no longer exists (rename-in-progress vs permanent
			// delete is indistinguishable; we err on the side of preserving).
			Consistently(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "del-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return "<error>"
				}
				return string(s.Data["cluster.xml"])
			}, 2*time.Second, 250*time.Millisecond).Should(ContainSubstring(fmt.Sprintf("<host>%s</host>", ownedName("del-cluster", "del-admin"))))
		})
	})
})

// announceAdminDeparted is exercised by the integration tests above. This
// block adds direct probes for the helper's edge cases that are awkward to
// drive from a full reconcile.
var _ = Describe("Admin-departed announcement edge cases", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("flips ClusterConfigReady=False without mutating Secret when admin's spec.type changes from admin to runtime in same cluster", func() {
		// Regression guard for the S14 finding during smoke testing on
		// 2026-05-04. The cache-lag guard now also requires
		// priorAdmin.Spec.Type == NodeTypeAdmin: a user changing an
		// admin to a runtime within the same cluster legitimately
		// signals "this cluster lost its admin" and we surface that via
		// the AdminDeparted event + ClusterConfigReady condition flip.
		// We do NOT mutate the Secret data because that would force a
		// rolling restart of any runtime pods on this cluster (their
		// pod-template config-hash would flip), and they'd boot reading
		// stale data from kubelet's subPath cache and crashloop.
		testCreateCluster(ns, "tc-cluster")
		testCreateNode(ns, "tc-admin", v1alpha1.NodeTypeAdmin, "tc-cluster")

		secret := &corev1.Secret{}
		Eventually(func() string {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tc-cluster-cluster-config", Namespace: ns}, secret); err != nil {
				return ""
			}
			return secret.Annotations["curity.io/admin-node"]
		}, timeout, interval).Should(Equal("tc-admin"))

		// Pre-populate Secret with real XML referencing the admin host.
		realXML := []byte(fmt.Sprintf("<config><host>%s</host></config>", ownedName("tc-cluster", "tc-admin")))
		secret.Data = map[string][]byte{"cluster.xml": realXML}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		// Flip spec.type from admin to runtime — same name, same clusterRef.
		node := &v1alpha1.IdentityServerNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tc-admin", Namespace: ns}, node)).To(Succeed())
		node.Spec.Type = v1alpha1.NodeTypeRuntime
		Expect(k8sClient.Update(ctx, node)).To(Succeed())

		// Cluster's ClusterConfigReady must flip to False, reason WaitingForAdmin.
		Eventually(func() string {
			var c v1alpha1.IdentityServerCluster
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tc-cluster", Namespace: ns}, &c); err != nil {
				return ""
			}
			return conditionReason(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
		}, timeout, interval).Should(Equal("WaitingForAdmin"))

		// Secret data + admin-node annotation must NOT change. Kubelet's
		// subPath mount keeps surviving runtime pods alive only if we
		// don't touch this Secret.
		Consistently(func(g Gomega) {
			s := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tc-cluster-cluster-config", Namespace: ns}, s)).To(Succeed())
			g.Expect(s.Data["cluster.xml"]).To(Equal(realXML))
			g.Expect(s.Annotations["curity.io/admin-node"]).To(Equal("tc-admin"))
		}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
	})

	It("stays a no-op when Secret is already in placeholder state", func() {
		testCreateCluster(ns, "ph-cluster")
		// No admin created — Secret is created by the cluster reconciler
		// in placeholder state and stays that way.
		secret := &corev1.Secret{}
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "ph-cluster-cluster-config", Namespace: ns}, secret)
			return apierrors.IsNotFound(err)
		}, 2*time.Second, 250*time.Millisecond).Should(BeTrue(),
			"Secret should not be created when cluster has no admin")

		// Add an admin so the placeholder Secret is created, then verify
		// reset is a no-op when the Secret is already placeholder.
		testCreateNode(ns, "ph-admin", v1alpha1.NodeTypeAdmin, "ph-cluster")
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ph-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return string(s.Data["cluster.xml"])
		}, timeout, interval).Should(Equal("placeholder"))

		// Remove the admin. The Secret is in placeholder state already, so
		// the reset path returns nil immediately — no AdminDeparted event,
		// no mutation, no log noise.
		node := &v1alpha1.IdentityServerNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ph-admin", Namespace: ns}, node)).To(Succeed())
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		eventuallyDeleted(ns, "ph-admin", &v1alpha1.IdentityServerNode{})

		Consistently(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ph-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return "<error>"
			}
			return string(s.Data["cluster.xml"])
		}, 2*time.Second, 250*time.Millisecond).Should(Equal("placeholder"))
	})
})
