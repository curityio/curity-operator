package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

var _ = Describe("cluster.xml regeneration", func() {

	Describe("spec.version bump triggers regen", Ordered, func() {
		const (
			ns          = "e2e-regen-version"
			clusterName = "regen-ver"
			adminName   = "regen-ver-admin"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("regenerates cluster.xml when spec.version changes", func() {
			ctx := context.Background()

			By("creating cluster + admin and waiting for the cluster-config Secret to populate")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterName, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminName, "namespace": ns, "clusterName": clusterName})
			utils.SimulateClusterConfigReady(ns, clusterName, e2eTimeout, e2eInterval)

			By("snapshot — cluster CR status at SecretReady (initial)")
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			}
			utils.WaitForConditions(cluster, e2eTimeout, e2eInterval)
			utils.MatchCRDResource(cluster, "regen-ver/01-secret-ready-initial")

			By("capturing the pre-bump cluster-config-hash annotation")
			secretBefore := &corev1.Secret{}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
			hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]
			Expect(hashBefore).NotTo(BeEmpty(), "cluster-config-hash must be present after populate")

			By("bumping spec.version on the cluster CR")
			Eventually(func() error {
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				cluster.Spec.Version = "11.0.1"
				return k().Update(ctx, cluster)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("observing the regen branch fire — Secret data resets and hash changes")
			Eventually(func(g Gomega) {
				s := &corev1.Secret{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config", Namespace: ns}, s)).To(Succeed())
				g.Expect(s.Annotations["curity.io/cluster-config-hash"]).NotTo(Equal(hashBefore),
					"cluster-config-hash must change after spec.version bump")
				g.Expect(string(s.Data["cluster.xml"])).To(Equal("placeholder"),
					"Secret data must reset to placeholder during regen")
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("observing a fresh genclust Job created against the new spec")
			Eventually(func() error {
				return k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config-job", Namespace: ns}, &batchv1.Job{})
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("snapshot — cluster CR status mid-regen (Job recreated for new version)")
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			utils.MatchCRDResource(cluster, "regen-ver/02-regenerating-after-version-bump")
		})
	})

	Describe("admin-credentials Secret rotation triggers regen", Ordered, func() {
		const (
			ns          = "e2e-regen-key"
			clusterName = "regen-key"
			adminName   = "regen-key-admin"
			credsName   = "ext-creds"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("regenerates within seconds when CONFIG_ENCRYPTION_KEY changes externally", func() {
			ctx := context.Background()

			By("pre-creating an externally-provided admin-credentials Secret")
			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: credsName, Namespace: ns},
				Data: map[string][]byte{
					"ADMIN_PASSWORD":        []byte("e2e-password"),
					"CONFIG_ENCRYPTION_KEY": []byte("initial-key-e2e"),
					"KEYSTORE_PASSWORD":     []byte("e2e-keystore-pw"),
				},
			}
			Expect(k().Create(ctx, creds)).To(Succeed())

			By("creating cluster pointing at the externally-provided credentials Secret")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
				map[string]interface{}{"name": clusterName, "namespace": ns, "secretName": credsName})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminName, "namespace": ns, "clusterName": clusterName})
			utils.SimulateClusterConfigReady(ns, clusterName, e2eTimeout, e2eInterval)

			By("capturing the initial encryption-key-hash annotation")
			secretBefore := &corev1.Secret{}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
			keyHashBefore := secretBefore.Annotations["curity.io/encryption-key-hash"]
			Expect(keyHashBefore).NotTo(BeEmpty(), "encryption-key-hash must be present after populate")

			By("rotating CONFIG_ENCRYPTION_KEY in the externally-provided Secret")
			Eventually(func() error {
				current := &corev1.Secret{}
				if err := k().Get(ctx, client.ObjectKey{Name: credsName, Namespace: ns}, current); err != nil {
					return err
				}
				current.Data["CONFIG_ENCRYPTION_KEY"] = []byte("rotated-key-e2e")
				return k().Update(ctx, current)
			}, 30*time.Second, time.Second).Should(Succeed())

			expectedKeyHash := sha256.Sum256([]byte("rotated-key-e2e"))
			expectedKeyHashHex := hex.EncodeToString(expectedKeyHash[:])

			By("verifying the cluster-config Secret is reset and the new key hash is recorded")
			Eventually(func(g Gomega) {
				s := &corev1.Secret{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config", Namespace: ns}, s)).To(Succeed())
				g.Expect(s.Annotations["curity.io/encryption-key-hash"]).To(Equal(expectedKeyHashHex),
					"encryption-key-hash must reflect the rotated key")
				g.Expect(string(s.Data["cluster.xml"])).To(Equal("placeholder"),
					"Secret data must reset to placeholder when key rotates")
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("snapshot — cluster CR status after key rotation triggers regen")
			cluster := &v1alpha1.IdentityServerCluster{}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(Succeed())
			utils.MatchCRDResource(cluster, "regen-key/01-after-key-rotation")
		})
	})

	Describe("spec.packages change triggers regen and defers Deployment until ready", Ordered, func() {
		const (
			ns          = "e2e-regen-pkg"
			clusterName = "regen-pkg"
			adminName   = "regen-pkg-admin"
			runtimeName = "regen-pkg-runtime"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("regenerates cluster.xml when spec.packages changes and defers the node Deployment update until ClusterConfigReady=True", func() {
			ctx := context.Background()

			By("creating cluster + admin + runtime and waiting for the cluster-config Secret to populate")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterName, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminName, "namespace": ns, "clusterName": clusterName})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
				map[string]interface{}{"name": runtimeName, "namespace": ns, "clusterName": clusterName})
			utils.SimulateClusterConfigReady(ns, clusterName, e2eTimeout, e2eInterval)

			By("capturing the initial cluster-config-hash and runtime Deployment generation")
			secretBefore := &corev1.Secret{}
			Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config", Namespace: ns}, secretBefore)).To(Succeed())
			hashBefore := secretBefore.Annotations["curity.io/cluster-config-hash"]
			Expect(hashBefore).NotTo(BeEmpty(), "cluster-config-hash must be present after populate")

			deployKey := client.ObjectKey{Name: ownedName(clusterName, runtimeName), Namespace: ns}
			deployBefore := &appsv1.Deployment{}
			Eventually(func() error {
				return k().Get(ctx, deployKey, deployBefore)
			}, e2eTimeout, e2eInterval).Should(Succeed())
			generationBefore := deployBefore.Generation

			By("adding a package to the cluster CR")
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				cluster.Spec.Packages = []v1alpha1.PackageSpec{
					{
						Source:    v1alpha1.PackageSource{URL: "https://example.test/plugin.zip"},
						MountPath: "/opt/idsvr/plugins/p1",
					},
				}
				return k().Update(ctx, cluster)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("observing Branch C fire — Secret data resets and hash changes")
			Eventually(func(g Gomega) {
				s := &corev1.Secret{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config", Namespace: ns}, s)).To(Succeed())
				g.Expect(s.Annotations["curity.io/cluster-config-hash"]).NotTo(Equal(hashBefore),
					"cluster-config-hash must change after spec.packages edit")
				g.Expect(string(s.Data["cluster.xml"])).To(Equal("placeholder"),
					"Secret data must reset to placeholder during regen")
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("observing a fresh genclust Job created against the new spec")
			Eventually(func() error {
				return k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config-job", Namespace: ns}, &batchv1.Job{})
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("verifying the runtime Deployment update is deferred while ClusterConfigReady is False")
			Consistently(func(g Gomega) {
				d := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, deployKey, d)).To(Succeed())
				g.Expect(d.Generation).To(Equal(generationBefore),
					"Deployment.Generation must stay flat while the gate defers updates")
				for _, ic := range d.Spec.Template.Spec.InitContainers {
					g.Expect(ic.Name).NotTo(Equal("package-fetch-0"),
						"package init container must not appear until ClusterConfigReady=True")
				}
			}, 5*time.Second, e2eInterval).Should(Succeed())

			By("restoring ClusterConfigReady=True (simulates genclust Job completion in Kind)")
			utils.SimulateClusterConfigReady(ns, clusterName, e2eTimeout, e2eInterval)

			By("the deferred Deployment update lands in a single Generation bump, with the package mounted")
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, deployKey, d)).To(Succeed())
				g.Expect(d.Generation).To(BeNumerically(">", generationBefore),
					"Deployment.Generation must bump once the gate opens")
				found := false
				for _, ic := range d.Spec.Template.Spec.InitContainers {
					if ic.Name == "package-fetch-0" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), "package-fetch-0 init container must be present after gate opens")
			}, e2eTimeout, e2eInterval).Should(Succeed())
		})
	})

	Describe("cluster CR delete cascades to genclust Job", Ordered, func() {
		const (
			ns          = "e2e-regen-cascade"
			clusterName = "regen-cas"
			adminName   = "regen-cas-admin"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("garbage-collects the Job when the cluster CR is deleted", func() {
			ctx := context.Background()

			By("creating cluster + admin")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterName, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
				map[string]interface{}{"name": adminName, "namespace": ns, "clusterName": clusterName})

			By("waiting for the genclust Job to exist with an owner reference to the cluster CR")
			job := &batchv1.Job{}
			Eventually(func(g Gomega) {
				g.Expect(k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config-job", Namespace: ns}, job)).To(Succeed())
				g.Expect(job.OwnerReferences).NotTo(BeEmpty(), "Job must have an owner reference")
				g.Expect(job.OwnerReferences[0].Kind).To(Equal("IdentityServerCluster"))
				g.Expect(job.OwnerReferences[0].Name).To(Equal(clusterName))
				g.Expect(job.OwnerReferences[0].Controller).NotTo(BeNil())
				g.Expect(*job.OwnerReferences[0].Controller).To(BeTrue())
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("deleting the cluster CR (any associated admin Node first to avoid finalizer block)")
			node := &v1alpha1.IdentityServerNode{}
			Expect(k().Get(ctx, client.ObjectKey{Name: adminName, Namespace: ns}, node)).To(Succeed())
			Expect(k().Delete(ctx, node)).To(Succeed())
			Eventually(func() bool {
				err := k().Get(ctx, client.ObjectKey{Name: adminName, Namespace: ns}, &v1alpha1.IdentityServerNode{})
				return apierrors.IsNotFound(err)
			}, e2eTimeout, e2eInterval).Should(BeTrue())

			cluster := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns}}
			Expect(k().Delete(ctx, cluster)).To(Succeed())

			By("verifying the genclust Job is garbage-collected by Kubernetes via the owner ref")
			Eventually(func() bool {
				err := k().Get(ctx, client.ObjectKey{Name: clusterName + "-cluster-config-job", Namespace: ns}, &batchv1.Job{})
				return apierrors.IsNotFound(err)
			}, e2eTimeout, e2eInterval).Should(BeTrue())
		})
	})
})
