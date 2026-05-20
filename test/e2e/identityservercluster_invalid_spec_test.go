package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

// hasClusterEvent returns true if any Event on the IdentityServerCluster
// matches both reason and event type. The existing hasEvent helper in
// packages_test.go is hardcoded to IdentityServerNode, so this is a
// cluster-side sibling — not extracted to test/utils because cluster events
// are only used by this one test today.
func hasClusterEvent(ctx context.Context, ns, clusterName, reason, eventType string) bool {
	events := &corev1.EventList{}
	if err := k().List(ctx, events, client.InNamespace(ns)); err != nil {
		return false
	}
	for _, e := range events.Items {
		if e.InvolvedObject.Kind == "IdentityServerCluster" &&
			e.InvolvedObject.Name == clusterName &&
			e.Reason == reason &&
			e.Type == eventType {
			return true
		}
	}
	return false
}

// hasReconcilerErrorFor reports whether the operator's structured logs
// contain a controller-runtime "Reconciler error" line that mentions the
// given cluster name. Used to verify the storm-fix smoke target: zero
// retry-error log lines for a bad CR after the classifier kicks in.
func hasReconcilerErrorFor(clusterName string) bool {
	out, err := utils.Run("kubectl", "logs",
		"-n", "curity-operator",
		"deployment/curity-operator-controller-manager",
		"--tail=2000",
	)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"msg":"Reconciler error"`) &&
			strings.Contains(line, clusterName) &&
			strings.Contains(line, `"controllerKind":"IdentityServerCluster"`) {
			return true
		}
	}
	return false
}

var _ = Describe("IdentityServerCluster invalid-spec classifier", Ordered, func() {
	const (
		ns          = "e2e-invalid-spec"
		clusterName = "bad-cluster"
	)

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("surfaces Degraded=True/InvalidSpec on a CR with apiserver-invalid Secret name", func() {
		ctx := context.Background()

		By("applying a CR with a Secret name that passes the CRD Pattern but fails apiserver DNS-1123")
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       clusterName,
				"namespace":  ns,
				"secretName": "bad..secret..name",
			})

		By("Degraded condition flips to True/InvalidSpec with the apiserver message")
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil(), "Degraded condition not set yet")
			g.Expect(deg.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
			g.Expect(deg.Message).To(ContainSubstring("Secret rejected by apiserver:"))
			g.Expect(deg.Message).To(ContainSubstring("bad..secret..name"))
			// The unwrap regression-guard: operator-wrap text must not leak.
			g.Expect(deg.Message).NotTo(ContainSubstring("failed to create admin credentials secret"))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("helper-set Degraded is the ONLY condition; operational conditions stay absent on bad-spec bail")
		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
		for _, condType := range []string{
			v1alpha1.ConditionReady,
			v1alpha1.ConditionAvailable,
			v1alpha1.ConditionProgressing,
			v1alpha1.ConditionClusterConfigReady,
		} {
			c := apimeta.FindStatusCondition(cluster.Status.Conditions, condType)
			Expect(c).To(BeNil(), "helper must not write %s on bad-spec bail; got: %+v", condType, c)
		}

		By("a Warning event with reason=InvalidSpec was emitted")
		// Use Eventually so the event has time to propagate from the recorder buffer.
		Eventually(func(g Gomega) {
			g.Expect(hasClusterEvent(ctx, ns, clusterName, "InvalidSpec", "Warning")).
				To(BeTrue(), "Warning event with reason=InvalidSpec not observed")
		}, 15*time.Second, time.Second).Should(Succeed())
	})

	It("does not retry-storm: zero Reconciler error log lines over 60s while bad CR sits", func() {
		// The plan + CHECKLIST set the manual smoke target at 5 minutes; 60s
		// here is a CI-friendly window that still spans the early backoff
		// peaks (controller-runtime's default workqueue ramp-up).
		Consistently(func(g Gomega) {
			g.Expect(hasReconcilerErrorFor(clusterName)).
				To(BeFalse(), "Reconciler error line for %q appeared in operator logs — classifier did not bail", clusterName)
		}, 60*time.Second, 5*time.Second).Should(Succeed())
	})

	It("does NOT emit a second event when an unrelated spec field changes while still bad", func() {
		ctx := context.Background()

		// Capture current Warning event count for this CR.
		countInvalidSpecEvents := func() int {
			events := &corev1.EventList{}
			Expect(k().List(ctx, events, client.InNamespace(ns))).To(Succeed())
			n := 0
			for _, e := range events.Items {
				if e.InvolvedObject.Kind == "IdentityServerCluster" &&
					e.InvolvedObject.Name == clusterName &&
					e.Reason == "InvalidSpec" {
					n += int(e.Count)
				}
			}
			return n
		}
		before := countInvalidSpecEvents()
		Expect(before).To(BeNumerically(">=", 1))

		By("editing a non-classified field (spec.version) while Secret name is still bad")
		Eventually(func() error {
			cluster := &v1alpha1.IdentityServerCluster{}
			if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
				return err
			}
			cluster.Spec.Version = "11.0" // identical to current — bumps Generation though
			return k().Update(ctx, cluster)
		}, 30*time.Second, time.Second).Should(Succeed())

		// Wait a few reconcile turns. Event count must not grow because the
		// helper's changed-bool gate suppresses the duplicate Status().Update
		// + Event emission when the Degraded condition is already at the
		// target state.
		Consistently(func(g Gomega) {
			g.Expect(countInvalidSpecEvents()).To(Equal(before),
				"InvalidSpec event count grew during spec churn; changed-bool gate is leaking")
		}, 15*time.Second, 3*time.Second).Should(Succeed())
	})

	It("emits a NEW event when the user changes to a different bad spec value", func() {
		ctx := context.Background()

		By("changing to a different bad-name value")
		Eventually(func() error {
			cluster := &v1alpha1.IdentityServerCluster{}
			if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
				return err
			}
			cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name = "other..bad..name"
			return k().Update(ctx, cluster)
		}, 30*time.Second, time.Second).Should(Succeed())

		By("the Degraded message updates to reference the new bad name")
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
			g.Expect(deg.Message).To(ContainSubstring("other..bad..name"))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("recovers when the CR is patched with a valid Secret name", func() {
		ctx := context.Background()

		By("patching the CR with a valid Secret name")
		Eventually(func() error {
			cluster := &v1alpha1.IdentityServerCluster{}
			if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
				return err
			}
			cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name = "valid-secret-name"
			return k().Update(ctx, cluster)
		}, 30*time.Second, time.Second).Should(Succeed())

		By("Degraded transitions OFF InvalidSpec (computeClusterConditions rebuilds it)")
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).NotTo(Equal(v1alpha1.ReasonInvalidSpec),
				"Degraded should have transitioned off InvalidSpec; got reason=%q", deg.Reason)
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("deletes cleanly while in Degraded=InvalidSpec state", func() {
		ctx := context.Background()

		// Apply a fresh bad CR that we'll delete from this state.
		const deleteName = "delete-while-bad"
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       deleteName,
				"namespace":  ns,
				"secretName": "bad..delete..name",
			})

		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: deleteName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("deleting the CR — finalizer must release because no child nodes exist")
		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k().Get(ctx, client.ObjectKey{Name: deleteName, Namespace: ns}, cluster)).To(Succeed())
		Expect(k().Delete(ctx, cluster)).To(Succeed())

		Eventually(func() bool {
			cluster := &v1alpha1.IdentityServerCluster{}
			err := k().Get(ctx, client.ObjectKey{Name: deleteName, Namespace: ns}, cluster)
			return err != nil // expect NotFound
		}, 30*time.Second, time.Second).Should(BeTrue(),
			"CR did not delete cleanly from Degraded=InvalidSpec state")
	})
})

// Separate Describe so the operator-restart and multi-CR scenarios don't
// share Ordered/state with the main flow.
var _ = Describe("IdentityServerCluster invalid-spec — multi-CR isolation", Ordered, func() {
	const ns = "e2e-invalid-spec-multi"

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("classifies multiple bad CRs in the same namespace independently", func() {
		ctx := context.Background()

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       "bad-a",
				"namespace":  ns,
				"secretName": "first..bad..name",
			})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       "bad-b",
				"namespace":  ns,
				"secretName": "second..bad..name",
			})

		// Both CRs get classified, and their messages reference the
		// respective bad names — not each other's. Catches cross-CR state
		// bleed in the helper.
		Eventually(func(g Gomega) {
			for _, pair := range []struct{ name, want string }{
				{"bad-a", "first..bad..name"},
				{"bad-b", "second..bad..name"},
			} {
				cluster := &v1alpha1.IdentityServerCluster{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: pair.name, Namespace: ns}, cluster)).To(Succeed())
				deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
				g.Expect(deg).NotTo(BeNil(), "%s missing Degraded", pair.name)
				g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec), "%s wrong reason", pair.name)
				g.Expect(deg.Message).To(ContainSubstring(pair.want), "%s does not reference its own bad name", pair.name)
			}
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})

// Helper-bail preserves operational conditions injected by another writer
// (or by a prior reconcile that completed successfully). This is the
// structural test for the "leave operational conditions untouched" decision.
var _ = Describe("IdentityServerCluster invalid-spec — operational conditions preserved through helper bail", Ordered, func() {
	const (
		ns          = "e2e-invalid-spec-preserve"
		clusterName = "preserve-bad-cluster"
	)

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("does not strip Ready/Available conditions that were set externally while the spec stays bad", func() {
		ctx := context.Background()

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       clusterName,
				"namespace":  ns,
				"secretName": "bad..preserve..name",
			})

		// Wait for the helper to set Degraded.
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
		}, 30*time.Second, time.Second).Should(Succeed())

		// Externally inject Ready=True/ManualForTest into the CR status.
		// This simulates the previously-Ready scenario where a prior
		// reconcile populated these conditions before the spec went bad.
		By("manually adding Ready=True and Available=True to status")
		Eventually(func() error {
			cluster := &v1alpha1.IdentityServerCluster{}
			if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
				return err
			}
			apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    v1alpha1.ConditionReady,
				Status:  metav1.ConditionTrue,
				Reason:  "ManualForTest",
				Message: "injected to verify helper does not strip operational conditions",
			})
			apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    v1alpha1.ConditionAvailable,
				Status:  metav1.ConditionTrue,
				Reason:  "ManualForTest",
				Message: "injected to verify helper does not strip operational conditions",
			})
			return k().Status().Update(ctx, cluster)
		}, 30*time.Second, time.Second).Should(Succeed())

		// Touch the spec with a non-classified change to provoke more reconciles.
		// Each reconcile re-runs the helper. The changed-bool gate prevents
		// Status().Update; importantly, the helper also does NOT touch
		// Ready/Available so they must persist.
		By("provoking reconciles by touching a non-classified spec field")
		for i := 0; i < 3; i++ {
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				if cluster.Spec.PodLabels == nil {
					cluster.Spec.PodLabels = map[string]string{}
				}
				cluster.Spec.PodLabels["e2e-touch"] = fmt.Sprintf("v%d", i)
				return k().Update(ctx, cluster)
			}, 30*time.Second, time.Second).Should(Succeed())
		}

		// Consistently: Ready/Available must remain True/ManualForTest.
		// If the helper accidentally wrote operational conditions, this fails.
		Consistently(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())

			ready := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil(), "Ready stripped — helper touched a condition it shouldn't")
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(ready.Reason).To(Equal("ManualForTest"))

			avail := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionAvailable)
			g.Expect(avail).NotTo(BeNil(), "Available stripped — helper touched a condition it shouldn't")
			g.Expect(avail.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(avail.Reason).To(Equal("ManualForTest"))
		}, 20*time.Second, 4*time.Second).Should(Succeed())
	})
})

// Rapid-churn scenarios: stress the changed-bool gate across many writes.
var _ = Describe("IdentityServerCluster invalid-spec — rapid churn", Ordered, func() {
	const ns = "e2e-invalid-spec-churn"

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("survives 5 rapid bad-name changes — last name wins, no event explosion", func() {
		ctx := context.Background()
		const clusterName = "rapid-churn"

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       clusterName,
				"namespace":  ns,
				"secretName": "churn..bad..0",
			})

		// Wait for initial classification so we have a baseline.
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
		}, 30*time.Second, time.Second).Should(Succeed())

		// Rapid churn — 5 different bad names in quick succession.
		for i := 1; i <= 5; i++ {
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name = fmt.Sprintf("churn..bad..%d", i)
				return k().Update(ctx, cluster)
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed())
		}

		// Eventually the CR settles on the last name in its Degraded message.
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
			g.Expect(deg.Message).To(ContainSubstring("churn..bad..5"))
		}, 30*time.Second, time.Second).Should(Succeed())

		// Event count bounded by the number of distinct messages (≤6:
		// the initial churn..bad..0 + 5 increments). Allow some slop for
		// transient retries but assert it's not in the dozens.
		events := &corev1.EventList{}
		Expect(k().List(ctx, events, client.InNamespace(ns))).To(Succeed())
		invalidSpecEventCount := 0
		for _, e := range events.Items {
			if e.InvolvedObject.Kind == "IdentityServerCluster" &&
				e.InvolvedObject.Name == clusterName &&
				e.Reason == "InvalidSpec" {
				invalidSpecEventCount += int(e.Count)
			}
		}
		Expect(invalidSpecEventCount).To(BeNumerically("<=", 12),
			"event count exploded under churn — saw %d events for 6 distinct spec values", invalidSpecEventCount)
	})

	It("apply-then-delete race: rapid create+delete cycles do not leave operator confused", func() {
		ctx := context.Background()
		const clusterName = "apply-delete-race"

		// Apply, delete, apply, delete, apply — final apply is the survivor.
		for i := 0; i < 3; i++ {
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
				map[string]interface{}{
					"name":       clusterName,
					"namespace":  ns,
					"secretName": fmt.Sprintf("race..bad..%d", i),
				})

			if i < 2 {
				cluster := &v1alpha1.IdentityServerCluster{}
				Eventually(func() error {
					return k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)
				}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
				Expect(k().Delete(ctx, cluster)).To(Succeed())

				// Wait for deletion to actually complete before next apply.
				Eventually(func() bool {
					c := &v1alpha1.IdentityServerCluster{}
					err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, c)
					return err != nil
				}, 30*time.Second, time.Second).Should(BeTrue())
			}
		}

		// Final CR survives and gets correctly classified with the last bad name.
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
			g.Expect(deg.Message).To(ContainSubstring("race..bad..2"))
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})

// Operator-restart scenario: helper must be idempotent on synthetic-Create
// events fired by controller-runtime when its cache re-syncs after pod
// restart. The changed-bool gate suppresses spurious status writes + events.
var _ = Describe("IdentityServerCluster invalid-spec — operator restart resilience", Ordered, func() {
	const (
		ns          = "e2e-invalid-spec-restart"
		clusterName = "restart-bad-cluster"
	)

	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("does not emit duplicate events when the operator pod is restarted while CR is Degraded=InvalidSpec", func() {
		ctx := context.Background()

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{
				"name":       clusterName,
				"namespace":  ns,
				"secretName": "restart..bad..name",
			})

		// Wait for initial classification.
		Eventually(func(g Gomega) {
			cluster := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
		}, 30*time.Second, time.Second).Should(Succeed())

		// Capture event count before restart.
		countInvalidSpecEvents := func() int {
			events := &corev1.EventList{}
			Expect(k().List(ctx, events, client.InNamespace(ns))).To(Succeed())
			n := 0
			for _, e := range events.Items {
				if e.InvolvedObject.Kind == "IdentityServerCluster" &&
					e.InvolvedObject.Name == clusterName &&
					e.Reason == "InvalidSpec" {
					n += int(e.Count)
				}
			}
			return n
		}
		before := countInvalidSpecEvents()

		By("restarting the operator pod")
		_, err := utils.Run("kubectl", "-n", "curity-operator",
			"rollout", "restart", "deployment/curity-operator-controller-manager")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			_, err := utils.Run("kubectl", "-n", "curity-operator",
				"rollout", "status", "deployment/curity-operator-controller-manager",
				"--timeout=60s")
			return err
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		// Give the manager cache-sync time to fire its synthetic Create
		// event for the CR, then assert no event growth (changed-bool gate
		// gives us idempotent re-classification).
		Consistently(func(g Gomega) {
			g.Expect(countInvalidSpecEvents()).To(Equal(before),
				"InvalidSpec event count grew after operator restart; changed-bool gate is failing on synthetic Create")
		}, 20*time.Second, 5*time.Second).Should(Succeed())
	})
})
