package controller

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

const (
	convertKeystoreSecretSuffix = "-convert-ks-env"
	componentConvertKeystore    = "convert-ks"

	annotationConvertKsInputHash  = "curity.io/convert-ks-input-hash"
	annotationConvertKsOutputHash = "curity.io/convert-ks-output-hash"

	// Folded into the input hash so an encoder/format change regenerates existing
	// output Secrets on upgrade. Bump on any encoding change. v2: keystore password
	// "" → "default" to match what Curity's loader expects.
	convertKeystoreEncodingVersion = "v2"

	reasonConvertKeystoreReady = "Ready"
	reasonSourceSecretMissing  = "SourceSecretMissing"
	reasonSourceKeysMissing    = "SourceKeysMissing"
	reasonConversionFailed     = "ConversionFailed"
	reasonPartiallyReady       = "PartiallyReady"
)

// Curity opens SSL_SERVER_KEY with the fixed password "default" — its
// server-keystore config carries no password field, so the password is a fixed
// convention. Legacy (3DES, with MAC) is the broadly JDK-compatible PKCS#12 form.
// Bump convertKeystoreEncodingVersion on change.
var convertKeystoreEncoder = pkcs12.Legacy

const convertKeystorePassword = "default"

func convertKeystoreSecretName(clusterName string) string {
	return clusterName + convertKeystoreSecretSuffix
}

// convertTLSToKeystore turns a PEM TLS keypair into the value Curity expects in
// SSL_SERVER_KEY: base64 of a PKCS#12 keystore holding the private key and the
// full certificate chain from tls.crt (leaf + any intermediates). Returns a
// permanent error for an invalid/mismatched/unsupported keypair.
func convertTLSToKeystore(crtPEM, keyPEM []byte) ([]byte, error) {
	// Validates the pair and splits tls.crt into leaf + intermediates.
	pair, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid TLS keypair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parsing leaf certificate: %w", err)
	}
	var caCerts []*x509.Certificate
	for _, der := range pair.Certificate[1:] {
		ca, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("parsing intermediate certificate: %w", err)
		}
		caCerts = append(caCerts, ca)
	}
	pfx, err := convertKeystoreEncoder.Encode(pair.PrivateKey, leaf, caCerts, convertKeystorePassword)
	if err != nil {
		return nil, fmt.Errorf("encoding PKCS#12: %w", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(pfx)), nil
}

