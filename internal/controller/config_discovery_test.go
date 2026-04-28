package controller

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	return s
}

// --- discoverManagedResources ---

func TestDiscoverConfigResources_ReturnsOnlyLabeledResources(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "managed-cm",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
			},
			Data: map[string]string{"config.xml": "<config/>"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unlabeled-cm",
				Namespace: "ns",
			},
			Data: map[string]string{"other.xml": "<other/>"},
		},
	).Build()

	configs, _, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].Name != "managed-cm" {
		t.Errorf("expected name %q, got %q", "managed-cm", configs[0].Name)
	}
}

func TestDiscoverConfigResources_ExcludesClusterConfigSecret(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-1-cluster-config",
				Namespace: "ns",
				Labels: map[string]string{
					LabelManagedConfig:    "true",
					"curity.io/component": "cluster-config",
				},
			},
			Data: map[string][]byte{"cluster.xml": []byte("<cluster/>")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "user-secret",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
			},
			Data: map[string][]byte{"creds.xml": []byte("<creds/>")},
		},
	).Build()

	configs, _, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].Name != "user-secret" {
		t.Errorf("expected name %q, got %q", "user-secret", configs[0].Name)
	}
	if !configs[0].IsSecret {
		t.Error("expected IsSecret=true")
	}
}

func TestDiscoverConfigResources_DefaultsConfigTypeToBase(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "no-annotation",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
			},
			Data: map[string]string{"base.xml": "<base/>"},
		},
	).Build()

	configs, _, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].ConfigType != ConfigTypeBase {
		t.Errorf("expected config type %q, got %q", ConfigTypeBase, configs[0].ConfigType)
	}
}

func TestDiscoverConfigResources_LicenseConfigType(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "license-secret",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{
					AnnotationConfigType: ConfigTypeLicense,
				},
			},
			Data: map[string][]byte{"license.json": []byte(`{"key":"val"}`)},
		},
	).Build()

	configs, _, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].ConfigType != ConfigTypeLicense {
		t.Errorf("expected config type %q, got %q", ConfigTypeLicense, configs[0].ConfigType)
	}
}

func TestDiscoverConfigResources_UnknownConfigType_SkipsOffenderNotNeighbors(t *testing.T) {
	// One bad neighbor must NOT cascade: the bad CM goes into `skipped`
	// with a stable reason (used for EventRecorder dedup downstream), the
	// good neighbor still comes back in `configs`, and the overall call
	// succeeds (no hard error).
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bad-type",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{
					AnnotationConfigType: "bsae", // typo
				},
			},
			Data: map[string]string{"config.xml": "<config/>"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "good-neighbor",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{
					AnnotationConfigType: ConfigTypeBase,
				},
			},
			Data: map[string]string{"init.xml": "<config/>"},
		},
	).Build()

	configs, skipped, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("expected no hard error; skipping-the-offender must not cascade: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("want 1 valid config (good-neighbor), got %d: %v", len(configs), configs)
	}
	if configs[0].Name != "good-neighbor" {
		t.Errorf("want good-neighbor in results, got %q", configs[0].Name)
	}
	if len(skipped) != 1 {
		t.Fatalf("want 1 skipped (bad-type), got %d: %v", len(skipped), skipped)
	}
	if skipped[0].Name != "bad-type" || skipped[0].Kind != "ConfigMap" {
		t.Errorf("unexpected skipped: %+v", skipped[0])
	}
	// Reason must be stable (contains the invalid value verbatim, not the
	// rotating bytes of a network error) so EventRecorder dedups retries
	// into one series per bad resource.
	if !strings.Contains(skipped[0].Reason, "bsae") {
		t.Errorf("skipped reason should identify the bad value, got: %q", skipped[0].Reason)
	}
}

func TestDiscoverConfigResources_SortedByName(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "z-config",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "a-config",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "m-secret",
				Namespace: "ns",
				Labels:    map[string]string{LabelManagedConfig: "true"},
			},
		},
	).Build()

	configs, _, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 3 {
		t.Fatalf("expected 3 configs, got %d", len(configs))
	}
	if configs[0].Name != "a-config" || configs[1].Name != "m-secret" || configs[2].Name != "z-config" {
		t.Errorf("expected sorted order [a-config, m-secret, z-config], got [%s, %s, %s]",
			configs[0].Name, configs[1].Name, configs[2].Name)
	}
}

func TestDiscoverConfigResources_EmptyNamespace(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	configs, _, err := discoverManagedResources(ctx, c, "empty-ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(configs))
	}
}

// --- discoverManagedResources non-mutation guarantee ---

