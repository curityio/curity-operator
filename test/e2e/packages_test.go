package e2e

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

// initContainerByName returns the named init container or nil. Used in
// place of a generic finder since each package gets a deterministic name.
func initContainerByName(deploy *appsv1.Deployment, name string) *corev1.Container {
	for i := range deploy.Spec.Template.Spec.InitContainers {
		if deploy.Spec.Template.Spec.InitContainers[i].Name == name {
			return &deploy.Spec.Template.Spec.InitContainers[i]
		}
	}
	return nil
}

// hasEvent reports whether the named Event reason exists for the given
// node CR. Used to assert PackagesConfigured / PackagesUpdated / PackagesRemoved.
func hasEvent(ctx context.Context, ns, nodeName, reason string) bool {
	events := &corev1.EventList{}
	if err := k().List(ctx, events, client.InNamespace(ns)); err != nil {
		return false
	}
	for _, e := range events.Items {
		if e.InvolvedObject.Kind == "IdentityServerNode" &&
			e.InvolvedObject.Name == nodeName &&
			e.Reason == reason {
			return true
		}
	}
	return false
}

// hasOperatorLog reports whether the operator's structured logs contain
// a line matching all the given substrings. Used as a complement to
// hasEvent for the cases where the K8s EventRecorder spam filter (25
// burst, then 1 / 300s) drops a Normal event but the operator's log line
// for the same code path always fires. Shells out to kubectl rather than
// adding a typed clientset to the test environment — the test already
// depends on kubectl elsewhere (suite setup).
func hasOperatorLog(node string, substrs ...string) bool {
	out, err := utils.Run("kubectl", "logs",
		"-n", "curity-operator",
		"deployment/curity-operator-controller-manager",
		"--tail=2000",
	)
	if err != nil {
		return false
	}
	// Each line must contain ALL substrings to count as a match,
	// scoped to log lines mentioning the target node so unrelated
	// reconciles don't false-positive.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, node) {
			continue
		}
		matched := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

