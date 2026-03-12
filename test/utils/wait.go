package utils

import (
	"context"
	"fmt"
	"strings"

	"github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitForResource polls until the resource exists and passes validation.
func WaitForResource(obj client.Object, validateFnc func() bool, timeArgs ...interface{}) {
	gomega.Eventually(func() error {
		err := TestEnvironment.K8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		if err != nil {
			return err
		}

		if validateFnc() {
			return nil
		}

		return fmt.Errorf("%s should become ready", strings.ToLower(GetKind(obj)))
	}, timeArgs...).ShouldNot(gomega.HaveOccurred())
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
			Get(context.Background(), name, v1.GetOptions{})
		if err != nil {
			return err
		}

		if validateFnc(obj) {
			return nil
		}

		return fmt.Errorf("%s should become ready", strings.ToLower(GetKind(obj)))
	}, timeArgs...).ShouldNot(gomega.HaveOccurred())
}