// Operator-internal cluster-config Secrets are tagged
// curity.io/component=cluster-config and must be excluded from discovery
// even when (mistakenly) also tagged curity.io/managed=true.
func TestDiscoverManagedResources_ExcludesClusterConfigSecretEvenWhenManaged(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-1-cluster-config",
			Namespace: "ns",
			Labels: map[string]string{
				LabelManagedConfig:    "true",
				"curity.io/component": "cluster-config",
			},
		},
		Data: map[string][]byte{"cluster.xml": []byte("<config/>")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sec).Build()

	configs, skipped, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("cluster-config Secret must be excluded from discovery, got %+v", configs)
	}
	if len(skipped) != 0 {
		t.Errorf("cluster-config Secret must not appear as a skipped (UnknownConfigType) resource, got %+v", skipped)
	}
}

// Regression guard against re-introducing a writeback into discovery.
func TestDiscoverManagedResources_DoesNotWriteAnnotation(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-annotation-cm",
			Namespace: "ns",
			Labels:    map[string]string{LabelManagedConfig: "true"},
		},
		Data: map[string]string{"x.xml": "<x/>"},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-annotation-secret",
			Namespace: "ns",
			Labels:    map[string]string{LabelManagedConfig: "true"},
		},
		Data: map[string][]byte{"k": []byte("v")},
	}

	updates := 0
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm, sec).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				switch obj.(type) {
				case *corev1.ConfigMap, *corev1.Secret:
					updates++
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.(type) {
				case *corev1.ConfigMap, *corev1.Secret:
					updates++
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	configs, skipped, err := discoverManagedResources(ctx, c, "ns", "test-cluster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updates != 0 {
		t.Errorf("expected zero CM/Secret writes from discovery, got %d", updates)
	}
	if len(skipped) != 0 {
		t.Errorf("expected no skipped resources, got %+v", skipped)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 discovered configs, got %d (%+v)", len(configs), configs)
	}
	for _, cfg := range configs {
		if cfg.ConfigType != ConfigTypeBase {
			t.Errorf("config %q: expected ConfigType=%q (read-time default), got %q",
				cfg.Name, ConfigTypeBase, cfg.ConfigType)
		}
	}

	var liveCM corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &liveCM); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if v := liveCM.Annotations[AnnotationConfigType]; v != "" {
		t.Errorf("CM annotation must remain absent; got %q", v)
	}
	var liveSec corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(sec), &liveSec); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if v := liveSec.Annotations[AnnotationConfigType]; v != "" {
		t.Errorf("Secret annotation must remain absent; got %q", v)
	}
}

// --- mountPathForConfigType ---

func TestMountPathForConfigType_Base(t *testing.T) {
	got, isLeaf := mountPathForConfigType(ConfigTypeBase)
	if got != MountPathBase {
		t.Errorf("expected %q, got %q", MountPathBase, got)
	}
	if isLeaf {
		t.Error("base is a directory mount-base, not leaf")
	}
}

func TestMountPathForConfigType_License(t *testing.T) {
	got, isLeaf := mountPathForConfigType(ConfigTypeLicense)
	if got != MountPathLicense {
		t.Errorf("expected %q, got %q", MountPathLicense, got)
	}
	if isLeaf {
		t.Error("license is a directory mount-base, not leaf")
	}
}

func TestMountPathForConfigType_Logging(t *testing.T) {
	got, isLeaf := mountPathForConfigType(ConfigTypeLogging)
	if got != MountPathLogging {
		t.Errorf("expected %q, got %q", MountPathLogging, got)
	}
	if !isLeaf {
		t.Error("logging is a leaf path (single-file mount), not directory base")
	}
}

func TestMountPathForConfigType_DefaultsToBase(t *testing.T) {
	got, isLeaf := mountPathForConfigType("something-unknown")
	if got != MountPathBase {
		t.Errorf("expected %q for unknown type, got %q", MountPathBase, got)
	}
	if isLeaf {
		t.Error("unknown should fall back to directory base, not leaf")
	}
}

// --- shouldMountConfig ---

func TestShouldMountConfig_AdminWithAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeAdmin, true, ConfigTypeBase) {
		t.Error("expected true: admin node should mount base config when admin exists")
	}
}

func TestShouldMountConfig_RuntimeWithAdminExists(t *testing.T) {
	if shouldMountConfig(v1alpha1.NodeTypeRuntime, true, ConfigTypeBase) {
		t.Error("expected false: runtime should not mount base config when admin exists")
	}
}

