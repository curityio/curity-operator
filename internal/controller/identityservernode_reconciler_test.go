package controller_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/internal/controller"
)

var nodeTestCounter atomic.Int64

func nodeTestNamespace() string {
	return fmt.Sprintf("node-test-%d", nodeTestCounter.Add(1))
}

func testCreateCluster(ns, name string) {
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
	}
	Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
}

// defaultTestService returns a minimal valid ServiceSpec for tests that don't
// care about the specific Type/Port — keeps individual test sites free of the
// magic 8443 literal.
func defaultTestService() v1alpha1.ServiceSpec {
	return v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 8443}
}

func testCreateNode(ns, name string, nodeType v1alpha1.NodeType, clusterName string) {
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.IdentityServerNodeSpec{
			Type:                     nodeType,
			Role:                     name + "-role",
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
			Replicas:                 ptr.To(int32(1)),
			Service:                  defaultTestService(),
		},
	}
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
}

func hasCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus) bool {
	for _, c := range conditions {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}

func conditionReason(conditions []metav1.Condition, condType string) string {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Reason
		}
	}
	return ""
}

func eventuallyGetResource(ns, name string, obj client.Object) {
	Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)
	}, 30*time.Second, 250*time.Millisecond).Should(Succeed())
}

func eventuallyDeleted(ns, name string, obj client.Object) {
	Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj))
	}, 30*time.Second, 250*time.Millisecond).Should(BeTrue())
}

// ownedName delegates to the production OwnedResourceName so test assertions
// look up child resources by exactly the name the operator computed.
func ownedName(clusterName, nodeName string) string {
	return controller.OwnedResourceName(clusterName, nodeName)
}

func hasEnvFromSecret(envVars []corev1.EnvVar, envName, secretName, secretKey string) bool {
	for _, env := range envVars {
		if env.Name == envName && env.ValueFrom != nil &&
			env.ValueFrom.SecretKeyRef != nil &&
			env.ValueFrom.SecretKeyRef.Name == secretName &&
			env.ValueFrom.SecretKeyRef.Key == secretKey {
			return true
		}
	}
	return false
}

// longString returns a string of length n composed of repeated 'a' characters.
func longString(n int) string {
	return strings.Repeat("a", n)
}

// newUnstructuredNode builds a minimal IdentityServerNode as an unstructured object.
// This bypasses Go's omitempty so zero-valued fields are sent to the API server.
func newUnstructuredNode(ns, name, role, clusterRef string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "curity.io/v1alpha1",
			"kind":       "IdentityServerNode",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"type":     "runtime",
				"role":     role,
				"replicas": int64(1),
				"identityServerClusterRef": map[string]interface{}{
					"name": clusterRef,
				},
				"service": map[string]interface{}{
					"type": "ClusterIP",
					"port": int64(8443),
				},
			},
		},
	}
}

// testSimulateClusterConfigReady simulates a genclust Job completion by
// writing real data plus the annotations the operator would stamp, then
// waits for ConditionClusterConfigReady=True. Call AFTER the admin node
// exists so the cluster reconciler can resolve adminNodeName.
func testSimulateClusterConfigReady(ns, clusterName string) {
	secretName := clusterName + "-cluster-config"
	realData := map[string][]byte{
		"cluster.xml": []byte("<config xmlns=\"http://tail-f.com/ns/config/1.0\"><test/></config>"),
	}

	var cluster v1alpha1.IdentityServerCluster
	Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, &cluster)
	}, 10*time.Second, 250*time.Millisecond).Should(Succeed())

	var nodeList v1alpha1.IdentityServerNodeList
	Eventually(func() string {
		_ = k8sClient.List(ctx, &nodeList, client.InNamespace(ns))
		for i := range nodeList.Items {
			n := &nodeList.Items[i]
			if n.Spec.IdentityServerClusterRef.Name == clusterName && n.Spec.Type == v1alpha1.NodeTypeAdmin {
				return n.Name
			}
		}
		return ""
	}, 10*time.Second, 250*time.Millisecond).ShouldNot(BeEmpty())
	var adminNodeName string
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if n.Spec.IdentityServerClusterRef.Name == clusterName && n.Spec.Type == v1alpha1.NodeTypeAdmin {
			adminNodeName = n.Name
			break
		}
	}

	// Mirror the operator's in-memory adminCredentials defaulting.
	credName := clusterName + "-admin-creds"
	if cluster.Spec.AdminCredentials != nil {
		credName = cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
	}
	encryptionKeyHash := ""
	Eventually(func() bool {
		var credSecret corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: credName, Namespace: ns}, &credSecret); err != nil {
			return false
		}
		key, ok := credSecret.Data["CONFIG_ENCRYPTION_KEY"]
		if !ok || len(key) == 0 {
			return false
		}
		encryptionKeyHash = controller.EncryptionKeyHashForTest(key)
		return true
	}, 15*time.Second, 250*time.Millisecond).Should(BeTrue(), "credentials Secret %q must exist with CONFIG_ENCRYPTION_KEY", credName)
	configHash := controller.ComputeClusterConfigHashForTest(&cluster, adminNodeName)

	annotations := map[string]string{
		"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
		"curity.io/admin-node":               adminNodeName,
		"curity.io/cluster-config-hash":      configHash,
	}
	if encryptionKeyHash != "" {
		annotations["curity.io/encryption-key-hash"] = encryptionKeyHash
	}

	// Create or update the secret with real data.
	Eventually(func() error {
		var existing corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, &existing); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			// Secret doesn't exist yet — create it.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: secretName, Namespace: ns,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "curity-operator",
						"curity.io/cluster":            clusterName,
						"curity.io/component":          "cluster-config",
					},
					Annotations: annotations,
				},
				Data: realData,
			}
			return k8sClient.Create(ctx, secret)
		}
		// Secret exists (ISC created placeholder) — update with real data.
		existing.Data = realData
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for k, v := range annotations {
			existing.Annotations[k] = v
		}
		return k8sClient.Update(ctx, &existing)
	}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

	// Wait for the ISC reconciler to detect the ready secret and set the condition.
	Eventually(func() bool {
		cluster := &v1alpha1.IdentityServerCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, cluster); err != nil {
			return false
		}
		return hasCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady, metav1.ConditionTrue)
	}, 30*time.Second, 250*time.Millisecond).Should(BeTrue())
}

// testCreateManagedConfigMap creates a ConfigMap with the curity.io/managed=true label.
func testCreateManagedConfigMap(ns, name string, data map[string]string, annotations map[string]string) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"curity.io/managed": "true"},
		},
		Data: data,
	}
	if annotations != nil {
		cm.Annotations = annotations
	}
	Expect(k8sClient.Create(ctx, cm)).To(Succeed())
}

