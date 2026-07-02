package controller_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
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

var _ = Describe("IdentityServerNode Reconciler / HPA hardening", func() {
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
				CommonPodConfig: v1alpha1.CommonPodConfig{
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
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
