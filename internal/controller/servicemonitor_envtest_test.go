package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerCluster ServiceMonitor", func() {
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

	getServiceMonitor := func(clusterName string) (*monitoringv1.ServiceMonitor, error) {
		sm := &monitoringv1.ServiceMonitor{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, "metrics"), Namespace: ns}, sm)
		return sm, err
	}

	clusterStatusSMName := func(clusterName string) string {
		c := &v1alpha1.IdentityServerCluster{}
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c)
		return c.Status.ServiceMonitorName
	}

	Context("default (on by default)", func() {
		It("creates a cluster-owned ServiceMonitor and records its name in status", func() {
			testCreateCluster(ns, "obs-default")

			var sm *monitoringv1.ServiceMonitor
			Eventually(func() error {
				var err error
				sm, err = getServiceMonitor("obs-default")
				return err
			}, timeout, interval).Should(Succeed())

			By("selecting all node Services for the cluster")
			Expect(sm.Spec.Selector.MatchLabels).To(Equal(map[string]string{
				"curity.io/cluster":            "obs-default",
				"app.kubernetes.io/managed-by": "curity-operator",
			}))
			Expect(sm.Spec.NamespaceSelector.MatchNames).To(Equal([]string{ns}))
			Expect(sm.Spec.Endpoints).To(HaveLen(1))
			Expect(sm.Spec.Endpoints[0].Port).To(Equal("metrics"))
			Expect(sm.Spec.Endpoints[0].Path).To(Equal("/metrics"))
			Expect(string(sm.Spec.Endpoints[0].Interval)).To(Equal("30s"))

			By("owning the ServiceMonitor for garbage collection")
			Expect(sm.OwnerReferences).To(HaveLen(1))
			Expect(sm.OwnerReferences[0].Kind).To(Equal("IdentityServerCluster"))
			Expect(sm.OwnerReferences[0].Name).To(Equal("obs-default"))
			Expect(sm.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))

			By("surfacing the name in status")
			Eventually(func() string {
				return clusterStatusSMName("obs-default")
			}, timeout, interval).Should(Equal(ownedName("obs-default", "metrics")))

			By("not churning the ServiceMonitor on subsequent reconciles")
			sm, err := getServiceMonitor("obs-default")
			Expect(err).NotTo(HaveOccurred())
			rv := sm.ResourceVersion
			Consistently(func() string {
				got, gErr := getServiceMonitor("obs-default")
				if gErr != nil {
					return "gone"
				}
				return got.ResourceVersion
			}, 2*time.Second, 250*time.Millisecond).Should(Equal(rv), "ServiceMonitor must not be rewritten when nothing changed")
		})
	})

	Context("custom labels and interval", func() {
		It("applies user labels (operator keys win) and the configured interval", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "obs-custom", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Observability: &v1alpha1.ObservabilitySpec{
						ServiceMonitor: &v1alpha1.ServiceMonitorSpec{
							Labels:   map[string]string{"release": "kube-prometheus-stack"},
							Interval: "15s",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			Eventually(func() map[string]string {
				sm, err := getServiceMonitor("obs-custom")
				if err != nil {
					return nil
				}
				return sm.Labels
			}, timeout, interval).Should(SatisfyAll(
				HaveKeyWithValue("release", "kube-prometheus-stack"),
				HaveKeyWithValue("app.kubernetes.io/managed-by", "curity-operator"),
				HaveKeyWithValue("curity.io/cluster", "obs-custom"),
			))

			sm, _ := getServiceMonitor("obs-custom")
			Expect(string(sm.Spec.Endpoints[0].Interval)).To(Equal("15s"))
		})
	})

	Context("explicitly disabled", func() {
		It("deletes a previously-created ServiceMonitor and clears status", func() {
			testCreateCluster(ns, "obs-toggle")
			Eventually(func() error {
				_, err := getServiceMonitor("obs-toggle")
				return err
			}, timeout, interval).Should(Succeed())

			By("setting enabled: false")
			cluster := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obs-toggle", Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Observability = &v1alpha1.ObservabilitySpec{
				ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: ptr.To(false)},
			}
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			Eventually(func() bool {
				_, err := getServiceMonitor("obs-toggle")
				return err != nil
			}, timeout, interval).Should(BeTrue(), "ServiceMonitor should be deleted")

			Eventually(func() string {
				return clusterStatusSMName("obs-toggle")
			}, timeout, interval).Should(BeEmpty())
		})
	})

	Context("admission validation (negative)", func() {
		makeCluster := func(name string, sm *v1alpha1.ServiceMonitorSpec) *v1alpha1.IdentityServerCluster {
			return &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:       "11.0",
					Observability: &v1alpha1.ObservabilitySpec{ServiceMonitor: sm},
				},
			}
		}

		It("rejects labels in the operator-owned curity.io/* and app.kubernetes.io/* namespaces", func() {
			err := k8sClient.Create(ctx, makeCluster("bad-labels", &v1alpha1.ServiceMonitorSpec{
				Labels: map[string]string{"curity.io/team": "x"},
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("operator-owned keys"))

			err = k8sClient.Create(ctx, makeCluster("bad-labels-akb", &v1alpha1.ServiceMonitorSpec{
				Labels: map[string]string{"app.kubernetes.io/part-of": "x"},
			}))
			Expect(err).To(HaveOccurred())
		})

		It("rejects an interval without a time unit", func() {
			err := k8sClient.Create(ctx, makeCluster("bad-interval-nounit", &v1alpha1.ServiceMonitorSpec{
				Interval: "30",
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("interval"))
		})

		It("rejects a non-numeric interval", func() {
			err := k8sClient.Create(ctx, makeCluster("bad-interval-abc", &v1alpha1.ServiceMonitorSpec{
				Interval: "abc",
			}))
			Expect(err).To(HaveOccurred())
		})

		It("accepts valid compound/sub-second intervals", func() {
			Expect(k8sClient.Create(ctx, makeCluster("ok-ms", &v1alpha1.ServiceMonitorSpec{Interval: "500ms"}))).To(Succeed())
			Expect(k8sClient.Create(ctx, makeCluster("ok-compound", &v1alpha1.ServiceMonitorSpec{Interval: "2h30m"}))).To(Succeed())
		})

		It("defaults enabled=true and interval=30s when serviceMonitor is an empty block", func() {
			Expect(k8sClient.Create(ctx, makeCluster("defaulted", &v1alpha1.ServiceMonitorSpec{}))).To(Succeed())

			By("the apiserver applying the CRD defaults")
			Eventually(func() bool {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "defaulted", Namespace: ns}, c); err != nil {
					return false
				}
				sm := c.Spec.Observability.ServiceMonitor
				return sm.Enabled != nil && *sm.Enabled && sm.Interval == "30s"
			}, timeout, interval).Should(BeTrue())

			By("and still creating the ServiceMonitor")
			Eventually(func() error {
				_, err := getServiceMonitor("defaulted")
				return err
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("ownership guard (never hijack a foreign ServiceMonitor)", func() {
		foreignSM := func(clusterName string) *monitoringv1.ServiceMonitor {
			return &monitoringv1.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ownedName(clusterName, "metrics"),
					Namespace: ns,
					Labels:    map[string]string{"owner": "someone-else"},
				},
				Spec: monitoringv1.ServiceMonitorSpec{
					Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "foreign"}},
					Endpoints: []monitoringv1.Endpoint{{Port: "foreign"}},
				},
			}
		}

		It("does not adopt or overwrite a same-named ServiceMonitor it does not own", func() {
			Expect(k8sClient.Create(ctx, foreignSM("obs-guard"))).To(Succeed())
			testCreateCluster(ns, "obs-guard")

			By("emitting ServiceMonitorNotOwned")
			Eventually(func() bool {
				evs := &corev1.EventList{}
				_ = k8sClient.List(ctx, evs, client.InNamespace(ns))
				for i := range evs.Items {
					if evs.Items[i].Reason == "ServiceMonitorNotOwned" && evs.Items[i].InvolvedObject.Name == "obs-guard" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			By("leaving the foreign object untouched (no owner ref, original spec)")
			Consistently(func() bool {
				sm, err := getServiceMonitor("obs-guard")
				if err != nil {
					return false
				}
				return len(sm.OwnerReferences) == 0 &&
					sm.Labels["owner"] == "someone-else" &&
					sm.Spec.Endpoints[0].Port == "foreign"
			}, 2*time.Second, 250*time.Millisecond).Should(BeTrue())

			By("and not claiming it in status")
			Expect(clusterStatusSMName("obs-guard")).To(BeEmpty())

			By("surfacing the collision as Degraded (not a silent green)")
			Eventually(func() string {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "obs-guard", Namespace: ns}, c); err != nil {
					return ""
				}
				if d := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionDegraded); d != nil && d.Status == metav1.ConditionTrue {
					return d.Reason
				}
				return ""
			}, timeout, interval).Should(Equal("ServiceMonitorNotOwned"))
		})
	})

	Context("lifecycle", func() {
		It("recreates the ServiceMonitor if it is deleted out-of-band (drift)", func() {
			testCreateCluster(ns, "obs-drift")
			var uid1 types.UID
			Eventually(func() error {
				sm, err := getServiceMonitor("obs-drift")
				if err == nil {
					uid1 = sm.UID
				}
				return err
			}, timeout, interval).Should(Succeed())

			By("deleting the operator-owned ServiceMonitor")
			sm, _ := getServiceMonitor("obs-drift")
			Expect(k8sClient.Delete(ctx, sm)).To(Succeed())

			By("the operator recreating it (Owns watch re-enqueues)")
			Eventually(func() bool {
				got, err := getServiceMonitor("obs-drift")
				return err == nil && got.UID != uid1
			}, timeout, interval).Should(BeTrue())
		})

		It("updates the ServiceMonitor in place when labels change (same object)", func() {
			testCreateCluster(ns, "obs-update")
			var uid1 types.UID
			Eventually(func() error {
				sm, err := getServiceMonitor("obs-update")
				if err == nil {
					uid1 = sm.UID
				}
				return err
			}, timeout, interval).Should(Succeed())

			By("adding a label via the cluster spec")
			c := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obs-update", Namespace: ns}, c)).To(Succeed())
			c.Spec.Observability = &v1alpha1.ObservabilitySpec{
				ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Labels: map[string]string{"release": "kps"}},
			}
			Expect(k8sClient.Update(ctx, c)).To(Succeed())

			Eventually(func() bool {
				sm, err := getServiceMonitor("obs-update")
				return err == nil && sm.Labels["release"] == "kps" && sm.UID == uid1
			}, timeout, interval).Should(BeTrue(), "same object updated in place, not recreated")
		})

		It("recreates the ServiceMonitor when re-enabled after a disable", func() {
			testCreateCluster(ns, "obs-reenable")
			Eventually(func() error { _, err := getServiceMonitor("obs-reenable"); return err }, timeout, interval).Should(Succeed())

			setEnabled := func(v bool) {
				c := &v1alpha1.IdentityServerCluster{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obs-reenable", Namespace: ns}, c)).To(Succeed())
				c.Spec.Observability = &v1alpha1.ObservabilitySpec{ServiceMonitor: &v1alpha1.ServiceMonitorSpec{Enabled: ptr.To(v)}}
				Expect(k8sClient.Update(ctx, c)).To(Succeed())
			}

			By("disabling → deleted")
			setEnabled(false)
			Eventually(func() bool { _, err := getServiceMonitor("obs-reenable"); return err != nil }, timeout, interval).Should(BeTrue())

			By("re-enabling → recreated")
			setEnabled(true)
			Eventually(func() error { _, err := getServiceMonitor("obs-reenable"); return err }, timeout, interval).Should(Succeed())
			Eventually(func() string { return clusterStatusSMName("obs-reenable") }, timeout, interval).ShouldNot(BeEmpty())
		})
	})
})