// testCreateManagedSecret creates a Secret with the curity.io/managed=true label.
func testCreateManagedSecret(ns, name string, data map[string][]byte, annotations map[string]string) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"curity.io/managed": "true"},
		},
		Data: data,
	}
	if annotations != nil {
		secret.Annotations = annotations
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
}

// hasVolumeName checks if a Deployment has a volume with the given name.
func hasVolumeName(deploy *appsv1.Deployment, name string) bool {
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// hasVolumeMount checks if the first container has a mount with the given name and path.
func hasVolumeMount(deploy *appsv1.Deployment, name, mountPath string) bool {
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return false
	}
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == name && m.MountPath == mountPath {
			return true
		}
	}
	return false
}

// countCfgVolumes returns the number of volumes with "cfg-" prefix.
func countCfgVolumes(deploy *appsv1.Deployment) int {
	count := 0
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if len(v.Name) >= 4 && v.Name[:4] == "cfg-" {
			count++
		}
	}
	return count
}

var _ = Describe("IdentityServerNode Reconciler", func() {
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

	Context("Happy path — Admin node", func() {
		It("should create Deployment and Service for admin node", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "admin-node", v1alpha1.NodeTypeAdmin, "cluster-1")
			testSimulateClusterConfigReady(ns, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "admin-node"), deploy)
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--admin"))

			svc := &corev1.Service{}
			eventuallyGetResource(ns, ownedName("cluster-1", "admin-node"), svc)

			// Verify node status is updated
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "admin-node", Namespace: ns}, node)
				return node.Status.DeploymentName
			}, timeout, interval).Should(Equal(ownedName("cluster-1", "admin-node")))

			Expect(node.Status.ServiceName).To(Equal(ownedName("cluster-1", "admin-node")))
		})
	})

	Context("Deployment creation gate — ClusterConfigReady", func() {
		It("should defer Deployment until ClusterConfigReady when admin exists", func() {
			testCreateCluster(ns, "gate-cluster")
			testCreateNode(ns, "gate-admin", v1alpha1.NodeTypeAdmin, "gate-cluster")
			testCreateNode(ns, "gate-runtime", v1alpha1.NodeTypeRuntime, "gate-cluster")

			// Both nodes should be gated — no Deployment yet
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "gate-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)
			}, timeout, interval).Should(Equal("WaitingForClusterConfig"))

			Expect(node.Status.ServiceName).To(Equal(ownedName("gate-cluster", "gate-admin")))

			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName("gate-cluster", "gate-admin"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: ownedName("gate-cluster", "gate-runtime"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())

			// Services must exist while Deployment is gated — the genclust Job
			// needs DNS resolution of the admin hostname.
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-admin"), &corev1.Service{})
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-runtime"), &corev1.Service{})

			// Unblock by setting ClusterConfigReady=True
			testSimulateClusterConfigReady(ns, "gate-cluster")

			// Both Deployments should now appear
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-admin"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("gate-cluster", "gate-runtime"), &appsv1.Deployment{})
		})

		It("should create Deployment immediately for runtime-only cluster", func() {
			testCreateCluster(ns, "nonadmin-cluster")
			testCreateNode(ns, "nonadmin-runtime", v1alpha1.NodeTypeRuntime, "nonadmin-cluster")

			// No admin exists — Deployment should be created without waiting
			eventuallyGetResource(ns, ownedName("nonadmin-cluster", "nonadmin-runtime"), &appsv1.Deployment{})

			// Should NOT have WaitingForClusterConfig
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nonadmin-runtime", Namespace: ns}, node)).To(Succeed())
			Expect(conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)).NotTo(Equal("WaitingForClusterConfig"))
		})

		It("should not disrupt existing Deployment when admin is added to runtime-only cluster", func() {
			testCreateCluster(ns, "evolve-cluster")
			testCreateNode(ns, "evolve-runtime", v1alpha1.NodeTypeRuntime, "evolve-cluster")

			// Runtime Deployment should exist immediately (no admin)
			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("evolve-cluster", "evolve-runtime"), deploy)

			// Now add admin — ClusterConfigReady is NOT True
			testCreateNode(ns, "evolve-admin", v1alpha1.NodeTypeAdmin, "evolve-cluster")

			// Existing runtime Deployment should remain (gate bypassed for existing)
			Consistently(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("evolve-cluster", "evolve-runtime"), Namespace: ns}, deploy)
			}, 5*time.Second, 250*time.Millisecond).Should(Succeed())

			// Admin Deployment should be gated (no Deployment yet)
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "evolve-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionReady)
			}, timeout, interval).Should(Equal("WaitingForClusterConfig"))

			// Unblock — admin Deployment should appear
			testSimulateClusterConfigReady(ns, "evolve-cluster")
			eventuallyGetResource(ns, ownedName("evolve-cluster", "evolve-admin"), &appsv1.Deployment{})
		})

		It("should preserve cluster-config-hash during config regeneration", func() {
			testCreateCluster(ns, "hash-cluster")
			testCreateNode(ns, "hash-runtime", v1alpha1.NodeTypeRuntime, "hash-cluster")
			testCreateNode(ns, "hash-admin", v1alpha1.NodeTypeAdmin, "hash-cluster")
			testSimulateClusterConfigReady(ns, "hash-cluster")

			// Wait for Deployment and capture the hash annotation.
			deploy := &appsv1.Deployment{}
			var originalHash string
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hash-cluster", "hash-runtime"), Namespace: ns}, deploy)).To(Succeed())
				originalHash = deploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]
				g.Expect(originalHash).NotTo(BeEmpty())
			}, timeout, interval).Should(Succeed())

			// Simulate config regeneration: reset secret to placeholder, as
			// the cluster reconciler does during encryption key rotation.
			secretName := "hash-cluster-cluster-config"
			Eventually(func() error {
				var secret corev1.Secret
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, &secret); err != nil {
					return err
				}
				secret.Data = map[string][]byte{"cluster.xml": []byte("placeholder")}
				return k8sClient.Update(ctx, &secret)
			}, timeout, interval).Should(Succeed())

			// Wait for the cluster reconciler to detect the placeholder and
			// set ClusterConfigReady != True. Without this, the ISN reconciler
			// could race ahead, see stale True, and hash "placeholder".
			Eventually(func() bool {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hash-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return !hasCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady, metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Force a reconcile by touching the node spec.
			Eventually(func() error {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hash-runtime", Namespace: ns}, node); err != nil {
					return err
				}
				node.Spec.Replicas = ptr.To(int32(2))
				return k8sClient.Update(ctx, node)
			}, timeout, interval).Should(Succeed())

			// Confirm the ISN reconciler processed the update (replicas changed).
			Eventually(func(g Gomega) int32 {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hash-cluster", "hash-runtime"), Namespace: ns}, deploy)).To(Succeed())
				return *deploy.Spec.Replicas
			}, timeout, interval).Should(Equal(int32(2)))

			// The hash annotation should be preserved — not removed or changed.
			// Without the preservation logic, deploy.Spec = desiredDeploy.Spec
			// would strip the annotation and trigger an unnecessary rollout.
			Consistently(func(g Gomega) string {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hash-cluster", "hash-runtime"), Namespace: ns}, deploy)).To(Succeed())
				return deploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]
			}, 5*time.Second, interval).Should(Equal(originalHash))
		})
	})

	Context("Cluster label", func() {
		It("should set curity.io/cluster label on the node", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "labeled-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "labeled-node", Namespace: ns}, node)
				return node.Labels["curity.io/cluster"]
			}, timeout, interval).Should(Equal("cluster-1"))
		})

		It("should allow duplicate admin check to work via label-based filtering", func() {
			testCreateCluster(ns, "cluster-x")
			testCreateCluster(ns, "cluster-y")

			// Admin for cluster-x
			testCreateNode(ns, "admin-x", v1alpha1.NodeTypeAdmin, "cluster-x")
			testSimulateClusterConfigReady(ns, "cluster-x")
			eventuallyGetResource(ns, ownedName("cluster-x", "admin-x"), &appsv1.Deployment{})

			// Admin for cluster-y — different cluster, should NOT be blocked
			testCreateNode(ns, "admin-y", v1alpha1.NodeTypeAdmin, "cluster-y")
			testSimulateClusterConfigReady(ns, "cluster-y")
			eventuallyGetResource(ns, ownedName("cluster-y", "admin-y"), &appsv1.Deployment{})
		})
	})

	Context("Happy path — Runtime node", func() {
		It("should create Deployment with --no-admin args", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "runtime-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "runtime-node"), deploy)
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--no-admin"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("--admin"))
		})
	})

	Context("Error paths", func() {
		It("should set Degraded when cluster does not exist", func() {
			testCreateNode(ns, "orphan-node", v1alpha1.NodeTypeRuntime, "nonexistent-cluster")

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-node", Namespace: ns}, node); err != nil {
					return false
				}
				return hasCondition(node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue(), "expected Degraded condition to be set")
		})

		It("should flip ClusterReady=True and clear ClusterNotFound Degraded when the missing cluster is created", func() {
			// The reverse of the orphan test: once the referenced cluster
			// appears, the findNodesForCluster watch enqueues the node (the
			// node was labeled curity.io/cluster=<ref> on its first reconcile,
			// even while orphaned), and the reconciler must drop the
			// ClusterNotFound Degraded reason instead of leaving it stuck.
			testCreateNode(ns, "late-node", v1alpha1.NodeTypeRuntime, "late-cluster")

			By("waiting for orphan node to enter ClusterNotFound Degraded state")
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "late-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal(v1alpha1.ReasonClusterNotFound))

			By("creating the previously missing cluster")
			testCreateCluster(ns, "late-cluster")

			By("expecting ClusterReady to flip True with ReasonClusterFound")
			Eventually(func() bool {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "late-node", Namespace: ns}, node); err != nil {
					return false
				}
				return hasCondition(node.Status.Conditions, v1alpha1.ConditionClusterReady, metav1.ConditionTrue) &&
					conditionReason(node.Status.Conditions, v1alpha1.ConditionClusterReady) == v1alpha1.ReasonClusterFound
			}, timeout, interval).Should(BeTrue(), "expected ClusterReady=True/ClusterFound after cluster creation")

			By("expecting the stale ClusterNotFound Degraded reason to be gone")
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "late-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal(v1alpha1.ReasonClusterNotFound))

			By("expecting the Deployment to be created now that the cluster exists")
			eventuallyGetResource(ns, ownedName("late-cluster", "late-node"), &appsv1.Deployment{})
		})

		It("should block second admin node for same cluster", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "admin-1", v1alpha1.NodeTypeAdmin, "cluster-1")
			testSimulateClusterConfigReady(ns, "cluster-1")

			eventuallyGetResource(ns, ownedName("cluster-1", "admin-1"), &appsv1.Deployment{})

			// Create second admin
			testCreateNode(ns, "admin-2", v1alpha1.NodeTypeAdmin, "cluster-1")

			// Second admin should be Degraded with DuplicateAdmin reason
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "admin-2", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateAdmin"))

			// Verify no Deployment was created for admin-2
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-1", "admin-2"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		})

		It("should block node with duplicate role in same cluster", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "role-node-1", v1alpha1.NodeTypeRuntime, "cluster-1")

			eventuallyGetResource(ns, ownedName("cluster-1", "role-node-1"), &appsv1.Deployment{})

			// Create second node with explicit duplicate role
			dupNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "role-node-dup", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "role-node-1-role", // same role as role-node-1
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-1"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, dupNode)).To(Succeed())

			// Should get Degraded with DuplicateRole reason
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "role-node-dup", Namespace: ns}, dupNode); err != nil {
					return ""
				}
				return conditionReason(dupNode.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateRole"))

			Expect(hasCondition(dupNode.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)).To(BeTrue())

			// No Deployment for the duplicate
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-1", "role-node-dup"), Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		})

		It("should clear DuplicateRole on the originally-stuck node after the conflicting peer's role is changed", func() {
			// Both nodes end up Degraded once the duplicate exists (whichever
			// reconciles second sets itself Degraded; the cluster reconciler's
			// NodeCount bump then re-queues the first via the existing
			// clusterSpecOrNodeCountChangedPredicate, which also goes
			// Degraded). After the conflicting peer mutates its role, only
			// that peer auto-reconciles via its Generation bump; the
			// originally-stuck peer needs the validation-branch RequeueAfter
			// to recover, since a peer's spec mutation does not change
			// NodeCount.
			testCreateCluster(ns, "recovery-cluster")
			testCreateNode(ns, "stuck-node", v1alpha1.NodeTypeRuntime, "recovery-cluster")
			eventuallyGetResource(ns, ownedName("recovery-cluster", "stuck-node"), &appsv1.Deployment{})

			dupNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "peer-node", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "stuck-node-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "recovery-cluster"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, dupNode)).To(Succeed())

			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("DuplicateRole"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "peer-node", Namespace: ns}, dupNode)).To(Succeed())
			dupNode.Spec.Role = "stuck-node-role-w"
			Expect(k8sClient.Update(ctx, dupNode)).To(Succeed())

			// 60s allows for one full RequeueAfter cycle (30s) plus reconcile time.
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-node", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 60*time.Second, interval).ShouldNot(Equal("DuplicateRole"))
		})

		It("should clear DuplicateAdmin on the originally-stuck admin after the conflicting peer's type is changed", func() {
			testCreateCluster(ns, "admin-recovery-cluster")
			testCreateNode(ns, "stuck-admin", v1alpha1.NodeTypeAdmin, "admin-recovery-cluster")
			testSimulateClusterConfigReady(ns, "admin-recovery-cluster")
			eventuallyGetResource(ns, ownedName("admin-recovery-cluster", "stuck-admin"), &appsv1.Deployment{})

			testCreateNode(ns, "peer-admin", v1alpha1.NodeTypeAdmin, "admin-recovery-cluster")

			// 60s (not the 30s default) to ride out variable CI scheduling:
			// the stuck-admin's Degraded=DuplicateAdmin is set only AFTER
			// the cluster-watch fires from peer-admin's creation event
			// (incrementing Status.NodeCount), which can lag under heavy
			// envtest reconciler load. Matches the 60s timeout on the
			// recovery Eventually below.
			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 60*time.Second, interval).Should(Equal("DuplicateAdmin"))

			// Retry on conflict: the reconciler also writes the peer's
			// finalizer / labels concurrently with our spec change. Matches
			// the patchClusterRef helper added in clusterref_move_test.go.
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				peer := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "peer-admin", Namespace: ns}, peer); err != nil {
					return err
				}
				peer.Spec.Type = v1alpha1.NodeTypeRuntime
				peer.Spec.Role = "peer-admin-runtime-role"
				return k8sClient.Update(ctx, peer)
			})).To(Succeed())

			Eventually(func() string {
				node := &v1alpha1.IdentityServerNode{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "stuck-admin", Namespace: ns}, node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, 60*time.Second, interval).ShouldNot(Equal("DuplicateAdmin"))
		})

		It("should give two ambiguous-name nodes their own Deployments without collision", func() {
			// Regression guard for the cross-cluster ownedResourceName
			// ambiguity that the old code defended against with a runtime
			// collision check. With the SHA-256 suffix, ("foo-bar","baz")
			// and ("foo","bar-baz") produce distinct names — both nodes
			// run successfully, no Degraded condition, no events.
			testCreateCluster(ns, "foo-bar")
			testCreateCluster(ns, "foo")

			testCreateNode(ns, "baz", v1alpha1.NodeTypeRuntime, "foo-bar")
			testCreateNode(ns, "bar-baz", v1alpha1.NodeTypeRuntime, "foo")

			nameA := ownedName("foo-bar", "baz")
			nameB := ownedName("foo", "bar-baz")
			Expect(nameA).NotTo(Equal(nameB), "ambiguous pair must hash to distinct names")

			eventuallyGetResource(ns, nameA, &appsv1.Deployment{})
			eventuallyGetResource(ns, nameB, &appsv1.Deployment{})

			// Neither node ends up Degraded for collision. (DuplicateRole
			// is scoped per-cluster, so two nodes in different clusters
			// never collide on role regardless of the role string used.)
			nodeA := &v1alpha1.IdentityServerNode{}
			nodeB := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "baz", Namespace: ns}, nodeA)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "bar-baz", Namespace: ns}, nodeB)).To(Succeed())
			Expect(conditionReason(nodeA.Status.Conditions, v1alpha1.ConditionDegraded)).NotTo(Equal("OwnedResourceNameCollision"))
			Expect(conditionReason(nodeB.Status.Conditions, v1alpha1.ConditionDegraded)).NotTo(Equal("OwnedResourceNameCollision"))
		})

		It("should allow same role in different clusters", func() {
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")

			// Same role "shared-role" in two different clusters — should both work
			nodeA := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-a", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "shared-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-a"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())

			nodeB := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-b", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "shared-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-b"},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())

			// Both should get Deployments
			eventuallyGetResource(ns, ownedName("cluster-a", "node-a"), &appsv1.Deployment{})
			eventuallyGetResource(ns, ownedName("cluster-b", "node-b"), &appsv1.Deployment{})
		})
	})

	Context("Deletion", func() {
		It("should clean up via OwnerReference garbage collection", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "del-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "del-node"), deploy)

			Expect(deploy.OwnerReferences).To(HaveLen(1))
			Expect(deploy.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

			// Delete the node
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "del-node", Namespace: ns}, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			eventuallyDeleted(ns, "del-node", &v1alpha1.IdentityServerNode{})
		})
	})

	Context("Updates", func() {
		It("should update Deployment when replicas change", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "update-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-1", "update-node"), deploy)

			// Update replicas
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-node", Namespace: ns}, node)).To(Succeed())
			node.Spec.Replicas = ptr.To(int32(3))
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			// Verify Deployment replicas updated
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-1", "update-node"), Namespace: ns}, deploy)
				if deploy.Spec.Replicas == nil {
					return 0
				}
				return *deploy.Spec.Replicas
			}, timeout, interval).Should(Equal(int32(3)))
		})
	})

	Context("Config discovery", func() {
		It("should mount validated base ConfigMap on Deployment", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			// Create managed ConfigMap
			testCreateManagedConfigMap(ns, "my-config", map[string]string{"init.xml": "<config/>"}, nil)

			// Verify config volume appears on Deployment
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-my-config")).To(BeTrue(), "should have cfg-cm-my-config volume")
				g.Expect(hasVolumeMount(deploy, "cfg-cm-my-config", "/opt/idsvr/etc/init/cm_my-config_init.xml")).To(BeTrue(), "should mount at init path")
			}, timeout, interval).Should(Succeed())

			// Verify config-hash annotation on pod template
			Expect(deploy.Spec.Template.Annotations).To(HaveKey("curity.io/managed-configs-hash"))
		})

		It("should route configs only to admin when admin exists", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-admin", v1alpha1.NodeTypeAdmin, "cfg-cluster")
			testSimulateClusterConfigReady(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-runtime", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			adminDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-admin"), adminDeploy)
			runtimeDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-runtime"), runtimeDeploy)

			testCreateManagedConfigMap(ns, "admin-only-config", map[string]string{"settings.xml": "<settings/>"}, nil)

			// Admin should get the config
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-admin"), Namespace: ns}, adminDeploy)).To(Succeed())
				g.Expect(hasVolumeName(adminDeploy, "cfg-cm-admin-only-config")).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			// Runtime should NOT get the config
			Consistently(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-runtime"), Namespace: ns}, runtimeDeploy)
				return countCfgVolumes(runtimeDeploy)
			}, 5*time.Second, interval).Should(Equal(0), "runtime should not get config when admin exists")
		})

		It("should mount all configs on runtime when no admin exists", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-runtime", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-runtime"), deploy)

			testCreateManagedConfigMap(ns, "runtime-config", map[string]string{"app.xml": "<app/>"}, nil)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-runtime"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-runtime-config")).To(BeTrue())
				g.Expect(hasVolumeMount(deploy, "cfg-cm-runtime-config", "/opt/idsvr/etc/init/cm_runtime-config_app.xml")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should mount license Secret at license path", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			testCreateManagedSecret(ns, "my-license",
				map[string][]byte{"license.json": []byte(`{"key":"value"}`)},
				map[string]string{"curity.io/config-type": "license"})

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-secret-my-license")).To(BeTrue())
				g.Expect(hasVolumeMount(deploy, "cfg-secret-my-license", "/opt/idsvr/etc/init/license/secret_my-license_license.json")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should skip a CM with unknown config type but still mount its valid neighbor", func() {
			// A single typo must NOT cascade into everyone — the valid
			// neighbor must still get mounted. End-to-end asserts the
			// skip-the-offender behavior all the way through: discovery
			// skips the bad CM, cluster validates the good one, node
			// mounts the validated one.
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			// Bad neighbor: invalid config-type value.
			testCreateManagedConfigMap(ns, "bad-type-config",
				map[string]string{"data.xml": "<data/>"},
				map[string]string{"curity.io/config-type": "invalid"})

			// Good neighbor: valid config-type, will be mounted.
			testCreateManagedConfigMap(ns, "good-neighbor",
				map[string]string{"init.xml": "<config/>"},
				map[string]string{"curity.io/config-type": "base"})

			By("expecting the valid neighbor to mount while the bad one is skipped")
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(countCfgVolumes(deploy)).To(Equal(1),
					"only the valid CM should be mounted; a typo in one CM must not block its neighbor")
				g.Expect(hasVolumeName(deploy, "cfg-cm-good-neighbor")).To(BeTrue(),
					"good-neighbor must be mounted")
				g.Expect(hasVolumeName(deploy, "cfg-cm-bad-type-config")).To(BeFalse(),
					"bad-type-config must be skipped")
			}, timeout, interval).Should(Succeed())

			By("expecting a Warning Event on the offending ConfigMap (not on the node)")
			Eventually(func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "UnknownConfigType" &&
						e.InvolvedObject.Kind == "ConfigMap" &&
						e.InvolvedObject.Name == "bad-type-config" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(),
				"expected Warning/UnknownConfigType event on ConfigMap/bad-type-config")

			By("expecting NO UnknownConfigType event on the node (regression guard for the duplicate emit)")
			Consistently(func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctx, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "UnknownConfigType" && e.InvolvedObject.Kind == "IdentityServerNode" {
						return false
					}
				}
				return true
			}, "2s", "200ms").Should(BeTrue(),
				"node-side UnknownConfigType emit was removed; only the CM should carry it")
		})

		It("should update config-hash annotation when config data changes", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			testCreateManagedConfigMap(ns, "mutable-config", map[string]string{"data.xml": "<v1/>"}, nil)

			// Wait for initial hash
			var initialHash string
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)
				initialHash = deploy.Spec.Template.Annotations["curity.io/managed-configs-hash"]
				return initialHash
			}, timeout, interval).ShouldNot(BeEmpty())

			// Update ConfigMap data
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mutable-config", Namespace: ns}, cm)).To(Succeed())
			cm.Data["data.xml"] = "<v2/>"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Verify hash changed
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)
				return deploy.Spec.Template.Annotations["curity.io/managed-configs-hash"]
			}, timeout, interval).ShouldNot(Equal(initialHash))
		})

		It("should remove config volumes when label is removed", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			testCreateManagedConfigMap(ns, "removable-config", map[string]string{"init.xml": "<init/>"}, nil)

			// Wait for volume to appear
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-removable-config")).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			// Remove the managed label
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "removable-config", Namespace: ns}, cm)).To(Succeed())
			delete(cm.Labels, "curity.io/managed")
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Verify volume removed
			Eventually(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)
				return countCfgVolumes(deploy)
			}, timeout, interval).Should(Equal(0), "config volume should be removed after label removal")
		})

		It("should populate AppliedManagedResources with discovered managed configs", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), &appsv1.Deployment{})

			testCreateManagedConfigMap(ns, "status-config", map[string]string{"cfg.xml": "<cfg/>"}, nil)

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, node)).To(Succeed())
				g.Expect(node.Status.AppliedManagedResources).To(HaveLen(1))
				g.Expect(node.Status.AppliedManagedResources[0].Name).To(Equal("status-config"))
				g.Expect(node.Status.AppliedManagedResources[0].Kind).To(Equal("ConfigMap"))
				g.Expect(node.Status.AppliedManagedResources[0].ConfigType).To(Equal("base"))
			}, timeout, interval).Should(Succeed())
		})

		It("should mount multiple configs in deterministic order", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cfg-cluster", "cfg-node"), deploy)

			// Create configs with names that sort differently than creation order
			testCreateManagedConfigMap(ns, "zzz-config", map[string]string{"z.xml": "<z/>"}, nil)
			testCreateManagedConfigMap(ns, "aaa-config", map[string]string{"a.xml": "<a/>"}, nil)
			testCreateManagedSecret(ns, "mmm-secret",
				map[string][]byte{"m.xml": []byte("<m/>")}, nil)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cfg-cluster", "cfg-node"), Namespace: ns}, deploy)).To(Succeed())
				g.Expect(countCfgVolumes(deploy)).To(Equal(3))
				g.Expect(hasVolumeName(deploy, "cfg-cm-aaa-config")).To(BeTrue())
				g.Expect(hasVolumeName(deploy, "cfg-cm-zzz-config")).To(BeTrue())
				g.Expect(hasVolumeName(deploy, "cfg-secret-mmm-secret")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should re-scope a managed ConfigMap from cluster A to cluster B in one reconcile cycle", func() {
			// Feature's headline flow: editing curity.io/cluster from A to B
			// must make A lose the volume AND B gain it inside one Eventually.
			// The managedConfigHandler unions old+new scopes so both sides
			// get enqueued; this test verifies end-to-end convergence, not
			// just enqueue semantics.
			testCreateCluster(ns, "cluster-a")
			testCreateCluster(ns, "cluster-b")
			testCreateNode(ns, "node-a", v1alpha1.NodeTypeRuntime, "cluster-a")
			testCreateNode(ns, "node-b", v1alpha1.NodeTypeRuntime, "cluster-b")

			deployA := &appsv1.Deployment{}
			deployB := &appsv1.Deployment{}
			eventuallyGetResource(ns, ownedName("cluster-a", "node-a"), deployA)
			eventuallyGetResource(ns, ownedName("cluster-b", "node-b"), deployB)

			// Managed ConfigMap initially scoped to cluster-a only.
			testCreateManagedConfigMap(ns, "rescope-cm",
				map[string]string{"init.xml": "<config/>"},
				map[string]string{"curity.io/cluster": "cluster-a"},
			)

			By("asserting A has the volume and B does not (initial scope)")
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-a", "node-a"), Namespace: ns}, deployA)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-b", "node-b"), Namespace: ns}, deployB)).To(Succeed())
				g.Expect(hasVolumeName(deployA, "cfg-cm-rescope-cm")).To(BeTrue(), "A should mount the CM")
				g.Expect(countCfgVolumes(deployB)).To(Equal(0), "B should have no cfg volume yet")
			}, timeout, interval).Should(Succeed())

			By("flipping the ConfigMap's curity.io/cluster annotation from A to B")
			Eventually(func() error {
				var cm corev1.ConfigMap
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rescope-cm", Namespace: ns}, &cm); err != nil {
					return err
				}
				cm.Annotations["curity.io/cluster"] = "cluster-b"
				return k8sClient.Update(ctx, &cm)
			}, timeout, interval).Should(Succeed())

			By("asserting A loses the volume AND B gains it (both sides converge)")
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-a", "node-a"), Namespace: ns}, deployA)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("cluster-b", "node-b"), Namespace: ns}, deployB)).To(Succeed())
				g.Expect(countCfgVolumes(deployA)).To(Equal(0), "A must drop the CM volume after re-scope")
				g.Expect(hasVolumeName(deployB, "cfg-cm-rescope-cm")).To(BeTrue(), "B must mount the CM after re-scope")
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("PodDisruptionBudget", func() {
		const timeout = 30 * time.Second
		const interval = 250 * time.Millisecond

		var ns string

		BeforeEach(func() {
			ns = nodeTestNamespace()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		})

		It("creates PDB when cluster sets minAvailable and node does not override", func() {
			clusterName := "cluster-pdb-1"
			nodeName := "node-pdb-1"
			min := intstr.FromInt32(2)

			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:             "11.0",
					PodDisruptionBudget: &v1alpha1.PDBSpec{MinAvailable: &min},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			// Inheritance is the whole point of this test — if a future
			// testCreateNode helper starts defaulting PodDisruptionBudget,
			// the test would silently pass for the wrong reason.
			var freshNode v1alpha1.IdentityServerNode
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &freshNode)).To(Succeed())
			Expect(freshNode.Spec.PodDisruptionBudget).To(BeNil(), "node must not set PDB itself")

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(2))
			Expect(pdb.OwnerReferences).To(HaveLen(1))
			Expect(pdb.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
			Expect(pdb.OwnerReferences[0].Name).To(Equal(nodeName))
		})

		It("creates PDB with percentage minAvailable when set on node", func() {
			clusterName := "cluster-pdb-pct"
			nodeName := "node-pdb-pct"
			testCreateCluster(ns, clusterName)
			min := intstr.FromString("50%")
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(4)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
			Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
			Expect(pdb.Spec.MinAvailable.Type).To(Equal(intstr.String))
			Expect(pdb.Spec.MinAvailable.StrVal).To(Equal("50%"))
		})

		It("deletes PDB when field is removed from node spec", func() {
			clusterName := "cluster-pdb-del"
			nodeName := "node-pdb-del"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})

			// Clear the PDB spec.
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Spec.PodDisruptionBudget = nil
				return k8sClient.Update(ctx, &fresh)
			}, timeout, interval).Should(Succeed())

			eventuallyDeleted(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})
		})

		It("does not create PDB on admin node even when set", func() {
			clusterName := "cluster-pdb-admin"
			nodeName := "node-pdb-admin"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeAdmin,
					Role:                     "admin",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			// Wait a bit for reconcile, then confirm no PDB exists.
			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &policyv1.PodDisruptionBudget{}))
			}, 5*time.Second, 500*time.Millisecond).Should(BeTrue())
		})

		It("does not delete PDB that is not owned by the node", func() {
			clusterName := "cluster-pdb-notowned"
			nodeName := "node-pdb-notowned"
			testCreateCluster(ns, clusterName)

			// Pre-create a user PDB with the target name but no owner ref.
			min := intstr.FromInt32(7)
			userPDB := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, nodeName), Namespace: ns},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &min,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"user-pdb": "true"}},
				},
			}
			Expect(k8sClient.Create(ctx, userPDB)).To(Succeed())

			// Create a node with no PDB spec — cleanup path runs.
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			// The pre-existing PDB must still be there with its original values.
			Consistently(func() error {
				var pdb policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &pdb); err != nil {
					return err
				}
				if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 7 {
					return fmt.Errorf("user PDB was mutated")
				}
				return nil
			}, 5*time.Second, 500*time.Millisecond).Should(Succeed())

			// The node's Degraded condition should carry reason PDBNotOwned.
			Eventually(func() string {
				var node v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node); err != nil {
					return ""
				}
				return conditionReason(node.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("PDBNotOwned"))
		})

		It("emits PDBUnsatisfiable only when PDB status is fresh", func() {
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-unsat"
			nodeName := "node-pdb-unsat"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctxLocal, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)
			pdbGen := pdb.Generation

			hasUnsatisfiableEvent := func() bool {
				var events corev1.EventList
				if err := k8sClient.List(ctxLocal, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == "PDBUnsatisfiable" && e.InvolvedObject.Name == nodeName {
						return true
					}
				}
				return false
			}

			By("writing a STALE status with ObservedGeneration=0 and zero healthy counts")
			Eventually(func() error {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Status = policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 0, // intentionally behind fresh.Generation
					DisruptionsAllowed: 0,
					CurrentHealthy:     0,
					DesiredHealthy:     0,
				}
				return k8sClient.Status().Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("nudging the node to trigger a reconcile")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				if fresh.Annotations == nil {
					fresh.Annotations = map[string]string{}
				}
				fresh.Annotations["smoke.curity.io/nudge"] = "stale"
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying NO PDBUnsatisfiable event fires while status is stale")
			Consistently(hasUnsatisfiableEvent, 3*time.Second, 250*time.Millisecond).Should(BeFalse())

			By("writing a FRESH unsatisfiable status with ObservedGeneration matching the PDB's Generation")
			Eventually(func() error {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Status = policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: pdbGen,
					DisruptionsAllowed: 0,
					CurrentHealthy:     2,
					DesiredHealthy:     5, // exceeds CurrentHealthy — unsatisfiable
				}
				return k8sClient.Status().Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("nudging again to trigger reconcile on fresh status")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Annotations["smoke.curity.io/nudge"] = "fresh"
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying PDBUnsatisfiable event fires once status is fresh")
			Eventually(hasUnsatisfiableEvent, timeout, interval).Should(BeTrue())
		})

		It("clears Degraded=PDBNotOwned when the foreign PDB is deleted", func() {
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-clear"
			nodeName := "node-pdb-clear"
			testCreateCluster(ns, clusterName)

			By("pre-creating an unowned PDB with the target node's name")
			foreignMin := intstr.FromInt32(7)
			foreignPDB := &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: ownedName(clusterName, nodeName), Namespace: ns},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MinAvailable: &foreignMin,
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"user-pdb": "true"}},
				},
			}
			Expect(k8sClient.Create(ctxLocal, foreignPDB)).To(Succeed())

			By("creating the runtime node with no PDB spec — cleanup branch will see the collision")
			testCreateNode(ns, nodeName, v1alpha1.NodeTypeRuntime, clusterName)

			By("waiting for Degraded=True with reason PDBNotOwned")
			Eventually(func() string {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return ""
				}
				return conditionReason(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).Should(Equal("PDBNotOwned"))

			By("deleting the foreign PDB to resolve the collision")
			Expect(k8sClient.Delete(ctxLocal, foreignPDB)).To(Succeed())

			By("nudging the node to trigger a reconcile")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				if fresh.Annotations == nil {
					fresh.Annotations = map[string]string{}
				}
				fresh.Annotations["smoke.curity.io/nudge"] = "cleared"
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying Degraded's reason is no longer PDBNotOwned — computeNodeConditions takes over")
			Eventually(func() string {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return ""
				}
				return conditionReason(fresh.Status.Conditions, v1alpha1.ConditionDegraded)
			}, timeout, interval).ShouldNot(Equal("PDBNotOwned"))
		})

		It("deletes operator-owned PDB and suppresses PDBDeleted when node type flips runtime→admin", func() {
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-flip"
			nodeName := "node-pdb-flip"
			testCreateCluster(ns, clusterName)

			By("creating a runtime node with PDB so the operator creates a PDB it owns")
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(1)),
					Service:                  defaultTestService(),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
				},
			}
			Expect(k8sClient.Create(ctxLocal, node)).To(Succeed())
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})

			By("flipping the node type to admin")
			Eventually(func() error {
				var fresh v1alpha1.IdentityServerNode
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: nodeName, Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Spec.Type = v1alpha1.NodeTypeAdmin
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("verifying the operator-owned PDB is deleted")
			eventuallyDeleted(ns, ownedName(clusterName, nodeName), &policyv1.PodDisruptionBudget{})

			hasEvent := func(reason string) bool {
				var events corev1.EventList
				if err := k8sClient.List(ctxLocal, &events, client.InNamespace(ns)); err != nil {
					return false
				}
				for _, e := range events.Items {
					if e.Reason == reason && e.InvolvedObject.Name == nodeName {
						return true
					}
				}
				return false
			}

			By("verifying PDBIgnored event fires (admin guard)")
			Eventually(func() bool { return hasEvent("PDBIgnored") }, timeout, interval).Should(BeTrue())

			By("verifying PDBDeleted is suppressed on admin path — PDBIgnored already covers the signal")
			Consistently(func() bool { return hasEvent("PDBDeleted") }, 3*time.Second, 250*time.Millisecond).Should(BeFalse())
		})

		It("reverts unauthorized PDB spec mutations quickly (proves Owns watch is wired)", func() {
			// A PDB-only mutation wouldn't trigger reconcile from any of the
			// other Owns (Deployment/Service/HPA) or cluster watches, so a
			// sub-second revert proves Owns(&PodDisruptionBudget{}) in
			// SetupWithManager is firing. If Owns were removed, the mutated
			// spec would persist until some other unrelated watch event
			// (or the long default resync) triggered reconcile.
			ctxLocal := context.Background()
			clusterName := "cluster-pdb-owns"
			nodeName := "node-pdb-owns"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctxLocal, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)

			By("mutating the PDB's MinAvailable directly")
			bogus := intstr.FromInt32(99)
			Eventually(func() error {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return err
				}
				fresh.Spec.MinAvailable = &bogus
				return k8sClient.Update(ctxLocal, &fresh)
			}, timeout, interval).Should(Succeed())

			By("expecting fast revert to the desired value — this is the Owns watch firing")
			Eventually(func() int {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctxLocal, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil || fresh.Spec.MinAvailable == nil {
					return -1
				}
				return fresh.Spec.MinAvailable.IntValue()
			}, 2*time.Second, 100*time.Millisecond).Should(Equal(1))
		})

		It("re-creates PDB if deleted manually", func() {
			clusterName := "cluster-pdb-recreate"
			nodeName := "node-pdb-recreate"
			testCreateCluster(ns, clusterName)
			min := intstr.FromInt32(1)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     nodeName + "-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
					Replicas:                 ptr.To(int32(2)),
					PodDisruptionBudget:      &v1alpha1.PDBSpec{MinAvailable: &min},
					Service:                  defaultTestService(),
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			pdb := &policyv1.PodDisruptionBudget{}
			eventuallyGetResource(ns, ownedName(clusterName, nodeName), pdb)

			// Delete the PDB and watch it come back.
			originalUID := pdb.UID
			Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())
			Eventually(func() bool {
				var fresh policyv1.PodDisruptionBudget
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: ownedName(clusterName, nodeName), Namespace: ns}, &fresh); err != nil {
					return false
				}
				return fresh.UID != originalUID
			}, timeout, interval).Should(BeTrue())
		})
	})
})

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

