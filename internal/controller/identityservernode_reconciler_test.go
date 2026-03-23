package controller_test

import (
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var nodeTestCounter atomic.Int64

func nodeTestNamespace() string {
	return fmt.Sprintf("node-test-%d", nodeTestCounter.Add(1))
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

	createCluster := func(name string) {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	}

	createNode := func(name string, nodeType v1alpha1.NodeType, clusterName string) {
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

	Context("Happy path — Admin node", func() {
		It("should create Deployment and Service for admin node", func() {
			createCluster("cluster-1")
			createNode("admin-node", v1alpha1.NodeTypeAdmin, "cluster-1")

			// Verify Deployment is created
			deploy := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "admin-node", Namespace: ns}, deploy)
			}, timeout, interval).Should(Succeed())

			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--admin"))

			// Verify Service is created
			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "admin-node", Namespace: ns}, svc)
			}, timeout, interval).Should(Succeed())

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
			createCluster("cluster-1")
			createNode("labeled-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() string {
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "labeled-node", Namespace: ns}, node)
				return node.Labels["curity.io/cluster"]
			}, timeout, interval).Should(Equal("cluster-1"))
		})

		It("should allow duplicate admin check to work via label-based filtering", func() {
			createCluster("cluster-x")
			createCluster("cluster-y")

			// Admin for cluster-x
			createNode("admin-x", v1alpha1.NodeTypeAdmin, "cluster-x")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "admin-x", Namespace: ns}, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())

			// Admin for cluster-y — different cluster, should NOT be blocked
			createNode("admin-y", v1alpha1.NodeTypeAdmin, "cluster-y")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "admin-y", Namespace: ns}, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("Happy path — Runtime node", func() {
		It("should create Deployment with --no-admin args", func() {
			createCluster("cluster-1")
			createNode("runtime-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "runtime-node", Namespace: ns}, deploy)
			}, timeout, interval).Should(Succeed())

			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--no-admin"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("--admin"))
		})
	})

	Context("Error paths", func() {
		It("should set Degraded when cluster does not exist", func() {
			createNode("orphan-node", v1alpha1.NodeTypeRuntime, "nonexistent-cluster")

			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "orphan-node", Namespace: ns}, node); err != nil {
					return false
				}
				for _, c := range node.Status.Conditions {
					if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "expected Degraded condition to be set")
		})

		It("should block second admin node for same cluster", func() {
			createCluster("cluster-1")
			createNode("admin-1", v1alpha1.NodeTypeAdmin, "cluster-1")

			// Wait for first admin's deployment
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "admin-1", Namespace: ns}, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())

			// Create second admin
			createNode("admin-2", v1alpha1.NodeTypeAdmin, "cluster-1")

			// Second admin should be Degraded with no Deployment
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "admin-2", Namespace: ns}, node); err != nil {
					return false
				}
				for _, c := range node.Status.Conditions {
					if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue && c.Reason == "DuplicateAdmin" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Verify no Deployment was created for admin-2
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "admin-2", Namespace: ns}, &appsv1.Deployment{})
			Expect(err).To(HaveOccurred())
		})

		It("should block node with duplicate role in same cluster", func() {
			createCluster("cluster-1")
			createNode("role-node-1", v1alpha1.NodeTypeRuntime, "cluster-1")

			// Wait for first node's Deployment
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "role-node-1", Namespace: ns}, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())

			// Create second node with same role (createNode uses name+"-role")
			// so we create manually with an explicit duplicate role
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
				for _, c := range dupNode.Status.Conditions {
					if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
						return c.Reason
					}
				}
				return ""
			}, timeout, interval).Should(Equal("DuplicateRole"))

			// Ready should be False
			for _, c := range dupNode.Status.Conditions {
				if c.Type == v1alpha1.ConditionReady {
					Expect(c.Status).To(Equal(metav1.ConditionFalse))
				}
			}

			// No Deployment for the duplicate
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "role-node-dup", Namespace: ns}, &appsv1.Deployment{})
			Expect(err).To(HaveOccurred())
		})

		It("should allow same role in different clusters", func() {
			createCluster("cluster-a")
			createCluster("cluster-b")

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
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "node-a", Namespace: ns}, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "node-b", Namespace: ns}, &appsv1.Deployment{})
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("Deletion", func() {
		It("should clean up via OwnerReference garbage collection", func() {
			createCluster("cluster-1")
			createNode("del-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			// Wait for Deployment
			deploy := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "del-node", Namespace: ns}, deploy)
			}, timeout, interval).Should(Succeed())

			// Verify OwnerReference is set
			Expect(deploy.OwnerReferences).To(HaveLen(1))
			Expect(deploy.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

			// Delete the node
			node := &v1alpha1.IdentityServerNode{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "del-node", Namespace: ns}, node)).To(Succeed())
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			// Node should eventually be deleted (finalizer removed)
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "del-node", Namespace: ns}, &v1alpha1.IdentityServerNode{})
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("Updates", func() {
		It("should update Deployment when replicas change", func() {
			createCluster("cluster-1")
			createNode("update-node", v1alpha1.NodeTypeRuntime, "cluster-1")

			deploy := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "update-node", Namespace: ns}, deploy)
			}, timeout, interval).Should(Succeed())

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

			// Verify secret is created
			secret := &corev1.Secret{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "test-admin-secret", Namespace: ns}, secret)
			}, timeout, interval).Should(Succeed())

			Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
			Expect(secret.Data).To(HaveKey("CONFIG_ENCRYPTION_KEY"))
			Expect(secret.Data).To(HaveKey("KEYSTORE_PASSWORD"))

			// Verify no OwnerReference (survives cluster deletion)
			Expect(secret.OwnerReferences).To(BeEmpty())
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

			// Wait for node to be gone
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "block-node", Namespace: ns}, &v1alpha1.IdentityServerNode{})
				return err != nil
			}, timeout, interval).Should(BeTrue())

			// Now cluster should be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-del", Namespace: ns}, &v1alpha1.IdentityServerCluster{})
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})
