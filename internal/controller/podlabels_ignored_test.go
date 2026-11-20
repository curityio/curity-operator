package controller_test

import (
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerNode Reconciler / PodLabelsIgnored", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond
	const versionLabel = "app.kubernetes.io/version"

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

	// Sums Count: the apiserver merges identical events into one object.
	countEvents := func(objName, reason string) int {
		var events corev1.EventList
		if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
			return -1
		}
		n := 0
		for _, e := range events.Items {
			if e.InvolvedObject.Name == objName && e.Reason == reason {
				n += max(int(e.Count), 1)
			}
		}
		return n
	}

	// A cluster podLabels edit doesn't advance the node's generation, so a
	// generation-only guard never warned on this path.
	It("warns once when a cluster podLabels edit adds an operator-owned key", func() {
		const clusterName, nodeName = "pli-cluster", "pli-admin"
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			// Digest-only and no version: the operator sets no version label.
			Spec: v1alpha1.IdentityServerClusterSpec{Image: "reg/idsvr@sha256:" + strings.Repeat("a", 64)},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)
		eventuallyGetResource(ns, ownedName(clusterName, nodeName), &appsv1.Deployment{})

		var node v1alpha1.IdentityServerNode
		Eventually(func() bool {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node)
			return node.Generation > 0 && node.Status.ObservedGeneration == node.Generation
		}, timeout, interval).Should(BeTrue())
		generation := node.Generation
		Expect(countEvents(nodeName, "PodLabelsIgnored")).To(Equal(0))

		By("adding the version key to the cluster's podLabels")
		Eventually(func() error {
			var c v1alpha1.IdentityServerCluster
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, &c); err != nil {
				return err
			}
			c.Spec.PodLabels = map[string]string{versionLabel: "11.0", "team": "iam"}
			return k8sClient.Update(ctx, &c)
		}, timeout, interval).Should(Succeed())

		Eventually(func() int { return countEvents(nodeName, "PodLabelsIgnored") }, timeout, interval).Should(Equal(1))

		Eventually(func(g Gomega) {
			var d appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &d)).To(Succeed())
			g.Expect(d.Spec.Template.Labels).To(HaveKeyWithValue("team", "iam"))
			g.Expect(d.Spec.Template.Labels).NotTo(HaveKey(versionLabel))
		}, timeout, interval).Should(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node)).To(Succeed())
		Expect(node.Generation).To(Equal(generation), "the warning must not depend on a node spec edit")

		By("not re-warning on later reconciles while the ignored set is unchanged")
		Eventually(func() error {
			var n v1alpha1.IdentityServerNode
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &n); err != nil {
				return err
			}
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}
			n.Labels["pli-test.nudge/seq"] = strconv.FormatInt(time.Now().UnixNano(), 10)
			return k8sClient.Update(ctx, &n)
		}, timeout, interval).Should(Succeed())
		Consistently(func() int { return countEvents(nodeName, "PodLabelsIgnored") }, 3*time.Second, 500*time.Millisecond).Should(Equal(1))
	})
})
