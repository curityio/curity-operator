package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// TestClusterConfigStale covers the Deployment-deferral gate's observed-state
// check. The ClusterConfigReady condition alone is not enough: a cluster spec
// edit wakes the node and cluster reconcilers at once, so until the cluster
// reconciler wins the race the condition still reads True from the previous
// generation. Deferring on a stale cluster-config Secret closes that window,
// and needs no other controller to have acted first.
func TestClusterConfigStale(t *testing.T) {
	const (
		ns          = "ns"
		clusterName = "c1"
		adminNode   = "admin"
	)

	// Admin node supplies the name and config port that feed the hash.
	adminNodes := []v1alpha1.IdentityServerNode{{
		ObjectMeta: metav1.ObjectMeta{Name: adminNode, Namespace: ns},
		Spec: v1alpha1.IdentityServerNodeSpec{
			Type: v1alpha1.NodeTypeAdmin,
			Role: "admin-role",
		},
	}}

	clusterWithPackages := func(pkgs []v1alpha1.PackageSpec) *v1alpha1.IdentityServerCluster {
		return &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: v1alpha1.IdentityServerClusterSpec{
				Version:  "11.0",
				Packages: pkgs,
			},
		}
	}

	// Hash of the cluster before any package is added — what the Secret would
	// carry after a completed regeneration.
	baseCluster := clusterWithPackages(nil)
	baseHash := computeClusterConfigHash(baseCluster, adminNode, findAdminConfigPort(adminNodes))

	configSecret := func(data []byte, hash string) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName + clusterConfigSecretSuffix,
				Namespace: ns,
			},
			Data: map[string][]byte{clusterConfigKey: data},
		}
		if hash != "" {
			s.Annotations = map[string]string{"curity.io/cluster-config-hash": hash}
		}
		return s
	}

	realXML := []byte("<config/>")

	// Branch A fixtures: a cluster whose adminCredentials point at a Secret
	// carrying CONFIG_ENCRYPTION_KEY, and the hash Branch A stamps for it.
	const credsName = "c1-creds"
	clusterWithCreds := func() *v1alpha1.IdentityServerCluster {
		c := clusterWithPackages(nil)
		c.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
			ValueFrom: v1alpha1.CredentialsValueFrom{
				SecretKeyRef: v1alpha1.SecretKeyRefSource{Name: credsName},
			},
		}
		return c
	}
	credsSecret := func(key string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credsName, Namespace: ns},
			Data:       map[string][]byte{"CONFIG_ENCRYPTION_KEY": []byte(key)},
		}
	}
	keyHash := func(key string) string {
		h := sha256.Sum256([]byte(key))
		return hex.EncodeToString(h[:])
	}
	withKeyHash := func(s *corev1.Secret, hash string) *corev1.Secret {
		s.Annotations["curity.io/encryption-key-hash"] = hash
		return s
	}

	tests := []struct {
		name    string
		cluster *v1alpha1.IdentityServerCluster
		secret  *corev1.Secret
		creds   *corev1.Secret
		want    bool
	}{
		{
			// Only reached with ClusterConfigReady already True, which the
			// cluster reconciler sets only after seeing a ready Secret — so a
			// missing Secret means it was deleted, not that it is yet to appear.
			name:    "deleted Secret is stale",
			cluster: baseCluster,
			secret:  nil,
			want:    true,
		},
		{
			name:    "placeholder data means regeneration is under way",
			cluster: baseCluster,
			secret:  configSecret([]byte(clusterConfigPlaceholder), baseHash),
			want:    true,
		},
		{
			name:    "un-annotated Secret defers to the condition",
			cluster: baseCluster,
			secret:  configSecret(realXML, ""),
			want:    false,
		},
		{
			name:    "hash matching the spec is fresh",
			cluster: baseCluster,
			secret:  configSecret(realXML, baseHash),
			want:    false,
		},
		{
			// The race: spec already carries the package, the Secret still
			// records the pre-edit hash, and ClusterConfigReady has not been
			// flipped yet. Without this the node reconciler would write the
			// Deployment the gate exists to hold back.
			name: "hash left behind by a spec edit is stale",
			cluster: clusterWithPackages([]v1alpha1.PackageSpec{{
				Source:    v1alpha1.PackageSource{URL: "https://example.test/plugin.zip"},
				MountPath: "/opt/idsvr/plugins/p1",
			}}),
			secret: configSecret(realXML, baseHash),
			want:   true,
		},
		{
			// Branch A's race: computeClusterConfigHash omits the key on
			// purpose, so a rotated CONFIG_ENCRYPTION_KEY leaves the config hash
			// matching. Only the encryption-key-hash annotation gives it away.
			name:    "rotated encryption key is stale",
			cluster: clusterWithCreds(),
			secret:  withKeyHash(configSecret(realXML, baseHash), keyHash("old-key")),
			creds:   credsSecret("new-key"),
			want:    true,
		},
		{
			name:    "matching encryption key is fresh",
			cluster: clusterWithCreds(),
			secret:  withKeyHash(configSecret(realXML, baseHash), keyHash("same-key")),
			creds:   credsSecret("same-key"),
			want:    false,
		},
		{
			// Mirrors Branch A's empty-guard: a credentials Secret not yet
			// visible in cache must not read as a rotation.
			name:    "missing credentials Secret defers to the condition",
			cluster: clusterWithCreds(),
			secret:  withKeyHash(configSecret(realXML, baseHash), keyHash("old-key")),
			creds:   nil,
			want:    false,
		},
		{
			// No stored key hash means nothing to compare against — and no
			// credentials read is attempted at all.
			name:    "un-annotated key hash is fresh",
			cluster: clusterWithCreds(),
			secret:  configSecret(realXML, baseHash),
			creds:   credsSecret("whatever"),
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScheme(t)
			if err := v1alpha1.AddToScheme(s); err != nil {
				t.Fatalf("adding v1alpha1 to scheme: %v", err)
			}

			builder := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.cluster)
			if tc.secret != nil {
				builder = builder.WithObjects(tc.secret)
			}
			if tc.creds != nil {
				builder = builder.WithObjects(tc.creds)
			}

			r := &IdentityServerNodeReconciler{Client: builder.Build(), Scheme: s}
			got, err := r.clusterConfigStale(context.Background(), tc.cluster, adminNodes)
			if err != nil {
				t.Fatalf("clusterConfigStale() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("clusterConfigStale() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClusterConfigStale_ReadErrorPropagates asserts a non-NotFound read failure
// surfaces to the caller rather than being reported as "not stale". Swallowing
// it would open the same window the helper exists to close: a transient API
// error would let the node write a Deployment against unverified config.
func TestClusterConfigStale_ReadErrorPropagates(t *testing.T) {
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"},
		Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
	}
	readErr := errors.New("boom")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return readErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	r := &IdentityServerNodeReconciler{Client: c, Scheme: s}
	stale, err := r.clusterConfigStale(context.Background(), cluster, nil)
	if !errors.Is(err, readErr) {
		t.Fatalf("clusterConfigStale() error = %v, want %v", err, readErr)
	}
	if stale {
		t.Errorf("clusterConfigStale() = true on read error, want false alongside the error")
	}
}
