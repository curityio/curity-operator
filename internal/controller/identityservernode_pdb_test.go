package controller_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerNode Reconciler / PodDisruptionBudget", func() {
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

	It("creates PDB when node sets maxUnavailable", func() {
		clusterName := "cluster-pdb-max"
		nodeName := "node-pdb-max"
		testCreateCluster(ns, clusterName)
		max := intstr.FromInt32(1)
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     nodeName + "-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
				Replicas:                 ptr.To(int32(3)),
				PodDisruptionBudget:      &v1alpha1.PDBSpec{MaxUnavailable: &max},
				Service:                  defaultTestService(),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		pdb := &policyv1.PodDisruptionBudget{}
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
		Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
		Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))
		Expect(pdb.Spec.MinAvailable).To(BeNil(), "maxUnavailable PDB must not also set minAvailable")
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
			fresh.Spec.Replicas = nil // admin must not set replicas
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
