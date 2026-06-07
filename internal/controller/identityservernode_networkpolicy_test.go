package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerNode Reconciler / NetworkPolicy", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	createClusterWithNP := func(name string) {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:       "11.0",
				NetworkPolicy: &v1alpha1.NetworkPolicySpec{},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	}

	It("creates a NetworkPolicy owned by the admin node, targeting admin pods", func() {
		clusterName := "cluster-np-1"
		nodeName := "node-np-admin"
		createClusterWithNP(clusterName)
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)

		np := &networkingv1.NetworkPolicy{}
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), np)

		Expect(np.OwnerReferences).To(HaveLen(1))
		Expect(np.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
		Expect(np.OwnerReferences[0].Name).To(Equal(nodeName))
		Expect(np.Spec.PolicyTypes).To(Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}))
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("curity.io/owned-by", ownedName(clusterName, nodeName)))
		Expect(np.Spec.Ingress).To(HaveLen(1), "UI off → only the runtime→admin clustering rule")
	})

	It("does not create a NetworkPolicy for a runtime node", func() {
		clusterName := "cluster-np-runtime"
		nodeName := "node-np-runtime"
		createClusterWithNP(clusterName)
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// The Deployment proves the node reconciled; the NetworkPolicy must not exist.
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), &appsv1.Deployment{})
		Consistently(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
				&networkingv1.NetworkPolicy{}))
		}, 2*time.Second, interval).Should(BeTrue())
	})

	It("re-creates the NetworkPolicy if deleted manually (proves Owns watch)", func() {
		clusterName := "cluster-np-recreate"
		nodeName := "node-np-recreate"
		createClusterWithNP(clusterName)
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)

		np := &networkingv1.NetworkPolicy{}
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), np)
		Expect(k8sClient.Delete(ctx, np)).To(Succeed())

		Eventually(func() error {
			return k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
				&networkingv1.NetworkPolicy{})
		}, timeout, interval).Should(Succeed())
	})

	It("deletes the NetworkPolicy when removed from the cluster spec", func() {
		clusterName := "cluster-np-del"
		nodeName := "node-np-del"
		createClusterWithNP(clusterName)
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), &networkingv1.NetworkPolicy{})

		Eventually(func() error {
			var fresh v1alpha1.IdentityServerCluster
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, &fresh); err != nil {
				return err
			}
			fresh.Spec.NetworkPolicy = nil
			return k8sClient.Update(ctx, &fresh)
		}, timeout, interval).Should(Succeed())

		eventuallyDeleted(ns, ownedName(clusterName, nodeName), &networkingv1.NetworkPolicy{})
	})

	It("flags Degraded=NetworkPolicyNotOwned for a foreign NetworkPolicy squatting the admin name (no clobber)", func() {
		clusterName := "cluster-np-coll"
		nodeName := "admin-np-coll"
		testCreateCluster(ns, clusterName) // networkPolicy not requested
		npName := ownedName(clusterName, nodeName)

		// A pre-existing NetworkPolicy the operator does not own, squatting the name.
		foreign := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: npName, Namespace: ns},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)

		Eventually(func(g Gomega) {
			var n v1alpha1.IdentityServerNode
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n)).To(Succeed())
			g.Expect(conditionReason(n.Status.Conditions, "Degraded")).To(Equal("NetworkPolicyNotOwned"))
		}, timeout, interval).Should(Succeed())

		// The foreign NetworkPolicy is left untouched (not adopted, not overwritten).
		var np networkingv1.NetworkPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npName, Namespace: ns}, &np)).To(Succeed())
		Expect(np.OwnerReferences).To(BeEmpty(), "foreign NetworkPolicy must not be adopted/clobbered")
	})

	It("keeps NetworkPolicy and runtime Deployment generation stable across reconciles (no drift)", func() {
		clusterName := "cluster-np-idem"
		adminName := "admin-idem"
		runtimeName := "runtime-idem"
		createClusterWithNP(clusterName)
		testCreateNode(ns, adminName, v1alpha1.NodeTypeAdmin, clusterName)

		// Runtime node exercising every new pod-customization field.
		runtime := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: runtimeName, Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                          v1alpha1.NodeTypeRuntime,
				Role:                          "default",
				IdentityServerClusterRef:      v1alpha1.ObjectReference{Name: clusterName},
				Replicas:                      ptr.To(int32(1)),
				Service:                       v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 8443},
				ImagePullPolicy:               corev1.PullAlways,
				TerminationGracePeriodSeconds: ptr.To(int64(120)),
				SecurityContext:               &corev1.PodSecurityContext{FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)},
				ContainerSecurityContext:      &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
				InitContainers:                []corev1.Container{{Name: "wait", Image: "busybox:1.36", Command: []string{"sh", "-c", "true"}}},
				// Rich container (port + probe + lifecycle httpGet) exercises the full
				// container-defaulting path; over-defaulting shows up here as generation drift.
				ExtraContainers: []corev1.Container{{
					Name:  "sidecar",
					Image: "fluent/fluent-bit:3.0",
					Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 2020}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/api/v1/health", Port: intstr.FromInt32(2020)},
					}},
					Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/api/v1/shutdown", Port: intstr.FromInt32(2020)},
					}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, runtime)).To(Succeed())
		testSimulateClusterConfigReady(ns, clusterName)

		npName := ownedName(clusterName, adminName)
		deployName := ownedName(clusterName, runtimeName)

		// Settle: wait for the post-config-ready reconcile to stamp the runtime
		// Deployment's config-hash annotation, then let trailing no-op writes drain.
		Eventually(func(g Gomega) {
			var d appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: ns}, &d)).To(Succeed())
			g.Expect(d.Spec.Template.Annotations).To(HaveKey("curity.io/cluster-config-hash"))
		}, timeout, interval).Should(Succeed())
		eventuallyGetResource(ns, npName, &networkingv1.NetworkPolicy{})
		time.Sleep(2 * time.Second)

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npName, Namespace: ns}, np)).To(Succeed())
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
		npGen, deployGen := np.Generation, deploy.Generation

		// envtest has no controller-manager, so nothing reconciles on its own — force a
		// full rebuild→CreateOrUpdate of both nodes via a metadata poke (the For() watch
		// has no predicate, so any node change re-runs the reconciler).
		for _, n := range []string{adminName, runtimeName} {
			Eventually(func() error {
				var node v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: ns}, &node); err != nil {
					return err
				}
				if node.Annotations == nil {
					node.Annotations = map[string]string{}
				}
				node.Annotations["test.curity.io/poke"] = "1"
				return k8sClient.Update(ctx, &node)
			}, timeout, interval).Should(Succeed())
		}

		// The rebuilt desired objects must DeepEqual the stored ones → no Update →
		// generation flat. Gate on generation (not OperationResult/event count), per the
		// apiserver no-op-write quirk that makes CreateOrUpdate report "updated" falsely.
		Consistently(func(g Gomega) {
			var freshNP networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npName, Namespace: ns}, &freshNP)).To(Succeed())
			g.Expect(freshNP.Generation).To(Equal(npGen), "NetworkPolicy generation drifted")
			var freshDeploy appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: ns}, &freshDeploy)).To(Succeed())
			g.Expect(freshDeploy.Generation).To(Equal(deployGen), "Deployment generation drifted")
		}, 6*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("deletes the operator-owned NetworkPolicy when the node flips admin→runtime", func() {
		clusterName := "cluster-np-flip"
		nodeName := "node-np-flip"
		createClusterWithNP(clusterName)
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), &networkingv1.NetworkPolicy{})

		// Flip admin→runtime: the NP is now orphaned (a runtime node must not carry an
		// admin ingress policy), so the operator must delete it — even though the node
		// keeps its name (so the orphan-by-name sweep would miss it).
		Eventually(func() error {
			var n v1alpha1.IdentityServerNode
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
				return err
			}
			n.Spec.Type = v1alpha1.NodeTypeRuntime
			return k8sClient.Update(ctx, &n)
		}, timeout, interval).Should(Succeed())

		eventuallyDeleted(ns, ownedName(clusterName, nodeName), &networkingv1.NetworkPolicy{})
	})

	It("creates the NetworkPolicy when a node flips runtime→admin", func() {
		clusterName := "cluster-np-flip-up"
		nodeName := "node-np-flip-up"
		createClusterWithNP(clusterName)
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Runtime node: Deployment exists, NP does not.
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), &appsv1.Deployment{})
		Consistently(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
				&networkingv1.NetworkPolicy{}))
		}, 2*time.Second, interval).Should(BeTrue())

		// Flip runtime→admin (replicas=1 keeps it CEL-valid): the operator must now create the NP.
		Eventually(func() error {
			var n v1alpha1.IdentityServerNode
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
				return err
			}
			n.Spec.Type = v1alpha1.NodeTypeAdmin
			return k8sClient.Update(ctx, &n)
		}, timeout, interval).Should(Succeed())
		testSimulateClusterConfigReady(ns, clusterName)

		eventuallyGetResource(ns, ownedName(clusterName, nodeName), &networkingv1.NetworkPolicy{})
	})

	It("clears Degraded=NetworkPolicyNotOwned once the foreign NetworkPolicy is removed", func() {
		clusterName := "cluster-np-recover"
		nodeName := "admin-np-recover"
		testCreateCluster(ns, clusterName) // networkPolicy not requested → delete-path collision
		npName := ownedName(clusterName, nodeName)

		foreign := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: npName, Namespace: ns},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)

		Eventually(func(g Gomega) {
			var n v1alpha1.IdentityServerNode
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n)).To(Succeed())
			g.Expect(conditionReason(n.Status.Conditions, "Degraded")).To(Equal("NetworkPolicyNotOwned"))
		}, timeout, interval).Should(Succeed())

		// Remove the squatter. The foreign NP isn't owned by the node, so its
		// deletion won't auto-trigger a reconcile — poke the node to prove the
		// reconcile LOGIC (rebuild-then-overlay) drops the stale Degraded.
		Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
		Eventually(func() error {
			var n v1alpha1.IdentityServerNode
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
				return err
			}
			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}
			n.Annotations["test.curity.io/poke"] = "1"
			return k8sClient.Update(ctx, &n)
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var n v1alpha1.IdentityServerNode
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n)).To(Succeed())
			g.Expect(conditionReason(n.Status.Conditions, "Degraded")).NotTo(Equal("NetworkPolicyNotOwned"))
		}, timeout, interval).Should(Succeed())
	})
})
