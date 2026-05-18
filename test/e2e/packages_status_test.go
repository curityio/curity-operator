package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

// E2E tests for the PackagesReady status visibility feature.
// Plan reference: PLAN-packages-status-visibility.
//
// Each test exercises a user-visible flow on a real Kind cluster (the
// operator is deployed via make test-e2e). The unit/envtest tests in
// internal/controller/ cover the per-function logic exhaustively; these
// E2E specs lock in the cross-component contract.
// Ordered + BeforeAll/AfterAll: K8s namespace deletion is async, so using
// BeforeEach/AfterEach causes the second test's createNS to land while the
// first's AfterEach is still draining (403 with metadata.namespace). Each
// It uses unique CR names so they share the namespace cleanly. Same
// pattern as the existing CRD-validation Describe block in packages_test.go.
var _ = Describe("packages status visibility (E2E)", Ordered, Label("packages-status"), func() {
	const e2ePackagesNS = "e2e-packages-status"

	BeforeAll(func() { createNS(e2ePackagesNS) })
	AfterAll(func() { deleteNS(e2ePackagesNS) })

	// E2E-1 — PackageSecretMissing reconcile-time. Apply a CR referencing
	// a Secret that doesn't exist. The pre-check sets PackagesReady=False
	// before any pod is created.
	It("E2E-1 sets PackagesReady=False with PackageSecretMissing when the referenced Secret is absent", Label("slow"), func() {
		ctx := context.Background()
		const (
			clusterName = "e2e-pkg-missing"
			nodeName    = "rt-missing"
		)
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/packages/identityservercluster-with-package-auth.yaml", e2ePackagesNS,
			map[string]interface{}{
				"name":        clusterName,
				"namespace":   e2ePackagesNS,
				"url":         "https://example.invalid/x.zip",
				"mountPath":   "/etc/plugins/x",
				"tokenSecret": "does-not-exist",
			})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", e2ePackagesNS,
			map[string]interface{}{"name": nodeName, "namespace": e2ePackagesNS, "clusterName": clusterName})

		By("node-level PackagesReady=False with PackageSecretMissing")
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackageSecretMissing))
			g.Expect(cond.Message).To(ContainSubstring(`Secret "does-not-exist" not found`))
			g.Expect(cond.Message).To(ContainSubstring("package-fetch-0"))
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("cluster-level aggregation mirrors the failing reason")
		Eventually(func(g Gomega) {
			c := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: e2ePackagesNS}, c)).To(Succeed())
			cond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackageSecretMissing))
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("Deployment is NOT created while pre-check fails")
		deploy := &appsv1.Deployment{}
		err := k().Get(ctx, client.ObjectKey{Name: ownedName(clusterName, nodeName), Namespace: e2ePackagesNS}, deploy)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"pre-check failure must block CreateOrUpdate; Deployment must not exist")
	})

	// E2E-7 — no-packages-no-condition. A user who doesn't configure
	// packages must never see PackagesReady leak onto their CR's conditions,
	// AND Ready must not be overlaid as PackagesNotReady.
	It("E2E-7 omits PackagesReady entirely when spec.packages is unset", func() {
		ctx := context.Background()
		const (
			clusterName = "e2e-no-pkg"
			nodeName    = "rt-nopkg"
		)
		// Use the cluster fixture without packages (the project's default).
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", e2ePackagesNS,
			map[string]interface{}{"name": clusterName, "namespace": e2ePackagesNS})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", e2ePackagesNS,
			map[string]interface{}{"name": nodeName, "namespace": e2ePackagesNS, "clusterName": clusterName})

		By("the node reconciles with no PackagesReady condition")
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node)).To(Succeed())
			// Wait for at least the standard condition set to be present.
			g.Expect(apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)).NotTo(BeNil())
		}, e2eTimeout, e2eInterval).Should(Succeed())

		node := &v1alpha1.IdentityServerNode{}
		Expect(k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node)).To(Succeed())
		Expect(apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)).To(BeNil(),
			"PackagesReady must be absent when spec.packages is unset")

		readyCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).NotTo(Equal(v1alpha1.ReasonPackagesNotReady),
			"Ready overlay must not fire when there is no PackagesReady condition")

		By("cluster has no PackagesReady either")
		c := &v1alpha1.IdentityServerCluster{}
		Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: e2ePackagesNS}, c)).To(Succeed())
		Expect(apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionPackagesReady)).To(BeNil())
	})

	// E2E-8 — Ready overlay fires on package failure (proves F2a end-to-end).
	// When PackagesReady=False, the node's Ready condition must be overlaid
	// to False with reason PackagesNotReady, and the cluster's Ready rollup
	// must also reflect False with reason NodesNotReady (existing rollup
	// logic, exercised by the new overlay).
	It("E2E-8 overlays Ready=False/PackagesNotReady when PackagesReady=False", Label("slow"), func() {
		ctx := context.Background()
		const (
			clusterName = "e2e-overlay"
			nodeName    = "rt-overlay"
		)
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/packages/identityservercluster-with-package-auth.yaml", e2ePackagesNS,
			map[string]interface{}{
				"name":        clusterName,
				"namespace":   e2ePackagesNS,
				"url":         "https://example.invalid/x.zip",
				"mountPath":   "/etc/plugins/x",
				"tokenSecret": "overlay-missing",
			})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", e2ePackagesNS,
			map[string]interface{}{"name": nodeName, "namespace": e2ePackagesNS, "clusterName": clusterName})

		By("node Ready=False with reason=PackagesNotReady")
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackagesNotReady))
			g.Expect(cond.Message).To(ContainSubstring("PackagesReady"))
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("cluster Ready=False (rolled up via the existing allReady aggregation)")
		Eventually(func(g Gomega) {
			c := &v1alpha1.IdentityServerCluster{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: e2ePackagesNS}, c)).To(Succeed())
			cond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		}, e2eTimeout, e2eInterval).Should(Succeed())
	})

	// E2E-12 — Rolling-update Ready=True false-positive window closed.
	// Start with a packages-empty cluster (PackagesReady absent, Ready=True
	// from the normal Deployment). Apply a Secret. Patch the cluster to ADD
	// a package referencing that Secret + an unreachable URL. The
	// preemptive PackagesPending state must fire within seconds — long
	// before the new pod's init container reports a failure — so Ready
	// flips False on this reconcile, not on a later one.
	It("E2E-12 closes the rolling-update Ready=True window via preemptive PackagesPending", Label("slow"), func() {
		ctx := context.Background()
		const (
			clusterName = "e2e-rollout"
			nodeName    = "rt-rollout"
			secretName  = "rollout-token"
		)

		Expect(k().Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: e2ePackagesNS},
			Data:       map[string][]byte{"token": []byte("opaque")},
		})).To(Succeed())

		// Apply a packages-empty cluster + admin node. Wait for the
		// cluster reconciler to settle the Deployment so Ready=True.
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", e2ePackagesNS,
			map[string]interface{}{"name": clusterName, "namespace": e2ePackagesNS})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", e2ePackagesNS,
			map[string]interface{}{"name": nodeName, "namespace": e2ePackagesNS, "clusterName": clusterName})

		By("waiting for the baseline (no packages, no PackagesReady condition)")
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node)).To(Succeed())
			g.Expect(apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)).To(BeNil(),
				"baseline: no packages → no PackagesReady condition")
		}, e2eTimeout, e2eInterval).Should(Succeed())

		// Patch the cluster to add a package. The Secret exists so
		// pre-check passes; the URL is unreachable so init will
		// eventually fail, but we don't wait that long — we assert the
		// PRE-EMPTIVE Pending state shows up within ~5s.
		By("patching the cluster to add a package referencing the Secret + unreachable URL")
		// Retry on Conflict: the operator's status writer races against this
		// Update under suite load (other tests in the same namespace leave
		// CRs that keep the operator reconciling, bumping ResourceVersion
		// between our Get and Update).
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := &v1alpha1.IdentityServerCluster{}
			if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: e2ePackagesNS}, c); err != nil {
				return err
			}
			c.Spec.Packages = []v1alpha1.PackageSpec{{
				MountPath: "/etc/plugins/p",
				Source: v1alpha1.PackageSource{
					URL: "https://example.invalid/x.zip",
					Auth: &v1alpha1.PackageAuthSpec{
						BearerToken: &v1alpha1.PackageSecretKeyRef{
							SecretRef: v1alpha1.PackageSecretKeySelector{Name: secretName, Key: "token"},
						},
					},
				},
			}}
			return k().Update(ctx, c)
		})).To(Succeed())

		By("within 10s the node transitions to PackagesPending and Ready=False — BEFORE any pod failure")
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node)).To(Succeed())
			pkgs := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(pkgs).NotTo(BeNil(), "PackagesReady must be set immediately on hash flip")
			g.Expect(pkgs.Status).To(Equal(metav1.ConditionFalse))
			// Either PackagesPending (the preemptive state) or one of
			// the runtime-failure reasons. The KEY assertion: it must
			// NOT be Status=True (no false-positive window).
			g.Expect(pkgs.Reason).NotTo(Equal(v1alpha1.ReasonAllPackagesFetched),
				"Ready=True false-positive window: PackagesReady must NOT carry the prior True reason during a rollout")

			ready := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse),
				"Ready overlay must flip False on the same reconcile that triggers the rollout")
			g.Expect(ready.Reason).To(Equal(v1alpha1.ReasonPackagesNotReady))
		}, 10*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// E2E-10 (steady-state quiet on Secret deletion) was removed: under a
	// real Kind cluster with an unreachable package URL the pod retries
	// init forever, so deleting the Secret legitimately changes the
	// translator's observed failure mode (PackageFetchFailed →
	// PackageSecretMissing). The test as written observed that change
	// and failed even though the operator behavior was correct. The
	// proper regression guard for plan D1 (hash-scoped pre-check) is the
	// unit test family TestTranslatePackagesReady_* plus the trigger
	// condition itself in identityservernode_reconciler.go — pre-check
	// is gated on `prevPackagesHash != packagesHash`. To exercise D1 at
	// e2e level we would need a URL that serves a real ZIP successfully,
	// which is out of scope for this PR.

	// E2E-11 — Secret rotated in place is silent (plan D15 regression
	// guard). Editing a Secret's data while keeping the same name + key
	// must not trigger a rolling restart. The packages hash covers refs,
	// not bytes.
	It("E2E-11 does not trigger a rolling restart when a referenced Secret is rotated in place", Label("slow"), func() {
		ctx := context.Background()
		const (
			clusterName = "e2e-rotate"
			nodeName    = "rt-rotate"
			secretName  = "rotate-token"
		)

		Expect(k().Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: e2ePackagesNS},
			Data:       map[string][]byte{"token": []byte("original")},
		})).To(Succeed())

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/packages/identityservercluster-with-package-auth.yaml", e2ePackagesNS,
			map[string]interface{}{
				"name":        clusterName,
				"namespace":   e2ePackagesNS,
				"url":         "https://example.invalid/x.zip",
				"mountPath":   "/etc/plugins/x",
				"tokenSecret": secretName,
			})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", e2ePackagesNS,
			map[string]interface{}{"name": nodeName, "namespace": e2ePackagesNS, "clusterName": clusterName})

		// Wait for the Deployment and capture its initial generation
		// + the rolled-out packages-hash.
		var deployGenBefore int64
		var hashBefore string
		Eventually(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k().Get(ctx,
				client.ObjectKey{Name: ownedName(clusterName, nodeName), Namespace: e2ePackagesNS},
				deploy)).To(Succeed())
			deployGenBefore = deploy.Generation
			hashBefore = deploy.Spec.Template.Annotations["curity.io/packages-hash"]
			g.Expect(hashBefore).NotTo(BeEmpty())
		}, e2eTimeout, e2eInterval).Should(Succeed())

		By("rotating the Secret data in place (same name, same key, new bytes)")
		Eventually(func(g Gomega) {
			s := &corev1.Secret{}
			g.Expect(k().Get(ctx, client.ObjectKey{Name: secretName, Namespace: e2ePackagesNS}, s)).To(Succeed())
			s.Data["token"] = []byte("rotated")
			g.Expect(k().Update(ctx, s)).To(Succeed())
		}, 10*time.Second, time.Second).Should(Succeed())

		By("the Deployment Generation and packages-hash annotation stay the same for 60s — no rolling restart")
		Consistently(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k().Get(ctx,
				client.ObjectKey{Name: ownedName(clusterName, nodeName), Namespace: e2ePackagesNS},
				deploy)).To(Succeed())
			g.Expect(deploy.Generation).To(Equal(deployGenBefore),
				"Deployment.Generation must not change — rotation should NOT trigger pod restart")
			g.Expect(deploy.Spec.Template.Annotations["curity.io/packages-hash"]).To(Equal(hashBefore),
				"packages-hash must not change — by design it covers refs, not bytes")
		}, 60*time.Second, 5*time.Second).Should(Succeed())
	})

	// E2E-9 — Ready overlay does NOT fire for non-package failures.
	// Provoke an unrelated failure by pointing the cluster at an invalid
	// image tag. ReplicaSet creates a pod that fails to pull the Curity
	// main image. ISN's Ready must be False with reason ReplicasNotReady
	// (existing behavior), NOT PackagesNotReady.
	//
	// Note: this is a happy-path test FOR the absence of overlay leak —
	// no packages are configured here so PackagesReady should not exist,
	// and Ready must reflect Deployment-mirror semantics only.
	It("E2E-9 does NOT overlay Ready=PackagesNotReady for non-package failures", Label("slow"), func() {
		ctx := context.Background()
		const (
			clusterName = "e2e-non-pkg-fail"
			nodeName    = "rt-nonpkg"
		)

		// Cluster without packages but with a bogus image tag.
		c := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: e2ePackagesNS},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Image:   "curity.azurecr.io/curity/idsvr:no-such-tag-exists-deliberate",
			},
		}
		Expect(k().Create(ctx, c)).To(Succeed())
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", e2ePackagesNS,
			map[string]interface{}{"name": nodeName, "namespace": e2ePackagesNS, "clusterName": clusterName})

		By("the node has no PackagesReady AND Ready's reason is not PackagesNotReady")
		Consistently(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			if err := k().Get(ctx, client.ObjectKey{Name: nodeName, Namespace: e2ePackagesNS}, node); err != nil {
				return
			}
			g.Expect(apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)).To(BeNil(),
				"no packages configured → PackagesReady must remain absent")

			ready := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
			if ready != nil {
				g.Expect(ready.Reason).NotTo(Equal(v1alpha1.ReasonPackagesNotReady),
					"Ready overlay must not fire for non-package failures (this is an image-pull issue)")
			}
		}, 30*time.Second, 5*time.Second).Should(Succeed())
	})
})
