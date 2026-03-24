package e2e

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

const (
	e2eTimeout    = 60 * time.Second
	e2eInterval   = time.Second
	remoteTimeout = 5 * time.Minute
)

func createNS(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := utils.TestEnvironment.K8sClient.Create(context.Background(), ns)
	if err != nil {
		GinkgoWriter.Println(fmt.Sprintf("namespace create: %v", err))
	}
}

func deleteNS(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_ = utils.TestEnvironment.K8sClient.Delete(context.Background(), ns)
}

func k() client.Client { return utils.TestEnvironment.K8sClient }

// ====================================================================
// TEST 1: Deploy admin + runtime nodes
// ====================================================================
var _ = Describe("Test 1: Deploy admin + runtime nodes", Ordered, func() {
	const ns = "e2e-deploy"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should create Deployments and Services for both node types", func() {
		ctx := context.Background()

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "deploy-cluster", "namespace": ns})

		// Pre-deployment: cluster with no nodes yet
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))
		utils.MatchCRDResource(cluster, "deploy-cluster pre-deployment")

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
			map[string]interface{}{"name": "deploy-admin", "namespace": ns, "clusterName": "deploy-cluster"})

		adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy-admin", Namespace: ns}}
		utils.WaitForResource(adminDeploy, func() bool { return true }, e2eTimeout, e2eInterval)
		utils.MatchYAMLResource(adminDeploy, "[admin-deployment] deploy-admin")

		adminSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "deploy-admin", Namespace: ns}}
		utils.WaitForResource(adminSvc, func() bool { return true }, e2eTimeout, e2eInterval)
		utils.MatchYAMLResource(adminSvc, "[admin-service] deploy-admin")

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "deploy-runtime", "namespace": ns, "clusterName": "deploy-cluster"})

		runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy-runtime", Namespace: ns}}
		utils.WaitForResource(runtimeDeploy, func() bool { return true }, e2eTimeout, e2eInterval)
		utils.MatchYAMLResource(runtimeDeploy, "[runtime-deployment] deploy-runtime")

		runtimeSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "deploy-runtime", Namespace: ns}}
		utils.WaitForResource(runtimeSvc, func() bool { return true }, e2eTimeout, e2eInterval)
		utils.MatchYAMLResource(runtimeSvc, "[runtime-service] deploy-runtime")

		// Post-deployment: wait for cluster to reflect both nodes
		Eventually(func() int32 {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-cluster", Namespace: ns}, cluster)
			return cluster.Status.NodeCount
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">=", 2))

		adminNode := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-admin", Namespace: ns}, adminNode)
			return len(adminNode.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		runtimeNode := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-runtime", Namespace: ns}, runtimeNode)
			return len(runtimeNode.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "deploy-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "deploy-cluster post-deployment")
		utils.MatchCRDResource(adminNode, "deploy-admin post-deployment")
		utils.MatchCRDResource(runtimeNode, "deploy-runtime post-deployment")
	})

	It("should capture ready-state CRD snapshots when all nodes available", func() {
		if os.Getenv("E2E_REMOTE") != "true" {
			Skip("ready-state snapshots require remote cluster with pullable image")
		}

		ctx := context.Background()

		// Wait for both deployments to have ready replicas
		adminDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy-admin", Namespace: ns}}
		Eventually(func() int32 {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-admin", Namespace: ns}, adminDeploy)
			return adminDeploy.Status.ReadyReplicas
		}, remoteTimeout, e2eInterval).Should(BeNumerically(">=", 1))

		runtimeDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy-runtime", Namespace: ns}}
		Eventually(func() int32 {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-runtime", Namespace: ns}, runtimeDeploy)
			return runtimeDeploy.Status.ReadyReplicas
		}, remoteTimeout, e2eInterval).Should(BeNumerically(">=", 1))

		// Wait for node Ready=True conditions
		adminNode := &v1alpha1.IdentityServerNode{}
		Eventually(func() bool {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-admin", Namespace: ns}, adminNode)
			for _, c := range adminNode.Status.Conditions {
				if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionTrue {
					return true
				}
			}
			return false
		}, remoteTimeout, e2eInterval).Should(BeTrue())

		runtimeNode := &v1alpha1.IdentityServerNode{}
		Eventually(func() bool {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-runtime", Namespace: ns}, runtimeNode)
			for _, c := range runtimeNode.Status.Conditions {
				if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionTrue {
					return true
				}
			}
			return false
		}, remoteTimeout, e2eInterval).Should(BeTrue())

		// Wait for cluster Ready=True
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() bool {
			_ = k().Get(ctx, client.ObjectKey{Name: "deploy-cluster", Namespace: ns}, cluster)
			for _, c := range cluster.Status.Conditions {
				if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionTrue {
					return true
				}
			}
			return false
		}, remoteTimeout, e2eInterval).Should(BeTrue())

		utils.MatchCRDResource(cluster, "deploy-cluster ready")
		utils.MatchCRDResource(adminNode, "deploy-admin ready")
		utils.MatchCRDResource(runtimeNode, "deploy-runtime ready")
	})
})