// hashConvertKeystoreData fingerprints the output Secret's data deterministically
// so tampering (a value edit) is detected even when the input is unchanged.
func hashConvertKeystoreData(data map[string][]byte) string {
	h := sha256.New()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ensureConvertedKeystores reconciles spec.convertKeystore: it converts each
// item's source TLS keypair in-process and publishes the results into one
// cluster-owned env Secret (<cluster>-convert-ks-env), which the managed-config
// machinery then injects via envFrom. No Job, no token, no extra RBAC.
func (r *IdentityServerClusterReconciler) ensureConvertedKeystores(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) error {
	secretName := convertKeystoreSecretName(cluster.Name)

	// 1. Feature unused: clear any stale output and drop the condition.
	if len(cluster.Spec.ConvertKeystore) == 0 {
		apimeta.RemoveStatusCondition(&cluster.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady)
		var existing corev1.Secret
		err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &existing)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting convert-keystore Secret: %w", err)
		}
		if len(existing.Data) > 0 {
			existing.Data = map[string][]byte{}
			delete(existing.Annotations, annotationConvertKsInputHash)
			delete(existing.Annotations, annotationConvertKsOutputHash)
			if err := r.Update(ctx, &existing); err != nil {
				return fmt.Errorf("clearing convert-keystore Secret: %w", err)
			}
		}
		return nil
	}

	// 2. Read + pre-check every source. A missing source (or missing tls.crt/tls.key)
	// is transient: set the condition and wait — never wipe an existing output a
	// running pod depends on. Sorted for deterministic hashing.
	items := append([]v1alpha1.ConvertKeystoreItem(nil), cluster.Spec.ConvertKeystore...)
	sort.Slice(items, func(i, j int) bool {
		return items[i].SourceTLS.KeyName < items[j].SourceTLS.KeyName
	})

	type readItem struct {
		src      v1alpha1.ConvertKeystoreSource
		crt, key []byte
	}
	reads := make([]readItem, 0, len(items))
	for _, item := range items {
		src := item.SourceTLS
		var srcSecret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: src.FromSecretRef, Namespace: cluster.Namespace}, &srcSecret); err != nil {
			if apierrors.IsNotFound(err) {
				r.setConvertKeystoreCondition(cluster, metav1.ConditionFalse, reasonSourceSecretMissing,
					fmt.Sprintf("source Secret %q for keyName %q not found", src.FromSecretRef, src.KeyName))
				return nil
			}
			return fmt.Errorf("getting source Secret %q: %w", src.FromSecretRef, err)
		}
		crt, key := srcSecret.Data["tls.crt"], srcSecret.Data["tls.key"]
		if len(crt) == 0 || len(key) == 0 {
			r.setConvertKeystoreCondition(cluster, metav1.ConditionFalse, reasonSourceKeysMissing,
				fmt.Sprintf("source Secret %q is missing tls.crt and/or tls.key for keyName %q", src.FromSecretRef, src.KeyName))
			return nil
		}
		reads = append(reads, readItem{src: src, crt: crt, key: key})
	}

	// 3. Input hash over encoding version + Curity version + sorted inputs, so a
	// Curity upgrade or encoder change regenerates the output.
	ih := sha256.New()
	ih.Write([]byte(convertKeystoreEncodingVersion))
	ih.Write([]byte{0})
	ih.Write([]byte(cluster.Spec.Version))
	ih.Write([]byte{0})
	for _, ri := range reads {
		ih.Write([]byte(ri.src.KeyName))
		ih.Write([]byte{0})
		ih.Write([]byte(ri.src.Cert))
		ih.Write([]byte{0})
		ih.Write(ri.crt)
		ih.Write([]byte{0})
		ih.Write(ri.key)
		ih.Write([]byte{0})
	}
	desiredInputHash := hex.EncodeToString(ih.Sum(nil))

	// 4. Read the existing output Secret to decide whether a (re)write is needed.
	var existing corev1.Secret
	existingErr := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &existing)
	if existingErr != nil && !apierrors.IsNotFound(existingErr) {
		return fmt.Errorf("getting convert-keystore Secret: %w", existingErr)
	}

	// 5. Convert per item every reconcile (cheap) and derive the condition from the
	// CURRENT inputs — never from a stamped hash, which would mask a now-failing
	// conversion as success. A permanent failure isolates its item; the rest publish.
	// Cross-item collisions CEL can't express (cert(A)==keyName(B), cert(A)==cert(B))
	// land here. Items are sorted by keyName, so the first producer wins
	// deterministically; producedBy names both items in the failure.
	desired := make(map[string][]byte, len(reads)*2)
	producedBy := make(map[string]string, len(reads)*2)
	var failures []string
	addKey := func(name, itemKey string, val []byte) bool {
		if prev, exists := producedBy[name]; exists {
			failures = append(failures, fmt.Sprintf("output env var %q is produced by both item %q and item %q", name, prev, itemKey))
			return false
		}
		producedBy[name] = itemKey
		desired[name] = val
		return true
	}
	for _, ri := range reads {
		val, err := convertTLSToKeystore(ri.crt, ri.key)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ri.src.KeyName, err))
			continue
		}
		if !addKey(ri.src.KeyName, ri.src.KeyName, val) {
			continue
		}
		if ri.src.Cert != "" {
			addKey(ri.src.Cert, ri.src.KeyName, ri.crt)
		}
	}

	// 6. Total failure: don't publish an empty Secret or overwrite a good one — an
	// empty output reads back as a "0 keys" success and wiping breaks dependent pods.
	// Preserve any existing output and surface the failure.
	if len(desired) == 0 {
		r.setConvertKeystoreFailure(cluster, reasonConversionFailed,
			fmt.Sprintf("0/%d keystore(s) converted; failed: %s", len(reads), strings.Join(failures, "; ")))
		return nil
	}

	// 7. (Re)write only on a real change. PKCS#12 salt makes bytes differ every
	// encode, so writing them would churn a rollout. Write on: missing, input change,
	// key-set change, or tamper (stored hash ≠ data); else keep the existing bytes.
	needWrite := apierrors.IsNotFound(existingErr) ||
		existing.Annotations[annotationConvertKsInputHash] != desiredInputHash ||
		!sameKeySet(existing.Data, desired) ||
		existing.Annotations[annotationConvertKsOutputHash] != hashConvertKeystoreData(existing.Data)
	if needWrite {
		outputHash := hashConvertKeystoreData(desired)
		secret := &corev1.Secret{}
		secret.Name = secretName
		secret.Namespace = cluster.Namespace
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.Labels == nil {
				secret.Labels = map[string]string{}
			}
			secret.Labels[LabelManagedConfig] = "true"
			secret.Labels[AnnotationClusterScope] = cluster.Name // also a label: findClusterForSecret maps on it
			secret.Labels["curity.io/component"] = componentConvertKeystore
			secret.Labels["app.kubernetes.io/managed-by"] = "curity-operator"
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}
			secret.Annotations[AnnotationConfigType] = ConfigTypeEnv
			secret.Annotations[AnnotationClusterScope] = cluster.Name // scope discovery to this cluster
			secret.Annotations[annotationConvertKsInputHash] = desiredInputHash
			secret.Annotations[annotationConvertKsOutputHash] = outputHash
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = desired
			return controllerutil.SetControllerReference(cluster, secret, r.Scheme)
		}); err != nil {
			return fmt.Errorf("writing convert-keystore Secret: %w", err)
		}
	}

	// 8. Condition + event — event only on a real (re)write or transition.
	if len(failures) > 0 {
		r.setConvertKeystoreFailure(cluster, reasonPartiallyReady,
			fmt.Sprintf("%d/%d keystore(s) converted; failed: %s", len(reads)-len(failures), len(reads), strings.Join(failures, "; ")))
		return nil
	}
	r.setConvertKeystoreCondition(cluster, metav1.ConditionTrue, reasonConvertKeystoreReady,
		fmt.Sprintf("published %d converted key(s) to Secret %q", len(desired), secretName))
	if needWrite {
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ConvertKeystoreReady",
			"published %d converted key(s) to Secret %q", len(desired), secretName)
	}
	return nil
}