func TestShouldMountConfig_AdminNoAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeAdmin, false, ConfigTypeBase) {
		t.Error("expected true: admin node should mount base config when no admin exists")
	}
}

func TestShouldMountConfig_RuntimeNoAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeRuntime, false, ConfigTypeBase) {
		t.Error("expected true: runtime should mount base config when no admin exists")
	}
}

func TestShouldMountConfig_LoggingMountsOnRuntimeWithAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeRuntime, true, ConfigTypeLogging) {
		t.Error("expected true: logging is per-pod and must mount on runtime even when admin exists")
	}
}

func TestShouldMountConfig_LoggingMountsOnAdminWithAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeAdmin, true, ConfigTypeLogging) {
		t.Error("expected true: logging mounts on admin")
	}
}

func TestShouldMountConfig_LicenseFollowsBasePattern(t *testing.T) {
	if shouldMountConfig(v1alpha1.NodeTypeRuntime, true, ConfigTypeLicense) {
		t.Error("expected false: license follows base admin-routing rule, not logging override")
	}
}

// --- computeConfigHash ---

func TestComputeConfigHash_Deterministic(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm-1", Data: map[string][]byte{"a.xml": []byte("<a/>")}},
		{Name: "cm-2", Data: map[string][]byte{"b.xml": []byte("<b/>")}},
	}
	hash1 := computeConfigHash(configs)
	hash2 := computeConfigHash(configs)
	if hash1 != hash2 {
		t.Errorf("expected deterministic hash, got %q and %q", hash1, hash2)
	}
	if hash1 == "" {
		t.Error("expected non-empty hash")
	}
}

func TestComputeConfigHash_DifferentData(t *testing.T) {
	configs1 := []DiscoveredManagedResource{
		{Name: "cm-1", Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	configs2 := []DiscoveredManagedResource{
		{Name: "cm-1", Data: map[string][]byte{"a.xml": []byte("<b/>")}},
	}
	if computeConfigHash(configs1) == computeConfigHash(configs2) {
		t.Error("expected different hashes for different data")
	}
}

func TestComputeConfigHash_DifferentKindSameName(t *testing.T) {
	cm := []DiscoveredManagedResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	secret := []DiscoveredManagedResource{
		{Name: "foo", IsSecret: true, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	if computeConfigHash(cm) == computeConfigHash(secret) {
		t.Error("expected different hashes for ConfigMap vs Secret with same name and data")
	}
}

func TestComputeConfigHash_DifferentConfigType(t *testing.T) {
	base := []DiscoveredManagedResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	license := []DiscoveredManagedResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeLicense, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	if computeConfigHash(base) == computeConfigHash(license) {
		t.Error("expected different hashes for different config types")
	}
}

func TestComputeConfigHash_EmptyConfigs(t *testing.T) {
	if got := computeConfigHash([]DiscoveredManagedResource{}); got != "" {
		t.Errorf("expected empty string for empty configs, got %q", got)
	}
}

func TestComputeConfigHash_NilConfigs(t *testing.T) {
	if got := computeConfigHash(nil); got != "" {
		t.Errorf("expected empty string for nil configs, got %q", got)
	}
}

// --- Live-Object invariant on scan/discover outputs ---
//
// emitScopeAnnotationEvents and the cluster reconciler's status writer both
// nil-guard on the Object field. If the upstream constructor leaves Object
// nil, downstream Warning Events vanish silently. These tests prove the
// invariant holds end-to-end at the boundary of the scan/discover helpers.

func TestScanConfigScopeIssues_ObjectIsAlwaysPopulated(t *testing.T) {
	// Both ScopeIssue variants (Empty, Unknown) must carry the live
	// CM/Secret pointer in Object — required by emitScopeAnnotationEvents to
	// produce a Warning Event with the right involvedObject.
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "empty-cm", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: ""},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "unknown-cm", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: "this,typo"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "unknown-sec", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: "this,typo"},
			},
		},
	).Build()
	existing := map[string]struct{}{"this": {}}

	issues, err := scanConfigScopeIssues(ctx, c, "ns", existing)
	if err != nil {
		t.Fatalf("scanConfigScopeIssues: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("expected 3 issues (empty CM + unknown CM + unknown Secret), got %d: %+v", len(issues), issues)
	}
	for i, is := range issues {
		if is.Object == nil {
			t.Errorf("issues[%d].Object must be non-nil (otherwise downstream emit is silently dropped); got %+v", i, is)
		}
	}
}