// ====================================================================
// TEST 2: Cluster status reflects node health
// ====================================================================
var _ = Describe("Test 2: Cluster status reflects node health", Ordered, func() {
	const ns = "e2e-status"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should set conditions and status fields", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "status-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "status-node", "namespace": ns, "clusterName": "status-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "status-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "status-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))
		Expect(node.Status.ObservedGeneration).To(BeNumerically(">", 0))
		Expect(node.Status.DeploymentName).To(Equal("status-node"))
		Expect(node.Status.ServiceName).To(Equal("status-node"))
		Expect(node.Status.Replicas).To(Equal(int32(1)))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "status-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))
		Expect(cluster.Status.NodeCount).To(Equal(int32(1)))
		Expect(cluster.Status.Version).To(Equal("11.0"))
		Expect(cluster.Status.ObservedGeneration).To(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "status-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "status-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "status-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "status-node")
	})
})

// ====================================================================
// TEST 3: Delete node cleans up resources
// ====================================================================
var _ = Describe("Test 3: Delete node cleans up resources", Ordered, func() {
	const ns = "e2e-delete"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should garbage collect Deployment and Service via OwnerRef", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "del-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "del-node", "namespace": ns, "clusterName": "del-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "del-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		Expect(deploy.OwnerReferences).To(HaveLen(1))
		Expect(deploy.OwnerReferences[0].Kind).To(Equal("IdentityServerNode"))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "del-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "del-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "del-cluster before-delete")
		Expect(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "del-node before-delete")

		Expect(k().Delete(ctx, node)).To(Succeed())

		Eventually(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, &v1alpha1.IdentityServerNode{}))
		}, e2eTimeout, e2eInterval).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, &appsv1.Deployment{}))
		}, e2eTimeout, e2eInterval).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "del-node", Namespace: ns}, &corev1.Service{}))
		}, e2eTimeout, e2eInterval).Should(BeTrue())
	})
})

// ====================================================================
// TEST 4: Cluster deletion blocked by nodes
// ====================================================================
var _ = Describe("Test 4: Cluster deletion blocked by nodes", Ordered, func() {
	const ns = "e2e-finalizer"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should block cluster deletion until all nodes removed", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "fin-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "fin-node", "namespace": ns, "clusterName": "fin-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "fin-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() bool {
			_ = k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, cluster)
			for _, f := range cluster.Finalizers {
				if f == v1alpha1.ClusterFinalizer {
					return true
				}
			}
			return false
		}, e2eTimeout, e2eInterval).Should(BeTrue())

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "fin-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "fin-cluster before-delete")
		Expect(k().Get(ctx, client.ObjectKey{Name: "fin-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "fin-node before-delete")

		Expect(k().Delete(ctx, cluster)).To(Succeed())
		Consistently(func() error {
			return k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, &v1alpha1.IdentityServerCluster{})
		}, 5*time.Second, e2eInterval).Should(Succeed())

		Expect(k().Get(ctx, client.ObjectKey{Name: "fin-node", Namespace: ns}, node)).To(Succeed())
		Expect(k().Delete(ctx, node)).To(Succeed())

		Eventually(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "fin-cluster", Namespace: ns}, &v1alpha1.IdentityServerCluster{}))
		}, e2eTimeout, e2eInterval).Should(BeTrue())
	})
})

// ====================================================================
// TEST 5: Admin credentials auto-generation
// ====================================================================
var _ = Describe("Test 5: Admin credentials auto-generation", Ordered, func() {
	const ns = "e2e-creds"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should create secret with random values when it does not exist", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{"name": "creds-cluster", "namespace": ns, "secretName": "auto-gen-secret"})

		secret := &corev1.Secret{}
		Eventually(func() error {
			return k().Get(ctx, client.ObjectKey{Name: "auto-gen-secret", Namespace: ns}, secret)
		}, e2eTimeout, e2eInterval).Should(Succeed())

		Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
		Expect(secret.Data).To(HaveKey("CONFIG_ENCRYPTION_KEY"))
		Expect(secret.Data).To(HaveKey("KEYSTORE_PASSWORD"))
		Expect(len(secret.Data["ADMIN_PASSWORD"])).To(BeNumerically(">", 0))
		Expect(secret.OwnerReferences).To(BeEmpty())
		Expect(secret.Labels["app.kubernetes.io/managed-by"]).To(Equal("curity-operator"))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "creds-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))
		Expect(k().Get(ctx, client.ObjectKey{Name: "creds-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "creds-cluster")
	})
})

// ====================================================================
// TEST 6: Existing secret not overwritten
// ====================================================================
var _ = Describe("Test 6: Existing secret not overwritten", Ordered, func() {
	const ns = "e2e-creds-exist"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should not overwrite pre-existing secret", func() {
		ctx := context.Background()
		preExisting := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pre-existing-secret", Namespace: ns},
			Data: map[string][]byte{
				"ADMIN_PASSWORD":        []byte("my-known-password"),
				"CONFIG_ENCRYPTION_KEY": []byte("my-known-key"),
				"KEYSTORE_PASSWORD":     []byte("my-known-ks"),
			},
		}
		Expect(k().Create(ctx, preExisting)).To(Succeed())

		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{"name": "creds-cluster-2", "namespace": ns, "secretName": "pre-existing-secret"})

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int64 {
			_ = k().Get(ctx, client.ObjectKey{Name: "creds-cluster-2", Namespace: ns}, cluster)
			return cluster.Status.ObservedGeneration
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		secret := &corev1.Secret{}
		Expect(k().Get(ctx, client.ObjectKey{Name: "pre-existing-secret", Namespace: ns}, secret)).To(Succeed())
		Expect(string(secret.Data["ADMIN_PASSWORD"])).To(Equal("my-known-password"))

		Expect(k().Get(ctx, client.ObjectKey{Name: "creds-cluster-2", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "creds-cluster-2")
	})
})

