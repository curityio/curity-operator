package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func newConvertKSReconciler(t *testing.T, objs ...client.Object) *IdentityServerClusterReconciler {
	t.Helper()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &IdentityServerClusterReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
}

func tlsSourceSecret(t *testing.T, name, ns string) *corev1.Secret {
	t.Helper()
	crt, key, _, _ := makeSelfSigned(t, "src-"+name, false)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": crt, "tls.key": key},
	}
}

func convertKSCluster(name, ns string, items ...v1alpha1.ConvertKeystoreSource) *v1alpha1.IdentityServerCluster {
	cl := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.2.0"},
	}
	for _, it := range items {
		cl.Spec.ConvertKeystore = append(cl.Spec.ConvertKeystore, v1alpha1.ConvertKeystoreItem{SourceTLS: it})
	}
	return cl
}

func ckCondition(cl *v1alpha1.IdentityServerCluster) *metav1.Condition {
	return apimeta.FindStatusCondition(cl.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady)
}

func TestEnsureConvertedKeystores_Success(t *testing.T) {
	ctx := context.Background()
	src := tlsSourceSecret(t, "curity-local-tls", "ns")
	cl := convertKSCluster("demo", "ns", v1alpha1.ConvertKeystoreSource{
		KeyName: "SSL_SERVER_KEY", FromSecretRef: "curity-local-tls", Cert: "SSL_SERVER_CERT",
	})
	r := newConvertKSReconciler(t, src, cl)

	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("ensureConvertedKeystores: %v", err)
	}

	var out corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
		t.Fatalf("output Secret not created: %v", err)
	}
	if len(out.Data["SSL_SERVER_KEY"]) == 0 {
		t.Error("SSL_SERVER_KEY missing from output Secret")
	}
	if string(out.Data["SSL_SERVER_CERT"]) != string(src.Data["tls.crt"]) {
		t.Error("SSL_SERVER_CERT should be the raw source tls.crt")
	}
	if out.Labels[LabelManagedConfig] != "true" || out.Labels["curity.io/cluster"] != "demo" || out.Labels["curity.io/component"] != componentConvertKeystore {
		t.Errorf("output Secret missing expected labels: %v", out.Labels)
	}
	if out.Annotations[AnnotationConfigType] != ConfigTypeEnv || out.Annotations[AnnotationClusterScope] != "demo" {
		t.Errorf("output Secret missing expected annotations: %v", out.Annotations)
	}
	if out.Annotations[annotationConvertKsInputHash] == "" || out.Annotations[annotationConvertKsOutputHash] == "" {
		t.Error("output Secret missing input/output hash annotations")
	}
	if len(out.OwnerReferences) != 1 || out.OwnerReferences[0].Name != "demo" {
		t.Errorf("output Secret should be owned by the cluster, got %v", out.OwnerReferences)
	}
	if c := ckCondition(cl); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("expected ConvertKeystoreReady=True, got %v", c)
	}
}

func TestEnsureConvertedKeystores_MissingSource(t *testing.T) {
	ctx := context.Background()
	cl := convertKSCluster("demo", "ns", v1alpha1.ConvertKeystoreSource{
		KeyName: "SSL_SERVER_KEY", FromSecretRef: "does-not-exist",
	})
	r := newConvertKSReconciler(t, cl)

	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("expected nil (transient handled internally), got %v", err)
	}
	if c := ckCondition(cl); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonSourceSecretMissing {
		t.Errorf("expected False/SourceSecretMissing, got %v", c)
	}
	var out corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err == nil {
		t.Error("output Secret must NOT be created while source is missing")
	}
}

func TestEnsureConvertedKeystores_EmptyListClearsData(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-convert-ks-env", Namespace: "ns",
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: map[string]string{annotationConvertKsInputHash: "old", annotationConvertKsOutputHash: "old"},
		},
		Data: map[string][]byte{"SSL_SERVER_KEY": []byte("stale")},
	}
	cl := convertKSCluster("demo", "ns") // empty convertKeystore
	cl.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionConvertKeystoreReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}}
	r := newConvertKSReconciler(t, existing, cl)

	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("ensureConvertedKeystores: %v", err)
	}
	var out corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("expected data cleared, got %v", out.Data)
	}
	if ckCondition(cl) != nil {
		t.Error("ConvertKeystoreReady condition should be removed when feature is unused")
	}
}

