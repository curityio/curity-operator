package controller_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// Per-key CR names (cl-<slug>, node-<slug>) so a CEL miss on one key
// surfaces as a regression rather than getting masked by "already exists"
// from a prior iteration.
var _ = Describe("podLabels reserved-key CEL", func() {
	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	for _, reserved := range []string{"curity.io/owned-by", "curity.io/cluster", "curity.io/role"} {
		reserved := reserved
		slug := strings.ReplaceAll(reserved, "/", "-")
		slug = strings.ReplaceAll(slug, ".", "-")

		It("should reject IdentityServerCluster with "+reserved+" in podLabels", func() {
			cluster := &v1alpha1.IdentityServerCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cl-" + slug, Namespace: ns},
				Spec: v1alpha1.IdentityServerClusterSpec{
					Version:   "11.0",
					PodLabels: map[string]string{reserved: "user-attempted-hijack"},
				},
			}
			err := k8sClient.Create(ctx, cluster)
			Expect(err).To(HaveOccurred(), "podLabels with %q must be rejected", reserved)
			Expect(err.Error()).To(ContainSubstring("operator-owned curity.io/* keys"))
			Expect(err.Error()).To(ContainSubstring("spec.podLabels"))
		})

		It("should reject IdentityServerNode with "+reserved+" in podLabels", func() {
			node := &v1alpha1.IdentityServerNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node-" + slug, Namespace: ns},
				Spec: v1alpha1.IdentityServerNodeSpec{
					Type:                     v1alpha1.NodeTypeRuntime,
					Role:                     "runtime-role",
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "any-cluster"},
					Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 8443},
					PodLabels:                map[string]string{reserved: "user-attempted-hijack"},
				},
			}
			err := k8sClient.Create(ctx, node)
			Expect(err).To(HaveOccurred(), "podLabels with %q must be rejected", reserved)
			Expect(err.Error()).To(ContainSubstring("operator-owned curity.io/* keys"))
			Expect(err.Error()).To(ContainSubstring("spec.podLabels"))
		})
	}

	// Narrow block (3 keys), not the whole curity.io/ prefix.
	It("should accept non-reserved curity.io/* and app.kubernetes.io/* keys in podLabels", func() {
		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-accept", Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version: "11.0",
				PodLabels: map[string]string{
					"curity.io/team":          "platform",
					"app.kubernetes.io/owner": "identity-eng",
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	})

	// Drift guard for the triplicated key set (cluster CEL, node CEL, loop above).
	It("cluster and node CRDs enforce the identical reserved-key CEL rule", func() {
		const wantRule = "self.all(k, !(k in ['curity.io/owned-by', 'curity.io/cluster', 'curity.io/role']))"

		clusterRule := podLabelsCELRule(ctx, "identityserverclusters.curity.io")
		nodeRule := podLabelsCELRule(ctx, "identityservernodes.curity.io")

		Expect(clusterRule).To(Equal(wantRule),
			"cluster CRD CEL rule drifted from canonical")
		Expect(nodeRule).To(Equal(wantRule),
			"node CRD CEL rule drifted from canonical")
		Expect(clusterRule).To(Equal(nodeRule),
			"cluster and node CRDs disagree on reserved-key set")
	})
})

// podLabelsCELRule reads the live CRD's spec.podLabels CEL rule string.
func podLabelsCELRule(ctx context.Context, crdName string) string {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
		return "<get failed: " + err.Error() + ">"
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if len(versions) == 0 {
		return "<no versions>"
	}
	v, _ := versions[0].(map[string]interface{})
	validations, _, _ := unstructured.NestedSlice(v,
		"schema", "openAPIV3Schema",
		"properties", "spec",
		"properties", "podLabels",
		"x-kubernetes-validations")
	if len(validations) == 0 {
		return "<no validations>"
	}
	first, _ := validations[0].(map[string]interface{})
	rule, _ := first["rule"].(string)
	return rule
}
