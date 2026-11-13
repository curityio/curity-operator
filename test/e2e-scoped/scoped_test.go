package e2escoped

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/test/utils"
)

var _ = Describe("Namespace-scoped operator", Ordered, func() {
	const clusterName = "scoped"
	ctx := context.Background()

	It("installs without any cluster-scoped objects", func() {
		manifest, err := utils.Run("helm", "get", "manifest", releaseName, "--namespace", watchedNS)
		Expect(err).NotTo(HaveOccurred())

		var kinds []string
		for _, line := range strings.Split(manifest, "\n") {
			if kind, ok := strings.CutPrefix(line, "kind: "); ok {
				kinds = append(kinds, strings.TrimSpace(kind))
			}
		}
		Expect(kinds).To(ContainElements("Role", "RoleBinding", "Deployment", "ServiceAccount"))
		Expect(kinds).NotTo(ContainElement(BeElementOf("ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition")))
	})

	It("tells the operator to watch only its own namespace", func() {
		env, err := utils.Run("kubectl", "get", "deployment", deploymentName, "-n", watchedNS, "-o",
			`jsonpath={.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="WATCH_NAMESPACE")].value}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(Equal(watchedNS))
	})

	It("grants the service account access only in its own namespace", func() {
		sa, err := utils.Run("kubectl", "get", "deployment", deploymentName, "-n", watchedNS,
			"-o", "jsonpath={.spec.template.spec.serviceAccountName}")
		Expect(err).NotTo(HaveOccurred())
		Expect(sa).NotTo(BeEmpty())

		expectations := []struct {
			args []string
			want string
		}{
			{[]string{"list", "secrets", "-n", watchedNS}, "yes"},
			{[]string{"create", "deployments.apps", "-n", watchedNS}, "yes"},
			{[]string{"watch", "identityserverclusters.curity.io", "-n", watchedNS}, "yes"},
			{[]string{"list", "secrets", "-n", otherNS}, "no"},
			{[]string{"create", "deployments.apps", "-n", otherNS}, "no"},
			{[]string{"watch", "identityserverclusters.curity.io", "-n", otherNS}, "no"},
			{[]string{"list", "secrets", "--all-namespaces"}, "no"},
		}
		for _, e := range expectations {
			Expect(canI(sa, e.args...)).To(Equal(e.want), "kubectl auth can-i %s", strings.Join(e.args, " "))
		}
	})

	It("reconciles resources in its own namespace and ignores other namespaces", func() {
		for _, ns := range []string{watchedNS, otherNS} {
			recordCluster(ns, clusterName)
			utils.ApplyFixtureTemplate("./test/e2e/fixtures/identityservercluster.yaml", ns,
				map[string]interface{}{"name": clusterName, "namespace": ns})
		}

		By("waiting for the operator to take ownership of the cluster in " + watchedNS)
		Eventually(func(g Gomega) {
			var c v1alpha1.IdentityServerCluster
			g.Expect(k8s.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: watchedNS}, &c)).To(Succeed())
			g.Expect(c.Finalizers).To(ContainElement(v1alpha1.ClusterFinalizer))
			g.Expect(c.Status.Conditions).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())

		By("checking that the cluster in " + otherNS + " stays untouched")
		Consistently(func(g Gomega) {
			var c v1alpha1.IdentityServerCluster
			g.Expect(k8s.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: otherNS}, &c)).To(Succeed())
			g.Expect(c.Finalizers).To(BeEmpty())
			g.Expect(c.Status.Conditions).To(BeEmpty())
		}, 20*time.Second, interval).Should(Succeed())
	})

	It("logs no permission errors", func() {
		logs, err := utils.Run("kubectl", "logs", "deployment/"+deploymentName, "-n", watchedNS, "-c", "manager")
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring("watching selected namespaces only"))
		Expect(strings.ToLower(logs)).NotTo(ContainSubstring("forbidden"))
	})
})