func TestDiscoverConfigResources_ObjectIsAlwaysPopulated(t *testing.T) {
	// Both DiscoveredManagedResource and SkippedResource carry Object.
	// detectDuplicateKeys propagates DiscoveredManagedResource.Object into
	// DuplicateKeyOwner.Object; the cluster reconciler's UnknownConfigType
	// emitter reads SkippedResource.Object. Either being nil silently
	// drops the Warning Event downstream.
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ok-cm", Namespace: "ns",
				Labels: map[string]string{LabelManagedConfig: "true"},
			},
			Data: map[string]string{"x.xml": "<x/>"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ok-sec", Namespace: "ns",
				Labels: map[string]string{LabelManagedConfig: "true"},
			},
			Data: map[string][]byte{"y.xml": []byte("<y/>")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "bad-cm", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationConfigType: "bogus"},
			},
		},
	).Build()

	configs, skipped, err := discoverManagedResources(ctx, c, "ns", "this")
	if err != nil {
		t.Fatalf("discoverManagedResources: %v", err)
	}
	for i, cfg := range configs {
		if cfg.Object == nil {
			t.Errorf("configs[%d].Object must be non-nil; got %+v", i, cfg)
		}
	}
	for i, sk := range skipped {
		if sk.Object == nil {
			t.Errorf("skipped[%d].Object must be non-nil; got %+v", i, sk)
		}
	}
}

// --- configVolumeName ---

func TestConfigVolumeName_ConfigMap(t *testing.T) {
	got := configVolumeName(false, "my-config")
	if got != "cfg-cm-my-config" {
		t.Errorf("expected %q, got %q", "cfg-cm-my-config", got)
	}
}

func TestConfigVolumeName_Secret(t *testing.T) {
	got := configVolumeName(true, "my-secret")
	if got != "cfg-secret-my-secret" {
		t.Errorf("expected %q, got %q", "cfg-secret-my-secret", got)
	}
}

func TestConfigVolumeName_NoCollisionSameNameDifferentKind(t *testing.T) {
	cmName := configVolumeName(false, "foo")
	secretName := configVolumeName(true, "foo")
	if cmName == secretName {
		t.Errorf("ConfigMap and Secret with same name should have different volume names, got %q", cmName)
	}
}

func TestConfigVolumeName_LongNameTruncated(t *testing.T) {
	longName := "very-long-resource-name-that-exceeds-the-kubernetes-volume-name-limit-of-63-characters"
	got := configVolumeName(false, longName)
	if len(got) > maxVolumeNameLength {
		t.Errorf("expected volume name ≤ %d chars, got %d: %q", maxVolumeNameLength, len(got), got)
	}
	if len(got) == 0 {
		t.Error("expected non-empty volume name")
	}
}

func TestConfigVolumeName_LongNameUnique(t *testing.T) {
	name1 := configVolumeName(false, "very-long-resource-name-that-exceeds-the-limit-aaa")
	name2 := configVolumeName(false, "very-long-resource-name-that-exceeds-the-limit-bbb")
	if name1 == name2 {
		t.Errorf("different long names should produce different volume names")
	}
}

// --- buildAppliedManagedResource ---

func TestBuildAppliedManagedResource_ConfigMap(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm-1", IsSecret: false, ConfigType: ConfigTypeBase},
	}
	status := buildAppliedManagedResources(configs)
	if len(status) != 1 {
		t.Fatalf("expected 1 status entry, got %d", len(status))
	}
	if status[0].Kind != "ConfigMap" {
		t.Errorf("expected Kind %q, got %q", "ConfigMap", status[0].Kind)
	}
}

func TestBuildAppliedManagedResource_Secret(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "sec-1", IsSecret: true, ConfigType: ConfigTypeLicense},
	}
	status := buildAppliedManagedResources(configs)
	if len(status) != 1 {
		t.Fatalf("expected 1 status entry, got %d", len(status))
	}
	if status[0].Kind != "Secret" {
		t.Errorf("expected Kind %q, got %q", "Secret", status[0].Kind)
	}
	if status[0].ConfigType != ConfigTypeLicense {
		t.Errorf("expected ConfigType %q, got %q", ConfigTypeLicense, status[0].ConfigType)
	}
}

func TestBuildAppliedManagedResource_EmptyConfigs(t *testing.T) {
	status := buildAppliedManagedResources(nil)
	if status != nil {
		t.Errorf("expected nil for empty configs, got %v", status)
	}
}

// --- mountFilename ---

func TestMountFilename_ConfigMap(t *testing.T) {
	got := mountFilename(false, "my-cm", "config.xml")
	want := "cm_my-cm_config.xml"
	if got != want {
		t.Errorf("mountFilename(false, \"my-cm\", \"config.xml\") = %q, want %q", got, want)
	}
}

