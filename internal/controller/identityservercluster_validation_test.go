package controller_test

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var _ = Describe("IdentityServerCluster Reconciler / CRD validation", func() {
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
		Expect(err).To(HaveOccurred(), "empty Items should be rejected by MinItems=2 validation")
		Expect(err.Error()).To(ContainSubstring("secretKeyRef.items"))
		Expect(err.Error()).To(ContainSubstring("at least 2 items"))
	})

	It("should accept cluster with exactly two credential items (boundary)", func() {
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
		Expect(err).NotTo(HaveOccurred(), "exactly ADMIN_PASSWORD + CONFIG_ENCRYPTION_KEY satisfies MinItems=2/MaxItems=2 and the CEL rule")
	})

	It("should reject cluster with three credential items (exceeds MaxItems=2)", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-three-items", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				AdminCredentials: &v1alpha1.CredentialsSource{
					ValueFrom: v1alpha1.CredentialsValueFrom{
						SecretKeyRef: v1alpha1.SecretKeyRefSource{
							Name: "three-items-secret",
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
		Expect(err).To(HaveOccurred(), "three-item list (incl. legacy KEYSTORE_PASSWORD) exceeds MaxItems=2 boundary")
		Expect(err.Error()).To(ContainSubstring("secretKeyRef.items"))
		Expect(err.Error()).To(ContainSubstring("at most 2 items"))
	})

	It("should reject cluster with two credential items missing ADMIN_PASSWORD", func() {
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

	It("should reject cluster with two credential items missing CONFIG_ENCRYPTION_KEY", func() {
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
								{Key: "ADMIN_PASSWORD", Path: "PASSWORD2"},
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
								{Key: "ADMIN_PASSWORD", Path: "_LEADING_UNDERSCORE"},
								{Key: "CONFIG_ENCRYPTION_KEY", Path: "Mixed123Case_OK"},
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
		Expect(err.Error()).To(ContainSubstring("replicas cannot be set on admin-type nodes"))
	})

	It("should reject admin node with replicas = 1 via CEL", func() {
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
		err := k8sClient.Create(ctx, node)
		Expect(err).To(HaveOccurred(), "admin must not set replicas at all — not even 1")
		Expect(err.Error()).To(ContainSubstring("replicas cannot be set on admin-type nodes"))
	})

	It("should accept admin node with replicas omitted", func() {
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
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "omitting replicas is the only valid admin form; reconciler coerces to 1")
	})

	It("should reject admin node with autoscaling enabled via CEL", func() {
		testCreateCluster(ns, "val-cluster-admas")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-admin-as", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeAdmin,
				Role:                     "admin-as-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admas"},
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
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled: false, MinReplicas: 1, MaxReplicas: 1, TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "explicit autoscaling.enabled=false is fine on admin")
	})

	It("should accept admin node with service omitted (operator defaults it)", func() {
		testCreateCluster(ns, "val-cluster-admnosvc")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-admin-nosvc", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeAdmin,
				Role:                     "admin-nosvc-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admnosvc"},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "service is optional; the operator provisions a default Service")
	})

	It("should accept admin node with ui.enabled and service omitted", func() {
		testCreateCluster(ns, "val-cluster-admuinosvc")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-admin-uinosvc", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeAdmin,
				Role:                     "admin-uinosvc-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admuinosvc"},
				UI:                       &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "enabling the admin UI must not make service required")
	})

	It("should accept runtime node with service omitted (defaults to ClusterIP/8443)", func() {
		testCreateCluster(ns, "val-cluster-rtnosvc")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-rt-nosvc", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "rt-nosvc-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rtnosvc"},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "service is optional for runtime too")
	})

	It("should reject runtime node with skipInstall set via CEL", func() {
		testCreateCluster(ns, "val-cluster-rtski")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-rt-skip", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "rt-skip-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rtski"},
				Service:                  defaultTestService(),
				SkipInstall:              ptr.To(true),
			},
		}
		err := k8sClient.Create(ctx, node)
		Expect(err).To(HaveOccurred(), "skipInstall is admin-only; runtime never installs")
		Expect(err.Error()).To(ContainSubstring("skipInstall can only be set on admin-type nodes"))
	})

	It("should reject runtime node with skipInstall=false via CEL (strict: presence rejected, even false)", func() {
		testCreateCluster(ns, "val-cluster-rtskif")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-rt-skipf", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "rt-skipf-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rtskif"},
				Service:                  defaultTestService(),
				SkipInstall:              ptr.To(false),
			},
		}
		err := k8sClient.Create(ctx, node)
		Expect(err).To(HaveOccurred(), "strict rule rejects any skipInstall on runtime, including false")
		Expect(err.Error()).To(ContainSubstring("skipInstall can only be set on admin-type nodes"))
	})

	It("should accept admin node with skipInstall=true", func() {
		testCreateCluster(ns, "val-cluster-admski")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-adm-skip", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeAdmin,
				Role:                     "adm-skip-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admski"},
				Service:                  defaultTestService(),
				SkipInstall:              ptr.To(true),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "skipInstall is valid on admin")
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
				Service:                  &v1alpha1.ServiceSpec{Type: corev1.ServiceType("InvalidType"), Port: 8443},
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
				Service:                  &v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 70000},
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
				Service:                  &v1alpha1.ServiceSpec{Type: corev1.ServiceTypeExternalName, Port: 8443},
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
				Service:                  &v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 65535},
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

	It("should accept node with PDB maxUnavailable (int and percentage) via unstructured", func() {
		testCreateCluster(ns, "val-cluster-pdbmax")
		obj := newUnstructuredNode(ns, "node-pdb-max", "pdb-max-role", "val-cluster-pdbmax")
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
			"maxUnavailable": int64(1),
		}, "spec", "podDisruptionBudget")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "maxUnavailable integer must be accepted")

		obj2 := newUnstructuredNode(ns, "node-pdb-maxpct", "pdb-maxpct-role", "val-cluster-pdbmax")
		_ = unstructured.SetNestedField(obj2.Object, map[string]interface{}{
			"maxUnavailable": "50%",
		}, "spec", "podDisruptionBudget")
		Expect(k8sClient.Create(ctx, obj2)).To(Succeed(), "maxUnavailable percentage must be accepted")
	})

	It("should reject node with negative PDB maxUnavailable via unstructured", func() {
		testCreateCluster(ns, "val-cluster-pdbmaxneg")
		obj := newUnstructuredNode(ns, "node-pdb-maxneg", "pdb-maxneg-role", "val-cluster-pdbmaxneg")
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
			"maxUnavailable": int64(-1),
		}, "spec", "podDisruptionBudget")
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("maxUnavailable must be non-negative"))
	})

	It("should reject PDB with both minAvailable and maxUnavailable (exactly-one CEL)", func() {
		testCreateCluster(ns, "val-cluster-pdbboth")
		obj := newUnstructuredNode(ns, "node-pdb-both", "pdb-both-role", "val-cluster-pdbboth")
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
			"minAvailable":   int64(1),
			"maxUnavailable": int64(1),
		}, "spec", "podDisruptionBudget")
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of minAvailable or maxUnavailable"))
	})

	It("should reject PDB with neither minAvailable nor maxUnavailable (exactly-one CEL)", func() {
		testCreateCluster(ns, "val-cluster-pdbneither")
		obj := newUnstructuredNode(ns, "node-pdb-neither", "pdb-neither-role", "val-cluster-pdbneither")
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{}, "spec", "podDisruptionBudget")
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of minAvailable or maxUnavailable"))
	})

	It("should accept service block with only port (type defaults) via unstructured", func() {
		testCreateCluster(ns, "val-cluster-svcnotype")
		obj := newUnstructuredNode(ns, "node-svc-notype", "svc-notype-role", "val-cluster-svcnotype")
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
			"port": int64(8443),
		}, "spec", "service")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "service.type is optional; the operator defaults it to ClusterIP")
	})

	It("should accept service block with only type (port omitted) via unstructured", func() {
		testCreateCluster(ns, "val-cluster-svcnoport")
		obj := newUnstructuredNode(ns, "node-svc-noport", "svc-noport-role", "val-cluster-svcnoport")
		_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
			"type": "ClusterIP",
		}, "spec", "service")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "service.port is optional; the role port is simply not exposed")
	})

	It("should reject distributedServicePort on a runtime node via CEL", func() {
		testCreateCluster(ns, "val-cluster-rtds")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-rt-ds", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "rt-ds-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rtds"},
				Service:                  &v1alpha1.ServiceSpec{DistributedServicePort: 6790},
			},
		}
		err := k8sClient.Create(ctx, node)
		Expect(err).To(HaveOccurred(), "distributedServicePort is admin-only")
		Expect(err.Error()).To(ContainSubstring("only valid on admin-type nodes"))
	})

	It("should reject uiPort on a runtime node via CEL", func() {
		testCreateCluster(ns, "val-cluster-rtui")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-rt-ui", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "rt-ui-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-rtui"},
				Service:                  &v1alpha1.ServiceSpec{UIPort: 6749},
			},
		}
		err := k8sClient.Create(ctx, node)
		Expect(err).To(HaveOccurred(), "uiPort is admin-only")
		Expect(err.Error()).To(ContainSubstring("only valid on admin-type nodes"))
	})

	It("should accept admin node with distributedServicePort and uiPort set", func() {
		testCreateCluster(ns, "val-cluster-admports")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-adm-ports", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeAdmin,
				Role:                     "adm-ports-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "val-cluster-admports"},
				UI:                       &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)},
				Service:                  &v1alpha1.ServiceSpec{DistributedServicePort: 7790, UIPort: 7749},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed(), "distributedServicePort and uiPort are valid on admin")
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

	// --- Pod-customization escape hatches + NetworkPolicy validation ---
	// DryRunAll runs admission (structural + CEL) without persisting, so these
	// stay pure validation checks with no reconcile side effects. The same
	// markers are generated identically onto IdentityServerNodeSpec.

	mkCluster := func(name string, mutate func(*v1alpha1.IdentityServerClusterSpec)) *v1alpha1.IdentityServerCluster {
		c := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
		}
		mutate(&c.Spec)
		return c
	}

	It("rejects terminationGracePeriodSeconds below 0", func() {
		err := k8sClient.Create(ctx, mkCluster("tgps-neg", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.TerminationGracePeriodSeconds = ptr.To(int64(-1))
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.terminationGracePeriodSeconds"))
	})

	It("rejects terminationGracePeriodSeconds above 3600", func() {
		err := k8sClient.Create(ctx, mkCluster("tgps-big", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.TerminationGracePeriodSeconds = ptr.To(int64(3601))
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.terminationGracePeriodSeconds"))
	})

	It("accepts terminationGracePeriodSeconds boundaries 0 and 3600", func() {
		Expect(k8sClient.Create(ctx, mkCluster("tgps-0", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.TerminationGracePeriodSeconds = ptr.To(int64(0))
		}), client.DryRunAll)).To(Succeed())
		Expect(k8sClient.Create(ctx, mkCluster("tgps-3600", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.TerminationGracePeriodSeconds = ptr.To(int64(3600))
		}), client.DryRunAll)).To(Succeed())
	})

	It("rejects an invalid imagePullPolicy", func() {
		err := k8sClient.Create(ctx, mkCluster("ipp-bad", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.ImagePullPolicy = corev1.PullPolicy("Sometimes")
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.imagePullPolicy"))
	})

	It("rejects a bad apiGatewayNamespace pattern", func() {
		err := k8sClient.Create(ctx, mkCluster("np-badns", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.NetworkPolicy = &v1alpha1.NetworkPolicySpec{APIGatewayNamespace: "Bad_NS!"}
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.networkPolicy.apiGatewayNamespace"))
	})

	It("rejects apiGatewayNamespace exceeding max length", func() {
		err := k8sClient.Create(ctx, mkCluster("np-longns", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.NetworkPolicy = &v1alpha1.NetworkPolicySpec{APIGatewayNamespace: longString(64)}
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.networkPolicy.apiGatewayNamespace"))
	})

	It("rejects more than 16 initContainers (MaxItems)", func() {
		cs := make([]corev1.Container, 17)
		for i := range cs {
			cs[i] = corev1.Container{Name: fmt.Sprintf("c%d", i), Image: "busybox:1.36"}
		}
		err := k8sClient.Create(ctx, mkCluster("init-max", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.InitContainers = cs
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.initContainers"))
	})

	It("rejects an initContainer named 'curity' (reserved-name CEL)", func() {
		err := k8sClient.Create(ctx, mkCluster("init-curity", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.InitContainers = []corev1.Container{{Name: "curity", Image: "busybox:1.36"}}
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("'curity' is reserved"))
	})

	It("rejects an extraContainer named 'curity' (reserved-name CEL)", func() {
		err := k8sClient.Create(ctx, mkCluster("extra-curity", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.ExtraContainers = []corev1.Container{{Name: "curity", Image: "busybox:1.36"}}
		}), client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("'curity' is reserved"))
	})

	It("accepts exactly 16 initContainers (MaxItems boundary)", func() {
		cs := make([]corev1.Container, 16)
		for i := range cs {
			cs[i] = corev1.Container{Name: fmt.Sprintf("c%d", i), Image: "busybox:1.36"}
		}
		Expect(k8sClient.Create(ctx, mkCluster("init-16", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.InitContainers = cs
		}), client.DryRunAll)).To(Succeed())
	})

	It("accepts apiGatewayNamespace at exactly 63 chars (MaxLength boundary)", func() {
		Expect(k8sClient.Create(ctx, mkCluster("np-63", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.NetworkPolicy = &v1alpha1.NetworkPolicySpec{APIGatewayNamespace: "a" + longString(62)}
		}), client.DryRunAll)).To(Succeed())
	})

	It("accepts a rich initContainer with probe + lifecycle httpGet (the defaulting shape)", func() {
		Expect(k8sClient.Create(ctx, mkCluster("init-rich", func(s *v1alpha1.IdentityServerClusterSpec) {
			s.InitContainers = []corev1.Container{{
				Name:  "warmup",
				Image: "busybox:1.36",
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
				}},
				Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/drain", Port: intstr.FromInt32(8080)},
				}},
			}}
		}), client.DryRunAll)).To(Succeed())
	})

})
