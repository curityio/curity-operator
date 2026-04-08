package controller_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
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

func testCreateNode(ns, name string, nodeType v1alpha1.NodeType, clusterName string) {
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.IdentityServerNodeSpec{
			Type:                     nodeType,
			Role:                     name + "-role",
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
			Replicas:                 ptr.To(int32(1)),
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

// configHashEntry mirrors the DiscoveredConfigResource shape for hash computation.
type configHashEntry struct {
	Name       string
	IsSecret   bool
	ConfigType string
	Data       map[string][]byte
}

// testComputeConfigHash mirrors computeConfigHash() from config_discovery.go.
// See config_discovery.go:198 for the canonical implementation.
func testComputeConfigHash(entries []configHashEntry) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := make([]configHashEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	for _, e := range sorted {
		h.Write([]byte(e.Name))
		h.Write([]byte{0})
		if e.IsSecret {
			h.Write([]byte("Secret"))
		} else {
			h.Write([]byte("ConfigMap"))
		}
		h.Write([]byte{0})
		h.Write([]byte(e.ConfigType))
		h.Write([]byte{0})
		keys := make([]string, 0, len(e.Data))
		for k := range e.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write(e.Data[k])
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// testSimulateValidation sets the validated config hash annotation on the cluster,
// triggering node re-reconciliation via the clusterSpecOrNodeCountChangedPredicate.
func testSimulateValidation(ns, clusterName, configHash string) {
	Eventually(func() error {
		cluster := &v1alpha1.IdentityServerCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, cluster); err != nil {
			return err
		}
		if cluster.Annotations == nil {
			cluster.Annotations = make(map[string]string)
		}
		cluster.Annotations["curity.io/validated-config-hash"] = configHash
		return k8sClient.Update(ctx, cluster)
	}, 30*time.Second, 250*time.Millisecond).Should(Succeed())
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

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "admin-node", deploy)
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--admin"))

			svc := &corev1.Service{}
			eventuallyGetResource(ns, "admin-node", svc)

			// Verify node status is updated
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "admin-node", Namespace: ns}, node)
				return node.Status.DeploymentName
			}, timeout, interval).Should(Equal("admin-node"))

			Expect(node.Status.ServiceName).To(Equal("admin-node"))
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
			eventuallyGetResource(ns, "admin-x", &appsv1.Deployment{})

			// Admin for cluster-y — different cluster, should NOT be blocked
			testCreateNode(ns, "admin-y", v1alpha1.NodeTypeAdmin, "cluster-y")
			eventuallyGetResource(ns, "admin-y", &appsv1.Deployment{})
		})
	})

	Context("Happy path — Runtime node", func() {
		It("should create Deployment with --no-admin args", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "runtime-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "runtime-node", deploy)
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

		It("should block second admin node for same cluster", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "admin-1", v1alpha1.NodeTypeAdmin, "cluster-1")

			eventuallyGetResource(ns, "admin-1", &appsv1.Deployment{})

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
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "admin-2", Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
		})

		It("should block node with duplicate role in same cluster", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "role-node-1", v1alpha1.NodeTypeRuntime, "cluster-1")

			eventuallyGetResource(ns, "role-node-1", &appsv1.Deployment{})

			// Create second node with explicit duplicate role
			dupNode := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "role-node-dup", Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "role-node-1-role", // same role as role-node-1
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-1"},
					Replicas:                 ptr.To(int32(1)),
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
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "role-node-dup", Namespace: ns}, &appsv1.Deployment{}))).To(BeTrue())
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
				},
			}
			Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())

			// Both should get Deployments
			eventuallyGetResource(ns, "node-a", &appsv1.Deployment{})
			eventuallyGetResource(ns, "node-b", &appsv1.Deployment{})
		})
	})

	Context("Deletion", func() {
		It("should clean up via OwnerReference garbage collection", func() {
			testCreateCluster(ns, "cluster-1")
			testCreateNode(ns, "del-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "del-node", deploy)

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
			eventuallyGetResource(ns, "update-node", deploy)

			// Update replicas
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-node", Namespace: ns}, node)).To(Succeed())
			node.Spec.Replicas = ptr.To(int32(3))
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			// Verify Deployment replicas updated
			Eventually(func() int32 {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "update-node", Namespace: ns}, deploy)
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
			eventuallyGetResource(ns, "cfg-node", deploy)

			// Create managed ConfigMap
			testCreateManagedConfigMap(ns, "my-config", map[string]string{"init.xml": "<config/>"}, nil)

			// Simulate validation
			hash := testComputeConfigHash([]configHashEntry{{
				Name: "my-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"init.xml": []byte("<config/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash)

			// Verify config volume appears on Deployment
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-my-config")).To(BeTrue(), "should have cfg-cm-my-config volume")
				g.Expect(hasVolumeMount(deploy, "cfg-cm-my-config", "/opt/idsvr/etc/init/init.xml")).To(BeTrue(), "should mount at init path")
			}, timeout, interval).Should(Succeed())

			// Verify config-hash annotation on pod template
			Expect(deploy.Spec.Template.Annotations).To(HaveKey("curity.io/config-hash"))
		})

		It("should NOT mount configs when validation hash is missing", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-node", deploy)

			// Create managed ConfigMap but do NOT simulate validation
			testCreateManagedConfigMap(ns, "unvalidated-config", map[string]string{"data.xml": "<data/>"}, nil)

			// Wait for reconcile to pick up the ConfigMap (watch triggers it)
			// The Deployment should NOT gain config volumes
			Consistently(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)
				return countCfgVolumes(deploy)
			}, 5*time.Second, interval).Should(Equal(0), "no cfg volumes when validation hash is missing")

			// Verify node status shows Pending
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, node); err != nil {
					return ""
				}
				if len(node.Status.AppliedConfigs) == 0 {
					return ""
				}
				return node.Status.AppliedConfigs[0].ValidationStatus
			}, timeout, interval).Should(Equal("Pending"))
		})

		It("should route configs only to admin when admin exists", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-admin", v1alpha1.NodeTypeAdmin, "cfg-cluster")
			testCreateNode(ns, "cfg-runtime", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			adminDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-admin", adminDeploy)
			runtimeDeploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-runtime", runtimeDeploy)

			testCreateManagedConfigMap(ns, "admin-only-config", map[string]string{"settings.xml": "<settings/>"}, nil)

			hash := testComputeConfigHash([]configHashEntry{{
				Name: "admin-only-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"settings.xml": []byte("<settings/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash)

			// Admin should get the config
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-admin", Namespace: ns}, adminDeploy)).To(Succeed())
				g.Expect(hasVolumeName(adminDeploy, "cfg-cm-admin-only-config")).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			// Runtime should NOT get the config
			Consistently(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-runtime", Namespace: ns}, runtimeDeploy)
				return countCfgVolumes(runtimeDeploy)
			}, 5*time.Second, interval).Should(Equal(0), "runtime should not get config when admin exists")
		})

		It("should mount all configs on runtime when no admin exists", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-runtime", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-runtime", deploy)

			testCreateManagedConfigMap(ns, "runtime-config", map[string]string{"app.xml": "<app/>"}, nil)

			hash := testComputeConfigHash([]configHashEntry{{
				Name: "runtime-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"app.xml": []byte("<app/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-runtime", Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-runtime-config")).To(BeTrue())
				g.Expect(hasVolumeMount(deploy, "cfg-cm-runtime-config", "/opt/idsvr/etc/init/app.xml")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should mount license Secret at license path", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-node", deploy)

			testCreateManagedSecret(ns, "my-license",
				map[string][]byte{"license.json": []byte(`{"key":"value"}`)},
				map[string]string{"curity.io/config-type": "license"})

			hash := testComputeConfigHash([]configHashEntry{{
				Name: "my-license", IsSecret: true, ConfigType: "license",
				Data: map[string][]byte{"license.json": []byte(`{"key":"value"}`)},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-secret-my-license")).To(BeTrue())
				g.Expect(hasVolumeMount(deploy, "cfg-secret-my-license", "/opt/idsvr/etc/init/license/license.json")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("should handle unknown config type gracefully", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-node", deploy)

			// Create ConfigMap with invalid config type
			testCreateManagedConfigMap(ns, "bad-type-config",
				map[string]string{"data.xml": "<data/>"},
				map[string]string{"curity.io/config-type": "invalid"})

			// Deployment should NOT gain any config volumes
			Consistently(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)
				return countCfgVolumes(deploy)
			}, 5*time.Second, interval).Should(Equal(0), "no cfg volumes with unknown config type")
		})

		It("should update config-hash annotation when config data changes", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-node", deploy)

			testCreateManagedConfigMap(ns, "mutable-config", map[string]string{"data.xml": "<v1/>"}, nil)

			hash1 := testComputeConfigHash([]configHashEntry{{
				Name: "mutable-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"data.xml": []byte("<v1/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash1)

			// Wait for initial hash
			var initialHash string
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)
				initialHash = deploy.Spec.Template.Annotations["curity.io/config-hash"]
				return initialHash
			}, timeout, interval).ShouldNot(BeEmpty())

			// Update ConfigMap data
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mutable-config", Namespace: ns}, cm)).To(Succeed())
			cm.Data["data.xml"] = "<v2/>"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			hash2 := testComputeConfigHash([]configHashEntry{{
				Name: "mutable-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"data.xml": []byte("<v2/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash2)

			// Verify hash changed
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)
				return deploy.Spec.Template.Annotations["curity.io/config-hash"]
			}, timeout, interval).ShouldNot(Equal(initialHash))
		})

		It("should remove config volumes when label is removed", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-node", deploy)

			testCreateManagedConfigMap(ns, "removable-config", map[string]string{"init.xml": "<init/>"}, nil)

			hash := testComputeConfigHash([]configHashEntry{{
				Name: "removable-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"init.xml": []byte("<init/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash)

			// Wait for volume to appear
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)).To(Succeed())
				g.Expect(hasVolumeName(deploy, "cfg-cm-removable-config")).To(BeTrue())
			}, timeout, interval).Should(Succeed())

			// Remove the managed label
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "removable-config", Namespace: ns}, cm)).To(Succeed())
			delete(cm.Labels, "curity.io/managed")
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Clear validated hash (no managed configs remain)
			Eventually(func() error {
				cluster := &v1alpha1.IdentityServerCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-cluster", Namespace: ns}, cluster); err != nil {
					return err
				}
				delete(cluster.Annotations, "curity.io/validated-config-hash")
				return k8sClient.Update(ctx, cluster)
			}, timeout, interval).Should(Succeed())

			// Verify volume removed
			Eventually(func() int {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)
				return countCfgVolumes(deploy)
			}, timeout, interval).Should(Equal(0), "config volume should be removed after label removal")
		})

		It("should set AppliedConfigs status with correct validation status", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			eventuallyGetResource(ns, "cfg-node", &appsv1.Deployment{})

			testCreateManagedConfigMap(ns, "status-config", map[string]string{"cfg.xml": "<cfg/>"}, nil)

			// Wait for Pending status
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, node); err != nil {
					return ""
				}
				if len(node.Status.AppliedConfigs) == 0 {
					return ""
				}
				return node.Status.AppliedConfigs[0].ValidationStatus
			}, timeout, interval).Should(Equal("Pending"))

			Expect(node.Status.AppliedConfigs[0].Name).To(Equal("status-config"))
			Expect(node.Status.AppliedConfigs[0].Kind).To(Equal("ConfigMap"))
			Expect(node.Status.AppliedConfigs[0].ConfigType).To(Equal("base"))

			// Simulate validation
			hash := testComputeConfigHash([]configHashEntry{{
				Name: "status-config", IsSecret: false, ConfigType: "base",
				Data: map[string][]byte{"cfg.xml": []byte("<cfg/>")},
			}})
			testSimulateValidation(ns, "cfg-cluster", hash)

			// Wait for Validated status
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, node); err != nil {
					return ""
				}
				if len(node.Status.AppliedConfigs) == 0 {
					return ""
				}
				return node.Status.AppliedConfigs[0].ValidationStatus
			}, timeout, interval).Should(Equal("Validated"))
		})

		It("should mount multiple configs in deterministic order", func() {
			testCreateCluster(ns, "cfg-cluster")
			testCreateNode(ns, "cfg-node", v1alpha1.NodeTypeRuntime, "cfg-cluster")

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "cfg-node", deploy)

			// Create configs with names that sort differently than creation order
			testCreateManagedConfigMap(ns, "zzz-config", map[string]string{"z.xml": "<z/>"}, nil)
			testCreateManagedConfigMap(ns, "aaa-config", map[string]string{"a.xml": "<a/>"}, nil)
			testCreateManagedSecret(ns, "mmm-secret",
				map[string][]byte{"m.xml": []byte("<m/>")}, nil)

			hash := testComputeConfigHash([]configHashEntry{
				{Name: "zzz-config", IsSecret: false, ConfigType: "base", Data: map[string][]byte{"z.xml": []byte("<z/>")}},
				{Name: "aaa-config", IsSecret: false, ConfigType: "base", Data: map[string][]byte{"a.xml": []byte("<a/>")}},
				{Name: "mmm-secret", IsSecret: true, ConfigType: "base", Data: map[string][]byte{"m.xml": []byte("<m/>")}},
			})
			testSimulateValidation(ns, "cfg-cluster", hash)

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg-node", Namespace: ns}, deploy)).To(Succeed())
				g.Expect(countCfgVolumes(deploy)).To(Equal(3))
				g.Expect(hasVolumeName(deploy, "cfg-cm-aaa-config")).To(BeTrue())
				g.Expect(hasVolumeName(deploy, "cfg-cm-zzz-config")).To(BeTrue())
				g.Expect(hasVolumeName(deploy, "cfg-secret-mmm-secret")).To(BeTrue())
			}, timeout, interval).Should(Succeed())
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
					UI:                       &v1alpha1.UISpec{Enabled: true},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			deploy := &appsv1.Deployment{}
			eventuallyGetResource(ns, "admin-default-env", deploy)

			envVars := deploy.Spec.Template.Spec.Containers[0].Env
			Expect(hasEnvFromSecret(envVars, "PASSWORD", "cluster-default-env-admin-creds", "ADMIN_PASSWORD")).To(BeTrue())
			Expect(hasEnvFromSecret(envVars, "CONFIG_ENCRYPTION_KEY", "cluster-default-env-admin-creds", "CONFIG_ENCRYPTION_KEY")).To(BeTrue())
			Expect(hasEnvFromSecret(envVars, "KEYSTORE_PASSWORD", "cluster-default-env-admin-creds", "KEYSTORE_PASSWORD")).To(BeTrue())
		})
	})

	Context("CRD validation", func() {
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
			Expect(err).To(HaveOccurred(), "empty Items should be rejected by MinItems=1 validation")
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

			// Verify operator deletes failed Job and creates a new one
			// (the new Job will have a different UID)
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

			// Pre-populate Secret with XML containing the admin hostname
			secret.Data = map[string][]byte{"cluster.xml": []byte(
				"<config><cluster><host>rename-admin</host><port>6789</port></cluster></config>")}
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
			Expect(string(updatedSecret.Data["cluster.xml"])).To(ContainSubstring("<host>rename-admin-v2</host>"))
			Expect(string(updatedSecret.Data["cluster.xml"])).NotTo(ContainSubstring("<host>rename-admin</host>"))
			// Rest of XML preserved
			Expect(string(updatedSecret.Data["cluster.xml"])).To(ContainSubstring("<port>6789</port>"))
		})

		It("should preserve XML when admin rename host tag is missing (Scenario 5 edge)", func() {
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

			// Pre-populate with XML that has no <host> tag (user-edited or malformed)
			originalXML := "<config><cluster><keystore>abc</keystore></cluster></config>"
			secret.Data = map[string][]byte{"cluster.xml": []byte(originalXML)}
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Rename admin
			oldNode := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-admin", Namespace: ns}, oldNode)).To(Succeed())
			Expect(k8sClient.Delete(ctx, oldNode)).To(Succeed())
			eventuallyDeleted(ns, "rename-nohost-admin", &v1alpha1.IdentityServerNode{})
			testCreateNode(ns, "rename-nohost-admin-v2", v1alpha1.NodeTypeAdmin, "rename-nohost-cluster")

			// Annotation updates, XML is unchanged (strings.Replace is a no-op)
			Eventually(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return s.Annotations["curity.io/admin-node"]
			}, timeout, interval).Should(Equal("rename-nohost-admin-v2"))

			updatedSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rename-nohost-cluster-cluster-config", Namespace: ns}, updatedSecret)).To(Succeed())
			Expect(string(updatedSecret.Data["cluster.xml"])).To(Equal(originalXML))
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
								Name:  "enckey-creds",
								Items: []v1alpha1.KeyToPath{{Key: "ADMIN_PASSWORD", Path: "PASSWORD"}},
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
								Name:  "firstkey-creds",
								Items: []v1alpha1.KeyToPath{{Key: "ADMIN_PASSWORD", Path: "PASSWORD"}},
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

			// Mark Job as Complete (but no pod exists with Succeeded phase → logs unavailable)
			job.Status.Conditions = []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Operator should detect logs unavailable, delete Job, and create a new one
			Eventually(func() bool {
				newJob := &batchv1.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "logs-cluster-cluster-config-job", Namespace: ns}, newJob); err != nil {
					return false
				}
				return newJob.UID != origUID
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("Cluster config workflows", func() {
		It("should complete full lifecycle: create → populate → version upgrade → reuse", func() {
			testCreateCluster(ns, "lifecycle-cluster")
			testCreateNode(ns, "lifecycle-admin", v1alpha1.NodeTypeAdmin, "lifecycle-cluster")

			configSecret := &corev1.Secret{}
			eventuallyGetResource(ns, "lifecycle-cluster-cluster-config", configSecret)

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "lifecycle-cluster-cluster-config-job", job)

			// Step 2: Simulate Job completion — pre-populate Secret with real data
			configSecret.Data = map[string][]byte{"cluster.xml": []byte(
				"<config><cluster><host>lifecycle-admin</host><port>6789</port></cluster></config>")}
			Expect(k8sClient.Update(ctx, configSecret)).To(Succeed())

			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster", Namespace: ns}, cluster); err != nil {
					return false
				}
				return hasCondition(cluster.Status.Conditions, "ClusterConfigReady", metav1.ConditionTrue)
			}, timeout, interval).Should(BeTrue())

			// Step 4: Version upgrade — cluster.xml should be reused
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster", Namespace: ns}, cluster)).To(Succeed())
			cluster.Spec.Version = "12.0"
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			// Verify Secret data is unchanged after version upgrade
			Consistently(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-cluster-cluster-config", Namespace: ns}, s); err != nil {
					return ""
				}
				return string(s.Data["cluster.xml"])
			}, 3*time.Second, interval).Should(ContainSubstring("<host>lifecycle-admin</host>"))
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
				"<config><cluster><keystore>abc123</keystore><host>wf-rename-admin</host><port>6789</port></cluster></config>")}
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
				ContainSubstring("<host>wf-rename-admin-v2</host>"),
				ContainSubstring("<keystore>abc123</keystore>"),
				Not(ContainSubstring("<host>wf-rename-admin</host>")),
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
								Name:  "wf-enckey-creds",
								Items: []v1alpha1.KeyToPath{{Key: "ADMIN_PASSWORD", Path: "PASSWORD"}},
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

			secret.Data = map[string][]byte{"cluster.xml": []byte("<config><host>wf-delsecret-admin</host></config>")}
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

			secret.Data = map[string][]byte{"cluster.xml": []byte("<config><host>wf-delcr-admin</host></config>")}
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
			Expect(string(survivedSecret.Data["cluster.xml"])).To(Equal("<config><host>wf-delcr-admin</host></config>"))
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
				Data: map[string][]byte{"cluster.xml": []byte("<config><host>wf-useredit-admin</host></config>")},
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

	Context("Config validation", func() {
		It("should create validation Job when managed config exists", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "val-config", map[string]string{"init.xml": "<init/>"}, nil)

			// Cluster reconciler should create a validation Job
			job := &batchv1.Job{}
			eventuallyGetResource(ns, "val-cluster-config-validation", job)
			Expect(job.Annotations).To(HaveKey("curity.io/config-hash"))
			Expect(job.Labels["curity.io/component"]).To(Equal("config-validation"))

			// Cluster condition should show pending
			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "val-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				return conditionReason(cluster.Status.Conditions, "ConfigValidationReady")
			}, timeout, interval).Should(Equal("ValidationPending"))
		})

		It("should set validated hash after Job succeeds", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "val-config", map[string]string{"data.xml": "<data/>"}, nil)

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "val-cluster-config-validation", job)
			expectedHash := job.Annotations["curity.io/config-hash"]

			// Mark Job as Complete
			job.Status.Conditions = []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			}}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Cluster should get validated hash annotation
			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "val-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				return cluster.Annotations["curity.io/validated-config-hash"]
			}, timeout, interval).Should(Equal(expectedHash))

			// Condition should be True
			Expect(conditionReason(cluster.Status.Conditions, "ConfigValidationReady")).To(Equal("Validated"))
		})

		It("should handle validation Job failure", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "val-config", map[string]string{"bad.xml": "<bad/>"}, nil)

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "val-cluster-config-validation", job)

			// Mark Job as Failed
			job.Status.Conditions = []batchv1.JobCondition{{
				Type:    batchv1.JobFailed,
				Status:  corev1.ConditionTrue,
				Message: "config invalid",
			}}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "val-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				return conditionReason(cluster.Status.Conditions, "ConfigValidationReady")
			}, timeout, interval).Should(Equal("ValidationFailed"))

			// No validated hash should be set
			Expect(cluster.Annotations).NotTo(HaveKey("curity.io/validated-config-hash"))
		})

		It("should delete stale Job when config hash changes", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "val-config", map[string]string{"v1.xml": "<v1/>"}, nil)

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "val-cluster-config-validation", job)
			origHash := job.Annotations["curity.io/config-hash"]
			origUID := job.UID

			// Update ConfigMap data to change the hash
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "val-config", Namespace: ns}, cm)).To(Succeed())
			cm.Data["v1.xml"] = "<v2/>"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Old Job should be deleted and new one created with different hash
			Eventually(func(g Gomega) {
				newJob := &batchv1.Job{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "val-cluster-config-validation", Namespace: ns}, newJob)).To(Succeed())
				g.Expect(newJob.UID).NotTo(Equal(origUID))
				g.Expect(newJob.Annotations["curity.io/config-hash"]).NotTo(Equal(origHash))
			}, timeout, interval).Should(Succeed())
		})

		It("should clear validation state when no managed configs exist", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "val-config", map[string]string{"tmp.xml": "<tmp/>"}, nil)

			job := &batchv1.Job{}
			eventuallyGetResource(ns, "val-cluster-config-validation", job)

			// Complete the Job
			job.Status.Conditions = []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			}}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Wait for validated hash
			cluster := &v1alpha1.IdentityServerCluster{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "val-cluster", Namespace: ns}, cluster); err != nil {
					return ""
				}
				return cluster.Annotations["curity.io/validated-config-hash"]
			}, timeout, interval).ShouldNot(BeEmpty())

			// Delete the managed ConfigMap
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "val-config", Namespace: ns}, cm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())

			// Validated hash should be cleared
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "val-cluster", Namespace: ns}, cluster); err != nil {
					return "error"
				}
				return cluster.Annotations["curity.io/validated-config-hash"]
			}, timeout, interval).Should(BeEmpty())
		})

		It("should default config-type annotation on managed resources", func() {
			testCreateCluster(ns, "val-cluster")
			testCreateManagedConfigMap(ns, "no-type-config", map[string]string{"x.xml": "<x/>"}, nil)

			// Cluster reconciler should default the annotation
			cm := &corev1.ConfigMap{}
			Eventually(func() string {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "no-type-config", Namespace: ns}, cm); err != nil {
					return ""
				}
				return cm.Annotations["curity.io/config-type"]
			}, timeout, interval).Should(Equal("base"))
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
		eventuallyGetResource(ns, "sched-node", deploy)
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
				NodeSelector:             map[string]string{"pool": "gpu"},
				Tolerations: []corev1.Toleration{
					{Key: "new-taint", Operator: corev1.TolerationOpEqual, Value: "yes"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{}
		eventuallyGetResource(ns, "override-node", deploy)

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
		eventuallyGetResource(ns, "topo-node", deploy)
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
		eventuallyGetResource(ns, "update-node", deploy)
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("env", "staging"))

		// Update cluster nodeSelector
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-cluster", Namespace: ns}, cluster)).To(Succeed())
		cluster.Spec.NodeSelector = map[string]string{"env": "production"}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

		// Deployment should eventually reflect the change
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "update-node", Namespace: ns}, deploy)
			return deploy.Spec.Template.Spec.NodeSelector["env"]
		}, timeout, interval).Should(Equal("production"))
	})
})