func TestMountFilename_Secret(t *testing.T) {
	got := mountFilename(true, "my-secret", "license.json")
	want := "secret_my-secret_license.json"
	if got != want {
		t.Errorf("mountFilename(true, \"my-secret\", \"license.json\") = %q, want %q", got, want)
	}
}

func TestMountFilename_UnambiguousSeparator(t *testing.T) {
	// "cm_foo_bar-baz.xml" can only mean ConfigMap "foo", key "bar-baz.xml"
	// because K8s resource names cannot contain underscores.
	a := mountFilename(false, "foo", "bar-baz.xml")
	b := mountFilename(false, "foo-bar", "baz.xml")
	if a == b {
		t.Errorf("expected different filenames for different (name, key) pairs, both got %q", a)
	}
}

func TestMountFilename_EmptyInputs(t *testing.T) {
	// Empty resource name and key should not panic.
	got := mountFilename(false, "", "")
	if got != "cm__" {
		t.Errorf("mountFilename(false, \"\", \"\") = %q, want %q", got, "cm__")
	}
}

func TestMountFilename_KeyWithUnderscore(t *testing.T) {
	// Underscores in the data key are valid. The "unambiguous" guarantee is
	// about the resource name boundary (names can't contain _), not about
	// parsing the key portion. cm_foo_my_config.xml is always ConfigMap "foo"
	// with key "my_config.xml" because "foo" can't contain underscores.
	got := mountFilename(false, "foo", "my_config.xml")
	if got != "cm_foo_my_config.xml" {
		t.Errorf("got %q, want %q", got, "cm_foo_my_config.xml")
	}
}

func TestMountFilename_KeyWithPathSeparator(t *testing.T) {
	// Data keys with path separators are technically valid in ConfigMaps.
	// mountFilename passes them through — Kubernetes creates nested
	// directories for SubPath values containing slashes.
	got := mountFilename(false, "foo", "sub/dir/config.xml")
	if got != "cm_foo_sub/dir/config.xml" {
		t.Errorf("got %q, want %q", got, "cm_foo_sub/dir/config.xml")
	}
}

// --- detectDuplicateKeys ---

func TestDetectDuplicateKeys_NoConfigs(t *testing.T) {
	warnings := detectDuplicateKeys(nil)
	if len(warnings) != 0 {
		t.Errorf("expected nil warnings for nil configs, got %v", warnings)
	}
}

func TestDetectDuplicateKeys_DistinctKeys(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"b.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for distinct keys, got %v", warnings)
	}
}

func TestDetectDuplicateKeys_SameKeyDifferentConfigType(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"foo.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeLicense, Data: map[string][]byte{"foo.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for same key different config type, got %v", warnings)
	}
}

func TestDetectDuplicateKeys_SameKeySameType(t *testing.T) {
	// Use real CM pointers so DiscoveredManagedResource.Object is populated;
	// this exercises the full Object propagation chain into DuplicateKeyOwner
	// and lets us assert the invariant downstream emitters depend on.
	cmA := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm-a", Namespace: "ns"}}
	cmB := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm-b", Namespace: "ns"}}
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"base-config.xml": []byte("<a/>")}, Object: cmA},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"base-config.xml": []byte("<b/>")}, Object: cmB},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Message, "base-config.xml") || !strings.Contains(warnings[0].Message, "ConfigMap/cm-a") || !strings.Contains(warnings[0].Message, "ConfigMap/cm-b") {
		t.Errorf("warning should mention filename and both resources with kind, got: %s", warnings[0].Message)
	}

	// Invariant: every Owner has a non-nil Object. The cluster reconciler's
	// emit path nil-guards on Object — if a future refactor leaves Object
	// nil here, the Warning Event would be silently dropped and only the
	// status entry would surface, hiding the issue. Assert the invariant
	// holds end-to-end through detectDuplicateKeys.
	if len(warnings[0].Owners) < 2 {
		t.Fatalf("invariant: DuplicateKeyWarning must have >= 2 owners; got %d", len(warnings[0].Owners))
	}
	for i, owner := range warnings[0].Owners {
		if owner.Object == nil {
			t.Errorf("Owner[%d].Object must be non-nil (otherwise downstream emit is silently dropped); got %+v", i, owner)
		}
	}
}

