package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerCluster invalid-spec classifier", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	It("flips Degraded=True/InvalidSpec and Ready=False/InvalidSpec when adminCredentials Secret name fails apiserver DNS-1123", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				AdminCredentials: &v1alpha1.CredentialsSource{
					ValueFrom: v1alpha1.CredentialsValueFrom{
						SecretKeyRef: v1alpha1.SecretKeyRefSource{
							Name: "bad..secret..name",
							Items: []v1alpha1.KeyToPath{
								{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
								{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
								{Key: "KEYSTORE_PASSWORD", Path: "KEYSTORE_PASSWORD"},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		// Eventually Degraded=True/InvalidSpec appears.
		Eventually(func(g Gomega) {
			var fresh v1alpha1.IdentityServerCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "bad-cluster", Namespace: ns}, &fresh)).To(Succeed())
			deg := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil(), "Degraded condition not set yet")
			g.Expect(deg.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
			g.Expect(deg.Message).To(ContainSubstring("Secret rejected by apiserver:"))
			g.Expect(deg.Message).To(ContainSubstring("bad..secret..name"))
			// Regression-guard the unwrap behavior — operator-wrap text must NOT appear.
			g.Expect(deg.Message).NotTo(ContainSubstring("failed to create admin credentials secret"))
		}, timeout, interval).Should(Succeed())

		// End-to-end Ready: cluster reports Ready=False with the same delegating
		// message shape as the node-side helper. Pre-PR-1 the helper left Ready
		// unset (stale-True window); PR-1 closed that gap.
		var fresh v1alpha1.IdentityServerCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "bad-cluster", Namespace: ns}, &fresh)).To(Succeed())
		ready := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil(), "Ready condition must be set on bad-spec bail (PR-1 convergence)")
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
		Expect(ready.Message).To(ContainSubstring("see Degraded condition"))

		// Other operational conditions stay unsynthesized — the helper only
		// observes spec validity, not Available/Progressing/ClusterConfigReady.
		for _, condType := range []string{
			v1alpha1.ConditionAvailable,
			v1alpha1.ConditionProgressing,
			v1alpha1.ConditionClusterConfigReady,
		} {
			c := apimeta.FindStatusCondition(fresh.Status.Conditions, condType)
			Expect(c).To(BeNil(), "helper must not write %s on bad-spec bail; got: %+v", condType, c)
		}

		// ObservedGeneration must reflect the bad spec being processed.
		Expect(fresh.Status.ObservedGeneration).To(Equal(fresh.Generation))
	})

	It("recovers naturally when the user patches the CR with a valid Secret name", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "recover-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				AdminCredentials: &v1alpha1.CredentialsSource{
					ValueFrom: v1alpha1.CredentialsValueFrom{
						SecretKeyRef: v1alpha1.SecretKeyRefSource{
							Name: "bad..secret..name",
							Items: []v1alpha1.KeyToPath{
								{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
								{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
								{Key: "KEYSTORE_PASSWORD", Path: "KEYSTORE_PASSWORD"},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		// Wait for the bad-spec state to settle.
		Eventually(func(g Gomega) {
			var fresh v1alpha1.IdentityServerCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-cluster", Namespace: ns}, &fresh)).To(Succeed())
			deg := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonInvalidSpec))
		}, timeout, interval).Should(Succeed())

		// Patch the CR with a valid Secret name.
		Eventually(func(g Gomega) {
			var fresh v1alpha1.IdentityServerCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-cluster", Namespace: ns}, &fresh)).To(Succeed())
			fresh.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name = "valid-secret-name"
			g.Expect(k8sClient.Update(ctx, &fresh)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		// The new name is valid but missing, so the operator now waits: Degraded
		// and Ready both move from InvalidSpec to AdminCredsSecretMissing. The
		// handler sets both, so neither stays stale at InvalidSpec.
		Eventually(func(g Gomega) {
			var fresh v1alpha1.IdentityServerCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-cluster", Namespace: ns}, &fresh)).To(Succeed())
			deg := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			g.Expect(deg).NotTo(BeNil())
			g.Expect(deg.Reason).To(Equal(v1alpha1.ReasonAdminCredsSecretMissing),
				"Degraded should move off InvalidSpec to AdminCredsSecretMissing (valid but missing name); got reason=%q", deg.Reason)
			ready := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal(v1alpha1.ReasonAdminCredsSecretMissing),
				"Ready should move off InvalidSpec to AdminCredsSecretMissing; got reason=%q", ready.Reason)
		}, timeout, interval).Should(Succeed())
	})
})
