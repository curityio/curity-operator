package utils

import (
	"context"
	"fmt"
	"strings"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateClusterConfigReady populates the cluster-config secret with real data,
// simulating genclust Job completion. The ISC reconciler then detects the ready
// secret and sets ClusterConfigReady=True, unblocking the ISN Deployment gate.
// Call this AFTER creating the admin node so the ISC reconciler can find it.
func SimulateClusterConfigReady(ns, clusterName string, timeArgs ...interface{}) {
	ctx := context.Background()
	k := TestEnvironment.K8sClient
	secretName := clusterName + "-cluster-config"
	realData := map[string][]byte{
		"cluster.xml": []byte("<config xmlns=\"http://tail-f.com/ns/config/1.0\"><test/></config>"),
	}

	// Create or update the secret with real data.
	gomega.Eventually(func() error {
		var existing corev1.Secret
		if err := k.Get(ctx, client.ObjectKey{Name: secretName, Namespace: ns}, &existing); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: secretName, Namespace: ns,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "curity-operator",
						"curity.io/cluster":            clusterName,
						"curity.io/component":          "cluster-config",
					},
				},
				Data: realData,
			}
			return k.Create(ctx, secret)
		}
		existing.Data = realData
		return k.Update(ctx, &existing)
	}, timeArgs...).Should(gomega.Succeed())

	// Wait for the cluster reconciler to detect the ready secret and set the condition.
	gomega.Eventually(func(g gomega.Gomega) {
		cluster := &v1alpha1.IdentityServerCluster{}
		g.Expect(k.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: ns}, cluster)).To(gomega.Succeed())
		found := false
		for _, c := range cluster.Status.Conditions {
			if c.Type == v1alpha1.ConditionClusterConfigReady && c.Status == metav1.ConditionTrue {
				found = true
				break
			}
		}
		g.Expect(found).To(gomega.BeTrue(), "ClusterConfigReady condition not yet True on cluster %s/%s", ns, clusterName)
	}, timeArgs...).Should(gomega.Succeed())
}

// WaitForResource polls until the resource exists in the API server.
func WaitForResource(obj client.Object, timeArgs ...interface{}) {
	gomega.Eventually(func() error {
		return TestEnvironment.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	}, timeArgs...).ShouldNot(gomega.HaveOccurred())
}

// WaitForConditions polls until the CRD object has at least one status condition.
// The object is populated with the latest state on return.
func WaitForConditions(obj client.Object, timeArgs ...interface{}) {
	gomega.Eventually(func(g gomega.Gomega) {
		g.Expect(TestEnvironment.K8sClient.Get(
			context.Background(), client.ObjectKeyFromObject(obj), obj,
		)).To(gomega.Succeed())
		g.Expect(getConditions(obj)).NotTo(gomega.BeEmpty(),
			fmt.Sprintf("%s %s/%s has no conditions yet",
				strings.ToLower(GetKind(obj)),
				obj.GetNamespace(),
				obj.GetName()))
	}, timeArgs...).Should(gomega.Succeed())
}

func getConditions(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *v1alpha1.IdentityServerCluster:
		return o.Status.Conditions
	case *v1alpha1.IdentityServerNode:
		return o.Status.Conditions
	default:
		return nil
	}
}

// WaitForResources polls until a list of resources passes validation.
func WaitForResources(
	obj client.ObjectList,
	options *client.ListOptions,
	validateFnc func() bool,
	timeArgs ...interface{},
) {
	gomega.Eventually(func() error {
		err := TestEnvironment.K8sClient.List(context.Background(), obj, options)
		if err != nil {
			return err
		}

		if validateFnc() {
			return nil
		}

		return fmt.Errorf("%s should become ready", strings.ToLower(GetKind(obj)))
	}, timeArgs...).ShouldNot(gomega.HaveOccurred())
}

// WaitForDynamicResource polls an untyped resource until it passes validation.
func WaitForDynamicResource(
	gvr schema.GroupVersionResource,
	name string,
	namespace string,
	validateFnc func(obj *unstructured.Unstructured) bool,
	timeArgs ...interface{},
) {
	gomega.Eventually(func() error {
		obj, err := TestEnvironment.DynamicClient.Resource(gvr).
			Namespace(namespace).
			Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if validateFnc(obj) {
			return nil
		}

		return fmt.Errorf("%s should become ready", strings.ToLower(GetKind(obj)))
	}, timeArgs...).ShouldNot(gomega.HaveOccurred())
}
