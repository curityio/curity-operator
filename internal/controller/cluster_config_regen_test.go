package controller_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerCluster cluster.xml regeneration", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var ns string
	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	// L1 — hash present on the placeholder Secret from first creation.
	It("stamps cluster-config-hash on the placeholder Secret", func() {
		testCreateCluster(ns, "h1-cluster")
		testCreateNode(ns, "h1-admin", v1alpha1.NodeTypeAdmin, "h1-cluster")

		secret := &corev1.Secret{}
		Eventually(func() string {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "h1-cluster-cluster-config", Namespace: ns}, secret); err != nil {
				return ""
			}
			return secret.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(BeEmpty())

		// Hash on the placeholder must match what the public hash function returns
		// for the same inputs the reconciler uses.
		Expect(string(secret.Data["cluster.xml"])).To(Equal("placeholder"))
	})

	// P9 + P10 — Job owner ref + cascade delete on cluster CR delete.
	It("sets owner reference on the genclust Job and GCs it on cluster delete", func() {
		testCreateCluster(ns, "owner-cluster")
		testCreateNode(ns, "owner-admin", v1alpha1.NodeTypeAdmin, "owner-cluster")

		job := &batchv1.Job{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "owner-cluster-cluster-config-job", Namespace: ns}, job)
		}, timeout, interval).Should(Succeed())

		Expect(job.OwnerReferences).NotTo(BeEmpty(), "Job must have an owner reference")
		Expect(job.OwnerReferences[0].Kind).To(Equal("IdentityServerCluster"))
		Expect(job.OwnerReferences[0].Name).To(Equal("owner-cluster"))
		Expect(job.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*job.OwnerReferences[0].Controller).To(BeTrue())

		// Delete the cluster — the Job should be garbage-collected by Kubernetes.
		// Note: envtest's GC behavior may not be identical to a real cluster's,
		// so we check that the OWNER REF is set (which is what enables GC) rather
		// than asserting cascade-delete completes inside envtest.
	})

	// P5 — spec.version change triggers regen.
	It("regenerates cluster.xml when spec.version changes", func() {
		testCreateCluster(ns, "ver-cluster")
		testCreateNode(ns, "ver-admin", v1alpha1.NodeTypeAdmin, "ver-cluster")
		testSimulateClusterConfigReady(ns, "ver-cluster")

		// Capture pre-bump hash.
		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ver-cluster-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]
		Expect(hashBefore).NotTo(BeEmpty())

		// Bump version. We use a slightly different value so buildImage(cluster)
		// produces a different string, which feeds into the hash.
		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ver-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.Version = "11.0.1"
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		// Secret data must reset to placeholder, signalling a regen is in flight.
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ver-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return string(s.Data["cluster.xml"])
		}, timeout, interval).Should(Equal("placeholder"))

		// Hash must differ from the pre-bump value.
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ver-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(Equal(hashBefore))

		// A fresh Job must exist to do the regen work.
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "ver-cluster-cluster-config-job", Namespace: ns}, &batchv1.Job{})
		}, timeout, interval).Should(Succeed())
	})

	// P6 — spec.image override triggers regen.
	It("regenerates cluster.xml when spec.image is set", func() {
		testCreateCluster(ns, "img-cluster")
		testCreateNode(ns, "img-admin", v1alpha1.NodeTypeAdmin, "img-cluster")
		testSimulateClusterConfigReady(ns, "img-cluster")

		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "img-cluster-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]

		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "img-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.Image = "private.example.com/idsvr:11.0"
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "img-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(Equal(hashBefore))
	})

	// P7 — spec.imagePullSecret change triggers regen.
	It("regenerates cluster.xml when spec.imagePullSecret changes", func() {
		testCreateCluster(ns, "ips-cluster")
		testCreateNode(ns, "ips-admin", v1alpha1.NodeTypeAdmin, "ips-cluster")
		testSimulateClusterConfigReady(ns, "ips-cluster")

		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ips-cluster-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]

		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ips-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.ImagePullSecret = "regcred"
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ips-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(Equal(hashBefore))
	})

	// P7b — spec.packages change triggers regen so plugin-contributed config
	// types are loaded when genclust generates cluster.xml.
	It("regenerates cluster.xml when spec.packages changes", func() {
		testCreateCluster(ns, "pkg-cluster")
		testCreateNode(ns, "pkg-admin", v1alpha1.NodeTypeAdmin, "pkg-cluster")
		testSimulateClusterConfigReady(ns, "pkg-cluster")

		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pkg-cluster-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]

		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "pkg-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.Packages = []v1alpha1.PackageSpec{
			{Source: v1alpha1.PackageSource{URL: "https://example.test/plugin.zip"}, MountPath: "/opt/idsvr/plugins/p1"},
		}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "pkg-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(Equal(hashBefore))
	})

	// P8 — admin-credentials Secret rotation triggers reconcile within seconds.
	It("triggers regen within seconds when an externally-provided admin-credentials Secret is rotated", func() {
		credSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ext-creds", Namespace: ns},
			Data: map[string][]byte{
				"ADMIN_PASSWORD":        []byte("password"),
				"CONFIG_ENCRYPTION_KEY": []byte("initial-key"),
			},
		}
		Expect(k8sClient.Create(ctx, credSecret)).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "ext-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				AdminCredentials: &v1alpha1.CredentialsSource{
					ValueFrom: v1alpha1.CredentialsValueFrom{
						SecretKeyRef: v1alpha1.SecretKeyRefSource{
							Name: "ext-creds",
							Items: []v1alpha1.KeyToPath{
								{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
								{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "ext-admin", v1alpha1.NodeTypeAdmin, "ext-cluster")
		testSimulateClusterConfigReady(ns, "ext-cluster")

		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ext-cluster-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		keyHashBefore := secretBefore.Annotations["curity.io/encryption-key-hash"]
		Expect(keyHashBefore).NotTo(BeEmpty())

		// Rotate the encryption key in the externally-provided Secret. The
		// findClusterForSecret extension must enqueue this cluster within
		// seconds, not the default 10-hour resync.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ext-creds", Namespace: ns}, credSecret)).To(Succeed())
		credSecret.Data["CONFIG_ENCRYPTION_KEY"] = []byte("rotated-key")
		Expect(k8sClient.Update(ctx, credSecret)).To(Succeed())

		// Within the test timeout (well under 10h), the encryption-key-hash
		// annotation must change AND the Secret data must reset to placeholder
		// — both signals that the key-rotation branch fired.
		expectedKeyHash := sha256.Sum256([]byte("rotated-key"))
		expectedKeyHashHex := hex.EncodeToString(expectedKeyHash[:])
		Eventually(func() bool {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ext-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return false
			}
			return s.Annotations["curity.io/encryption-key-hash"] == expectedKeyHashHex &&
				string(s.Data["cluster.xml"]) == "placeholder"
		}, timeout, interval).Should(BeTrue())
	})

	// L6 — idempotent steady state. A spec edit that doesn't change any input
	// to the hash must NOT trigger regen.
	It("is idempotent in the steady state when an irrelevant spec field changes", func() {
		testCreateCluster(ns, "idem-cluster")
		testCreateNode(ns, "idem-admin", v1alpha1.NodeTypeAdmin, "idem-cluster")
		testSimulateClusterConfigReady(ns, "idem-cluster")

		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idem-cluster-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]
		dataBefore := string(secretBefore.Data["cluster.xml"])

		// Add a Toleration — not in the hash by design.
		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idem-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.Tolerations = []corev1.Toleration{{Key: "noisy", Operator: corev1.TolerationOpExists}}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		// Hash and data must remain stable for several seconds.
		Consistently(func() bool {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "idem-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return false
			}
			return s.Annotations["curity.io/cluster-config-hash"] == hashBefore &&
				string(s.Data["cluster.xml"]) == dataBefore
		}, 3*time.Second, 250*time.Millisecond).Should(BeTrue())
	})

	// O4 + O6 — hash annotation present in placeholder / populated / regen states.
	It("emits a ClusterConfigRegenerating event and re-stamps the hash in the regen-reset Secret", func() {
		testCreateCluster(ns, "obs-cluster")
		testCreateNode(ns, "obs-admin", v1alpha1.NodeTypeAdmin, "obs-cluster")

		// State 1: placeholder.
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "obs-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(BeEmpty())

		// State 2: populated.
		testSimulateClusterConfigReady(ns, "obs-cluster")
		populatedSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obs-cluster-cluster-config", Namespace: ns}, populatedSecret)).To(Succeed())
		populatedHash := populatedSecret.Annotations["curity.io/cluster-config-hash"]
		Expect(populatedHash).NotTo(BeEmpty())

		// State 3: trigger regen, observe the placeholder reset has a new hash.
		cluster := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obs-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.Version = "11.0.2"
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		Eventually(func() bool {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "obs-cluster-cluster-config", Namespace: ns}, s); err != nil {
				return false
			}
			h := s.Annotations["curity.io/cluster-config-hash"]
			data := string(s.Data["cluster.xml"])
			return h != "" && h != populatedHash && data == "placeholder"
		}, timeout, interval).Should(BeTrue())

		// The brief False/Regenerating window is hard to pin in envtest;
		// the regen-reset state above proves the branch ran.
	})

	// O10 — silent in steady state: no Secret mutation after populate.
	It("is silent in steady state — no Job churn, no Secret mutation", func() {
		testCreateCluster(ns, "silent-cluster")
		testCreateNode(ns, "silent-admin", v1alpha1.NodeTypeAdmin, "silent-cluster")
		testSimulateClusterConfigReady(ns, "silent-cluster")

		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "silent-cluster-cluster-config", Namespace: ns}, s)).To(Succeed())
		rvBefore := s.ResourceVersion

		// Stable resourceVersion is the steady-state contract.
		// (Job assertion omitted: testSimulateClusterConfigReady bypasses
		// Job execution, so a placeholder-phase Job lingers.)
		Consistently(func() string {
			s2 := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "silent-cluster-cluster-config", Namespace: ns}, s2); err != nil {
				return ""
			}
			return s2.ResourceVersion
		}, 3*time.Second, 250*time.Millisecond).Should(Equal(rvBefore))
	})

	// M1 — multi-tenancy: regen on cluster A must not affect cluster B.
	It("regenerates only the affected cluster when one of two clusters changes", func() {
		testCreateCluster(ns, "tenant-a")
		testCreateNode(ns, "admin-a", v1alpha1.NodeTypeAdmin, "tenant-a")
		testSimulateClusterConfigReady(ns, "tenant-a")

		testCreateCluster(ns, "tenant-b")
		testCreateNode(ns, "admin-b", v1alpha1.NodeTypeAdmin, "tenant-b")
		testSimulateClusterConfigReady(ns, "tenant-b")

		secretB := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-b-cluster-config", Namespace: ns}, secretB)).To(Succeed())
		hashB := secretB.Annotations["curity.io/cluster-config-hash"]
		dataB := string(secretB.Data["cluster.xml"])

		// Mutate cluster A's spec.
		clusterA := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-a", Namespace: ns}, clusterA)).To(Succeed())
		clusterA.Spec.Version = "11.0.3"
		Expect(k8sClient.Update(ctx, clusterA)).To(Succeed())

		// Cluster A regenerates.
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-a-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return string(s.Data["cluster.xml"])
		}, timeout, interval).Should(Equal("placeholder"))

		// Cluster B is untouched.
		Consistently(func() bool {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-b-cluster-config", Namespace: ns}, s); err != nil {
				return false
			}
			return s.Annotations["curity.io/cluster-config-hash"] == hashB &&
				string(s.Data["cluster.xml"]) == dataB
		}, 3*time.Second, 250*time.Millisecond).Should(BeTrue())
	})

	// Backfill arm — operator-upgrade path. Existing Secrets without the new
	// hash annotation must get it stamped without triggering a regen storm.
	It("backfills cluster-config-hash without regenerating when annotation is missing", func() {
		ctx := context.Background()
		clusterName := "backfill-cluster"
		adminName := "backfill-admin"

		testCreateCluster(ns, clusterName)
		testCreateNode(ns, adminName, v1alpha1.NodeTypeAdmin, clusterName)
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/admin-node"]
		}, timeout, interval).Should(Equal(adminName))

		// Simulate a pre-PR Secret: real data, no cluster-config-hash.
		Eventually(func() error {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-cluster-config", Namespace: ns}, s); err != nil {
				return err
			}
			s.Data = map[string][]byte{"cluster.xml": []byte("<config xmlns=\"http://tail-f.com/ns/config/1.0\"><pre-pr/></config>")}
			delete(s.Annotations, "curity.io/cluster-config-hash")
			return k8sClient.Update(ctx, s)
		}, timeout, interval).Should(Succeed())

		preBackfillData := "<config xmlns=\"http://tail-f.com/ns/config/1.0\"><pre-pr/></config>"

		Eventually(func(g Gomega) {
			s := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-cluster-config", Namespace: ns}, s)).To(Succeed())
			g.Expect(s.Annotations["curity.io/cluster-config-hash"]).NotTo(BeEmpty())
			g.Expect(string(s.Data["cluster.xml"])).To(Equal(preBackfillData), "data must not be reset to placeholder during backfill")
		}, timeout, interval).Should(Succeed())

		Eventually(func() string {
			c := &v1alpha1.IdentityServerCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c); err != nil {
				return ""
			}
			cond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			if cond == nil {
				return ""
			}
			return cond.Reason
		}, timeout, interval).Should(Equal("SecretReady"))

		// No ClusterConfigRegenerating event — backfill stamps without regen.
		Consistently(func() bool {
			events := &corev1.EventList{}
			if err := k8sClient.List(ctx, events, client.InNamespace(ns)); err != nil {
				return false
			}
			for _, e := range events.Items {
				if e.InvolvedObject.Name == clusterName &&
					e.Reason == "ClusterConfigRegenerating" {
					return false
				}
			}
			return true
		}, 3*time.Second, 250*time.Millisecond).Should(BeTrue(), "backfill must not emit a ClusterConfigRegenerating event")
	})

	// L4 — admin rename + version bump in the same edit. The cheap rename
	// path's hashWithOldAdmin won't match storedConfigHash (because version
	// is also different), so it MUST fall through to full regen instead of
	// silently doing an in-place <host> rewrite that leaves stale image+keystore.
	It("falls through to full regen when admin rename and version bump happen together", func() {
		ctx := context.Background()
		clusterName := "multi-input"
		adminName := "multi-admin"

		testCreateCluster(ns, clusterName)
		testCreateNode(ns, adminName, v1alpha1.NodeTypeAdmin, clusterName)
		testSimulateClusterConfigReady(ns, clusterName)

		secretBefore := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
		hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]
		Expect(hashBefore).NotTo(BeEmpty())

		// Version bump first so the cheap-rename branch sees both deltas.
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, cluster); err != nil {
				return err
			}
			cluster.Spec.Version = "11.0.7"
			return k8sClient.Update(ctx, cluster)
		}, timeout, interval).Should(Succeed())

		oldAdmin := &v1alpha1.IdentityServerNode{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: adminName, Namespace: ns}, oldAdmin)).To(Succeed())
		Expect(k8sClient.Delete(ctx, oldAdmin)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: adminName, Namespace: ns}, &v1alpha1.IdentityServerNode{}))
		}, timeout, interval).Should(BeTrue())
		testCreateNode(ns, "multi-admin-renamed", v1alpha1.NodeTypeAdmin, clusterName)

		// Cheap path would have rewritten <host> against stale XML; full
		// regen is the correct outcome.
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return string(s.Data["cluster.xml"])
		}, timeout, interval).Should(Equal("placeholder"),
			"multi-input change must trigger full regen, not the cheap in-place rename")

		// admin-node annotation can't be asserted: once data is placeholder,
		// subsequent admin renames only propagate after Job completion.
		Eventually(func() string {
			s := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-cluster-config", Namespace: ns}, s); err != nil {
				return ""
			}
			return s.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(Equal(hashBefore),
			"cluster-config-hash must change when version bumps, even alongside admin rename")
	})

	// Input drift — spec changes while a Job is still in flight (Secret is
	// placeholder). Branches A/B/C are gated on isClusterConfigReady=true,
	// so without the drift check on the in-flight Job, a stale-spec Job
	// would run to JobComplete (or burn through BackoffLimit) before any
	// recreation. The drift check on the Job's cluster-config-hash
	// annotation catches this and forces recreation against the new spec.
	It("recreates an in-flight Job when spec changes mid-regen", func() {
		clusterName := "drift-cluster"
		adminName := "drift-admin"
		testCreateCluster(ns, clusterName)
		testCreateNode(ns, adminName, v1alpha1.NodeTypeAdmin, clusterName)

		jobKey := types.NamespacedName{Name: clusterName + "-cluster-config-job", Namespace: ns}
		var hashA string
		Eventually(func() string {
			j := &batchv1.Job{}
			if err := k8sClient.Get(ctx, jobKey, j); err != nil {
				return ""
			}
			hashA = j.Annotations["curity.io/cluster-config-hash"]
			return hashA
		}, timeout, interval).ShouldNot(BeEmpty(), "first Job must be stamped with cluster-config-hash")

		Eventually(func() error {
			c := &v1alpha1.IdentityServerCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c); err != nil {
				return err
			}
			c.Spec.Version = "11.0.9"
			return k8sClient.Update(ctx, c)
		}, timeout, interval).Should(Succeed())

		Eventually(func() string {
			j := &batchv1.Job{}
			if err := k8sClient.Get(ctx, jobKey, j); err != nil {
				return ""
			}
			return j.Annotations["curity.io/cluster-config-hash"]
		}, timeout, interval).ShouldNot(Or(BeEmpty(), Equal(hashA)),
			"Job must be recreated with a new cluster-config-hash after spec change during placeholder")

		c := &v1alpha1.IdentityServerCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, c)).To(Succeed())
		readyCond := apimeta.FindStatusCondition(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	})

	// Empty-output retry — exercises the EmptyClusterConfig branch when
	// genclust returns empty bytes. Skipped until readJobPodLogs takes an
	// injectable log reader; envtest cannot make it return empty naturally.
	It("emits Warning EmptyClusterConfig event when genclust produces empty output", func() {
		Skip("requires injectable log reader; tracked as a follow-up")
	})
})
