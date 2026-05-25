package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("Deployment scheduling", func() {
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

	It("should inherit scheduling constraints from cluster on Deployment", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "sched-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:      "11.0",
				NodeSelector: map[string]string{"disk": "ssd", "zone": "us-west"},
				Tolerations: []corev1.Toleration{
					{Key: "special", Operator: corev1.TolerationOpExists},
				},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      "kubernetes.io/arch",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"amd64"},
								}},
							}},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "sched-node", v1alpha1.NodeTypeRuntime, "sched-cluster")

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("sched-cluster", "sched-node"), deploy)
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("disk", "ssd"))
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("zone", "us-west"))
		// 1 user-supplied toleration + 2 NoExecute defaults appended by buildDeployment
		// to match what the DefaultTolerationSeconds admission controller would add.
		Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(3))
		Expect(deploy.Spec.Template.Spec.Affinity).NotTo(BeNil())
		Expect(deploy.Spec.Template.Spec.Affinity.NodeAffinity).NotTo(BeNil())
	})

	It("should apply node-level scheduling over cluster defaults", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "override-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:      "11.0",
				NodeSelector: map[string]string{"pool": "default"},
				Tolerations: []corev1.Toleration{
					{Key: "old-taint", Operator: corev1.TolerationOpExists},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "override-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "override-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "override-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				NodeSelector:             map[string]string{"pool": "gpu"},
				Tolerations: []corev1.Toleration{
					{Key: "new-taint", Operator: corev1.TolerationOpEqual, Value: "yes"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("override-cluster", "override-node"), deploy)

		// nodeSelector merges: node wins on conflict
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("pool", "gpu"))
		// tolerations: node fully replaces cluster, then 2 NoExecute defaults are appended.
		Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(3))
		Expect(deploy.Spec.Template.Spec.Tolerations[0].Key).To(Equal("new-taint"))
	})

	It("should apply topology spread constraints from cluster to Deployment", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "topo-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       "topology.kubernetes.io/zone",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "curity"},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "topo-node", v1alpha1.NodeTypeRuntime, "topo-cluster")

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("topo-cluster", "topo-node"), deploy)
		Expect(deploy.Spec.Template.Spec.TopologySpreadConstraints).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.TopologySpreadConstraints[0].TopologyKey).To(Equal("topology.kubernetes.io/zone"))
	})

	It("should update Deployment when cluster scheduling changes", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "update-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:      "11.0",
				NodeSelector: map[string]string{"env": "staging"},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "update-node", v1alpha1.NodeTypeRuntime, "update-cluster")

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("update-cluster", "update-node"), deploy)
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("env", "staging"))

		// Update cluster nodeSelector
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.NodeSelector = map[string]string{"env": "production"}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		// Deployment should eventually reflect the change
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("update-cluster", "update-node"), Namespace: ns}, deploy)
			return deploy.Spec.Template.Spec.NodeSelector["env"]
		}, timeout, interval).Should(Equal("production"))
	})

	// =================================================================
	// HPA lifecycle
	// =================================================================

	It("should create HPA for runtime node with autoscaling enabled", func() {
		testCreateCluster(ns, "hpa-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "hpa-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    2,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		eventuallyGetResource(ns, ownedName("hpa-cluster", "hpa-node"), hpa)

		Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(ownedName("hpa-cluster", "hpa-node")))
		Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
		Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))
		Expect(hpa.OwnerReferences).To(HaveLen(1))
		Expect(hpa.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
	})

	It("should set Deployment replicas to minReplicas when HPA enabled", func() {
		testCreateCluster(ns, "hpa-rep-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-rep-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "hpa-rep-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-rep-cluster"},
				Replicas:                 ptr.To(int32(5)),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    3,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
				Service: defaultTestService(),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("hpa-rep-cluster", "hpa-rep-node"), deploy)

		Eventually(func(g Gomega) int32 {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hpa-rep-cluster", "hpa-rep-node"), Namespace: ns}, deploy)).To(Succeed())
			if deploy.Spec.Replicas == nil {
				return 0
			}
			return *deploy.Spec.Replicas
		}, 30*time.Second, 250*time.Millisecond).Should(Equal(int32(3)))
	})

	// Note: "admin + autoscaling.enabled=true" is now rejected at admission
	// by a CEL rule on IdentityServerNodeSpec (see CRD validation tests for
	// the rejection assertion). The runtime "ignored on admin" path is no
	// longer reachable via the API; the admission-rejection test supersedes
	// what this Describe used to cover.

	It("should delete HPA when autoscaling is disabled", func() {
		testCreateCluster(ns, "hpa-del-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-del-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "del-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-del-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    2,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		// Wait for HPA to be created
		eventuallyGetResource(ns, ownedName("hpa-del-cluster", "hpa-del-node"), &autoscalingv2.HorizontalPodAutoscaler{})

		// Disable autoscaling
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hpa-del-node", Namespace: ns}, node); err != nil {
				return err
			}
			node.Spec.Autoscaling.Enabled = false
			return k8sClient.Update(ctx, node)
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		// HPA should be deleted
		eventuallyDeleted(ns, ownedName("hpa-del-cluster", "hpa-del-node"), &autoscalingv2.HorizontalPodAutoscaler{})
	})

	It("should clean up stale HPA when node type changes to admin", func() {
		testCreateCluster(ns, "hpa-stale-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-stale", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "stale-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-stale-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    2,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		// Wait for HPA to be created
		eventuallyGetResource(ns, ownedName("hpa-stale-cluster", "hpa-stale"), &autoscalingv2.HorizontalPodAutoscaler{})

		// Change node type to admin. CEL admission rejects admin+autoscaling.enabled=true,
		// so the type-change update must also disable autoscaling in the same patch.
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hpa-stale", Namespace: ns}, node); err != nil {
				return err
			}
			node.Spec.Type = v1alpha1.NodeTypeAdmin
			node.Spec.Autoscaling.Enabled = false
			return k8sClient.Update(ctx, node)
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		// HPA should be cleaned up even though node is now admin
		eventuallyDeleted(ns, ownedName("hpa-stale-cluster", "hpa-stale"), &autoscalingv2.HorizontalPodAutoscaler{})

		// Type-flip to admin makes adminExists=true and the cluster-config gate
		// kicks in on the next reconcile. Unblock so the admin-replica coercion
		// can land on the Deployment.
		testSimulateClusterConfigReady(ns, "hpa-stale-cluster")

		// Deployment replicas should be forced to 1
		deploy := &appsv1.Deployment{}
		Eventually(func(g Gomega) int32 {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hpa-stale-cluster", "hpa-stale"), Namespace: ns}, deploy)).To(Succeed())
			if deploy.Spec.Replicas == nil {
				return 0
			}
			return *deploy.Spec.Replicas
		}, 30*time.Second, 250*time.Millisecond).Should(Equal(int32(1)))
	})
})
