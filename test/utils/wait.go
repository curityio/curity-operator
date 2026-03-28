package utils

import (
	"context"
	"fmt"
	"strings"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