// ====================================================================
// TEST 7: Duplicate admin node rejection
// ====================================================================
var _ = Describe("Test 7: Duplicate admin node rejection", Ordered, func() {
	const ns = "e2e-dup-admin"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should set Degraded on second admin and not create its Deployment", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "dup-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin.yaml", ns,
			map[string]interface{}{"name": "admin-1", "namespace": ns, "clusterName": "dup-cluster"})

		deploy1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "admin-1", Namespace: ns}}
		utils.WaitForResource(deploy1, func() bool { return true }, e2eTimeout, e2eInterval)

		node2 := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-2", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeAdmin, Role: "admin-role-2",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "dup-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node2)).To(Succeed())

		Eventually(func() string {
			_ = k().Get(ctx, client.ObjectKey{Name: "admin-2", Namespace: ns}, node2)
			for _, c := range node2.Status.Conditions {
				if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
					return c.Reason
				}
			}
			return ""
		}, e2eTimeout, e2eInterval).Should(Equal("DuplicateAdmin"))

		Consistently(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "admin-2", Namespace: ns}, &appsv1.Deployment{}))
		}, 5*time.Second, e2eInterval).Should(BeTrue())

		admin1 := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "admin-1", Namespace: ns}, admin1)
			return len(admin1.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "dup-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "dup-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "dup-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "admin-1", Namespace: ns}, admin1)).To(Succeed())
		utils.MatchCRDResource(admin1, "admin-1")
		Expect(k().Get(ctx, client.ObjectKey{Name: "admin-2", Namespace: ns}, node2)).To(Succeed())
		utils.MatchCRDResource(node2, "admin-2 degraded")
	})
})

// ====================================================================
// TEST 8: Admin UI port configuration
// ====================================================================
var _ = Describe("Test 8: Admin UI port configuration", Ordered, func() {
	const ns = "e2e-admin-ui"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should expose admin-ui port and set ADMIN_UI_HTTP_MODE", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "ui-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-admin-ui.yaml", ns,
			map[string]interface{}{"name": "ui-admin", "namespace": ns, "clusterName": "ui-cluster",
				"uiEnabled": true, "uiSecure": false})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ui-admin", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		foundUIPort := false
		for _, p := range deploy.Spec.Template.Spec.Containers[0].Ports {
			if p.Name == "admin-ui" && p.ContainerPort == 6749 {
				foundUIPort = true
			}
		}
		Expect(foundUIPort).To(BeTrue(), "admin-ui port 6749 should exist")

		svc := &corev1.Service{}
		Expect(k().Get(ctx, client.ObjectKey{Name: "ui-admin", Namespace: ns}, svc)).To(Succeed())
		foundSvcPort := false
		for _, p := range svc.Spec.Ports {
			if p.Name == "admin-ui" && p.Port == 6749 {
				foundSvcPort = true
			}
		}
		Expect(foundSvcPort).To(BeTrue(), "admin-ui port on Service")

		foundHTTPMode := false
		for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "ADMIN_UI_HTTP_MODE" && e.Value == "true" {
				foundHTTPMode = true
			}
		}
		Expect(foundHTTPMode).To(BeTrue(), "ADMIN_UI_HTTP_MODE should be true")

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "ui-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "ui-admin", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "ui-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "ui-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "ui-admin", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "ui-admin")
	})
})

// ====================================================================
// TEST 9: Custom service port and env vars
// ====================================================================
var _ = Describe("Test 9: Custom service port and env vars", Ordered, func() {
	const ns = "e2e-custom"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should use custom port and pass env vars", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "custom-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime-custom.yaml", ns,
			map[string]interface{}{
				"name": "custom-node", "namespace": ns, "clusterName": "custom-cluster",
				"role": "custom-role", "replicas": 2, "serviceType": "ClusterIP",
				"servicePort": 9443, "customEnvValue": "hello-e2e",
			})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "custom-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))

		foundEnv := false
		for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "CUSTOM_ENV" && e.Value == "hello-e2e" {
				foundEnv = true
			}
		}
		Expect(foundEnv).To(BeTrue())

		svc := &corev1.Service{}
		Expect(k().Get(ctx, client.ObjectKey{Name: "custom-node", Namespace: ns}, svc)).To(Succeed())
		foundPort := false
		for _, p := range svc.Spec.Ports {
			if p.Name == "http" && p.Port == 9443 {
				foundPort = true
			}
		}
		Expect(foundPort).To(BeTrue(), "Service should have http port 9443")

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "custom-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "custom-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "custom-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "custom-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "custom-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "custom-node")
	})
})