func (r *IdentityServerClusterReconciler) setConvertKeystoreCondition(cluster *v1alpha1.IdentityServerCluster, status metav1.ConditionStatus, reason, message string) {
	setCondition(&cluster.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady, status, reason, message, cluster.Generation)
}

// setConvertKeystoreFailure sets a False condition and emits a Warning only on
// change — the failure path runs every reconcile, so undeduped events would spam.
func (r *IdentityServerClusterReconciler) setConvertKeystoreFailure(cluster *v1alpha1.IdentityServerCluster, reason, message string) {
	prev := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady)
	changed := prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason || prev.Message != message
	r.setConvertKeystoreCondition(cluster, metav1.ConditionFalse, reason, message)
	if changed {
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ConvertKeystoreFailed", "%s", message)
	}
}

// applyConvertKeystoreDegraded surfaces a permanent convert failure as Degraded,
// but never clobbers a higher-priority cause (MultipleAdminNodes, node degradation)
// that already owns it — ConvertKeystoreReady=False carries the detail regardless.
func applyConvertKeystoreDegraded(cluster *v1alpha1.IdentityServerCluster) {
	ck := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionConvertKeystoreReady)
	if ck == nil || ck.Status != metav1.ConditionFalse ||
		(ck.Reason != reasonConversionFailed && ck.Reason != reasonPartiallyReady) {
		return
	}
	if deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded); deg != nil && deg.Status == metav1.ConditionTrue {
		return
	}
	setCondition(&cluster.Status.Conditions, v1alpha1.ConditionDegraded,
		metav1.ConditionTrue, ck.Reason, ck.Message, cluster.Generation)
}

// sameKeySet compares only keys, not values: PKCS#12 salt makes re-encoded bytes
// differ, so only a changed key set is a real content change.
func sameKeySet(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
