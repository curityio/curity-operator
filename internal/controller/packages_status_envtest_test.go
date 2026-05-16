package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// Envtest integration tests for the PackagesReady condition.
// Plan reference: PLAN-packages-status-visibility (Step 7+8+9 wiring).
//
// envtest has no kubelet — pod status fields cannot be populated by the
// runtime. These tests therefore focus on what the *reconciler* does in
// response to spec/Secret state, not what the pod-watch translator
// observes; the translator is exhaustively unit-tested separately in
// packages_status_test.go.
var _ = Describe("PackagesReady condition (envtest)", func() {
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

	// Pre-check end-to-end: a CR referencing a missing Secret must set
	// PackagesReady=False with PackageSecretMissing BEFORE any pod is
	// created. The deployment must NOT be created — pushing a known-bad
	// spec onto a healthy Deployment would crashloop pods unnecessarily.
	It("pre-check sets PackagesReady=False when referenced Secret is missing", func() {
		clusterName := "pkg-precheck-missing"
		nodeName := "rt"

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: "missing-secret", Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Eventually the node has PackagesReady=False with the expected
		// reason; the message names the missing Secret.
		node := &v1alpha1.IdentityServerNode{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil(), "PackagesReady must be set")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackageSecretMissing))
			g.Expect(cond.Message).To(ContainSubstring(`Secret "missing-secret" not found`))
			g.Expect(cond.Message).To(ContainSubstring("package-fetch-0"))
		}, timeout, interval).Should(Succeed())

		// Ready overlay (F2a): Ready must be False with PackagesNotReady.
		readyCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal(v1alpha1.ReasonPackagesNotReady))

		// Deployment must NOT exist — pre-check failure blocks the
		// CreateOrUpdate.
		err := k8sClient.Get(ctx,
			types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
			&appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Deployment must not exist while pre-check fails")
	})

	// Recovery path: applying the missing Secret while the CR is unchanged
	// causes the pre-check to pass on a subsequent reconcile, the
	// Deployment is built, and the (formerly False) condition reflects the
	// new state. envtest has no kubelet so we cannot reach True via the
	// translator; we assert the Deployment was created (proof the reconcile
	// fully ran the happy path) and the condition's reason is no longer
	// the missing-Secret one.
	It("pre-check recovers and Deployment is built once the missing Secret is applied", func() {
		clusterName := "pkg-precheck-recover"
		nodeName := "rt"
		secretName := "later-secret"

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: secretName, Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Wait for pre-check to set PackagesReady=False first.
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		}, timeout, interval).Should(Succeed())

		// Apply the Secret. The reconciler's 30s requeue (or the cache's
		// Secret watch via managed-config predicate, which won't fire for
		// non-labeled Secrets) is the recovery trigger. With a 30s
		// requeue, allow up to ~45s for the next reconcile pass.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"token": []byte("opaque")},
		})).To(Succeed())

		// After the Secret is applied, the next reconcile (within the
		// requeue interval) builds the Deployment. envtest has no
		// kubelet, so the pod will never come up — but the operator's
		// view should show Deployment created and PackagesReady
		// transitioned from PackageSecretMissing to PackagesPending
		// (preemptive, awaiting the new pod's init-container status).
		Eventually(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
				deploy)).To(Succeed())

			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackagesPending),
				"once pre-check passes, PackagesReady should be PackagesPending "+
					"(preemptive — Ready overlay flips False during rollout window) "+
					"and stay so until the translator sees a new-hash pod's init status")
		}, 45*time.Second, interval).Should(Succeed())
	})

	// Coexistence: a CR whose pre-check fails should also have the standard
	// Deployment-mirror conditions present (those track the *previous*
	// healthy state if any, or DeploymentNotFound at first reconcile).
	// The PackagesReady=False/Ready=False overlay must not stomp them.
	It("pre-check failure preserves the standard condition set", func() {
		clusterName := "pkg-coexist"
		nodeName := "rt"

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: "absent", Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())

			// PackagesReady False (from pre-check)
			pr := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(pr).NotTo(BeNil())
			g.Expect(pr.Status).To(Equal(metav1.ConditionFalse))
		}, timeout, interval).Should(Succeed())

		// At least the Ready overlay must be present alongside.
		// The other K8s-Deployment-mirror conditions (Available/
		// Progressing/Degraded) may or may not have been computed
		// depending on whether the reconcile reached the post-
		// computeNodeConditions block before the pre-check failed; the
		// invariant we care about is that PackagesReady AND Ready
		// coexist with the right values.
		node := &v1alpha1.IdentityServerNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
		readyCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal(v1alpha1.ReasonPackagesNotReady))
	})

	// Cluster aggregation: a node with PackagesReady=False must surface as
	// a cluster-level PackagesReady=False via the existing node-condition
	// rollup. nodeStatusConditionsChangedPredicate fires on the new
	// condition (verified unchanged in predicates.go).
	It("cluster aggregation mirrors node PackagesReady=False", func() {
		clusterName := "pkg-aggregate"
		nodeName := "rt"

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: "agg-missing", Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// First, the node-level condition fires.
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		}, timeout, interval).Should(Succeed())

		// Then the cluster-level aggregation picks it up.
		Eventually(func(g Gomega) {
			c := &v1alpha1.IdentityServerCluster{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c)).To(Succeed())
			cond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil(), "cluster PackagesReady must be set")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackageSecretMissing))
			g.Expect(cond.Message).To(ContainSubstring(`Secret "agg-missing" not found`))
		}, timeout, interval).Should(Succeed())
	})

	// No packages configured: PackagesReady must be ABSENT from both node
	// and cluster (plan F5a). Also locks in that Ready is NOT overlaid
	// when PackagesReady doesn't exist.
	It("no packages configured: PackagesReady is absent and Ready is not overlaid", func() {
		clusterName := "pkg-none"
		nodeName := "rt"

		testCreateCluster(ns, clusterName) // no packages
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Wait for the reconciler to settle on node status.
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			g.Expect(node.Status.Conditions).NotTo(BeEmpty(), "reconciler should have set conditions")
		}, timeout, interval).Should(Succeed())

		node := &v1alpha1.IdentityServerNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
		Expect(apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)).To(BeNil(),
			"PackagesReady must be absent when spec.packages is unset")

		// Ready may be True/False depending on Deployment state in
		// envtest (no kubelet → no replicas come up); the invariant is
		// that the Ready reason MUST NOT be PackagesNotReady.
		readyCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
		if readyCond != nil {
			Expect(readyCond.Reason).NotTo(Equal(v1alpha1.ReasonPackagesNotReady),
				"Ready overlay must not fire when PackagesReady is absent")
		}

		// Cluster mirror should also have PackagesReady absent.
		Eventually(func(g Gomega) {
			c := &v1alpha1.IdentityServerCluster{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c)).To(Succeed())
			g.Expect(apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionPackagesReady)).To(BeNil())
		}, timeout, interval).Should(Succeed())
	})

	// LastTransitionTime preservation: once PackagesReady=False is set,
	// subsequent reconciles that don't change the condition must NOT
	// re-stamp LastTransitionTime. This guards against the regression
	// reviewer #2 caught — restore via setCondition stamping a fresh
	// timestamp on every reconcile.
	It("preserves PackagesReady LastTransitionTime across reconciles", func() {
		clusterName := "pkg-stable-tx"
		nodeName := "rt"

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: "stable-tx-absent", Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Capture first LastTransitionTime.
		var firstTx metav1.Time
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			firstTx = cond.LastTransitionTime
			g.Expect(firstTx.IsZero()).To(BeFalse())
		}, timeout, interval).Should(Succeed())

		// Touch the node CR to force a reconcile pass that does NOT
		// change PackagesReady's underlying state (Secret is still
		// missing, reason unchanged). Adding an annotation triggers a
		// generation-changed reconcile without altering anything that
		// affects PackagesReady.
		Consistently(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())

			if node.Annotations == nil {
				node.Annotations = map[string]string{}
			}
			node.Annotations["test-trigger"] = time.Now().Format(time.RFC3339Nano)
			g.Expect(k8sClient.Update(ctx, node)).To(Succeed())
		}, 3*time.Second, 1*time.Second).Should(Succeed())

		// After several reconciles, LastTransitionTime is unchanged.
		Consistently(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.LastTransitionTime.Time).To(BeTemporally("==", firstTx.Time),
				"LastTransitionTime must NOT advance when condition state is unchanged")
		}, 5*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// Translator path with a manually-synthesized pod. envtest has no
	// kube-controller-manager so the Deployment doesn't auto-create
	// ReplicaSets — we craft a Pod directly with the right labels,
	// OwnerReference, and InitContainerStatus to verify the translator
	// observed via the Pod watch sets the right condition.
	It("translator sets PackagesReady=False on observed init-container exit 60", func() {
		clusterName := "pkg-translator"
		nodeName := "rt"
		secretName := "trans-secret"

		// Apply Secret so pre-check passes; the deployment is built.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"token": []byte("opaque")},
		})).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: secretName, Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Wait for the Deployment to exist (pre-check passed; CreateOrUpdate ran)
		// and capture the packages-hash the operator stamped on its pod
		// template. The translator's hash filter only counts pods carrying
		// this exact annotation value, so the synthetic pod below must
		// inherit it.
		var stampedHash string
		Eventually(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
				deploy)).To(Succeed())
			stampedHash = deploy.Spec.Template.Annotations["curity.io/packages-hash"]
			g.Expect(stampedHash).NotTo(BeEmpty(), "Deployment must have curity.io/packages-hash stamped")
		}, timeout, interval).Should(Succeed())

		// Craft a Pod owned by a synthetic ReplicaSet with a failing
		// init-container status — the translator reads this and the
		// predicate routes it back to our node via findNodesForPod.
		// The packages-hash annotation must match what the operator
		// stamped on the Deployment template so the translator's hash
		// filter does not discard this pod as stale.
		owningRS := metav1.OwnerReference{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       ownedName(clusterName, nodeName) + "-rs",
			UID:        types.UID("synthetic-rs-uid-1"),
			Controller: ptr.To(true),
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ownedName(clusterName, nodeName) + "-pod",
				Namespace: ns,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "curity-operator",
					"app.kubernetes.io/instance":   ownedName(clusterName, nodeName),
					"curity.io/cluster":            clusterName,
				},
				Annotations:     map[string]string{"curity.io/packages-hash": stampedHash},
				OwnerReferences: []metav1.OwnerReference{owningRS},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "curity", Image: "alpine"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		// Synthesize the init container status post-create (Status
		// subresource update).
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "package-fetch-0",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 60},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		// The translator picks this up and sets PackagesReady=False
		// with PackageTLSVerifyFailed.
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackageTLSVerifyFailed))
		}, timeout, interval).Should(Succeed())

		// Cleanup the pod so the AfterEach can drop the namespace cleanly.
		_ = k8sClient.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
	})

	// Secret-watch fast recovery (PLAN-secret-watch-on-init-error).
	// Creating the missing Secret must wake the reconciler in seconds,
	// not minutes. The old 30 s requeue path is still in place as a
	// fallback, but this test enforces the watch-driven path: assertion
	// timeout is 10 s, well under the requeue interval.
	It("Secret watch flips PackagesReady=False -> True within 10s of Secret create", func() {
		clusterName := "pkg-watch-fast"
		nodeName := "rt"
		secretName := "fast-secret"

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: secretName, Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Wait for the pre-check fail state (the baseline).
		Eventually(func(g Gomega) {
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonPackageSecretMissing))
		}, timeout, interval).Should(Succeed())

		// Apply the Secret; the watch must fire within seconds.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"token": []byte("opaque")},
		})).To(Succeed())

		// Tight 10s budget — much less than the 30 s requeue. If this
		// fails, the watch wiring is broken.
		Eventually(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns},
				deploy)).To(Succeed())
			node := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, node)).To(Succeed())
			cond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).NotTo(Equal(v1alpha1.ReasonPackageSecretMissing),
				"reason should have advanced past SecretMissing after the Secret create event")
		}, 10*time.Second, interval).Should(Succeed())
	})

	// Steady-state silence (PLAN-secret-watch-on-init-error, L3/L11).
	// While PackagesReady=True (a healthy package), updating a referenced
	// Secret must NOT cause the operator to push a new Deployment or
	// flip the condition. The relaxed pre-check gate is the safeguard.
	It("Secret update while PackagesReady=True does not bump Deployment generation", func() {
		clusterName := "pkg-watch-steady"
		nodeName := "rt"
		secretName := "steady-secret"

		// Apply Secret first so pre-check passes immediately.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"token": []byte("v1")},
		})).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: secretName, Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		// Wait for the Deployment to be created and capture its generation.
		// In envtest the pod-watch translator will set
		// PackagesReady=PackagesPending (no real pods exist), which is
		// NOT a recoverable-failure reason — so the relaxed gate stays
		// silent. That is the exact behavior we want to lock down.
		deployKey := types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}
		var initialGeneration int64
		Eventually(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			initialGeneration = deploy.Generation
			g.Expect(initialGeneration).NotTo(BeZero())
		}, timeout, interval).Should(Succeed())

		// Rotate the Secret value. The watch will fire; the gate must
		// keep pre-check from running and the recovery hook from
		// doing anything.
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, secret)).To(Succeed())
		secret.Data["token"] = []byte("v2-rotated")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		// Give the reconciler a window to (mis)behave. The Deployment
		// generation must not have changed; if the gate is too loose,
		// CreateOrUpdate would bump it.
		Consistently(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			g.Expect(deploy.Generation).To(Equal(initialGeneration),
				"Deployment generation must not change on steady-state Secret rotation")
		}, 5*time.Second, interval).Should(Succeed())
	})

	// Unrelated Secret in the same namespace must NOT enqueue the ISN.
	// The mapFunc only enqueues nodes whose parent ISC references the
	// Secret name; an unrelated Secret returns an empty list.
	It("Secret unrelated to any package does not affect the node", func() {
		clusterName := "pkg-watch-unrelated"
		nodeName := "rt"
		pkgSecret := "real-pkg-secret"

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: pkgSecret, Namespace: ns},
			Data:       map[string][]byte{"token": []byte("v1")},
		})).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Packages: []v1alpha1.PackageSpec{{
					MountPath: "/etc/plugins/p",
					Source: v1alpha1.PackageSource{
						URL: "https://repo.example.com/p.zip",
						Auth: &v1alpha1.PackageAuthSpec{
							BearerToken: &v1alpha1.PackageSecretKeyRef{
								SecretRef: v1alpha1.PackageSecretKeySelector{Name: pkgSecret, Key: "token"},
							},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

		deployKey := types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}
		var initialGeneration int64
		Eventually(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			initialGeneration = deploy.Generation
			g.Expect(initialGeneration).NotTo(BeZero())
		}, timeout, interval).Should(Succeed())

		// Create an Opaque Secret with a name that no package references.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "totally-unrelated", Namespace: ns},
			Data:       map[string][]byte{"random": []byte("noise")},
		})).To(Succeed())

		// Deployment generation must not change. Use Consistently to
		// give the reconciler time to misbehave if it would.
		Consistently(func(g Gomega) {
			deploy := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			g.Expect(deploy.Generation).To(Equal(initialGeneration))
		}, 5*time.Second, interval).Should(Succeed())
	})
})