// ====================================================================
// TEST 10: Update replicas on existing node
// ====================================================================
var _ = Describe("Test 10: Update replicas", Ordered, func() {
	const ns = "e2e-update"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should update Deployment when replicas change", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "upd-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "upd-node", "namespace": ns, "clusterName": "upd-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "upd-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

		// Pre-update snapshot
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "upd-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		utils.MatchCRDResource(cluster, "upd-cluster pre-update")
		utils.MatchCRDResource(node, "upd-node pre-update")

		Eventually(func() error {
			if err := k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, node); err != nil {
				return err
			}
			node.Spec.Replicas = ptr.To(int32(3))
			return k().Update(ctx, node)
		}, e2eTimeout, e2eInterval).Should(Succeed())

		Eventually(func() int32 {
			_ = k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, deploy)
			if deploy.Spec.Replicas == nil {
				return 0
			}
			return *deploy.Spec.Replicas
		}, e2eTimeout, e2eInterval).Should(Equal(int32(3)))

		// Post-update snapshot
		Eventually(func() int32 {
			_ = k().Get(ctx, client.ObjectKey{Name: "upd-node", Namespace: ns}, node)
			return node.Status.Replicas
		}, e2eTimeout, e2eInterval).Should(Equal(int32(3)))

		Expect(k().Get(ctx, client.ObjectKey{Name: "upd-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "upd-cluster post-update")
		utils.MatchCRDResource(node, "upd-node post-update")
	})
})

// ====================================================================
// TEST 11: Node referencing non-existent cluster
// ====================================================================
var _ = Describe("Test 11: Orphan node", Ordered, func() {
	const ns = "e2e-orphan"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should set Degraded when cluster does not exist", func() {
		ctx := context.Background()
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "orphan-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "does-not-exist"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		Eventually(func() string {
			_ = k().Get(ctx, client.ObjectKey{Name: "orphan-node", Namespace: ns}, node)
			for _, c := range node.Status.Conditions {
				if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
					return c.Reason
				}
			}
			return ""
		}, e2eTimeout, e2eInterval).Should(Equal("ClusterNotFound"))

		Consistently(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "orphan-node", Namespace: ns}, &appsv1.Deployment{}))
		}, 5*time.Second, e2eInterval).Should(BeTrue())

		Expect(k().Get(ctx, client.ObjectKey{Name: "orphan-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "orphan-node degraded")
	})
})

// ====================================================================
// TEST 12: Multiple runtime nodes
// ====================================================================
var _ = Describe("Test 12: Multiple runtime nodes", Ordered, func() {
	const ns = "e2e-multi"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should create separate Deployments for each node", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "multi-cluster", "namespace": ns})

		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("runtime-%d", i)
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type: v1alpha1.NodeTypeRuntime, Role: fmt.Sprintf("role-%d", i),
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "multi-cluster"},
					Replicas:                 ptr.To(int32(1)),
				},
			}
			Expect(k().Create(ctx, node)).To(Succeed())
		}

		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("runtime-%d", i)
			deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
			utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		}

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int32 {
			_ = k().Get(ctx, client.ObjectKey{Name: "multi-cluster", Namespace: ns}, cluster)
			return cluster.Status.NodeCount
		}, e2eTimeout, e2eInterval).Should(Equal(int32(3)))
		Expect(k().Get(ctx, client.ObjectKey{Name: "multi-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "multi-cluster")

		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("runtime-%d", i)
			node := &v1alpha1.IdentityServerNode{}
			Eventually(func() int {
				_ = k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, node)
				return len(node.Status.Conditions)
			}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))
			Expect(k().Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, node)).To(Succeed())
			utils.MatchCRDResource(node, name)
		}
	})
})

// ====================================================================
// TEST 13: Admin replicas forced to 1
// ====================================================================
var _ = Describe("Test 13: Admin replicas forced to 1", Ordered, func() {
	const ns = "e2e-admin-rep"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should force admin Deployment to 1 replica", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "rep-cluster", "namespace": ns})

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-5", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeAdmin, Role: "admin-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "rep-cluster"},
				Replicas:                 ptr.To(int32(5)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "admin-5", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "rep-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "admin-5", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(context.Background(), client.ObjectKey{Name: "rep-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "rep-cluster")
		Expect(k().Get(context.Background(), client.ObjectKey{Name: "admin-5", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "admin-5")
	})
})