func TestEnsureConvertedKeystores_CrossItemCollision(t *testing.T) {
	ctx := context.Background()
	src := tlsSourceSecret(t, "src", "ns")
	// Two items with the same cert env name → cross-item collision the CEL can't
	// cheaply catch; detected at build time.
	cl := convertKSCluster("demo", "ns",
		v1alpha1.ConvertKeystoreSource{KeyName: "KEY_A", FromSecretRef: "src", Cert: "SHARED_CERT"},
		v1alpha1.ConvertKeystoreSource{KeyName: "KEY_B", FromSecretRef: "src", Cert: "SHARED_CERT"},
	)
	r := newConvertKSReconciler(t, src, cl)
	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("ensureConvertedKeystores: %v", err)
	}
	c := ckCondition(cl)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("expected False condition for collision, got %v", c)
	}
	if c.Reason != reasonPartiallyReady && c.Reason != reasonConversionFailed {
		t.Errorf("expected PartiallyReady/ConversionFailed, got %q", c.Reason)
	}
}

// Guards the bug where a stamped input hash on an empty Secret made the next
// reconcile read back "0 keys" as Ready: a source with present-but-unparseable
// tls.crt/tls.key must report ConversionFailed and STAY there, publishing nothing.
func TestEnsureConvertedKeystores_TotalFailureStaysFailed(t *testing.T) {
	ctx := context.Background()
	garbage := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "garbage", Namespace: "ns"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("not a cert"), "tls.key": []byte("not a key")},
	}
	cl := convertKSCluster("demo", "ns", v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "garbage"})
	r := newConvertKSReconciler(t, garbage, cl)

	for i := 0; i < 3; i++ {
		if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if c := ckCondition(cl); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonConversionFailed {
			t.Fatalf("reconcile %d: expected False/ConversionFailed, got %v", i, c)
		}
		var out corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err == nil {
			t.Fatalf("reconcile %d: no output Secret may be published on total failure, got data %v", i, out.Data)
		}
	}
}

// A converted keystore must be byte-stable across reconciles: PKCS#12 carries
// random salt, so re-encoding produces different bytes — but with no input change
// we must NOT rewrite the Secret, or every reconcile would churn a pod rollout.
func TestEnsureConvertedKeystores_SteadyStateNoChurn(t *testing.T) {
	ctx := context.Background()
	src := tlsSourceSecret(t, "src", "ns")
	cl := convertKSCluster("demo", "ns", v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "src"})
	r := newConvertKSReconciler(t, src, cl)
	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("first convert: %v", err)
	}
	var first corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &first); err != nil {
		t.Fatalf("get: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		var out corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if string(out.Data["SSL_SERVER_KEY"]) != string(first.Data["SSL_SERVER_KEY"]) {
			t.Fatalf("reconcile %d: keystore bytes changed without an input change (rollout churn)", i)
		}
		if out.ResourceVersion != first.ResourceVersion {
			t.Fatalf("reconcile %d: Secret rewritten (rv %s != %s) with no input change", i, out.ResourceVersion, first.ResourceVersion)
		}
		if c := ckCondition(cl); c == nil || c.Status != metav1.ConditionTrue {
			t.Fatalf("reconcile %d: expected steady Ready, got %v", i, c)
		}
	}
}

func TestEnsureConvertedKeystores_SelfHealsTamper(t *testing.T) {
	ctx := context.Background()
	src := tlsSourceSecret(t, "src", "ns")
	cl := convertKSCluster("demo", "ns", v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "src"})
	r := newConvertKSReconciler(t, src, cl)
	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("first convert: %v", err)
	}

	// Tamper: overwrite the published value.
	var out corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	out.Data["SSL_SERVER_KEY"] = []byte("TAMPERED")
	if err := r.Update(ctx, &out); err != nil {
		t.Fatalf("tamper update: %v", err)
	}

	// Reconcile again → output-hash mismatch → reverted.
	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("second convert: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(out.Data["SSL_SERVER_KEY"]) == "TAMPERED" {
		t.Error("tampered value should have been reverted (self-heal)")
	}
	if out.Annotations[annotationConvertKsOutputHash] != hashConvertKeystoreData(out.Data) {
		t.Error("output-hash annotation should match current data after self-heal")
	}
}

