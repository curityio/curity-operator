package controller_test

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

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

// defaultTestService returns a minimal valid Service (type only); every role
// port then defaults (admin config 6789, runtime http 8443). Don't pin a port
// here — on admin nodes service.port is the config port, so a runtime value
// like 8443 would mis-set it and trigger a genclust regen.
func defaultTestService() *v1alpha1.ServiceSpec {
	return &v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}
}

func testCreateNode(ns, name string, nodeType v1alpha1.NodeType, clusterName string) {
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.IdentityServerNodeSpec{
			Type:                     nodeType,
			Role:                     name + "-role",
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
			Service:                  defaultTestService(),
		},
	}
	// Admin nodes reject replicas at admission; runtime defaults to 1 anyway.
	if nodeType != v1alpha1.NodeTypeAdmin {
		node.Spec.Replicas = ptr.To(int32(1))
	}
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
}

// userAdminCreds builds a CredentialsSource referencing a user-provided Secret
// name with the conventional key→path items (mirrors defaultAdminCredentials).
func userAdminCreds(name string) *v1alpha1.CredentialsSource {
	return &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: name,
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
				},
			},
		},
	}
}

// createOpaqueCredsSecret pre-creates an admin-credentials Secret with the
// expected keys — for tests that exercise paths past the "Secret exists" gate
// (e.g. encryption-key rotation) now that the operator no longer auto-creates a
// user-provided Secret.
func createOpaqueCredsSecret(ns, name string) {
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ADMIN_PASSWORD":        []byte("test-password"),
			"CONFIG_ENCRYPTION_KEY": []byte("test-encryption-key"),
		},
	})).To(Succeed())
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
				"type": "runtime",
				"role": role,
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

// markJobFailed drives a Job to Failed via the status subresource (envtest has no
// Job controller), setting the StartTime + FailureTarget fields newer apiservers
// require alongside Failed so specs survive an ENVTEST_K8S_VERSION bump past ~1.33.
func markJobFailed(job *batchv1.Job, message string) {
	now := metav1.Now()
	if job.Status.StartTime == nil {
		job.Status.StartTime = &now
	}
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: message, LastTransitionTime: now},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "PodFailurePolicy", Message: message, LastTransitionTime: now},
	}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}