// ====================================================================
// TEST 14: Image override from cluster
// ====================================================================
var _ = Describe("Test 14: Image override", Ordered, func() {
	const ns = "e2e-image"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should use custom image from cluster spec", func() {
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-image.yaml", ns,
			map[string]interface{}{"name": "img-cluster", "namespace": ns, "image": "custom-registry/idsvr:custom"})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "img-node", "namespace": ns, "clusterName": "img-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "img-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("custom-registry/idsvr:custom"))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "img-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "img-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(context.Background(), client.ObjectKey{Name: "img-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "img-cluster")
		Expect(k().Get(context.Background(), client.ObjectKey{Name: "img-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "img-node")
	})
})

// ====================================================================
// TEST 15: Configuration volumes mounted
// ====================================================================
var _ = Describe("Test 15: Configuration volumes", Ordered, func() {
	const ns = "e2e-config"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should mount configmap as volume", func() {
		ctx := context.Background()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: ns},
			Data:       map[string]string{"base-config.xml": "<config/>"},
		}
		Expect(k().Create(ctx, cm)).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Configuration: &v1alpha1.ConfigurationSource{
					ValueFrom: v1alpha1.ConfigurationValueFrom{
						ConfigMapRef: &v1alpha1.ConfigMapRefSource{
							Name:  "test-config",
							Items: []v1alpha1.KeyToPath{{Key: "base-config.xml", Path: "base-config.xml"}},
						},
					},
				},
			},
		}
		Expect(k().Create(ctx, cluster)).To(Succeed())

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "cfg-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cfg-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cfg-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		Expect(deploy.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Containers[0].VolumeMounts).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath).To(ContainSubstring("/opt/idsvr/etc/init/"))

		clusterObj := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "cfg-cluster", Namespace: ns}, clusterObj)
			return len(clusterObj.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		nodeObj := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "cfg-node", Namespace: ns}, nodeObj)
			return len(nodeObj.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "cfg-cluster", Namespace: ns}, clusterObj)).To(Succeed())
		utils.MatchCRDResource(clusterObj, "cfg-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "cfg-node", Namespace: ns}, nodeObj)).To(Succeed())
		utils.MatchCRDResource(nodeObj, "cfg-node")
	})
})

