package controller_test

import (
	"context"
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/curityio/curity-operator/api/v1alpha1"
)

var nsCounter atomic.Int32

var _ = Describe("IdentityServer", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", generateName())
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	It("should store and retrieve an IdentityServer with replicas", func() {
		replicas := int32(2)
		idsvr := &v1alpha1.IdentityServer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-idsvr",
				Namespace: namespace,
			},
			Spec: v1alpha1.IdentityServerSpec{
				Replicas: ptr.To(replicas),
			},
		}
		Expect(k8sClient.Create(ctx, idsvr)).To(Succeed())

		fetched := &v1alpha1.IdentityServer{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-idsvr", Namespace: namespace}, fetched)).To(Succeed())
		Expect(fetched.Spec.Replicas).NotTo(BeNil())
		Expect(*fetched.Spec.Replicas).To(Equal(replicas))
	})

	It("should list all IdentityServers in a namespace", func() {
		for _, name := range []string{"idsvr-a", "idsvr-b"} {
			idsvr := &v1alpha1.IdentityServer{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			}
			Expect(k8sClient.Create(ctx, idsvr)).To(Succeed())
		}

		list := &v1alpha1.IdentityServerList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
		Expect(list.Items).To(HaveLen(2))
	})

	It("should delete an IdentityServer", func() {
		idsvr := &v1alpha1.IdentityServer{
			ObjectMeta: metav1.ObjectMeta{Name: "to-delete", Namespace: namespace},
		}
		Expect(k8sClient.Create(ctx, idsvr)).To(Succeed())
		Expect(k8sClient.Delete(ctx, idsvr)).To(Succeed())

		fetched := &v1alpha1.IdentityServer{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "to-delete", Namespace: namespace}, fetched)
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})

func generateName() string {
	return fmt.Sprintf("%d-%d", GinkgoRandomSeed(), nsCounter.Add(1))
}
