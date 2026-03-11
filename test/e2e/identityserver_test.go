package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

var _ = Describe("IdentityServer", Ordered, func() {
	const testNs = "curity-e2e"

	vars := map[string]interface{}{
		"name":     "test-idsvr",
		"replicas": 2,
	}

	BeforeAll(func() {
		By("creating test namespace")
		_, err := utils.Run("kubectl", "create", "namespace", testNs)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("removing test namespace")
		_, err := utils.Run("kubectl", "delete", "namespace", testNs, "--ignore-not-found")
		Expect(err).NotTo(HaveOccurred())
	})

	Context("operator", func() {
		It("should create IdentityServer instance", func() {
			By("applying IdentityServer CR")
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityserver.yaml", testNs, vars)

			idsvr := &v1alpha1.IdentityServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vars["name"].(string),
					Namespace: testNs,
				},
			}

			By("waiting for resource to be stored")
			utils.WaitForResource(idsvr, func() bool {
				return idsvr.Name != ""
			}, "30s", "1s")

			By("taking YAML snapshot")
			utils.MatchYAMLResource(idsvr, "idsvr", "instance")
		})
	})
})