// ====================================================================
// TEST 16: Labels and annotations merge
// ====================================================================
var _ = Describe("Test 16: Labels and annotations merge", Ordered, func() {
	const ns = "e2e-labels"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should merge cluster and node labels/annotations", func() {
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-labels.yaml", ns,
			map[string]interface{}{"name": "lbl-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime-labels.yaml", ns,
			map[string]interface{}{"name": "lbl-node", "namespace": ns, "clusterName": "lbl-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "lbl-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		podLabels := deploy.Spec.Template.Labels
		Expect(podLabels["team"]).To(Equal("identity"), "node label wins on conflict")

		podAnnotations := deploy.Spec.Template.Annotations
		Expect(podAnnotations["prometheus.io/scrape"]).To(Equal("true"), "cluster annotation preserved")

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "lbl-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "lbl-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(context.Background(), client.ObjectKey{Name: "lbl-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "lbl-cluster")
		Expect(k().Get(context.Background(), client.ObjectKey{Name: "lbl-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "lbl-node")
	})
})

// ====================================================================
// TEST 17: Security context on pods
// ====================================================================
var _ = Describe("Test 17: Security context", Ordered, func() {
	const ns = "e2e-security"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should set pod security context matching Helm chart", func() {
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "sec-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "sec-node", "namespace": ns, "clusterName": "sec-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "sec-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		podSC := deploy.Spec.Template.Spec.SecurityContext
		Expect(*podSC.RunAsUser).To(Equal(int64(10001)))
		Expect(*podSC.RunAsGroup).To(Equal(int64(10000)))
		Expect(*podSC.FSGroup).To(Equal(int64(10000)))

		// Container-level security context deferred to Phase 2

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "sec-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "sec-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(context.Background(), client.ObjectKey{Name: "sec-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "sec-cluster")
		Expect(k().Get(context.Background(), client.ObjectKey{Name: "sec-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "sec-node")
	})
})

// ====================================================================
// TEST 18: Admin node container args
// ====================================================================
var _ = Describe("Test 18: Admin container args", Ordered, func() {
	const ns = "e2e-admin-args"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should have correct admin args format", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "args-cluster", "namespace": ns})

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "args-admin", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeAdmin, Role: "my-admin-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "args-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "args-admin", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		args := deploy.Spec.Template.Spec.Containers[0].Args
		Expect(args).To(Equal([]string{"/opt/idsvr/bin/idsvr", "-s", "my-admin-role", "-N", "args-admin", "--admin"}))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "args-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "args-admin", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "args-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "args-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "args-admin", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "args-admin")
	})
})

// ====================================================================
// TEST 19: Runtime node container args
// ====================================================================
var _ = Describe("Test 19: Runtime container args", Ordered, func() {
	const ns = "e2e-rt-args"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should have correct runtime args format", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "rtargs-cluster", "namespace": ns})

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "args-runtime", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "my-runtime-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "rtargs-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "args-runtime", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		args := deploy.Spec.Template.Spec.Containers[0].Args
		Expect(args).To(Equal([]string{"/opt/idsvr/bin/idsvr", "-s", "my-runtime-role", "--no-admin"}))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "rtargs-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "args-runtime", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "rtargs-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "rtargs-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "args-runtime", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "args-runtime")
	})
})

// ====================================================================
// TEST 20: Probe defaults on Deployment
// ====================================================================
var _ = Describe("Test 20: Probe defaults", Ordered, func() {
	const ns = "e2e-probes"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should set correct default probe values", func() {
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "probe-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "probe-node", "namespace": ns, "clusterName": "probe-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "probe-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		lp := deploy.Spec.Template.Spec.Containers[0].LivenessProbe
		Expect(lp.HTTPGet.Path).To(Equal("/"))
		Expect(lp.HTTPGet.Port.IntValue()).To(Equal(4465))
		Expect(lp.InitialDelaySeconds).To(Equal(int32(30)))
		Expect(lp.PeriodSeconds).To(Equal(int32(10)))
		Expect(lp.TimeoutSeconds).To(Equal(int32(1)))
		Expect(lp.FailureThreshold).To(Equal(int32(3)))

		rp := deploy.Spec.Template.Spec.Containers[0].ReadinessProbe
		Expect(rp.HTTPGet.Path).To(Equal("/"))
		Expect(rp.SuccessThreshold).To(Equal(int32(3)))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "probe-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(context.Background(), client.ObjectKey{Name: "probe-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(context.Background(), client.ObjectKey{Name: "probe-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "probe-cluster")
		Expect(k().Get(context.Background(), client.ObjectKey{Name: "probe-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "probe-node")
	})
})

// ====================================================================
// TEST 21: Idempotency
// ====================================================================
var _ = Describe("Test 21: Idempotency", Ordered, func() {
	const ns = "e2e-idempotent"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should not update Deployment when spec unchanged", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "idem-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "idem-node", "namespace": ns, "clusterName": "idem-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "idem-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		gen := deploy.Generation

		// Trigger a reconcile by adding a label to the node (retry on conflict)
		Eventually(func() error {
			node := &v1alpha1.IdentityServerNode{}
			if err := k().Get(ctx, client.ObjectKey{Name: "idem-node", Namespace: ns}, node); err != nil {
				return err
			}
			if node.Labels == nil {
				node.Labels = map[string]string{}
			}
			node.Labels["trigger"] = "reconcile"
			return k().Update(ctx, node)
		}, e2eTimeout, e2eInterval).Should(Succeed())

		// Give reconciler time to process
		time.Sleep(3 * time.Second)

		// Deployment generation should not change (only increments on spec changes)
		Expect(k().Get(ctx, client.ObjectKey{Name: "idem-node", Namespace: ns}, deploy)).To(Succeed())
		Expect(deploy.Generation).To(Equal(gen), "Deployment should not be updated when spec unchanged")

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "idem-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		node := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "idem-node", Namespace: ns}, node)
			return len(node.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "idem-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "idem-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "idem-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "idem-node")
	})
})

// ====================================================================
// TEST 22: Cluster with no nodes
// ====================================================================
var _ = Describe("Test 22: Cluster with no nodes", Ordered, func() {
	const ns = "e2e-empty"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should set nodeCount=0 and Ready=False", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "empty-cluster", "namespace": ns})

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int64 {
			_ = k().Get(ctx, client.ObjectKey{Name: "empty-cluster", Namespace: ns}, cluster)
			return cluster.Status.ObservedGeneration
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(cluster.Status.NodeCount).To(Equal(int32(0)))

		foundReady := false
		for _, c := range cluster.Status.Conditions {
			if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionFalse {
				foundReady = true
			}
		}
		Expect(foundReady).To(BeTrue(), "Ready should be False for empty cluster")

		Expect(k().Get(ctx, client.ObjectKey{Name: "empty-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "empty-cluster")
	})
})

// ====================================================================
// TEST 23: Admin credentials secret survives cluster deletion
// ====================================================================
var _ = Describe("Test 23: Secret survives cluster deletion", Ordered, func() {
	const ns = "e2e-secret-survive"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should keep secret after cluster is deleted", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster-with-creds.yaml", ns,
			map[string]interface{}{"name": "surv-cluster", "namespace": ns, "secretName": "surv-secret"})

		// Wait for secret to be created
		secret := &corev1.Secret{}
		Eventually(func() error {
			return k().Get(ctx, client.ObjectKey{Name: "surv-secret", Namespace: ns}, secret)
		}, e2eTimeout, e2eInterval).Should(Succeed())

		// Wait for cluster finalizer
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() bool {
			_ = k().Get(ctx, client.ObjectKey{Name: "surv-cluster", Namespace: ns}, cluster)
			for _, f := range cluster.Finalizers {
				if f == v1alpha1.ClusterFinalizer {
					return true
				}
			}
			return false
		}, e2eTimeout, e2eInterval).Should(BeTrue())

		Expect(k().Get(ctx, client.ObjectKey{Name: "surv-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "surv-cluster before-delete")

		// Delete cluster (no nodes, so finalizer allows it)
		Expect(k().Delete(ctx, cluster)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "surv-cluster", Namespace: ns}, &v1alpha1.IdentityServerCluster{}))
		}, e2eTimeout, e2eInterval).Should(BeTrue())

		// Secret should still exist
		Expect(k().Get(ctx, client.ObjectKey{Name: "surv-secret", Namespace: ns}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey("ADMIN_PASSWORD"))
	})
})

// ====================================================================
// TEST 24: Duplicate role rejection
// ====================================================================
var _ = Describe("Test 24: Duplicate role rejection", Ordered, func() {
	const ns = "e2e-dup-role"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should block node with duplicate role in same cluster", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "role-cluster", "namespace": ns})

		// First runtime node with role "default"
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "role-node-1", "namespace": ns, "clusterName": "role-cluster"})

		deploy1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "role-node-1", Namespace: ns}}
		utils.WaitForResource(deploy1, func() bool { return true }, e2eTimeout, e2eInterval)

		// Second runtime node with same role "runtime-role" (from fixture)
		node2 := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "role-node-2", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "runtime-role", // same as fixture
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "role-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node2)).To(Succeed())

		// Should get Degraded with DuplicateRole
		Eventually(func() string {
			_ = k().Get(ctx, client.ObjectKey{Name: "role-node-2", Namespace: ns}, node2)
			for _, c := range node2.Status.Conditions {
				if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
					return c.Reason
				}
			}
			return ""
		}, e2eTimeout, e2eInterval).Should(Equal("DuplicateRole"))

		// No Deployment for duplicate
		Consistently(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "role-node-2", Namespace: ns}, &appsv1.Deployment{}))
		}, 5*time.Second, e2eInterval).Should(BeTrue())

		node1 := &v1alpha1.IdentityServerNode{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "role-node-1", Namespace: ns}, node1)
			return len(node1.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "role-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "role-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "role-cluster")
		Expect(k().Get(ctx, client.ObjectKey{Name: "role-node-1", Namespace: ns}, node1)).To(Succeed())
		utils.MatchCRDResource(node1, "role-node-1")
		Expect(k().Get(ctx, client.ObjectKey{Name: "role-node-2", Namespace: ns}, node2)).To(Succeed())
		utils.MatchCRDResource(node2, "role-node-2 degraded")
	})

	It("should allow same role in different clusters", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "role-cluster-a", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "role-cluster-b", "namespace": ns})

		// Same role in two different clusters
		nodeA := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cross-a", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "shared-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "role-cluster-a"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, nodeA)).To(Succeed())

		nodeB := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cross-b", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "shared-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "role-cluster-b"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, nodeB)).To(Succeed())

		// Both should get Deployments (different clusters = no conflict)
		deployA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cross-a", Namespace: ns}}
		utils.WaitForResource(deployA, func() bool { return true }, e2eTimeout, e2eInterval)
		deployB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cross-b", Namespace: ns}}
		utils.WaitForResource(deployB, func() bool { return true }, e2eTimeout, e2eInterval)

		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "cross-a", Namespace: ns}, nodeA)
			return len(nodeA.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "cross-b", Namespace: ns}, nodeB)
			return len(nodeB.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "cross-a", Namespace: ns}, nodeA)).To(Succeed())
		Expect(k().Get(ctx, client.ObjectKey{Name: "cross-b", Namespace: ns}, nodeB)).To(Succeed())
		utils.MatchCRDResource(nodeA, "cross-a")
		utils.MatchCRDResource(nodeB, "cross-b")
	})
})