// Removing ONE item from a multi-item list drops only that key from the env
// Secret; the remaining key stays present and valid, and the condition stays
// Ready. Guards the key-SET-change branch of the write gate.
func TestEnsureConvertedKeystores_PartialItemRemoval(t *testing.T) {
	ctx := context.Background()
	srcA := tlsSourceSecret(t, "srcA", "ns")
	srcB := tlsSourceSecret(t, "srcB", "ns")
	cl := convertKSCluster("demo", "ns",
		v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "srcA"},
		v1alpha1.ConvertKeystoreSource{KeyName: "SECOND_KEY", FromSecretRef: "srcB"},
	)
	r := newConvertKSReconciler(t, srcA, srcB, cl)
	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("first convert: %v", err)
	}
	var out corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := out.Data["SSL_SERVER_KEY"]; !ok {
		t.Fatal("SSL_SERVER_KEY missing after first convert")
	}
	if _, ok := out.Data["SECOND_KEY"]; !ok {
		t.Fatal("SECOND_KEY missing after first convert")
	}

	// Remove the second item.
	cl.Spec.ConvertKeystore = cl.Spec.ConvertKeystore[:1]
	if err := r.ensureConvertedKeystores(ctx, cl); err != nil {
		t.Fatalf("second convert: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Name: "demo-convert-ks-env", Namespace: "ns"}, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := out.Data["SECOND_KEY"]; ok {
		t.Error("SECOND_KEY should have been removed when its item was dropped")
	}
	val, ok := out.Data["SSL_SERVER_KEY"]
	if !ok {
		t.Fatal("SSL_SERVER_KEY must survive removal of an unrelated item")
	}
	// The surviving key must still be a valid PKCS#12 (decodable with the password
	// Curity uses).
	der, err := base64.StdEncoding.DecodeString(string(val))
	if err != nil {
		t.Fatalf("surviving key not valid base64: %v", err)
	}
	if _, _, _, err := pkcs12.DecodeChain(der, convertKeystorePassword); err != nil {
		t.Fatalf("surviving key not a valid PKCS#12: %v", err)
	}
	if c := ckCondition(cl); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready after partial removal, got %v", c)
	}
}

// applyConvertKeystoreDegraded must surface a convert failure as Degraded only
// when nothing higher-priority (MultipleAdminNodes, node degradation) already owns
// it, so the user-facing reason isn't shadowed.
func TestApplyConvertKeystoreDegraded(t *testing.T) {
	gen := int64(1)
	mk := func(ckStatus metav1.ConditionStatus, ckReason string, deg *metav1.Condition) *v1alpha1.IdentityServerCluster {
		cl := &v1alpha1.IdentityServerCluster{}
		cl.Generation = gen
		setCondition(&cl.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady, ckStatus, ckReason, "msg", gen)
		if deg != nil {
			setCondition(&cl.Status.Conditions, v1alpha1.ConditionDegraded, deg.Status, deg.Reason, deg.Message, gen)
		}
		return cl
	}
	degraded := func(cl *v1alpha1.IdentityServerCluster) *metav1.Condition {
		return apimeta.FindStatusCondition(cl.Status.Conditions, v1alpha1.ConditionDegraded)
	}

	t.Run("sets Degraded when none present", func(t *testing.T) {
		cl := mk(metav1.ConditionFalse, reasonConversionFailed, nil)
		applyConvertKeystoreDegraded(cl)
		if d := degraded(cl); d == nil || d.Status != metav1.ConditionTrue || d.Reason != reasonConversionFailed {
			t.Errorf("expected Degraded=True/ConversionFailed, got %v", d)
		}
	})
	t.Run("does not clobber a higher-priority Degraded", func(t *testing.T) {
		cl := mk(metav1.ConditionFalse, reasonConversionFailed,
			&metav1.Condition{Status: metav1.ConditionTrue, Reason: "MultipleAdminNodes", Message: "two admins"})
		applyConvertKeystoreDegraded(cl)
		if d := degraded(cl); d == nil || d.Reason != "MultipleAdminNodes" {
			t.Errorf("higher-priority Degraded reason must survive, got %v", d)
		}
	})
	t.Run("leaves Degraded untouched when convert is Ready", func(t *testing.T) {
		cl := mk(metav1.ConditionTrue, reasonConvertKeystoreReady,
			&metav1.Condition{Status: metav1.ConditionFalse, Reason: "NotDegraded", Message: "ok"})
		applyConvertKeystoreDegraded(cl)
		if d := degraded(cl); d == nil || d.Status != metav1.ConditionFalse || d.Reason != "NotDegraded" {
			t.Errorf("Degraded must stay False/NotDegraded, got %v", d)
		}
	})
}

// One source TLS Secret referenced by two clusters maps to BOTH — the watch
// fan-out that re-converts every dependent cluster on a shared-cert rotation.
func TestFindClusterForSecret_SharedSourceMultipleClusters(t *testing.T) {
	ctx := context.Background()
	clA := convertKSCluster("a", "ns", v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "shared"})
	clB := convertKSCluster("b", "ns", v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "shared"})
	r := newConvertKSReconciler(t, clA, clB)

	src := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "ns"}}
	got := r.findClusterForSecret(ctx, src)
	names := map[string]bool{}
	for _, g := range got {
		names[g.Name] = true
	}
	if len(got) != 2 || !names["a"] || !names["b"] {
		t.Errorf("expected shared source to map to both clusters a and b, got %v", got)
	}
}

