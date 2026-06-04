package controller_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

		It("flips ManagedConfigsValid=False on a mount-affecting issue and recovers", func() {
			testCreateCluster(ns, "mcv-cluster")
			testCreateManagedConfigMap(ns, "mcv-bad", map[string]string{"x.xml": "<x/>"},
				map[string]string{"curity.io/config-type": "baseddd"})

			cluster := &v1alpha1.IdentityServerCluster{}
			mcvStatus := func() metav1.ConditionStatus {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mcv-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				if c := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionManagedConfigsValid); c != nil {
					return c.Status
				}
				return ""
			}

			By("surfacing the skipped config as ManagedConfigsValid=False/UnknownConfigType")
			Eventually(mcvStatus, timeout, interval).Should(Equal(metav1.ConditionFalse))
			Expect(apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionManagedConfigsValid).Reason).
				To(Equal("UnknownConfigType"))

			By("not overloading Degraded with the config issue (guards bbb8379)")
			deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
			Expect(deg).ToNot(BeNil())
			Expect(deg.Status).To(Equal(metav1.ConditionFalse))

			By("clearing the condition once the config-type is fixed (omitted when no issues)")
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mcv-bad", Namespace: ns}, cm)).To(Succeed())
			cm.Annotations["curity.io/config-type"] = "base"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Empty managedResourceIssues → condition omitted; mcvStatus returns "".
			Eventually(mcvStatus, timeout, interval).Should(BeEmpty())
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