// ====================================================================
// TEST 25: Cluster version change updates node Deployments
// ====================================================================
var _ = Describe("Test 25: Cluster version change updates nodes", Ordered, func() {
	const ns = "e2e-version"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should update Deployment image when cluster version changes", func() {
		ctx := context.Background()
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "ver-cluster", "namespace": ns})
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservernode-runtime.yaml", ns,
			map[string]interface{}{"name": "ver-node", "namespace": ns, "clusterName": "ver-cluster"})

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ver-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("curity.azurecr.io/curity/idsvr:11.0"))

		// Update cluster version
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() error {
			if err := k().Get(ctx, client.ObjectKey{Name: "ver-cluster", Namespace: ns}, cluster); err != nil {
				return err
			}
			cluster.Spec.Version = "12.0"
			return k().Update(ctx, cluster)
		}, e2eTimeout, e2eInterval).Should(Succeed())

		// Wait for Deployment image to update
		Eventually(func() string {
			_ = k().Get(ctx, client.ObjectKey{Name: "ver-node", Namespace: ns}, deploy)
			if len(deploy.Spec.Template.Spec.Containers) > 0 {
				return deploy.Spec.Template.Spec.Containers[0].Image
			}
			return ""
		}, e2eTimeout, e2eInterval).Should(Equal("curity.azurecr.io/curity/idsvr:12.0"))

		// Snapshot CRDs after version change
		node := &v1alpha1.IdentityServerNode{}
		Expect(k().Get(ctx, client.ObjectKey{Name: "ver-node", Namespace: ns}, node)).To(Succeed())
		Expect(k().Get(ctx, client.ObjectKey{Name: "ver-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "ver-cluster after-version-change")
		utils.MatchCRDResource(node, "ver-node after-version-change")
	})
})

// ====================================================================
// TEST 26: Orphan node recovery
// ====================================================================
var _ = Describe("Test 26: Orphan node recovery", Ordered, func() {
	const ns = "e2e-recovery"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should recover when missing cluster is created", func() {
		ctx := context.Background()

		// Create node referencing non-existent cluster
		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "recovery-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "recovery-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		// Verify node is degraded (ClusterNotFound)
		Eventually(func() string {
			_ = k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, node)
			for _, c := range node.Status.Conditions {
				if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
					return c.Reason
				}
			}
			return ""
		}, e2eTimeout, e2eInterval).Should(Equal("ClusterNotFound"))

		Expect(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(node, "recovery-node degraded")

		// No Deployment should exist yet
		Consistently(func() bool {
			return apierrors.IsNotFound(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, &appsv1.Deployment{}))
		}, 5*time.Second, e2eInterval).Should(BeTrue())

		// Now create the missing cluster — node should recover
		utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
			map[string]interface{}{"name": "recovery-cluster", "namespace": ns})

		// Deployment should be created after recovery
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "recovery-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		// Snapshot recovered state
		cluster := &v1alpha1.IdentityServerCluster{}
		Eventually(func() int {
			_ = k().Get(ctx, client.ObjectKey{Name: "recovery-cluster", Namespace: ns}, cluster)
			return len(cluster.Status.Conditions)
		}, e2eTimeout, e2eInterval).Should(BeNumerically(">", 0))

		Expect(k().Get(ctx, client.ObjectKey{Name: "recovery-node", Namespace: ns}, node)).To(Succeed())
		Expect(k().Get(ctx, client.ObjectKey{Name: "recovery-cluster", Namespace: ns}, cluster)).To(Succeed())
		utils.MatchCRDResource(cluster, "recovery-cluster")
		utils.MatchCRDResource(node, "recovery-node recovered")
	})
})

