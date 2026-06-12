package controller_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/curityio/curity-operator/internal/controller"
)

// genTLSPEM returns a self-signed leaf as (tls.crt, tls.key) PEM for envtest.
func genTLSPEM() (crt, key []byte) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "envtest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func convertKSClusterObj(ns, name, sourceRef string) *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.IdentityServerClusterSpec{
			Version: "11.0",
			ConvertKeystore: []v1alpha1.ConvertKeystoreItem{{
				SourceTLS: v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: sourceRef},
			}},
		},
	}
}

var _ = Describe("convertKeystore wiring", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond
	var ns string

	BeforeEach(func() {
		ns = nodeTestNamespace()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("converts the source TLS Secret into the env Secret with the right labels", func() {
		crt, key := genTLSPEM()
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "src-tls", Namespace: ns},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{"tls.crt": crt, "tls.key": key},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, convertKSClusterObj(ns, "ck1", "src-tls"))).To(Succeed())

		out := &corev1.Secret{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ck1-convert-ks-env", Namespace: ns}, out)).To(Succeed())
			g.Expect(out.Data).To(HaveKey("SSL_SERVER_KEY"))
		}, timeout, interval).Should(Succeed())

		Expect(out.Labels["curity.io/managed"]).To(Equal("true"))
		Expect(out.Labels["curity.io/cluster"]).To(Equal("ck1"))
		Expect(out.Labels["curity.io/component"]).To(Equal("convert-ks"))
		Expect(out.Annotations["curity.io/config-type"]).To(Equal("env"))
		Expect(out.OwnerReferences).To(HaveLen(1))

		// Cluster condition reflects success.
		Eventually(func() metav1.ConditionStatus {
			cl := &v1alpha1.IdentityServerCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ck1", Namespace: ns}, cl); err != nil {
				return metav1.ConditionUnknown
			}
			c := apimeta.FindStatusCondition(cl.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady)
			if c == nil {
				return metav1.ConditionUnknown
			}
			return c.Status
		}, timeout, interval).Should(Equal(metav1.ConditionTrue))
	})

	It("defers a runtime node's Deployment until the source exists, then creates it", func() {
		// Runtime-only cluster (no admin) so the cluster.xml gate is skipped and the
		// convert-before-deploy gate is the deciding factor. Source is missing first.
		Expect(k8sClient.Create(ctx, convertKSClusterObj(ns, "ck2", "src-tls-2"))).To(Succeed())
		testCreateNode(ns, "ck2-rt", v1alpha1.NodeTypeRuntime, "ck2")

		deployName := controller.OwnedResourceName("ck2", "ck2-rt")
		// While the source is missing, the Deployment must NOT be created.
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: ns}, &appsv1.Deployment{})
			return apierrors.IsNotFound(err)
		}, 3*time.Second, interval).Should(BeTrue(), "Deployment must be deferred while SSL_SERVER_KEY is unavailable")

		// Provide the source → conversion succeeds → gate opens → Deployment created.
		crt, key := genTLSPEM()
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "src-tls-2", Namespace: ns},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{"tls.crt": crt, "tls.key": key},
		})).To(Succeed())

		// Cluster must publish the env Secret first (diagnostic isolation).
		Eventually(func(g Gomega) {
			out := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ck2-convert-ks-env", Namespace: ns}, out)).To(Succeed())
			g.Expect(out.Data).To(HaveKey("SSL_SERVER_KEY"))
		}, timeout, interval).Should(Succeed())

		// Then the gate opens and the Deployment is created.
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: ns}, &appsv1.Deployment{})
		}, 60*time.Second, interval).Should(Succeed())

		// Guards the bug where the status enum omitted "env": the post-Deployment
		// status write was rejected forever, freezing the node at
		// Ready/WaitingForConvertKeystore. The node must record the env Secret and
		// leave the defer reason.
		Eventually(func(g Gomega) {
			n := &v1alpha1.IdentityServerNode{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ck2-rt", Namespace: ns}, n)).To(Succeed())
			var found bool
			for _, amr := range n.Status.AppliedManagedResources {
				if amr.Name == "ck2-convert-ks-env" && amr.ConfigType == controller.ConfigTypeEnv {
					found = true
				}
			}
			g.Expect(found).To(BeTrue(), "node status must record the env Secret (configType=env)")
			ready := apimeta.FindStatusCondition(n.Status.Conditions, v1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).NotTo(Equal("WaitingForConvertKeystore"), "node must leave the defer state once the keystore is published")
		}, 60*time.Second, interval).Should(Succeed())
	})
})