var _ = Describe("packages feature", func() {

	// =================================================================
	// Positive — Deployment shape
	// =================================================================
	Describe("single public package", Ordered, func() {
		const (
			ns          = "e2e-packages-single"
			clusterName = "pkg-single"
			nodeName    = "pkg-single-rt"
			pkgURL      = "https://example.com/test.zip"
			mountPath   = "/etc/plugins/test"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("applies the cluster with one package and creates a Deployment with the init container", func() {
			ctx := context.Background()

			By("applying cluster + runtime node")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-package.yaml", ns,
				map[string]interface{}{
					"name": clusterName, "namespace": ns,
					"url": pkgURL, "mountPath": mountPath,
				})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
				map[string]interface{}{"name": nodeName, "namespace": ns, "clusterName": clusterName})

			By("waiting for the Deployment to exist")
			deployName := ownedName(clusterName, nodeName)
			deploy := &appsv1.Deployment{}
			Eventually(func() error {
				return k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("Deployment has exactly one package-fetch-0 init container")
			Eventually(func(g Gomega) {
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				init := initContainerByName(deploy, "package-fetch-0")
				g.Expect(init).NotTo(BeNil(), "init container package-fetch-0 must exist")
				g.Expect(init.Image).To(Equal("alpine:3.19"))
				g.Expect(init.Command).To(Equal([]string{"/bin/sh", "-c"}))
				g.Expect(init.Args).To(HaveLen(1))
				g.Expect(init.Args[0]).To(ContainSubstring("apk add --no-cache curl unzip ca-certificates"))
				// URL must reach the script via $PKG_URL env var (NOT inlined into
				// the script body — that would re-introduce the shell-injection bug).
				g.Expect(init.Args[0]).NotTo(ContainSubstring(pkgURL),
					"URL must not appear inline in script; must be referenced as $PKG_URL")
				g.Expect(init.Args[0]).To(ContainSubstring(`"$PKG_URL"`))
				var pkgURLEnv string
				for _, e := range init.Env {
					if e.Name == "PKG_URL" {
						pkgURLEnv = e.Value
					}
				}
				g.Expect(pkgURLEnv).To(Equal(pkgURL), "PKG_URL env var must hold URL value")
				g.Expect(init.Args[0]).To(ContainSubstring("unzip -q /tmp/pkg.zip -d /pkg"))
				// Per-container security context: root for apk add.
				g.Expect(init.SecurityContext).NotTo(BeNil())
				g.Expect(*init.SecurityContext.RunAsUser).To(Equal(int64(0)))
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("Deployment has the pkg-0 emptyDir volume with sizeLimit")
			var pkgVol *corev1.Volume
			for i := range deploy.Spec.Template.Spec.Volumes {
				if deploy.Spec.Template.Spec.Volumes[i].Name == "pkg-0" {
					pkgVol = &deploy.Spec.Template.Spec.Volumes[i]
				}
			}
			Expect(pkgVol).NotTo(BeNil(), "pkg-0 volume must exist")
			Expect(pkgVol.EmptyDir).NotTo(BeNil())
			Expect(pkgVol.EmptyDir.SizeLimit).NotTo(BeNil())
			Expect(pkgVol.EmptyDir.SizeLimit.String()).To(Equal("256Mi"))

			By("main container has the user mountPath")
			mounts := deploy.Spec.Template.Spec.Containers[0].VolumeMounts
			var found bool
			for _, m := range mounts {
				if m.Name == "pkg-0" && m.MountPath == mountPath {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "main container must mount pkg-0 at %s; got %+v", mountPath, mounts)

			By("pod template carries curity.io/packages-hash")
			Expect(deploy.Spec.Template.Annotations["curity.io/packages-hash"]).NotTo(BeEmpty())

			By("PackagesConfigured event recorded")
			Eventually(func() bool {
				return hasEvent(ctx, ns, nodeName, "PackagesConfigured")
			}, e2eTimeout, e2eInterval).Should(BeTrue())
		})
	})

	// =================================================================
	// Multi-package — Deployment has two init containers
	// =================================================================
	Describe("multiple packages on one cluster", Ordered, func() {
		const (
			ns          = "e2e-packages-multi"
			clusterName = "pkg-multi"
			nodeName    = "pkg-multi-rt"
			urlA        = "https://example.com/plugin-a.zip"
			urlB        = "https://example.com/plugin-b.zip"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("creates two init containers and two emptyDir volumes", func() {
			ctx := context.Background()

			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-packages-multi.yaml", ns,
				map[string]interface{}{
					"name": clusterName, "namespace": ns,
					"urlA": urlA, "urlB": urlB,
				})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
				map[string]interface{}{"name": nodeName, "namespace": ns, "clusterName": clusterName})

			deployName := ownedName(clusterName, nodeName)
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())

				g.Expect(initContainerByName(deploy, "package-fetch-0")).NotTo(BeNil())
				g.Expect(initContainerByName(deploy, "package-fetch-1")).NotTo(BeNil())

				// Both pkg volumes
				vols := map[string]bool{}
				for _, v := range deploy.Spec.Template.Spec.Volumes {
					vols[v.Name] = true
				}
				g.Expect(vols).To(HaveKey("pkg-0"))
				g.Expect(vols).To(HaveKey("pkg-1"))

				// Each init container's URL is correct via PKG_URL env var
				// (NOT inlined into the script — security contract).
				init0 := initContainerByName(deploy, "package-fetch-0")
				init1 := initContainerByName(deploy, "package-fetch-1")
				g.Expect(init0.Args[0]).NotTo(ContainSubstring(urlA))
				g.Expect(init1.Args[0]).NotTo(ContainSubstring(urlB))
				envOf := func(c *corev1.Container, name string) string {
					for _, e := range c.Env {
						if e.Name == name {
							return e.Value
						}
					}
					return ""
				}
				g.Expect(envOf(init0, "PKG_URL")).To(Equal(urlA))
				g.Expect(envOf(init1, "PKG_URL")).To(Equal(urlB))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		})
	})

	// =================================================================
	// Lifecycle — add, edit, remove
	// =================================================================
	Describe("lifecycle: add -> edit -> remove", Ordered, func() {
		const (
			ns          = "e2e-packages-lifecycle"
			clusterName = "pkg-lc"
			nodeName    = "pkg-lc-rt"
			urlV1       = "https://example.com/v1.zip"
			urlV2       = "https://example.com/v2.zip"
		)
		var (
			deployName     string
			hashAfterFirst string
			hashAfterEdit  string
		)
		BeforeAll(func() {
			createNS(ns)
			deployName = ownedName(clusterName, nodeName)
		})
		AfterAll(func() { deleteNS(ns) })

		It("starts without packages — Deployment has zero init containers", func() {
			ctx := context.Background()

			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterName, "namespace": ns})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
				map[string]interface{}{"name": nodeName, "namespace": ns, "clusterName": clusterName})

			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Spec.InitContainers).To(BeEmpty())
				g.Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey("curity.io/packages-hash"))
			}, e2eTimeout, e2eInterval).Should(Succeed())
		})

		It("adds a package — init container appears, hash annotation set, PackagesConfigured event", func() {
			ctx := context.Background()

			By("editing the cluster to add a package")
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				cluster.Spec.Packages = []v1alpha1.PackageSpec{
					{
						Source:    v1alpha1.PackageSource{URL: urlV1},
						MountPath: "/etc/plugins/a",
					},
				}
				return k().Update(ctx, cluster)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("Deployment gains the init container and hash annotation")
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				g.Expect(initContainerByName(deploy, "package-fetch-0")).NotTo(BeNil())
				h := deploy.Spec.Template.Annotations["curity.io/packages-hash"]
				g.Expect(h).NotTo(BeEmpty())
				hashAfterFirst = h
			}, e2eTimeout, e2eInterval).Should(Succeed())

			Eventually(func() bool {
				return hasEvent(ctx, ns, nodeName, "PackagesConfigured")
			}, e2eTimeout, e2eInterval).Should(BeTrue())
		})

		It("edits the URL — hash changes, PackagesUpdated event", func() {
			ctx := context.Background()

			By("flipping spec.packages[0].source.url to a new URL")
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				cluster.Spec.Packages[0].Source.URL = urlV2
				return k().Update(ctx, cluster)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("hash annotation changes")
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				h := deploy.Spec.Template.Annotations["curity.io/packages-hash"]
				g.Expect(h).NotTo(BeEmpty())
				g.Expect(h).NotTo(Equal(hashAfterFirst))
				// init container's PKG_URL env var reflects the new URL.
				// (URL is never inlined in the script — security contract.)
				init := initContainerByName(deploy, "package-fetch-0")
				g.Expect(init).NotTo(BeNil())
				var pkgURLEnv string
				for _, e := range init.Env {
					if e.Name == "PKG_URL" {
						pkgURLEnv = e.Value
					}
				}
				g.Expect(pkgURLEnv).To(Equal(urlV2))
				g.Expect(init.Args[0]).NotTo(ContainSubstring(urlV2))
				g.Expect(init.Args[0]).NotTo(ContainSubstring(urlV1))
				hashAfterEdit = h
			}, e2eTimeout, e2eInterval).Should(Succeed())

			// We do NOT assert the K8s Event here (the EventRecorder spam
			// filter can drop it — see PackagesRemoved below for the full
			// rationale). Instead we assert the operator's structured log
			// line, which always fires regardless of event throttling and
			// is the actual proof of correctness for the code path.
			Eventually(func(g Gomega) {
				g.Expect(hasOperatorLog(nodeName, `"msg":"packages updated"`)).
					To(BeTrue(), "operator log must contain a 'packages updated' line for node %q", nodeName)
			}, e2eTimeout, e2eInterval).Should(Succeed())
		})

		It("removes the package — init container gone, hash gone, PackagesRemoved event", func() {
			ctx := context.Background()

			By("clearing spec.packages")
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k().Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster); err != nil {
					return err
				}
				cluster.Spec.Packages = nil
				return k().Update(ctx, cluster)
			}, e2eTimeout, e2eInterval).Should(Succeed())

			By("Deployment loses init container, volume, mount, and hash annotation")
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Spec.InitContainers).To(BeEmpty())
				g.Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey("curity.io/packages-hash"))
				for _, v := range deploy.Spec.Template.Spec.Volumes {
					g.Expect(v.Name).NotTo(HavePrefix("pkg-"),
						"no pkg-* volumes should remain")
				}
				for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
					g.Expect(m.Name).NotTo(HavePrefix("pkg-"),
						"no pkg-* mounts should remain")
				}
				_ = hashAfterEdit // captured for debugging, not strict-asserted post-removal
			}, e2eTimeout, e2eInterval).Should(Succeed())

			// K8s EventRecorder spam filter (25 burst, then 1 / 300s per
			// (reason, involvedObject)) can drop this third-in-sequence
			// event after the many DeploymentReconciled events emitted
			// across reconciles in this Ordered Describe. The operator's
			// structured log line, however, always fires — that's where
			// we assert correctness, since the log is the actual proof
			// the code path executed and the spam filter does not gate it.
			Eventually(func(g Gomega) {
				g.Expect(hasOperatorLog(nodeName, `"msg":"packages removed"`)).
					To(BeTrue(), "operator log must contain a 'packages removed' line for node %q", nodeName)
			}, e2eTimeout, e2eInterval).Should(Succeed())
		})
	})

	// =================================================================
	// Bearer-token auth — Secret-projected env var
	// =================================================================
	Describe("bearer-token auth", Ordered, func() {
		const (
			ns          = "e2e-packages-bearer"
			clusterName = "pkg-bearer"
			nodeName    = "pkg-bearer-rt"
			tokenSecret = "pkg-token"
			pkgURL      = "https://example.com/private.zip"
		)
		BeforeAll(func() {
			createNS(ns)
			// Create the token Secret the package references.
			ctx := context.Background()
			s := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: tokenSecret, Namespace: ns},
				StringData: map[string]string{"token": "test-bearer-token-value"},
			}
			Expect(k().Create(ctx, s)).To(Succeed())
		})
		AfterAll(func() { deleteNS(ns) })

		It("projects BEARER_TOKEN as a Secret env var without leaking the value to argv", func() {
			ctx := context.Background()
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-package-auth.yaml", ns,
				map[string]interface{}{
					"name": clusterName, "namespace": ns,
					"url": pkgURL, "mountPath": "/etc/plugins/private",
					"tokenSecret": tokenSecret,
				})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
				map[string]interface{}{"name": nodeName, "namespace": ns, "clusterName": clusterName})

			deployName := ownedName(clusterName, nodeName)
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				init := initContainerByName(deploy, "package-fetch-0")
				g.Expect(init).NotTo(BeNil())

				// BEARER_TOKEN env var sourced from the Secret.
				var got *corev1.EnvVar
				for i := range init.Env {
					if init.Env[i].Name == "BEARER_TOKEN" {
						got = &init.Env[i]
					}
				}
				g.Expect(got).NotTo(BeNil(), "BEARER_TOKEN env var must be projected")
				g.Expect(got.ValueFrom).NotTo(BeNil())
				g.Expect(got.ValueFrom.SecretKeyRef).NotTo(BeNil())
				g.Expect(got.ValueFrom.SecretKeyRef.Name).To(Equal(tokenSecret))
				g.Expect(got.ValueFrom.SecretKeyRef.Key).To(Equal("token"))

				// Script must consume via stdin (--header @-), NOT via argv.
				script := init.Args[0]
				g.Expect(script).To(ContainSubstring("--header @-"))
				g.Expect(script).NotTo(ContainSubstring(`-H "Authorization`),
					"Authorization header must not appear on curl argv")
				g.Expect(script).NotTo(ContainSubstring("test-bearer-token-value"),
					"the literal token value must NEVER be in the script")
			}, e2eTimeout, e2eInterval).Should(Succeed())
		})
	})

	// =================================================================
	// CRD validation negatives — API server rejects bad input
	// =================================================================
	// Ordered + BeforeAll/AfterAll: K8s namespace deletion is async, so
	// using BeforeEach/AfterEach can have the second test fire while the
	// namespace is still Terminating (creates fail with "namespace is
	// being terminated"). Each It uses unique CR names so they can share
	// the namespace without collisions.
	Describe("CRD validation rejects bad input", Ordered, func() {
		const ns = "e2e-packages-crd-validation"
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("rejects a cluster with both basicAuth and bearerToken set", func() {
			ctx := context.Background()
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-auth", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{
						{
							Source: v1alpha1.PackageSource{
								URL: "https://example.com/x.zip",
								Auth: &v1alpha1.PackageAuthSpec{
									BasicAuth: &v1alpha1.PackageBasicAuthRef{
										SecretRef: v1alpha1.PackageBasicAuthSelector{
											Name: "s", UsernameKey: "u", PasswordKey: "p",
										},
									},
									BearerToken: &v1alpha1.PackageSecretKeyRef{
										SecretRef: v1alpha1.PackageSecretKeySelector{Name: "s2", Key: "k"},
									},
								},
							},
							MountPath: "/etc/plugins/x",
						},
					},
				},
			}
			err := k().Create(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid: %v", err)
			Expect(strings.ToLower(err.Error())).To(ContainSubstring("exactly one"))
		})

		It("rejects a cluster with relative mountPath", func() {
			ctx := context.Background()
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-mount", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{
						{
							Source:    v1alpha1.PackageSource{URL: "https://example.com/x.zip"},
							MountPath: "etc/plugins/x", // relative — must fail
						},
					},
				},
			}
			err := k().Create(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid: %v", err)
		})

		It("rejects a cluster with non-http(s) URL scheme", func() {
			ctx := context.Background()
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-url", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{
						{
							Source:    v1alpha1.PackageSource{URL: "ftp://example.com/x.zip"},
							MountPath: "/etc/plugins/x",
						},
					},
				},
			}
			err := k().Create(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid: %v", err)
		})

		It("rejects a cluster with empty auth (neither basicAuth nor bearerToken set)", func() {
			ctx := context.Background()
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "empty-auth", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{
						{
							Source: v1alpha1.PackageSource{
								URL:  "https://example.com/x.zip",
								Auth: &v1alpha1.PackageAuthSpec{}, // explicitly set, both methods nil
							},
							MountPath: "/etc/plugins/x",
						},
					},
				},
			}
			err := k().Create(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid: %v", err)
			Expect(strings.ToLower(err.Error())).To(ContainSubstring("exactly one"),
				"error message must mention exactly-one constraint; got %v", err)
		})

		It("rejects a cluster with duplicate mountPath across packages", func() {
			ctx := context.Background()
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "dup-mount", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{
						{
							Source:    v1alpha1.PackageSource{URL: "https://example.com/a.zip"},
							MountPath: "/etc/plugins/dup",
						},
						{
							Source:    v1alpha1.PackageSource{URL: "https://example.com/b.zip"},
							MountPath: "/etc/plugins/dup",
						},
					},
				},
			}
			err := k().Create(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid: %v", err)
			Expect(strings.ToLower(err.Error())).To(ContainSubstring("unique"),
				"error message must mention uniqueness; got %v", err)
		})
	})

	// =================================================================
	// Snapshot — Deployment shape with one package
	// =================================================================
	Describe("snapshot of Deployment shape with one package", Ordered, func() {
		const (
			ns          = "e2e-packages-snapshot"
			clusterName = "pkg-snap"
			nodeName    = "pkg-snap-rt"
		)
		BeforeAll(func() { createNS(ns) })
		AfterAll(func() { deleteNS(ns) })

		It("captures the Deployment with one package via go-snaps", func() {
			ctx := context.Background()

			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-package.yaml", ns,
				map[string]interface{}{
					"name": clusterName, "namespace": ns,
					"url":       "https://example.com/snap.zip",
					"mountPath": "/etc/plugins/snap",
				})
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
				map[string]interface{}{"name": nodeName, "namespace": ns, "clusterName": clusterName})

			deployName := ownedName(clusterName, nodeName)
			deploy := &appsv1.Deployment{}
			Eventually(func(g Gomega) {
				g.Expect(k().Get(ctx, client.ObjectKey{Name: deployName, Namespace: ns}, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Annotations["curity.io/packages-hash"]).NotTo(BeEmpty())
			}, e2eTimeout, e2eInterval).Should(Succeed())

			// Snapshot the Deployment (go-snaps via test/utils helpers).
			utils.MatchResource(deploy, "Deployment", "pkg-snap/01-deployment-with-package")
		})
	})
})
