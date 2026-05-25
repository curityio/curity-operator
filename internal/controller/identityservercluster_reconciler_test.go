package controller_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerCluster Reconciler", func() {
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

	Context("Happy path", func() {
		It("should add finalizer and set initial conditions", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			// Verify finalizer is added
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-1", Namespace: ns}, cluster)
				for _, f := range cluster.Finalizers {
					if f == v1alpha1.ClusterFinalizer {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify status is set
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-1", Namespace: ns}, cluster)
				return cluster.Status.Version
			}, timeout, interval).Should(Equal("11.0"))
		})

		It("should track node count", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-2", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			// Create a runtime node
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "runtime-1", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "runtime-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-2"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			// Verify node count increases
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-2", Namespace: ns}, cluster)
				return cluster.Status.NodeCount
			}, timeout, interval).Should(Equal(int32(1)))
		})
	})

	Context("Admin credentials", func() {
		It("should create secret if it does not exist", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "test-admin-secret",
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

			secret := &corev1.Secret{}
			eventuallyGetResource(ns, "test-admin-secret", secret)
			Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
			Expect(secret.Data).To(HaveKey("CONFIG_ENCRYPTION_KEY"))
			Expect(secret.Data).To(HaveKey("KEYSTORE_PASSWORD"))

			// Verify no OwnerReference (survives cluster deletion)
			Expect(secret.OwnerReferences).To(BeEmpty())
		})

		It("should default admin credentials when not specified", func() {
			testCreateCluster(ns, "cluster-no-creds")

			// The reconciler should auto-create the default secret
			secret := &corev1.Secret{}
			eventuallyGetResource(ns, "cluster-no-creds-admin-creds", secret)
			Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
			Expect(secret.Data).To(HaveKey("CONFIG_ENCRYPTION_KEY"))
			Expect(secret.Data).To(HaveKey("KEYSTORE_PASSWORD"))
			Expect(secret.Labels["app.kubernetes.io/managed-by"]).To(Equal("curity-operator"))
			Expect(secret.Labels["curity.io/cluster"]).To(Equal("cluster-no-creds"))
			Expect(secret.OwnerReferences).To(BeEmpty())
		})

		It("should inject defaulted credential env vars into deployment", func() {
			testCreateCluster(ns, "cluster-default-env")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "admin-default-env", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-default-env"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					UI:                       &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			testSimulateClusterConfigReady(ns, "cluster-default-env")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-default-env", "admin-default-env"), deploy)

			envVars := deploy.Spec.Template.Spec.Containers[0].Env
			Expect(hasEnvFromSecret(envVars, "PASSWORD", "cluster-default-env-admin-creds", "ADMIN_PASSWORD")).To(BeTrue())
			Expect(hasEnvFromSecret(envVars, "CONFIG_ENCRYPTION_KEY", "cluster-default-env-admin-creds", "CONFIG_ENCRYPTION_KEY")).To(BeTrue())
			Expect(hasEnvFromSecret(envVars, "KEYSTORE_PASSWORD", "cluster-default-env-admin-creds", "KEYSTORE_PASSWORD")).To(BeTrue())
		})
	})

	Context("CRD validation", func() {
		// --- Cluster spec validations ---

		It("should reject cluster with empty version", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-empty-ver", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: ""},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty version should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("spec.version"))
		})

		It("should reject cluster with version exceeding max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-long-ver", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: longString(129)},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "version >128 chars should be rejected by MaxLength=128")
			Expect(err.Error()).To(ContainSubstring("spec.version"))
		})

		It("should reject cluster with image exceeding max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-long-img", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Image:   longString(513),
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "image >512 chars should be rejected by MaxLength=512")
			Expect(err.Error()).To(ContainSubstring("spec.image"))
		})

		It("should reject cluster with imagePullSecret exceeding max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-long-ips", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:         "11.0",
					ImagePullSecret: longString(254),
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "imagePullSecret >253 chars should be rejected by MaxLength=253")
			Expect(err.Error()).To(ContainSubstring("spec.imagePullSecret"))
		})

		It("should reject cluster with empty credential secret name", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-empty-cred", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "",
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
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty secretKeyRef name should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("secretKeyRef.name"))
		})

		It("should reject cluster with empty credential items", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-empty-items", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name:  "empty-items-secret",
								Items: []v1alpha1.KeyToPath{},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty Items should be rejected by MinItems=3 validation")
			Expect(err.Error()).To(ContainSubstring("secretKeyRef.items"))
			Expect(err.Error()).To(ContainSubstring("at least 3 items"))
		})

		It("should reject cluster with two credential items (boundary)", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-two-items", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "two-items-secret",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "two-item list is below MinItems=3 boundary")
			Expect(err.Error()).To(ContainSubstring("secretKeyRef.items"))
			Expect(err.Error()).To(ContainSubstring("at least 3 items"))
		})

		It("should reject cluster with four credential items (boundary)", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-four-items", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "four-items-secret",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
									{Key: "KEYSTORE_PASSWORD", Path: "KEYSTORE_PASSWORD"},
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD_DUP"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "four-item list exceeds MaxItems=3 boundary")
			Expect(err.Error()).To(ContainSubstring("secretKeyRef.items"))
			Expect(err.Error()).To(ContainSubstring("at most 3 items"))
		})

		It("should reject cluster with three credential items missing KEYSTORE_PASSWORD", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-miss-ks", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "miss-ks-secret",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD2"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "missing KEYSTORE_PASSWORD must be rejected by CEL rule")
			Expect(err.Error()).To(ContainSubstring("KEYSTORE_PASSWORD"))
		})

		It("should reject cluster with three credential items missing ADMIN_PASSWORD", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-miss-ap", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "miss-ap-secret",
								Items: []v1alpha1.KeyToPath{
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "ENC1"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "ENC2"},
									{Key: "KEYSTORE_PASSWORD", Path: "KEYSTORE_PASSWORD"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "missing ADMIN_PASSWORD must be rejected by CEL rule")
			Expect(err.Error()).To(ContainSubstring("ADMIN_PASSWORD"))
		})

		It("should reject cluster with three credential items missing CONFIG_ENCRYPTION_KEY", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-miss-enc", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "miss-enc-secret",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
									{Key: "KEYSTORE_PASSWORD", Path: "KS1"},
									{Key: "KEYSTORE_PASSWORD", Path: "KS2"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "missing CONFIG_ENCRYPTION_KEY must be rejected by CEL rule")
			Expect(err.Error()).To(ContainSubstring("CONFIG_ENCRYPTION_KEY"))
		})

		It("should reject cluster with package bearerToken secretRef empty name", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-empty-name", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source: v1alpha1.PackageSource{
							URL: "https://example.com/pkg.zip",
							Auth: &v1alpha1.PackageAuthSpec{
								BearerToken: &v1alpha1.PackageSecretKeyRef{
									SecretRef: v1alpha1.PackageSecretKeySelector{Name: "", Key: "token"},
								},
							},
						},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty package secretRef name should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("secretRef.name"))
		})

		It("should reject cluster with package basicAuth empty usernameKey", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-empty-userkey", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source: v1alpha1.PackageSource{
							URL: "https://example.com/pkg.zip",
							Auth: &v1alpha1.PackageAuthSpec{
								BasicAuth: &v1alpha1.PackageBasicAuthRef{
									SecretRef: v1alpha1.PackageBasicAuthSelector{
										Name: "creds", UsernameKey: "", PasswordKey: "pw",
									},
								},
							},
						},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty basicAuth usernameKey should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("usernameKey"))
		})

		It("should reject cluster with clientCert empty secretRef name", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-cc-empty-name", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source: v1alpha1.PackageSource{
							URL: "https://example.com/pkg.zip",
							TLS: &v1alpha1.PackageTLSSpec{
								Enabled: true,
								ClientCert: &v1alpha1.PackageClientCertRef{
									SecretRef: v1alpha1.PackageClientCertSelector{Name: "", Key: "tls.crt"},
								},
							},
						},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty clientCert.secretRef.name should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("clientCert.secretRef.name"))
		})

		It("should reject cluster with clientCert uppercase secretRef name", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-cc-uc-name", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source: v1alpha1.PackageSource{
							URL: "https://example.com/pkg.zip",
							TLS: &v1alpha1.PackageTLSSpec{
								Enabled: true,
								ClientCert: &v1alpha1.PackageClientCertRef{
									SecretRef: v1alpha1.PackageClientCertSelector{Name: "MyCert", Key: "tls.crt"},
								},
							},
						},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "uppercase clientCert.secretRef.name violates DNS-1123 pattern")
			Expect(err.Error()).To(ContainSubstring("clientCert.secretRef.name"))
		})

		It("should reject cluster with clientCert empty secretRef key", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-cc-empty-key", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source: v1alpha1.PackageSource{
							URL: "https://example.com/pkg.zip",
							TLS: &v1alpha1.PackageTLSSpec{
								Enabled: true,
								ClientCert: &v1alpha1.PackageClientCertRef{
									SecretRef: v1alpha1.PackageClientCertSelector{Name: "mtls", Key: ""},
								},
							},
						},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty clientCert.secretRef.key should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("clientCert.secretRef.key"))
		})

		It("should reject cluster with clientCert secretRef name exceeding max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-cc-longname", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source: v1alpha1.PackageSource{
							URL: "https://example.com/pkg.zip",
							TLS: &v1alpha1.PackageTLSSpec{
								Enabled: true,
								ClientCert: &v1alpha1.PackageClientCertRef{
									SecretRef: v1alpha1.PackageClientCertSelector{Name: longString(254), Key: "tls.crt"},
								},
							},
						},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "clientCert.secretRef.name >253 chars should be rejected by MaxLength=253")
			Expect(err.Error()).To(ContainSubstring("clientCert.secretRef.name"))
		})

		It("should reject cluster with whitespace-only admin secret name", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-ws-name", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: " ",
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
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "whitespace-only Secret name slips past MinLength=1; Pattern must reject")
			Expect(err.Error()).To(ContainSubstring("secretKeyRef.name"))
			Expect(err.Error()).To(ContainSubstring("should match"))
		})

		It("should reject cluster with uppercase admin secret name", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-uc-name", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "BadName",
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
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "uppercase Secret name violates DNS-1123 pattern")
			Expect(err.Error()).To(ContainSubstring("secretKeyRef.name"))
		})

		It("should reject cluster with package URL containing whitespace", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-ws-url", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source:    v1alpha1.PackageSource{URL: "https://exa\tmple.com/x.zip"},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "URL with embedded tab must be rejected by Pattern")
			Expect(err.Error()).To(ContainSubstring("source.url"))
		})

		It("should reject cluster with package URL containing embedded credentials", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds-url", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source:    v1alpha1.PackageSource{URL: "https://user:pass@host/x.zip"},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "URL with userinfo@authority must be rejected — use structured auth field")
			Expect(err.Error()).To(ContainSubstring("source.url"))
		})

		It("should reject cluster with empty items[].path", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-path-empty", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "s",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: ""},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "ENC"},
									{Key: "KEYSTORE_PASSWORD", Path: "KS"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "empty path slips past Required; MinLength=1 must reject")
			Expect(err.Error()).To(ContainSubstring("items"))
		})

		It("should reject cluster with items[].path containing slash", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-path-slash", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "s",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "a/b"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "ENC"},
									{Key: "KEYSTORE_PASSWORD", Path: "KS"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "items.path is an env var name; slash is invalid POSIX shape")
			Expect(err.Error()).To(ContainSubstring("items"))
		})

		It("should reject cluster with items[].path starting with digit", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-path-digit", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "s",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "1starts"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "ENC"},
									{Key: "KEYSTORE_PASSWORD", Path: "KS"},
								},
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "POSIX env var names cannot start with a digit")
			Expect(err.Error()).To(ContainSubstring("items"))
		})

		It("should accept cluster with items[].path POSIX-valid forms", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-path-ok", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "s",
								Items: []v1alpha1.KeyToPath{
									{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
									{Key: "CONFIG_ENCRYPTION_KEY", Path: "_LEADING_UNDERSCORE"},
									{Key: "KEYSTORE_PASSWORD", Path: "Mixed123Case_OK"},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("should reject node with whitespace-only clusterRef name", func() {
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-crws", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "crws-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: " "},
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "whitespace-only clusterRef.name slips past MinLength=1; Pattern must reject")
			Expect(err.Error()).To(ContainSubstring("identityServerClusterRef.name"))
		})

		// Cross-namespace clusterRef is rejected because K8s GC treats
		// OwnerReference as namespace-local: a node with a controller
		// ownerRef to a foreign-namespace cluster is silently cascade-
		// deleted by the garbage collector when the UID is not found in
		// the dependent's namespace.
		It("should reject node with cross-namespace clusterRef.namespace", func() {
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-crns-foreign", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "crns-foreign-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "c", Namespace: "other-ns"},
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "cross-namespace clusterRef.namespace must be rejected — K8s GC would cascade-delete the node")
			Expect(err.Error()).To(ContainSubstring("cross-namespace references are not supported"))
		})

		It("should reject node with same-namespace clusterRef.namespace (any non-empty value)", func() {
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-crns-same", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "crns-same-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "c", Namespace: ns},
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "any non-empty namespace is rejected; the field has no legitimate use")
			Expect(err.Error()).To(ContainSubstring("cross-namespace references are not supported"))
		})

		It("should accept cluster with @ only in URL path", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-at-path", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source:    v1alpha1.PackageSource{URL: "https://host/path@with@at/x.zip"},
						MountPath: "/opt/x/",
					}},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed(), "@ in path is fine — only authority-section @ is the security smell")
		})

		It("should accept cluster with https:// package URL", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-https", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source:    v1alpha1.PackageSource{URL: "https://example.com/pkg.zip"},
						MountPath: "/opt/x/",
					}},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed(), "https:// is accepted")
		})

		It("should accept cluster with http:// package URL (both schemes supported)", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-http", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source:    v1alpha1.PackageSource{URL: "http://internal-mirror.svc/pkg.zip"},
						MountPath: "/opt/x/",
					}},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed(), "http:// is also accepted — CRD doesn't enforce HTTPS; use NetworkPolicy if needed")
		})

		It("should reject cluster with non-http(s) URL scheme", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pkg-ftp", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Packages: []v1alpha1.PackageSpec{{
						Source:    v1alpha1.PackageSource{URL: "ftp://example.com/pkg.zip"},
						MountPath: "/opt/x/",
					}},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "ftp:// must be rejected — only http and https are allowed")
			Expect(err.Error()).To(ContainSubstring("source.url"))
		})

		// --- Node spec validations ---

		It("should reject node with invalid type", func() {
			testCreateCluster(ns, "val-cluster-type")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-bad-type", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeType("worker"),
					Role:                     "bad-type-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-type"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "invalid type 'worker' should be rejected by Enum")
			Expect(err.Error()).To(ContainSubstring("spec.type"))
		})

		It("should reject node with empty role", func() {
			testCreateCluster(ns, "val-cluster-role")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-empty-role", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-role"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "empty role should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("spec.role"))
		})

		It("should reject node with role exceeding max length", func() {
			testCreateCluster(ns, "val-cluster-rolelen")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-long-role", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     longString(64),
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rolelen"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "role >63 chars should be rejected by MaxLength=63")
			Expect(err.Error()).To(ContainSubstring("spec.role"))
		})

		It("should reject node with uppercase role", func() {
			testCreateCluster(ns, "val-cluster-roleup")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-upper-role", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "Admin-Role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-roleup"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "uppercase role should be rejected by Pattern")
			Expect(err.Error()).To(ContainSubstring("spec.role"))
		})

		It("should reject node with role starting with hyphen", func() {
			testCreateCluster(ns, "val-cluster-rolehyp")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-hyp-role", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "-bad-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rolehyp"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "role starting with hyphen should be rejected by Pattern")
			Expect(err.Error()).To(ContainSubstring("spec.role"))
		})

		It("should reject node with empty cluster ref name", func() {
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-empty-ref", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "empty-ref-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: ""},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "empty clusterRef name should be rejected by MinLength=1")
			Expect(err.Error()).To(ContainSubstring("identityServerClusterRef.name"))
		})

		It("should reject node with replicas of zero", func() {
			testCreateCluster(ns, "val-cluster-rep0")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-rep-zero", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "rep-zero-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rep0"},
					Replicas:                 ptr.To(int32(0)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "replicas=0 should be rejected by Minimum=1")
			Expect(err.Error()).To(ContainSubstring("spec.replicas"))
		})

		It("should reject admin node with replicas > 1 via CEL", func() {
			testCreateCluster(ns, "val-cluster-admrep")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-admin-rep5", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin-rep5-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admrep"},
					Replicas:                 ptr.To(int32(5)),
					Service:                  defaultTestService(),
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "admin with replicas>1 must be rejected by CEL — Curity admin is single-active")
			Expect(err.Error()).To(ContainSubstring("replicas must be 1 for admin-type nodes"))
		})

		It("should accept admin node with replicas = 1 (boundary)", func() {
			testCreateCluster(ns, "val-cluster-adm1")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-admin-rep1", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin-rep1-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-adm1"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed(), "admin with replicas=1 is the only valid admin value")
		})

		It("should accept admin node with replicas omitted (default=1 fills in)", func() {
			testCreateCluster(ns, "val-cluster-admdef")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-admin-repdef", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin-repdef-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admdef"},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed(), "default=1 fills in, CEL is satisfied")
		})

		It("should reject admin node with autoscaling enabled via CEL", func() {
			testCreateCluster(ns, "val-cluster-admas")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-admin-as", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin-as-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admas"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled: true, MinReplicas: 1, MaxReplicas: 5, TargetCPUUtilizationPercentage: 80,
					},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "admin with autoscaling.enabled=true must be rejected — would be silently ignored otherwise")
			Expect(err.Error()).To(ContainSubstring("autoscaling cannot be enabled on admin-type nodes"))
		})

		It("should accept admin node with autoscaling block but enabled=false", func() {
			testCreateCluster(ns, "val-cluster-admasf")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-admin-asf", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin-asf-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admasf"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled: false, MinReplicas: 1, MaxReplicas: 1, TargetCPUUtilizationPercentage: 80,
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed(), "explicit autoscaling.enabled=false is fine on admin")
		})

		// --- Autoscaling validations ---

		It("should reject node with minReplicas > maxReplicas", func() {
			testCreateCluster(ns, "val-cluster-hpa")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-bad-hpa", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "bad-hpa-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-hpa"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled:                        true,
						MinReplicas:                    10,
						MaxReplicas:                    5,
						TargetCPUUtilizationPercentage: 80,
					},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "minReplicas > maxReplicas should be rejected by CEL validation")
			Expect(err.Error()).To(ContainSubstring("minReplicas must be less than or equal to maxReplicas"))
		})

		It("should accept node with minReplicas == maxReplicas", func() {
			testCreateCluster(ns, "val-cluster-hpa-eq")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-hpa-eq", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "hpa-eq-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-hpa-eq"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling: &v1alpha1.AutoscalingSpec{
						Enabled:                        true,
						MinReplicas:                    5,
						MaxReplicas:                    5,
						TargetCPUUtilizationPercentage: 80,
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed(), "minReplicas == maxReplicas should be accepted")
		})

		// --- Service validations ---

		It("should reject node with invalid service type", func() {
			testCreateCluster(ns, "val-cluster-svctype")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-bad-svc", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "bad-svc-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-svctype"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceType("InvalidType"), Port: 8443},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "invalid service type should be rejected by Enum")
			Expect(err.Error()).To(ContainSubstring("spec.service.type"))
		})

		It("should reject node with service port zero via unstructured", func() {
			testCreateCluster(ns, "val-cluster-port0")
			obj := newUnstructuredNode(ns, "node-port-zero", "port0-role", "val-cluster-port0")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"type": "ClusterIP",
				"port": int64(0),
			}, "spec", "service")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "port=0 should be rejected by Minimum=1")
			Expect(err.Error()).To(ContainSubstring("spec.service.port"))
		})

		It("should reject node with service port exceeding max", func() {
			testCreateCluster(ns, "val-cluster-portmax")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-port-max", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "port-max-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-portmax"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 70000},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "port=70000 should be rejected by Maximum=65535")
			Expect(err.Error()).To(ContainSubstring("spec.service.port"))
		})

		// --- Logging validations ---

		It("should reject node with invalid log level", func() {
			testCreateCluster(ns, "val-cluster-loglvl")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-bad-loglvl", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "bad-loglvl-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-loglvl"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Logging:                  &v1alpha1.LoggingSpec{Level: "VERBOSE"},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "invalid log level should be rejected by Enum")
			Expect(err.Error()).To(ContainSubstring("spec.logging.level"))
		})

		It("should reject node with invalid log stream name", func() {
			testCreateCluster(ns, "val-cluster-logstr")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-bad-logstr", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "bad-logstr-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-logstr"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Logging:                  &v1alpha1.LoggingSpec{Level: "INFO", Logs: []string{"invalid-stream"}},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "invalid log stream name should be rejected by items:Enum")
			Expect(err.Error()).To(ContainSubstring("spec.logging.logs"))
		})

		It("should reject node with logging image exceeding max length", func() {
			testCreateCluster(ns, "val-cluster-logimg")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-long-logimg", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "long-logimg-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-logimg"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Logging:                  &v1alpha1.LoggingSpec{Level: "INFO", Image: longString(513)},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "logging image >512 chars should be rejected by MaxLength=512")
			Expect(err.Error()).To(ContainSubstring("spec.logging.image"))
		})

		// --- Autoscaling validations ---

		It("should reject node with autoscaling minReplicas zero via unstructured", func() {
			testCreateCluster(ns, "val-cluster-asmin0")
			obj := newUnstructuredNode(ns, "node-asmin0", "asmin0-role", "val-cluster-asmin0")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"enabled":                        true,
				"minReplicas":                    int64(0),
				"maxReplicas":                    int64(5),
				"targetCPUUtilizationPercentage": int64(80),
			}, "spec", "autoscaling")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "autoscaling minReplicas=0 should be rejected by Minimum=1")
			Expect(err.Error()).To(ContainSubstring("spec.autoscaling.minReplicas"))
		})

		It("should reject node with autoscaling maxReplicas zero via unstructured", func() {
			testCreateCluster(ns, "val-cluster-asmx0")
			obj := newUnstructuredNode(ns, "node-asmx0", "asmx0-role", "val-cluster-asmx0")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"enabled":                        true,
				"minReplicas":                    int64(1),
				"maxReplicas":                    int64(0),
				"targetCPUUtilizationPercentage": int64(80),
			}, "spec", "autoscaling")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "autoscaling maxReplicas=0 should be rejected by Minimum=1")
			Expect(err.Error()).To(ContainSubstring("spec.autoscaling.maxReplicas"))
		})

		It("should reject node with targetCPU zero via unstructured", func() {
			testCreateCluster(ns, "val-cluster-tcpu0")
			obj := newUnstructuredNode(ns, "node-tcpu0", "tcpu0-role", "val-cluster-tcpu0")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"enabled":                        true,
				"minReplicas":                    int64(1),
				"maxReplicas":                    int64(5),
				"targetCPUUtilizationPercentage": int64(0),
			}, "spec", "autoscaling")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "targetCPU=0 should be rejected by Minimum=1")
			Expect(err.Error()).To(ContainSubstring("spec.autoscaling.targetCPUUtilizationPercentage"))
		})

		It("should reject node with autoscaling maxReplicas exceeding maximum", func() {
			testCreateCluster(ns, "val-cluster-asmax")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-as-maxhi", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "as-maxhi-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-asmax"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling:              &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 10001, TargetCPUUtilizationPercentage: 80},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "autoscaling maxReplicas=10001 should be rejected by Maximum=10000")
			Expect(err.Error()).To(ContainSubstring("spec.autoscaling.maxReplicas"))
		})

		It("should reject node with targetCPU over 100", func() {
			testCreateCluster(ns, "val-cluster-cpu101")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-cpu101", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "cpu101-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-cpu101"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling:              &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 5, TargetCPUUtilizationPercentage: 101},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "targetCPU=101 should be rejected by Maximum=100")
			Expect(err.Error()).To(ContainSubstring("spec.autoscaling.targetCPUUtilizationPercentage"))
		})

		// --- Positive tests ---

		It("should accept cluster with all valid fields", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-valid-all", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:         "11.0",
					Image:           "registry.example.com/curity:11.0",
					ImagePullSecret: "my-pull-secret",
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("should accept node with valid role pattern", func() {
			testCreateCluster(ns, "val-cluster-ok-role")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-ok-role", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "my-runtime-node-1",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-ok-role"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		It("should accept node with ExternalName service type", func() {
			testCreateCluster(ns, "val-cluster-extname")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-extname", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "extname-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-extname"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceTypeExternalName, Port: 8443},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		It("should accept node with valid log streams", func() {
			testCreateCluster(ns, "val-cluster-logok")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-logok", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "logok-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-logok"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Logging: &v1alpha1.LoggingSpec{
						Level: "INFO",
						Logs:  []string{"audit", "request", "cluster", "confsvc", "confsvc-internal", "post-commit-scripts"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		// --- Boundary-exact positive tests (catch off-by-one in markers) ---

		It("should accept cluster with version at exactly max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-ver-128", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: longString(128)},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("should accept cluster with image at exactly max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-img-512", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					Image:   longString(512),
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("should accept cluster with imagePullSecret at exactly max length", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-ips-253", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:         "11.0",
					ImagePullSecret: longString(253),
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("should accept node with role at exactly max length", func() {
			testCreateCluster(ns, "val-cluster-role63")
			// 63 chars, DNS-label safe: starts/ends with alnum, only lowercase+hyphens
			role63 := "a" + strings.Repeat("-a", 31)
			Expect(len(role63)).To(Equal(63))
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-role-63", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     role63,
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-role63"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		It("should accept node with replicas at exactly minimum", func() {
			testCreateCluster(ns, "val-cluster-rep1")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-rep-1", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "rep1-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rep1"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		It("should accept node with service port at boundaries", func() {
			testCreateCluster(ns, "val-cluster-portbd")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-port-max", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "port-max-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-portbd"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 65535},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		It("should accept node with autoscaling at boundary values", func() {
			testCreateCluster(ns, "val-cluster-asbd")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-as-boundary", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "as-boundary-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-asbd"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Autoscaling:              &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 1, MaxReplicas: 10000, TargetCPUUtilizationPercentage: 100},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		It("should accept node with logging image at exactly max length", func() {
			testCreateCluster(ns, "val-cluster-logimgbd")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-logimg-512", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "logimg512-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-logimgbd"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Logging:                  &v1alpha1.LoggingSpec{Level: "INFO", Image: longString(512)},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		// --- MaxProperties / MaxItems upper-bound tests ---

		It("should reject cluster with nodeSelector exceeding max properties", func() {
			sel := make(map[string]string, 101)
			for i := range 101 {
				sel[fmt.Sprintf("key-%d", i)] = "v"
			}
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-ns-101", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:      "11.0",
					NodeSelector: sel,
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "nodeSelector with 101 entries should be rejected by MaxProperties=100")
			Expect(err.Error()).To(ContainSubstring("spec.nodeSelector"))
		})

		It("should reject node with tolerations exceeding max items", func() {
			tols := make([]corev1.Toleration, 101)
			for i := range 101 {
				tols[i] = corev1.Toleration{Key: fmt.Sprintf("key-%d", i), Operator: corev1.TolerationOpExists}
			}
			testCreateCluster(ns, "val-cluster-tol101")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-tol-101", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "tol101-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-tol101"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					Tolerations:              tols,
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "tolerations with 101 items should be rejected by MaxItems=100")
			Expect(err.Error()).To(ContainSubstring("spec.tolerations"))
		})

		It("should reject node with topologySpreadConstraints exceeding max items", func() {
			tscs := make([]corev1.TopologySpreadConstraint, 33)
			for i := range 33 {
				tscs[i] = corev1.TopologySpreadConstraint{
					MaxSkew:           1,
					TopologyKey:       fmt.Sprintf("zone-%d", i),
					WhenUnsatisfiable: corev1.DoNotSchedule,
				}
			}
			testCreateCluster(ns, "val-cluster-tsc33")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-tsc-33", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                      v1alpha1.NodeTypeRuntime,
					Role:                      "tsc33-role",
					IdentityServerClusterRef:  v1alpha1.ObjectReference{Name: "val-cluster-tsc33"},
					Replicas:                  ptr.To(int32(1)),
					Service:                   defaultTestService(),
					TopologySpreadConstraints: tscs,
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "topologySpreadConstraints with 33 items should be rejected by MaxItems=32")
			Expect(err.Error()).To(ContainSubstring("spec.topologySpreadConstraints"))
		})

		It("should reject admin node UI enabled=true without secure field via unstructured", func() {
			testCreateCluster(ns, "val-cluster-uisecure")
			obj := newUnstructuredNode(ns, "node-ui-no-secure", "ui-no-secure-role", "val-cluster-uisecure")
			_ = unstructured.SetNestedField(obj.Object, "admin", "spec", "type")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"enabled": true,
			}, "spec", "ui")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "UI enabled=true without secure should be rejected by CEL rule")
			Expect(err.Error()).To(ContainSubstring("secure is required when enabled is true"))
		})

		It("should accept admin node UI enabled=false without secure field via unstructured", func() {
			testCreateCluster(ns, "val-cluster-uidisabled")
			obj := newUnstructuredNode(ns, "node-ui-disabled", "ui-disabled-role", "val-cluster-uidisabled")
			_ = unstructured.SetNestedField(obj.Object, "admin", "spec", "type")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"enabled": false,
			}, "spec", "ui")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "UI enabled=false should not require secure")
		})

		It("should accept admin node UI enabled=true with secure=false (HTTP mode)", func() {
			// Invariant guard: the CEL rule on UISpec is `!self.enabled || has(self.secure)` —
			// it checks PRESENCE of secure, not its value. HTTP-mode admin UI is a supported
			// configuration. Any future tightening of the CEL (e.g. "secure must be true")
			// must fail this named test, forcing the author to consider whether breaking
			// HTTP admin UI is intentional.
			testCreateCluster(ns, "val-cluster-uihttp")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-ui-http", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "ui-http-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-uihttp"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					UI:                       &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(false)},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed(), "admin UI with enabled=true + secure=false (HTTP mode) is a valid configuration")
		})

		It("should reject node with negative PDB minAvailable via unstructured", func() {
			testCreateCluster(ns, "val-cluster-pdbneg")
			obj := newUnstructuredNode(ns, "node-pdb-neg", "pdb-neg-role", "val-cluster-pdbneg")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"minAvailable": "-1",
			}, "spec", "podDisruptionBudget")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "negative minAvailable should be rejected by Pattern")
			Expect(err.Error()).To(ContainSubstring("spec.podDisruptionBudget.minAvailable"))
		})

		It("should reject node with negative integer PDB minAvailable via unstructured", func() {
			testCreateCluster(ns, "val-cluster-pdbnegint")
			obj := newUnstructuredNode(ns, "node-pdb-negint", "pdb-negint-role", "val-cluster-pdbnegint")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"minAvailable": int64(-2),
			}, "spec", "podDisruptionBudget")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "negative integer minAvailable should be rejected by CEL rule")
			Expect(err.Error()).To(ContainSubstring("minAvailable must be non-negative"))
		})

		It("should reject node with non-numeric PDB minAvailable string via unstructured", func() {
			testCreateCluster(ns, "val-cluster-pdbstr")
			obj := newUnstructuredNode(ns, "node-pdb-str", "pdb-str-role", "val-cluster-pdbstr")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"minAvailable": "half",
			}, "spec", "podDisruptionBudget")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "non-numeric minAvailable should be rejected by Pattern")
			Expect(err.Error()).To(ContainSubstring("spec.podDisruptionBudget.minAvailable"))
		})

		It("should accept node with PDB minAvailable integer zero boundary via unstructured", func() {
			testCreateCluster(ns, "val-cluster-pdbzero")
			obj := newUnstructuredNode(ns, "node-pdb-zero", "pdb-zero-role", "val-cluster-pdbzero")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"minAvailable": int64(0),
			}, "spec", "podDisruptionBudget")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "minAvailable=0 is the non-negative boundary and must be accepted")
		})

		It("should accept node with PDB minAvailable string 100% boundary via unstructured", func() {
			testCreateCluster(ns, "val-cluster-pdbpct")
			obj := newUnstructuredNode(ns, "node-pdb-pct", "pdb-pct-role", "val-cluster-pdbpct")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"minAvailable": "100%",
			}, "spec", "podDisruptionBudget")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "minAvailable=100% is a valid percentage and must be accepted")
		})

		It("should reject service block missing type via unstructured", func() {
			testCreateCluster(ns, "val-cluster-svcnotype")
			obj := newUnstructuredNode(ns, "node-svc-notype", "svc-notype-role", "val-cluster-svcnotype")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"port": int64(8443),
			}, "spec", "service")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "service without type should be rejected by Required")
			Expect(err.Error()).To(ContainSubstring("spec.service.type"))
		})

		It("should reject service block missing port via unstructured", func() {
			testCreateCluster(ns, "val-cluster-svcnoport")
			obj := newUnstructuredNode(ns, "node-svc-noport", "svc-noport-role", "val-cluster-svcnoport")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"type": "ClusterIP",
			}, "spec", "service")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "service without port should be rejected by Required")
			Expect(err.Error()).To(ContainSubstring("spec.service.port"))
		})

		It("should reject ui block missing enabled via unstructured", func() {
			testCreateCluster(ns, "val-cluster-uinoenabled")
			obj := newUnstructuredNode(ns, "node-ui-noenabled", "ui-noenabled-role", "val-cluster-uinoenabled")
			_ = unstructured.SetNestedField(obj.Object, "admin", "spec", "type")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"secure": true,
			}, "spec", "ui")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "ui without enabled should be rejected by Required")
			Expect(err.Error()).To(ContainSubstring("spec.ui.enabled"))
		})

		It("should reject autoscaling block missing enabled via unstructured", func() {
			testCreateCluster(ns, "val-cluster-asnoenabled")
			obj := newUnstructuredNode(ns, "node-as-noenabled", "as-noenabled-role", "val-cluster-asnoenabled")
			_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
				"minReplicas":                    int64(2),
				"maxReplicas":                    int64(5),
				"targetCPUUtilizationPercentage": int64(80),
			}, "spec", "autoscaling")
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "autoscaling without enabled should be rejected by Required")
			Expect(err.Error()).To(ContainSubstring("spec.autoscaling.enabled"))
		})
	})

	Context("Cluster config generation", func() {
		It("should create placeholder Secret and Job when admin node exists", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cc-cluster", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "cc-admin", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "cc-admin-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cc-cluster"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			secret := &corev1.Secret{}
			eventuallyGetResource(ns, "cc-cluster-cluster-config", secret)
			Expect(string(secret.Data["cluster.xml"])).To(Equal("placeholder"))
			Expect(secret.Annotations).To(HaveKeyWithValue("argocd.argoproj.io/compare-options", "IgnoreExtraneous"))
			Expect(secret.Annotations).To(HaveKey("curity.io/admin-node"))
			Expect(secret.OwnerReferences).To(BeEmpty())

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "cc-cluster-cluster-config-job", job)
			Expect(job.Labels["curity.io/cluster"]).To(Equal("cc-cluster"))
			Expect(*job.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		})

		It("should set WaitingForAdmin when no admin node exists", func() {
			testCreateCluster(ns, "wait-cluster")

			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wait-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				return conditionReason(cluster.Status.Conditions, "ClusterConfigReady")
			}, timeout, interval).Should(Equal("WaitingForAdmin"))
		})

		It("should skip Job when cluster config Secret already populated", func() {
			// Pre-create a populated Secret
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "skip-cluster-cluster-config",
					Namespace: ns,
					Annotations: map[string]string{
						"curity.io/admin-node":               "skip-admin",
						"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
						"curity.io/encryption-key-hash":      "",
					},
					Labels: map[string]string{
						"curity.io/cluster":   "skip-cluster",
						"curity.io/component": "cluster-config",
					},
				},
				Data: map[string][]byte{"cluster.xml": []byte("<config>real data</config>")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			testCreateCluster(ns, "skip-cluster")
			testCreateNode(ns, "skip-admin", v1alpha1.NodeTypeAdmin, "skip-cluster")

			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "skip-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return hasCondition(cluster.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())
			Expect(cluster.Status.ClusterConfigSecretName).To(Equal("skip-cluster-cluster-config"))

			// Verify NO Job was created
			job := &batchv1.Job{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "skip-cluster-cluster-config-job", Namespace: ns}, job)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Job should not be created when Secret is already populated")
		})

		It("should delete failed Job and create a new one", func() {
			testCreateCluster(ns, "fail-cluster")
			testCreateNode(ns, "fail-admin", v1alpha1.NodeTypeAdmin, "fail-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "fail-cluster-cluster-config-job", job)
			origUID := job.UID

			// Manually set Job as Failed (envtest has no Job controller)
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Message: "ImagePullBackOff",
				},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Assert via Event (persists), not condition (operator flips
			// JobFailed → JobCreated within ms when it recreates the Job).
			Eventually(func() bool {
				events := &corev1.EventList{}
				if err := k8sClient.List(ctx, events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.InvolvedObject.Kind == "IdentityServerCluster" &&
						e.InvolvedObject.Name == "fail-cluster" &&
						e.Reason == "ClusterConfigJobFailed" &&
						e.Type == corev1.EventTypeWarning {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "expected Warning ClusterConfigJobFailed event")

			Eventually(func() bool {
				newJob := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "fail-cluster-cluster-config-job", Namespace: ns}, newJob); err != nil {
					return false
				}
				return newJob.UID != origUID
			}, timeout, interval).Should(BeTrue())
		})

		It("should not create duplicate Job on concurrent reconcile", func() {
			testCreateCluster(ns, "race-cluster")
			testCreateNode(ns, "race-admin", v1alpha1.NodeTypeAdmin, "race-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "race-cluster-cluster-config-job", job)

			// Only one Job should exist (AlreadyExists guard)
			jobList := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobList,
				client.InNamespace(ns),
				client.MatchingLabels{"curity.io/cluster": "race-cluster"},
			)).To(Succeed())
			Expect(jobList.Items).To(HaveLen(1))
		})

		It("should inherit scheduling constraints on Job", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "sched-cluster", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:      "11.0",
					NodeSelector: map[string]string{"disk": "ssd"},
					Tolerations: []corev1.Toleration{
						{Key: "special", Operator: corev1.TolerationOpExists},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			testCreateNode(ns, "sched-admin", v1alpha1.NodeTypeAdmin, "sched-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "sched-cluster-cluster-config-job", job)
			Expect(job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("disk", "ssd"))
			Expect(job.Spec.Template.Spec.Tolerations).To(HaveLen(1))
		})

		It("should add ArgoCD annotation on cluster config Secret", func() {
			testCreateCluster(ns, "argo-cluster")
			testCreateNode(ns, "argo-admin", v1alpha1.NodeTypeAdmin, "argo-cluster")

			secret := &corev1.Secret{}
			eventuallyGetResource(ns, "argo-cluster-cluster-config", secret)
			Expect(secret.Annotations["argocd.argoproj.io/compare-options"]).To(Equal("IgnoreExtraneous"))
		})

		It("should update XML in-place when admin node is renamed (Scenario 5)", func() {
			testCreateCluster(ns, "rename-cluster")
			testCreateNode(ns, "rename-admin", v1alpha1.NodeTypeAdmin, "rename-cluster")

			// Wait for placeholder Secret with admin-node annotation
			secret := &corev1.Secret{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rename-cluster-cluster-config", Namespace: ns}, secret); err != nil {
					return ""
				}
				return secret.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("rename-admin"))

			// Pre-populate Secret with XML containing the admin hostname (prefixed Service name).
			secret.Data = map[string][]byte{"cluster.xml": []byte(
				fmt.Sprintf("<config><cluster><host>%s</host><port>6789</port></cluster></config>",
					ownedName("rename-cluster", "rename-admin")))}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Delete old admin node, create new one with different name
			oldNode := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rename-admin", Namespace: ns}, oldNode)).To(Succeed())
			Expect(k8sClient.Delete(ctx, oldNode)).To(Succeed())

			eventuallyDeleted(ns, "rename-admin", &v1alpha1.IdentityServerNode{})
			testCreateNode(ns, "rename-admin-v2", v1alpha1.NodeTypeAdmin, "rename-cluster")

			// Annotation should be updated in-place (no delete/recreate)
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rename-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return s.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("rename-admin-v2"))

			// Verify the XML was updated in-place with new hostname
			updatedSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rename-cluster-cluster-config", Namespace: ns}, updatedSecret)).To(Succeed())
			Expect(string(updatedSecret.Data["cluster.xml"])).To(ContainSubstring(fmt.Sprintf("<host>%s</host>", ownedName("rename-cluster", "rename-admin-v2"))))
			Expect(string(updatedSecret.Data["cluster.xml"])).NotTo(ContainSubstring(fmt.Sprintf("<host>%s</host>", ownedName("rename-cluster", "rename-admin"))))
			// Rest of XML preserved
			Expect(string(updatedSecret.Data["cluster.xml"])).To(ContainSubstring("<port>6789</port>"))
		})

		It("should fall through to full regen when admin renames but XML lacks the expected <host> tag", func() {
			testCreateCluster(ns, "rename-nohost-cluster")
			testCreateNode(ns, "rename-nohost-admin", v1alpha1.NodeTypeAdmin, "rename-nohost-cluster")

			// Wait for Secret
			secret := &corev1.Secret{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-cluster-cluster-config", Namespace: ns}, secret); err != nil {
					return ""
				}
				return secret.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("rename-nohost-admin"))

			// Pre-populate with XML that has no <host> tag (user-edited or malformed).
			// The cheap rename path's strings.Replace would silently no-op on this
			// input — but the cluster-config-hash guard now refuses to take the
			// cheap path when the expected tag is missing, falling through to full
			// regen instead. That resets the Secret to placeholder and a fresh
			// genclust Job runs with the new admin name.
			originalXML := "<config><cluster><keystore>abc</keystore></cluster></config>"
			secret.Data = map[string][]byte{"cluster.xml": []byte(originalXML)}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Rename admin
			oldNode := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-admin", Namespace: ns}, oldNode)).To(Succeed())
			Expect(k8sClient.Delete(ctx, oldNode)).To(Succeed())
			eventuallyDeleted(ns, "rename-nohost-admin", &v1alpha1.IdentityServerNode{})
			testCreateNode(ns, "rename-nohost-admin-v2", v1alpha1.NodeTypeAdmin, "rename-nohost-cluster")

			// admin-node annotation eventually updates to the new name.
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return s.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("rename-nohost-admin-v2"))

			// Data was reset to placeholder by the full-regen branch (the cheap
			// rename was rejected because the original XML lacked <host>).
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return string(s.Data["cluster.xml"])
			}, timeout, interval).Should(Equal("placeholder"))
		})

		It("should reset to placeholder when encryption key changes (Scenario 11)", func() {
			// Create cluster with admin credentials
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "enckey-cluster", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "enckey-creds",
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

			credSecret := &corev1.Secret{}
			eventuallyGetResource(ns, "enckey-creds", credSecret)

			testCreateNode(ns, "enckey-admin", v1alpha1.NodeTypeAdmin, "enckey-cluster")

			// Wait for cluster config Secret with encryption key hash
			configSecret := &corev1.Secret{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "enckey-cluster-cluster-config", Namespace: ns}, configSecret); err != nil {
					return false
				}
				_, ok := configSecret.Annotations["curity.io/encryption-key-hash"]
				return ok
			}, timeout, interval).Should(BeTrue())

			oldHash := configSecret.Annotations["curity.io/encryption-key-hash"]
			Expect(oldHash).NotTo(BeEmpty())

			// Pre-populate Secret so it's "ready"
			configSecret.Data = map[string][]byte{"cluster.xml": []byte("<config>encrypted-data</config>")}
			Expect(k8sClient.Update(ctx, configSecret)).To(Succeed())

			// Change the encryption key in credentials Secret
			// The Secret watch triggers a cluster reconcile automatically
			credSecret.Data["CONFIG_ENCRYPTION_KEY"] = []byte("completely-new-encryption-key-value")
			Expect(k8sClient.Update(ctx, credSecret)).To(Succeed())

			// Secret should be reset to placeholder with new hash (not deleted)
			Eventually(func() bool {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "enckey-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return false
				}
				newHash := s.Annotations["curity.io/encryption-key-hash"]
				data := string(s.Data["cluster.xml"])
				return newHash != oldHash && data == "placeholder"
			}, timeout, interval).Should(BeTrue())
		})

		It("should not reset when encryption key hash is set for the first time", func() {
			// Create cluster with admin credentials pointing to a pre-existing Secret
			// that initially has NO CONFIG_ENCRYPTION_KEY
			credSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "firstkey-creds",
					Namespace: ns,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "curity-operator",
						"curity.io/cluster":            "firstkey-cluster",
					},
				},
				Data: map[string][]byte{
					"ADMIN_PASSWORD": []byte("password123"),
				},
			}
			Expect(k8sClient.Create(ctx, credSecret)).To(Succeed())

			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "firstkey-cluster", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "firstkey-creds",
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
			testCreateNode(ns, "firstkey-admin", v1alpha1.NodeTypeAdmin, "firstkey-cluster")

			configSecret := &corev1.Secret{}
			eventuallyGetResource(ns, "firstkey-cluster-cluster-config", configSecret)

			// Pre-populate so it's "ready"
			configSecret.Data = map[string][]byte{"cluster.xml": []byte("<config>some-data</config>")}
			Expect(k8sClient.Update(ctx, configSecret)).To(Succeed())

			// Verify it stays ready (not reset) — no encryption key hash means skip check
			Consistently(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "firstkey-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return string(s.Data["cluster.xml"])
			}, 3*time.Second, interval).Should(Equal("<config>some-data</config>"))
		})

		It("should retry when pod logs are unavailable (Scenario 9)", func() {
			testCreateCluster(ns, "logs-cluster")
			testCreateNode(ns, "logs-admin", v1alpha1.NodeTypeAdmin, "logs-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "logs-cluster-cluster-config-job", job)
			origUID := job.UID

			// Mark Job as Complete (but no pod exists with Succeeded phase → logs unavailable).
			// Set LastTransitionTime to now so the 30s fallback timeout doesn't trigger.
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Condition should become LogsUnavailable
			Eventually(func() string {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "logs-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				return conditionReason(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			}, timeout, interval).Should(Equal("LogsUnavailable"))

			// Job must survive — not deleted and recreated
			Consistently(func() types.UID {
				j := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "logs-cluster-cluster-config-job", Namespace: ns}, j); err != nil {
					return ""
				}
				return j.UID
			}, 5*time.Second, interval).Should(Equal(origUID))

			// Recovery: populate the Secret with real data (simulating the log
			// read eventually succeeding). The ISC reconciler should detect the
			// ready Secret and transition from LogsUnavailable → SecretReady.
			testSimulateClusterConfigReady(ns, "logs-cluster")

			cluster := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "logs-cluster", Namespace: ns}, cluster)).To(Succeed())
			Expect(conditionReason(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)).To(Equal("SecretReady"))
		})

		It("removes stale ConfigScopeIssues condition while in LogsUnavailable", func() {
			// LogsUnavailable persists Status without the wholesale rebuild,
			// so a stale ConfigScopeIssues would otherwise survive forever.
			testCreateCluster(ns, "stale-logs-cluster")
			testCreateNode(ns, "stale-logs-admin", v1alpha1.NodeTypeAdmin, "stale-logs-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "stale-logs-cluster-cluster-config-job", job)
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Wait for cluster to reach LogsUnavailable so we know the
			// early-return branch is being exercised on each requeue.
			Eventually(func() string {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stale-logs-cluster", Namespace: ns}, c); err != nil {
					return ""
				}
				return conditionReason(c.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			}, timeout, interval).Should(Equal("LogsUnavailable"))

			// Inject a stale ConfigScopeIssues condition.
			cluster := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stale-logs-cluster", Namespace: ns}, cluster)).To(Succeed())
			cluster.Status.Conditions = append(cluster.Status.Conditions, metav1.Condition{
				Type:               "ConfigScopeIssues",
				Status:             metav1.ConditionTrue,
				Reason:             "UnknownClusters",
				Message:            "[stale entry from older operator]",
				LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			// Next requeue must drop the stale condition.
			Eventually(func() bool {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stale-logs-cluster", Namespace: ns}, c); err != nil {
					return true
				}
				for _, cond := range c.Status.Conditions {
					if cond.Type == "ConfigScopeIssues" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeFalse(), "stale ConfigScopeIssues was not removed")

			// LogsUnavailable must survive.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stale-logs-cluster", Namespace: ns}, cluster)).To(Succeed())
			Expect(conditionReason(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)).To(Equal("LogsUnavailable"))
		})

		It("should delete Job after 30s timeout when pod logs are permanently unavailable", func() {
			testCreateCluster(ns, "stale-cluster")
			testCreateNode(ns, "stale-admin", v1alpha1.NodeTypeAdmin, "stale-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "stale-cluster-cluster-config-job", job)
			origUID := job.UID

			// Mark Job as Complete with a timestamp >30s in the past,
			// simulating a pod that was garbage-collected long ago.
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:               batchv1.JobComplete,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now().Add(-60 * time.Second)),
				},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Job should be deleted and recreated with a new UID
			Eventually(func() bool {
				newJob := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stale-cluster-cluster-config-job", Namespace: ns}, newJob); err != nil {
					return false
				}
				return newJob.UID != origUID
			}, timeout, interval).Should(BeTrue())
		})

	})

	Context("Cluster config workflows", func() {
		It("should complete full lifecycle: create → populate → version upgrade → regenerate", func() {
			testCreateCluster(ns, "lifecycle-cluster")
			testCreateNode(ns, "lifecycle-admin", v1alpha1.NodeTypeAdmin, "lifecycle-cluster")

			// Step 1+2: Simulate Job completion — populate Secret with real data
			// AND the annotations the operator would have stamped, so the regen
			// logic recognises the steady state.
			testSimulateClusterConfigReady(ns, "lifecycle-cluster")

			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return hasCondition(cluster.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Capture pre-upgrade hash so we can verify it changes after the version bump.
			preUpgradeSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster-cluster-config", Namespace: ns}, preUpgradeSecret)).To(Succeed())
			oldHash := preUpgradeSecret.Annotations["curity.io/cluster-config-hash"]
			Expect(oldHash).NotTo(BeEmpty(), "expected cluster-config-hash annotation after simulated population")

			// Step 4: Version upgrade — cluster.xml MUST regenerate.
			// Retry on optimistic-concurrency conflicts: the cluster reconciler
			// writes to status concurrently and may bump resourceVersion between
			// our Get and Update.
			Eventually(func() error {
				fresh := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster", Namespace: ns}, fresh); err != nil {
					return err
				}
				fresh.Spec.Version = "12.0"
				return k8sClient.Update(ctx, fresh)
			}, timeout, interval).Should(Succeed())

			// Secret data should be reset to placeholder (regen branch fired).
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return string(s.Data["cluster.xml"])
			}, timeout, interval).Should(Equal("placeholder"))

			// Stored hash should differ from the pre-upgrade value.
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return s.Annotations["curity.io/cluster-config-hash"]
			}, timeout, interval).ShouldNot(Equal(oldHash))

			// Condition should pass through Regenerating → JobCreated/JobRunning
			// before settling. We don't strictly assert the brief Regenerating
			// state here; that is covered by dedicated observability tests.
			// Verify a fresh Job was created against the new spec.
			job := &batchv1.Job{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster-cluster-config-job", Namespace: ns}, job)
			}, timeout, interval).Should(Succeed())
		})

		It("should do admin rename workflow: rename → in-place update → condition stays ready", func() {
			testCreateCluster(ns, "wf-rename-cluster")
			testCreateNode(ns, "wf-rename-admin", v1alpha1.NodeTypeAdmin, "wf-rename-cluster")

			// Wait for Secret and pre-populate with real XML
			secret := &corev1.Secret{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-rename-cluster-cluster-config", Namespace: ns}, secret); err != nil {
					return ""
				}
				return secret.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("wf-rename-admin"))

			secret.Data = map[string][]byte{"cluster.xml": []byte(
				fmt.Sprintf("<config><cluster><keystore>abc123</keystore><host>%s</host><port>6789</port></cluster></config>",
					ownedName("wf-rename-cluster", "wf-rename-admin")))}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-rename-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return hasCondition(cluster.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Rename admin node
			oldNode := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wf-rename-admin", Namespace: ns}, oldNode)).To(Succeed())
			Expect(k8sClient.Delete(ctx, oldNode)).To(Succeed())
			eventuallyDeleted(ns, "wf-rename-admin", &v1alpha1.IdentityServerNode{})
			testCreateNode(ns, "wf-rename-admin-v2", v1alpha1.NodeTypeAdmin, "wf-rename-cluster")

			// Verify in-place update: new hostname, keystore preserved
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-rename-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return string(s.Data["cluster.xml"])
			}, timeout, interval).Should(And(
				ContainSubstring(fmt.Sprintf("<host>%s</host>", ownedName("wf-rename-cluster", "wf-rename-admin-v2"))),
				ContainSubstring("<keystore>abc123</keystore>"),
				Not(ContainSubstring(fmt.Sprintf("<host>%s</host>", ownedName("wf-rename-cluster", "wf-rename-admin")))),
			))

			// ClusterConfigReady should still be True (not reset)
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-rename-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return hasCondition(cluster.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())
		})

		It("should do encryption key rotation workflow: change key → reset → new Job", func() {
			// Create cluster with credentials
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "wf-enckey-cluster", Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version: "11.0",
					AdminCredentials: &v1alpha1.CredentialsSource{
						ValueFrom: v1alpha1.CredentialsValueFrom{
							SecretKeyRef: v1alpha1.SecretKeyRefSource{
								Name: "wf-enckey-creds",
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

			credSecret := &corev1.Secret{}
			eventuallyGetResource(ns, "wf-enckey-creds", credSecret)

			testCreateNode(ns, "wf-enckey-admin", v1alpha1.NodeTypeAdmin, "wf-enckey-cluster")

			// Wait for cluster config Secret with hash
			configSecret := &corev1.Secret{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-enckey-cluster-cluster-config", Namespace: ns}, configSecret); err != nil {
					return false
				}
				_, ok := configSecret.Annotations["curity.io/encryption-key-hash"]
				return ok
			}, timeout, interval).Should(BeTrue())

			oldHash := configSecret.Annotations["curity.io/encryption-key-hash"]

			// Pre-populate Secret
			configSecret.Data = map[string][]byte{"cluster.xml": []byte("<config>encrypted-data</config>")}
			Expect(k8sClient.Update(ctx, configSecret)).To(Succeed())

			Eventually(func() bool {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-enckey-cluster", Namespace: ns}, c); err != nil {
					return false
				}
				return hasCondition(c.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Change encryption key
			credSecret.Data["CONFIG_ENCRYPTION_KEY"] = []byte("brand-new-key-value")
			Expect(k8sClient.Update(ctx, credSecret)).To(Succeed())

			// Secret should be reset to placeholder with new hash
			Eventually(func() bool {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-enckey-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return false
				}
				return s.Annotations["curity.io/encryption-key-hash"] != oldHash &&
					string(s.Data["cluster.xml"]) == "placeholder"
			}, timeout, interval).Should(BeTrue())

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "wf-enckey-cluster-cluster-config-job", job)
		})

		It("should regenerate when Secret is deleted externally (Scenario 1)", func() {
			testCreateCluster(ns, "wf-delsecret-cluster")
			testCreateNode(ns, "wf-delsecret-admin", v1alpha1.NodeTypeAdmin, "wf-delsecret-cluster")

			// Wait for Secret and pre-populate
			secret := &corev1.Secret{}
			eventuallyGetResource(ns, "wf-delsecret-cluster-cluster-config", secret)

			secret.Data = map[string][]byte{"cluster.xml": []byte(
				fmt.Sprintf("<config><host>%s</host></config>", ownedName("wf-delsecret-cluster", "wf-delsecret-admin")))}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			Eventually(func() bool {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-delsecret-cluster", Namespace: ns}, c); err != nil {
					return false
				}
				return hasCondition(c.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Delete the Secret externally (simulating user or accidental deletion)
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())

			// Operator should detect missing Secret and create a new placeholder + Job
			newSecret := &corev1.Secret{}
			eventuallyGetResource(ns, "wf-delsecret-cluster-cluster-config", newSecret)

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "wf-delsecret-cluster-cluster-config-job", job)
		})

		It("should preserve Secret when Cluster CR is deleted (Scenario 2)", func() {
			testCreateCluster(ns, "wf-delcr-cluster")
			testCreateNode(ns, "wf-delcr-admin", v1alpha1.NodeTypeAdmin, "wf-delcr-cluster")

			secret := &corev1.Secret{}
			eventuallyGetResource(ns, "wf-delcr-cluster-cluster-config", secret)

			secret.Data = map[string][]byte{"cluster.xml": []byte(
				fmt.Sprintf("<config><host>%s</host></config>", ownedName("wf-delcr-cluster", "wf-delcr-admin")))}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Delete the Node first (finalizer requires this)
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wf-delcr-admin", Namespace: ns}, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			eventuallyDeleted(ns, "wf-delcr-admin", &v1alpha1.IdentityServerNode{})

			// Delete the Cluster CR
			cluster := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wf-delcr-cluster", Namespace: ns}, cluster)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			eventuallyDeleted(ns, "wf-delcr-cluster", &v1alpha1.IdentityServerCluster{})

			// Secret should still exist (no OwnerReference on Cluster CR)
			survivedSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wf-delcr-cluster-cluster-config", Namespace: ns}, survivedSecret)).To(Succeed())
			Expect(string(survivedSecret.Data["cluster.xml"])).To(Equal(
				fmt.Sprintf("<config><host>%s</host></config>", ownedName("wf-delcr-cluster", "wf-delcr-admin"))))
		})

		It("should accept user-edited Secret without regenerating (Scenario 6)", func() {
			// Pre-create the Secret with real data BEFORE the cluster/node,
			// so the operator never creates a Job (Secret is already ready).
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wf-useredit-cluster-cluster-config",
					Namespace: ns,
					Annotations: map[string]string{
						"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
						"curity.io/admin-node":               "wf-useredit-admin",
						"curity.io/encryption-key-hash":      "",
					},
					Labels: map[string]string{
						"curity.io/cluster":            "wf-useredit-cluster",
						"curity.io/component":          "cluster-config",
						"app.kubernetes.io/managed-by": "curity-operator",
					},
				},
				Data: map[string][]byte{"cluster.xml": []byte(
					fmt.Sprintf("<config><host>%s</host></config>", ownedName("wf-useredit-cluster", "wf-useredit-admin")))},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			testCreateCluster(ns, "wf-useredit-cluster")
			testCreateNode(ns, "wf-useredit-admin", v1alpha1.NodeTypeAdmin, "wf-useredit-cluster")

			Eventually(func() bool {
				c := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-useredit-cluster", Namespace: ns}, c); err != nil {
					return false
				}
				return hasCondition(c.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// User edits the Secret directly (custom cluster.xml)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wf-useredit-cluster-cluster-config", Namespace: ns}, secret)).To(Succeed())
			secret.Data = map[string][]byte{"cluster.xml": []byte("<config>user-custom-xml</config>")}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Operator should accept it — data stays as user set it
			Consistently(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-useredit-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return string(s.Data["cluster.xml"])
			}, 3*time.Second, interval).Should(Equal("<config>user-custom-xml</config>"))

			// No Job should exist (operator respects user edits)
			jobList := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobList,
				client.InNamespace(ns),
				client.MatchingLabels{"curity.io/cluster": "wf-useredit-cluster"},
			)).To(Succeed())
			Expect(jobList.Items).To(BeEmpty())
		})

		It("should find existing running Job after operator restart (Scenario 8)", func() {
			testCreateCluster(ns, "wf-restart-cluster")
			testCreateNode(ns, "wf-restart-admin", v1alpha1.NodeTypeAdmin, "wf-restart-cluster")

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "wf-restart-cluster-cluster-config-job", job)
			origUID := job.UID

			// Simulate operator restart: trigger reconcile by touching cluster annotation
			cluster := &v1alpha1.IdentityServerCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "wf-restart-cluster", Namespace: ns}, cluster)).To(Succeed())
			if cluster.Annotations == nil {
				cluster.Annotations = map[string]string{}
			}
			cluster.Annotations["trigger-reconcile"] = "restart-sim"
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			// Job should still be the same one after reconcile (same UID, no duplicate)
			Consistently(func() types.UID {
				sameJob := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "wf-restart-cluster-cluster-config-job", Namespace: ns}, sameJob); err != nil {
					return ""
				}
				return sameJob.UID
			}, 3*time.Second, interval).Should(Equal(origUID))

			// Only one Job should exist
			jobList := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobList,
				client.InNamespace(ns),
				client.MatchingLabels{"curity.io/cluster": "wf-restart-cluster"},
			)).To(Succeed())
			Expect(jobList.Items).To(HaveLen(1))
		})
	})

	Context("Managed config discovery", func() {
		It("should not write the config-type annotation onto managed resources", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "no-type-config", map[string]string{"x.xml": "<x/>"}, nil)

			cm := &corev1.ConfigMap{}
			Consistently(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "no-type-config", Namespace: ns}, cm); err != nil {
					return ""
				}
				return cm.Annotations["curity.io/config-type"]
			}, 5*time.Second, interval).Should(BeEmpty())
		})

		It("should emit DuplicateConfigKey warning on each colliding ConfigMap (not the cluster)", func() {
			testCreateCluster(ns, "dup-cluster")

			// Create two ConfigMaps with the same data key
			testCreateManagedConfigMap(ns, "dup-cm-a", map[string]string{"shared.xml": "<a/>"}, nil)
			testCreateManagedConfigMap(ns, "dup-cm-b", map[string]string{"shared.xml": "<b/>"}, nil)

			// The warning is routed to each colliding CM via UID-keyed dedup.
			// Verify both CMs receive the event, and the cluster does not.
			findOnCM := func(cmName string) *corev1.Event {
				var events corev1.EventList
				if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
					return nil
				}
				for i := range events.Items {
					if events.Items[i].Reason == "DuplicateConfigKey" &&
						events.Items[i].InvolvedObject.Kind == "ConfigMap" &&
						events.Items[i].InvolvedObject.Name == cmName {
						return &events.Items[i]
					}
				}
				return nil
			}

			Eventually(func() *corev1.Event { return findOnCM("dup-cm-a") }, timeout, interval).ShouldNot(BeNil(),
				"expected DuplicateConfigKey event on ConfigMap/dup-cm-a")
			Eventually(func() *corev1.Event { return findOnCM("dup-cm-b") }, timeout, interval).ShouldNot(BeNil(),
				"expected DuplicateConfigKey event on ConfigMap/dup-cm-b")

			evt := findOnCM("dup-cm-a")
			Expect(evt.Message).To(ContainSubstring("shared.xml"), "event message should mention the duplicated key")
			Expect(evt.Message).To(ContainSubstring("dup-cm-a"), "event message should mention first resource")
			Expect(evt.Message).To(ContainSubstring("dup-cm-b"), "event message should mention second resource")

			By("verifying NO DuplicateConfigKey event landed on the cluster CR")
			Consistently(func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "DuplicateConfigKey" && e.InvolvedObject.Kind == "IdentityServerCluster" {
						return false
					}
				}
				return true
			}, "2s", "200ms").Should(BeTrue(),
				"DuplicateConfigKey events must land on the offending CMs, not on the cluster")
		})
	})

	Context("Deletion", func() {
		It("should block deletion while nodes exist", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-del", Namespace: ns},
				Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			// Wait for finalizer
			Eventually(func() bool {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-del", Namespace: ns}, cluster)
				for _, f := range cluster.Finalizers {
					if f == v1alpha1.ClusterFinalizer {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Create a node
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "block-node", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "block-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-del"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			// Try to delete cluster
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

			// Cluster should still exist (finalizer blocks)
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-del", Namespace: ns}, cluster)
			}, 3*time.Second, interval).Should(Succeed())

			// Delete the node
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "block-node", Namespace: ns}, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			eventuallyDeleted(ns, "block-node", &v1alpha1.IdentityServerNode{})
			eventuallyDeleted(ns, "cluster-del", &v1alpha1.IdentityServerCluster{})
		})
	})
})