// ====================================================================
// TEST 27: Secret configuration volumes
// ====================================================================
var _ = Describe("Test 27: Secret configuration volumes", Ordered, func() {
	const ns = "e2e-secret-config"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should mount secret as volume", func() {
		ctx := context.Background()
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "test-secret-config", Namespace: ns},
			Data:       map[string][]byte{"datasource-config.xml": []byte("<config/>")},
		}
		Expect(k().Create(ctx, secret)).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "scfg-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Configuration: &v1alpha1.ConfigurationSource{
					ValueFrom: v1alpha1.ConfigurationValueFrom{
						SecretRef: &v1alpha1.SecretKeyRefSource{
							Name:  "test-secret-config",
							Items: []v1alpha1.KeyToPath{{Key: "datasource-config.xml", Path: "datasource-config.xml"}},
						},
					},
				},
			},
		}
		Expect(k().Create(ctx, cluster)).To(Succeed())

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "scfg-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "scfg-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "scfg-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "scfg-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		Expect(deploy.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Volumes[0].Secret).NotTo(BeNil())
		Expect(deploy.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal("test-secret-config"))

		Expect(deploy.Spec.Template.Spec.Containers[0].VolumeMounts).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath).To(Equal("/opt/idsvr/etc/init/datasource-config.xml"))

		Expect(k().Get(ctx, client.ObjectKey{Name: "scfg-cluster", Namespace: ns}, cluster)).To(Succeed())
		Expect(k().Get(ctx, client.ObjectKey{Name: "scfg-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(cluster, "scfg-cluster")
		utils.MatchCRDResource(node, "scfg-node")
	})
})

// ====================================================================
// TEST 28: Logging sidecars
// ====================================================================
var _ = Describe("Test 28: Logging sidecars", Ordered, func() {
	const ns = "e2e-logging"
	BeforeAll(func() { createNS(ns) })
	AfterAll(func() { deleteNS(ns) })

	It("should create sidecar containers when stdout logging enabled", func() {
		ctx := context.Background()

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "log-cluster", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				Logging: &v1alpha1.LoggingSpec{
					Level:  "DEBUG",
					Stdout: true,
					Logs:   []string{"audit", "request"},
				},
			},
		}
		Expect(k().Create(ctx, cluster)).To(Succeed())

		node := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "log-node", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type: v1alpha1.NodeTypeRuntime, Role: "log-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "log-cluster"},
				Replicas:                 ptr.To(int32(1)),
			},
		}
		Expect(k().Create(ctx, node)).To(Succeed())

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "log-node", Namespace: ns}}
		utils.WaitForResource(deploy, func() bool { return true }, e2eTimeout, e2eInterval)

		// Should have main container + 2 sidecar containers (audit, request)
		containers := deploy.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(3), "expected 1 main + 2 sidecar containers")

		// Verify sidecar names and commands
		sidecarNames := make([]string, 0)
		for _, c := range containers[1:] {
			sidecarNames = append(sidecarNames, c.Name)
			Expect(c.Image).To(Equal("busybox:latest"))
			Expect(c.Command[0]).To(Equal("tail"))
		}
		Expect(sidecarNames).To(ConsistOf("audit", "request"))

		// Verify log volume exists
		foundLogVol := false
		for _, v := range deploy.Spec.Template.Spec.Volumes {
			if v.Name == "log-volume" {
				foundLogVol = true
				Expect(v.EmptyDir).NotTo(BeNil())
			}
		}
		Expect(foundLogVol).To(BeTrue(), "log-volume EmptyDir should exist")

		// Verify LOGGING_LEVEL is DEBUG
		foundLevel := false
		for _, e := range containers[0].Env {
			if e.Name == "LOGGING_LEVEL" && e.Value == "DEBUG" {
				foundLevel = true
			}
		}
		Expect(foundLevel).To(BeTrue(), "LOGGING_LEVEL should be DEBUG")

		Expect(k().Get(ctx, client.ObjectKey{Name: "log-cluster", Namespace: ns}, cluster)).To(Succeed())
		Expect(k().Get(ctx, client.ObjectKey{Name: "log-node", Namespace: ns}, node)).To(Succeed())
		utils.MatchCRDResource(cluster, "log-cluster")
		utils.MatchCRDResource(node, "log-node")
	})
})