func TestFindClusterForSecret_ConvertKeystoreSource(t *testing.T) {
	ctx := context.Background()
	cl := convertKSCluster("demo", "ns", v1alpha1.ConvertKeystoreSource{KeyName: "SSL_SERVER_KEY", FromSecretRef: "curity-local-tls"})
	r := newConvertKSReconciler(t, cl)

	// A source TLS Secret (no curity.io/cluster label) maps to the cluster.
	src := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "curity-local-tls", Namespace: "ns"}}
	got := r.findClusterForSecret(ctx, src)
	if len(got) != 1 || got[0].Name != "demo" {
		t.Errorf("expected source Secret to map to cluster demo, got %v", got)
	}

	// An unrelated Secret maps to nothing.
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "ns"}}
	if got := r.findClusterForSecret(ctx, other); len(got) != 0 {
		t.Errorf("expected no mapping for unrelated Secret, got %v", got)
	}
}

// makeSelfSigned returns PEM (cert, key) for a fresh self-signed leaf using the
// given key type. When parent != nil the cert is signed by it (for chains).
func makeSelfSigned(t *testing.T, cn string, ecKey bool) (crtPEM, keyPEM []byte, cert *x509.Certificate, priv any) {
	t.Helper()
	var pub, signer any
	switch ecKey {
	case true:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		pub, signer, priv = &k.PublicKey, k, k
	default:
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		pub, signer, priv = &k.PublicKey, k, k
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	crtPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return crtPEM, keyPEM, cert, priv
}

func TestConvertTLSToKeystore_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		ec   bool
	}{{"RSA", false}, {"ECDSA", true}} {
		t.Run(tc.name, func(t *testing.T) {
			crtPEM, keyPEM, wantCert, _ := makeSelfSigned(t, "test", tc.ec)

			out, err := convertTLSToKeystore(crtPEM, keyPEM)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			// Curity expects base64(PKCS#12), opened with the "default" password
			// (convertKeystorePassword) that Curity's server-keystore loader uses.
			der, err := base64.StdEncoding.DecodeString(string(out))
			if err != nil {
				t.Fatalf("output is not valid base64: %v", err)
			}
			gotKey, gotCert, caCerts, err := pkcs12.DecodeChain(der, convertKeystorePassword)
			if err != nil {
				t.Fatalf("decoding produced PKCS#12 with %q password: %v", convertKeystorePassword, err)
			}
			if gotKey == nil {
				t.Fatal("no private key in keystore")
			}
			if !gotCert.Equal(wantCert) {
				t.Errorf("leaf cert mismatch")
			}
			if len(caCerts) != 0 {
				t.Errorf("expected no CA certs for a single self-signed leaf, got %d", len(caCerts))
			}
		})
	}
}

func TestConvertTLSToKeystore_IncludesChain(t *testing.T) {
	// Build leaf signed by an intermediate, and put leaf+intermediate in tls.crt.
	// The keystore must carry the intermediate as a CA cert.
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "intermediate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	crtPEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
	)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	out, err := convertTLSToKeystore(crtPEM, keyPEM)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	der, _ := base64.StdEncoding.DecodeString(string(out))
	_, _, caCerts, err := pkcs12.DecodeChain(der, convertKeystorePassword)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(caCerts) != 1 {
		t.Fatalf("expected 1 CA cert (the intermediate) in the keystore, got %d", len(caCerts))
	}
}

func TestConvertTLSToKeystore_PermanentErrors(t *testing.T) {
	crtA, _, _, _ := makeSelfSigned(t, "a", false)
	_, keyB, _, _ := makeSelfSigned(t, "b", false)

	t.Run("key/cert mismatch", func(t *testing.T) {
		if _, err := convertTLSToKeystore(crtA, keyB); err == nil {
			t.Fatal("expected error for mismatched key/cert")
		}
	})
	t.Run("garbage PEM", func(t *testing.T) {
		if _, err := convertTLSToKeystore([]byte("not a cert"), []byte("not a key")); err == nil {
			t.Fatal("expected error for garbage input")
		}
	})
}

func TestHashConvertKeystoreData_StableAndSensitive(t *testing.T) {
	a := map[string][]byte{"X": []byte("1"), "Y": []byte("2")}
	b := map[string][]byte{"Y": []byte("2"), "X": []byte("1")} // same content, different order
	if hashConvertKeystoreData(a) != hashConvertKeystoreData(b) {
		t.Error("hash should be order-independent")
	}
	c := map[string][]byte{"X": []byte("1"), "Y": []byte("changed")}
	if hashConvertKeystoreData(a) == hashConvertKeystoreData(c) {
		t.Error("hash should change when a value changes (tamper detection)")
	}
}