func TestDetectDuplicateKeys_ConfigMapAndSecret(t *testing.T) {
	// Even though mountFilename() produces distinct paths (cm_* vs secret_*),
	// having the same data key in both a ConfigMap and Secret of the same
	// config type is likely a user mistake — it may produce unexpected merged
	// configuration. The warning is about user intent, not mount path collisions.
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"config.xml": []byte("<a/>")}},
		{Name: "sec-a", IsSecret: true, ConfigType: ConfigTypeBase, Data: map[string][]byte{"config.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for CM+Secret same key, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Message, "ConfigMap/cm-a") || !strings.Contains(warnings[0].Message, "Secret/sec-a") {
		t.Errorf("warning should mention both resources with kind, got: %s", warnings[0].Message)
	}
}

func TestDetectDuplicateKeys_SingleResourceMultipleKeys(t *testing.T) {
	// A single resource with multiple distinct keys should produce 0 warnings.
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{
			"a.xml": []byte("<a/>"),
			"b.xml": []byte("<b/>"),
			"c.xml": []byte("<c/>"),
		}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings for single resource with multiple keys, got %v", warnings)
	}
}

func TestDetectDuplicateKeys_ThreeResourcesSameKey(t *testing.T) {
	// Three resources sharing the same key should produce 1 warning listing all three.
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"shared.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"shared.xml": []byte("<b/>")}},
		{Name: "cm-c", ConfigType: ConfigTypeBase, Data: map[string][]byte{"shared.xml": []byte("<c/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Message, "ConfigMap/cm-a") || !strings.Contains(warnings[0].Message, "ConfigMap/cm-b") || !strings.Contains(warnings[0].Message, "ConfigMap/cm-c") {
		t.Errorf("warning should list all three resources with kind, got: %s", warnings[0].Message)
	}
}

func TestDetectDuplicateKeys_MultipleDistinctDuplicates(t *testing.T) {
	// Two different keys are each duplicated — should produce 2 sorted warnings.
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"x.xml": []byte("<a/>"), "y.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"x.xml": []byte("<b/>"), "y.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings for two distinct duplicated keys, got %d: %v", len(warnings), warnings)
	}
	// Warnings are sorted — "x.xml" before "y.xml".
	if !strings.Contains(warnings[0].Message, "x.xml") {
		t.Errorf("first warning should be about x.xml, got: %s", warnings[0].Message)
	}
	if !strings.Contains(warnings[1].Message, "y.xml") {
		t.Errorf("second warning should be about y.xml, got: %s", warnings[1].Message)
	}
}

func TestDetectDuplicateKeys_SkipsLoggingType(t *testing.T) {
	// Two logging-typed CMs with the same data key would otherwise trip
	// DuplicateConfigKey, double-reporting the same root cause that
	// DuplicateLoggingConfig already surfaces. detectDuplicateKeys must
	// skip logging configs to keep the issue count clean.
	configs := []DiscoveredManagedResource{
		{Name: "log-a", ConfigType: ConfigTypeLogging, Data: map[string][]byte{LoggingDataKey: []byte("<a/>")}},
		{Name: "log-b", ConfigType: ConfigTypeLogging, Data: map[string][]byte{LoggingDataKey: []byte("<b/>")}},
	}
	if got := detectDuplicateKeys(configs); len(got) != 0 {
		t.Errorf("expected zero DuplicateKeyWarnings for logging-typed duplicates; got %d: %v", len(got), got)
	}
}

// --- resolveConfigType ---

func TestResolveConfigType_EmptyDefaultsToBase(t *testing.T) {
	got, err := resolveConfigType(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ConfigTypeBase {
		t.Errorf("expected %q, got %q", ConfigTypeBase, got)
	}
}

func TestResolveConfigType_ExplicitBase(t *testing.T) {
	got, err := resolveConfigType(map[string]string{AnnotationConfigType: "base"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ConfigTypeBase {
		t.Errorf("expected %q, got %q", ConfigTypeBase, got)
	}
}

func TestResolveConfigType_License(t *testing.T) {
	got, err := resolveConfigType(map[string]string{AnnotationConfigType: "license"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ConfigTypeLicense {
		t.Errorf("expected %q, got %q", ConfigTypeLicense, got)
	}
}

func TestResolveConfigType_Logging(t *testing.T) {
	got, err := resolveConfigType(map[string]string{AnnotationConfigType: "logging"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ConfigTypeLogging {
		t.Errorf("expected %q, got %q", ConfigTypeLogging, got)
	}
}

func TestResolveConfigType_Unknown(t *testing.T) {
	_, err := resolveConfigType(map[string]string{AnnotationConfigType: "Logging"}) // case-mismatched
	if err == nil {
		t.Fatal("expected error for case-mismatched type, got nil")
	}
}

// --- validateLoggingResource ---

func TestValidateLoggingResource_CorrectSingleKey(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "ok", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")},
	}
	if msg, ok := validateLoggingResource(cfg); !ok {
		t.Errorf("expected valid, got: %s", msg)
	}
}

func TestValidateLoggingResource_EmptyValueIsValid(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "ok", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{LoggingDataKey: []byte("")},
	}
	if _, ok := validateLoggingResource(cfg); !ok {
		t.Error("expected empty value to pass shape validation")
	}
}

func TestValidateLoggingResource_ZeroKeys(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "empty", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{},
	}
	msg, ok := validateLoggingResource(cfg)
	if ok {
		t.Fatal("expected zero keys to fail validation")
	}
	if !strings.Contains(msg, "0 keys") {
		t.Errorf("message should mention 0 keys, got: %s", msg)
	}
}

func TestValidateLoggingResource_WrongKeyName(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "wrong", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{"config.xml": []byte("<Configuration/>")},
	}
	msg, ok := validateLoggingResource(cfg)
	if ok {
		t.Fatal("expected wrong key name to fail validation")
	}
	if !strings.Contains(msg, "config.xml") {
		t.Errorf("message should name the wrong key, got: %s", msg)
	}
}

func TestValidateLoggingResource_MultipleKeysIncludingLog4j2(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "extra", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{
			LoggingDataKey: []byte("<Configuration/>"),
			"notes.txt":    []byte("hello"),
		},
	}
	msg, ok := validateLoggingResource(cfg)
	if ok {
		t.Fatal("expected multiple keys to fail validation")
	}
	if !strings.Contains(msg, "2 keys") {
		t.Errorf("message should mention 2 keys, got: %s", msg)
	}
}

func TestValidateLoggingResource_MultipleKeysWithoutLog4j2(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "no-log", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{
			"a.xml": []byte("<a/>"),
			"b.xml": []byte("<b/>"),
		},
	}
	if _, ok := validateLoggingResource(cfg); ok {
		t.Fatal("expected multiple keys without log4j2.xml to fail validation")
	}
}

func TestValidateLoggingResource_CaseMismatchUppercaseL(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "case", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{"Log4j2.xml": []byte("<Configuration/>")},
	}
	if _, ok := validateLoggingResource(cfg); ok {
		t.Fatal("expected case-mismatched key to fail validation")
	}
}

func TestValidateLoggingResource_CaseMismatchAllUpper(t *testing.T) {
	cfg := DiscoveredManagedResource{
		Name: "shout", ConfigType: ConfigTypeLogging,
		Data: map[string][]byte{"LOG4J2.XML": []byte("<Configuration/>")},
	}
	if _, ok := validateLoggingResource(cfg); ok {
		t.Fatal("expected uppercase key to fail validation")
	}
}

// --- detectDuplicateLoggingConfigs ---

func newCMObject(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"}}
}

func newSecretObject(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"}}
}

func TestDetectDuplicateLoggingConfigs_None(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "log", ConfigType: ConfigTypeLogging, Object: newCMObject("log"),
			Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")}},
		{Name: "base", ConfigType: ConfigTypeBase, Object: newCMObject("base"),
			Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	if got := detectDuplicateLoggingConfigs(configs); got != nil {
		t.Errorf("expected nil for single logging resource, got %v", got)
	}
}

func TestDetectDuplicateLoggingConfigs_TwoApplicable(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "log-a", ConfigType: ConfigTypeLogging, Object: newCMObject("log-a"),
			Data: map[string][]byte{LoggingDataKey: []byte("<a/>")}},
		{Name: "log-b", ConfigType: ConfigTypeLogging, IsSecret: true, Object: newSecretObject("log-b"),
			Data: map[string][]byte{LoggingDataKey: []byte("<b/>")}},
	}
	got := detectDuplicateLoggingConfigs(configs)
	if len(got) != 2 {
		t.Fatalf("expected 2 owners, got %d", len(got))
	}
	// Sort: ConfigMap before Secret
	if got[0].Kind != "ConfigMap" || got[0].Name != "log-a" {
		t.Errorf("owners[0] expected ConfigMap/log-a, got %+v", got[0])
	}
	if got[1].Kind != "Secret" || got[1].Name != "log-b" {
		t.Errorf("owners[1] expected Secret/log-b, got %+v", got[1])
	}
}