var _ = Describe("Deployment scheduling", func() {
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

	It("should inherit scheduling constraints from cluster on Deployment", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "sched-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:      "11.0",
				NodeSelector: map[string]string{"disk": "ssd", "zone": "us-west"},
				Tolerations: []corev1.Toleration{
					{Key: "special", Operator: corev1.TolerationOpExists},
				},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      "kubernetes.io/arch",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"amd64"},
								}},
							}},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "sched-node", v1alpha1.NodeTypeRuntime, "sched-cluster")

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("sched-cluster", "sched-node"), deploy)
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("disk", "ssd"))
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("zone", "us-west"))
		Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Affinity).NotTo(BeNil())
		Expect(deploy.Spec.Template.Spec.Affinity.NodeAffinity).NotTo(BeNil())
	})

	It("should apply node-level scheduling over cluster defaults", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "override-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:      "11.0",
				NodeSelector: map[string]string{"pool": "default"},
				Tolerations: []corev1.Toleration{
					{Key: "old-taint", Operator: corev1.TolerationOpExists},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "override-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "override-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "override-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				NodeSelector:             map[string]string{"pool": "gpu"},
				Tolerations: []corev1.Toleration{
					{Key: "new-taint", Operator: corev1.TolerationOpEqual, Value: "yes"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("override-cluster", "override-node"), deploy)

		// nodeSelector merges: node wins on conflict
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("pool", "gpu"))
		// tolerations: node fully replaces cluster
		Expect(deploy.Spec.Template.Spec.Tolerations).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Tolerations[0].Key).To(Equal("new-taint"))
	})

	It("should apply topology spread constraints from cluster to Deployment", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "topo-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
					MaxSkew:           1,
					TopologyKey:       "topology.kubernetes.io/zone",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "curity"},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "topo-node", v1alpha1.NodeTypeRuntime, "topo-cluster")

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("topo-cluster", "topo-node"), deploy)
		Expect(deploy.Spec.Template.Spec.TopologySpreadConstraints).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.TopologySpreadConstraints[0].TopologyKey).To(Equal("topology.kubernetes.io/zone"))
	})

	It("should update Deployment when cluster scheduling changes", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "update-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:      "11.0",
				NodeSelector: map[string]string{"env": "staging"},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		testCreateNode(ns, "update-node", v1alpha1.NodeTypeRuntime, "update-cluster")

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("update-cluster", "update-node"), deploy)
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("env", "staging"))

		// Update cluster nodeSelector
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.NodeSelector = map[string]string{"env": "production"}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		// Deployment should eventually reflect the change
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("update-cluster", "update-node"), Namespace: ns}, deploy)
			return deploy.Spec.Template.Spec.NodeSelector["env"]
		}, timeout, interval).Should(Equal("production"))
	})

	// =================================================================
	// HPA lifecycle
	// =================================================================

	It("should create HPA for runtime node with autoscaling enabled", func() {
		testCreateCluster(ns, "hpa-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "hpa-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    2,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		eventuallyGetResource(ns, ownedName("hpa-cluster", "hpa-node"), hpa)

		Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(ownedName("hpa-cluster", "hpa-node")))
		Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
		Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))
		Expect(hpa.OwnerReferences).To(HaveLen(1))
		Expect(hpa.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))
	})

	It("should set Deployment replicas to minReplicas when HPA enabled", func() {
		testCreateCluster(ns, "hpa-rep-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-rep-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "hpa-rep-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-rep-cluster"},
				Replicas:                 ptr.To(int32(5)),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    3,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
				Service: defaultTestService(),
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, ownedName("hpa-rep-cluster", "hpa-rep-node"), deploy)

		Eventually(func(g Gomega) int32 {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hpa-rep-cluster", "hpa-rep-node"), Namespace: ns}, deploy)).To(Succeed())
			if deploy.Spec.Replicas == nil {
				return 0
			}
			return *deploy.Spec.Replicas
		}, 30*time.Second, 250*time.Millisecond).Should(Equal(int32(3)))
	})

	// Note: "admin + autoscaling.enabled=true" is now rejected at admission
	// by a CEL rule on IdentityServerNodeSpec (see CRD validation tests for
	// the rejection assertion). The runtime "ignored on admin" path is no
	// longer reachable via the API; the admission-rejection test supersedes
	// what this Describe used to cover.

	It("should delete HPA when autoscaling is disabled", func() {
		testCreateCluster(ns, "hpa-del-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-del-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "del-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-del-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    2,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		// Wait for HPA to be created
		eventuallyGetResource(ns, ownedName("hpa-del-cluster", "hpa-del-node"), &autoscalingv2.HorizontalPodAutoscaler{})

		// Disable autoscaling
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hpa-del-node", Namespace: ns}, node); err != nil {
				return err
			}
			node.Spec.Autoscaling.Enabled = false
			return k8sClient.Update(ctx, node)
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		// HPA should be deleted
		eventuallyDeleted(ns, ownedName("hpa-del-cluster", "hpa-del-node"), &autoscalingv2.HorizontalPodAutoscaler{})
	})

	It("should clean up stale HPA when node type changes to admin", func() {
		testCreateCluster(ns, "hpa-stale-cluster")
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-stale", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeRuntime,
				Role:                     "stale-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "hpa-stale-cluster"},
				Replicas:                 ptr.To(int32(1)),
				Service:                  defaultTestService(),
				Autoscaling: &v1alpha1.AutoscalingSpec{
					Enabled:                        true,
					MinReplicas:                    2,
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: 80,
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		// Wait for HPA to be created
		eventuallyGetResource(ns, ownedName("hpa-stale-cluster", "hpa-stale"), &autoscalingv2.HorizontalPodAutoscaler{})

		// Change node type to admin. CEL admission rejects admin+autoscaling.enabled=true,
		// so the type-change update must also disable autoscaling in the same patch.
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hpa-stale", Namespace: ns}, node); err != nil {
				return err
			}
			node.Spec.Type = v1alpha1.NodeTypeAdmin
			node.Spec.Autoscaling.Enabled = false
			return k8sClient.Update(ctx, node)
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		// HPA should be cleaned up even though node is now admin
		eventuallyDeleted(ns, ownedName("hpa-stale-cluster", "hpa-stale"), &autoscalingv2.HorizontalPodAutoscaler{})

		// Deployment replicas should be forced to 1
		deploy := &appsv1.Deployment{}
		Eventually(func(g Gomega) int32 {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownedName("hpa-stale-cluster", "hpa-stale"), Namespace: ns}, deploy)).To(Succeed())
			if deploy.Spec.Replicas == nil {
				return 0
			}
			return *deploy.Spec.Replicas
		}, 30*time.Second, 250*time.Millisecond).Should(Equal(int32(1)))
	})
})