func TestDetectDuplicateLoggingConfigs_IgnoresNonLogging(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "log", ConfigType: ConfigTypeLogging, Object: newCMObject("log"),
			Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")}},
		{Name: "other", ConfigType: ConfigTypeBase, Object: newCMObject("other"),
			Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")}}, // base, not counted
	}
	if got := detectDuplicateLoggingConfigs(configs); got != nil {
		t.Errorf("base resource with log4j2.xml key must not count; got %v", got)
	}
}

// --- applyLoggingValidations ---

func TestApplyLoggingValidations_ValidLoggingPasses(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "log", ConfigType: ConfigTypeLogging, Object: newCMObject("log"),
			Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")}},
	}
	filtered, issues := applyLoggingValidations(configs)
	if len(filtered) != 1 {
		t.Errorf("expected valid logging resource to pass through, got %d", len(filtered))
	}
	if len(issues) != 0 {
		t.Errorf("expected no issues, got %v", issues)
	}
}

func TestApplyLoggingValidations_InvalidExcludedAndReported(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "bad", ConfigType: ConfigTypeLogging, Object: newCMObject("bad"),
			Data: map[string][]byte{}}, // zero keys
		{Name: "base-x", ConfigType: ConfigTypeBase, Object: newCMObject("base-x"),
			Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	filtered, issues := applyLoggingValidations(configs)
	if len(filtered) != 1 || filtered[0].Name != "base-x" {
		t.Errorf("expected only base-x to remain, got %d configs: %v", len(filtered), filtered)
	}
	if len(issues) != 1 || issues[0].EventReason != EventReasonLoggingConfigInvalid {
		t.Errorf("expected one LoggingConfigInvalid issue, got %v", issues)
	}
}

func TestApplyLoggingValidations_DuplicatesExcludeBoth(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "log-a", ConfigType: ConfigTypeLogging, Object: newCMObject("log-a"),
			Data: map[string][]byte{LoggingDataKey: []byte("<a/>")}},
		{Name: "log-b", ConfigType: ConfigTypeLogging, Object: newCMObject("log-b"),
			Data: map[string][]byte{LoggingDataKey: []byte("<b/>")}},
		{Name: "base-y", ConfigType: ConfigTypeBase, Object: newCMObject("base-y"),
			Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	filtered, issues := applyLoggingValidations(configs)
	if len(filtered) != 1 || filtered[0].Name != "base-y" {
		t.Errorf("expected only base-y to remain, got %v", filtered)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 DuplicateLoggingConfig issues, got %d: %v", len(issues), issues)
	}
	for _, i := range issues {
		if i.EventReason != EventReasonDuplicateLoggingConfig {
			t.Errorf("expected DuplicateLoggingConfig reason, got %s", i.EventReason)
		}
	}
}

func TestApplyLoggingValidations_InvalidNotCountedAsDuplicate(t *testing.T) {
	// One valid logging resource + one invalid logging resource = NOT a duplicate.
	// The invalid one is excluded by validation before the duplicate check runs.
	configs := []DiscoveredManagedResource{
		{Name: "good", ConfigType: ConfigTypeLogging, Object: newCMObject("good"),
			Data: map[string][]byte{LoggingDataKey: []byte("<a/>")}},
		{Name: "bad", ConfigType: ConfigTypeLogging, Object: newCMObject("bad"),
			Data: map[string][]byte{"wrong.xml": []byte("<b/>")}},
	}
	filtered, issues := applyLoggingValidations(configs)
	if len(filtered) != 1 || filtered[0].Name != "good" {
		t.Errorf("expected only 'good' to remain, got %v", filtered)
	}
	if len(issues) != 1 || issues[0].EventReason != EventReasonLoggingConfigInvalid {
		t.Errorf("expected one LoggingConfigInvalid issue, got %v", issues)
	}
}

func TestApplyLoggingValidations_NonLoggingResourcesPassThroughUntouched(t *testing.T) {
	// Regression guard: data keys of non-logging resources are not inspected.
	configs := []DiscoveredManagedResource{
		{Name: "base-x", ConfigType: ConfigTypeBase, Object: newCMObject("base-x"),
			Data: map[string][]byte{LoggingDataKey: []byte("<a/>")}},
		{Name: "lic-y", ConfigType: ConfigTypeLicense, Object: newCMObject("lic-y"),
			Data: map[string][]byte{LoggingDataKey: []byte("<b/>")}},
	}
	filtered, issues := applyLoggingValidations(configs)
	if len(filtered) != 2 {
		t.Errorf("expected both resources to pass through, got %d", len(filtered))
	}
	if len(issues) != 0 {
		t.Errorf("expected zero issues for non-logging resources with log4j2.xml key; got %v", issues)
	}
}
