package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	return s
}

// --- discoverConfigResources ---

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

	configs, err := discoverConfigResources(ctx, c, "ns")
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

	configs, err := discoverConfigResources(ctx, c, "ns")
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

	configs, err := discoverConfigResources(ctx, c, "ns")
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

	configs, err := discoverConfigResources(ctx, c, "ns")
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

func TestDiscoverConfigResources_UnknownConfigTypeReturnsError(t *testing.T) {
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
	).Build()

	_, err := discoverConfigResources(ctx, c, "ns")
	if err == nil {
		t.Fatal("expected error for unknown config type")
	}
	if !errors.Is(err, ErrUnknownConfigType) {
		t.Errorf("expected ErrUnknownConfigType, got: %v", err)
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

	configs, err := discoverConfigResources(ctx, c, "ns")
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

	configs, err := discoverConfigResources(ctx, c, "empty-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(configs))
	}
}

// --- defaultConfigTypeAnnotations ---

func TestDefaultConfigTypeAnnotations_SetsMissingAnnotation(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-annotation",
			Namespace: "ns",
			Labels:    map[string]string{LabelManagedConfig: "true"},
		},
		Data: map[string]string{"config.xml": "<config/>"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	if err := defaultConfigTypeAnnotations(ctx, c, "ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &updated); err != nil {
		t.Fatalf("getting configmap: %v", err)
	}
	if updated.Annotations[AnnotationConfigType] != ConfigTypeBase {
		t.Errorf("expected annotation %q=%q, got %q", AnnotationConfigType, ConfigTypeBase, updated.Annotations[AnnotationConfigType])
	}
}

func TestDefaultConfigTypeAnnotations_SkipsExistingAnnotation(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "has-annotation",
			Namespace: "ns",
			Labels:    map[string]string{LabelManagedConfig: "true"},
			Annotations: map[string]string{
				AnnotationConfigType: ConfigTypeLicense,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cm).Build()

	if err := defaultConfigTypeAnnotations(ctx, c, "ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &updated); err != nil {
		t.Fatalf("getting configmap: %v", err)
	}
	if updated.Annotations[AnnotationConfigType] != ConfigTypeLicense {
		t.Errorf("expected annotation unchanged at %q, got %q", ConfigTypeLicense, updated.Annotations[AnnotationConfigType])
	}
}

func TestDefaultConfigTypeAnnotations_SkipsClusterConfigSecret(t *testing.T) {
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
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sec).Build()

	if err := defaultConfigTypeAnnotations(ctx, c, "ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(sec), &updated); err != nil {
		t.Fatalf("getting secret: %v", err)
	}
	if updated.Annotations != nil && updated.Annotations[AnnotationConfigType] != "" {
		t.Errorf("cluster-config secret should not get annotation, got %q", updated.Annotations[AnnotationConfigType])
	}
}

func TestDefaultConfigTypeAnnotations_NilAnnotationsMap(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nil-annotations",
			Namespace: "ns",
			Labels:    map[string]string{LabelManagedConfig: "true"},
			// Annotations is nil
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sec).Build()

	if err := defaultConfigTypeAnnotations(ctx, c, "ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(sec), &updated); err != nil {
		t.Fatalf("getting secret: %v", err)
	}
	if updated.Annotations[AnnotationConfigType] != ConfigTypeBase {
		t.Errorf("expected annotation %q=%q on secret with nil annotations map, got %q",
			AnnotationConfigType, ConfigTypeBase, updated.Annotations[AnnotationConfigType])
	}
}

func TestDefaultConfigTypeAnnotations_EmptyNamespace(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	if err := defaultConfigTypeAnnotations(ctx, c, "empty-ns"); err != nil {
		t.Fatalf("unexpected error for empty namespace: %v", err)
	}
}

// --- mountPathForConfigType ---

func TestMountPathForConfigType_Base(t *testing.T) {
	if got := mountPathForConfigType(ConfigTypeBase); got != MountPathBase {
		t.Errorf("expected %q, got %q", MountPathBase, got)
	}
}

func TestMountPathForConfigType_License(t *testing.T) {
	if got := mountPathForConfigType(ConfigTypeLicense); got != MountPathLicense {
		t.Errorf("expected %q, got %q", MountPathLicense, got)
	}
}

func TestMountPathForConfigType_DefaultsToBase(t *testing.T) {
	if got := mountPathForConfigType("something-unknown"); got != MountPathBase {
		t.Errorf("expected %q for unknown type, got %q", MountPathBase, got)
	}
}

// --- shouldMountConfig ---

func TestShouldMountConfig_AdminWithAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeAdmin, true) {
		t.Error("expected true: admin node should mount when admin exists")
	}
}

func TestShouldMountConfig_RuntimeWithAdminExists(t *testing.T) {
	if shouldMountConfig(v1alpha1.NodeTypeRuntime, true) {
		t.Error("expected false: runtime should not mount when admin exists")
	}
}

func TestShouldMountConfig_AdminNoAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeAdmin, false) {
		t.Error("expected true: admin node should mount when no admin exists")
	}
}

func TestShouldMountConfig_RuntimeNoAdminExists(t *testing.T) {
	if !shouldMountConfig(v1alpha1.NodeTypeRuntime, false) {
		t.Error("expected true: runtime should mount when no admin exists")
	}
}

// --- computeConfigHash ---

func TestComputeConfigHash_Deterministic(t *testing.T) {
	configs := []DiscoveredConfigResource{
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
	configs1 := []DiscoveredConfigResource{
		{Name: "cm-1", Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	configs2 := []DiscoveredConfigResource{
		{Name: "cm-1", Data: map[string][]byte{"a.xml": []byte("<b/>")}},
	}
	if computeConfigHash(configs1) == computeConfigHash(configs2) {
		t.Error("expected different hashes for different data")
	}
}

func TestComputeConfigHash_DifferentKindSameName(t *testing.T) {
	cm := []DiscoveredConfigResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	secret := []DiscoveredConfigResource{
		{Name: "foo", IsSecret: true, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	if computeConfigHash(cm) == computeConfigHash(secret) {
		t.Error("expected different hashes for ConfigMap vs Secret with same name and data")
	}
}

func TestComputeConfigHash_DifferentConfigType(t *testing.T) {
	base := []DiscoveredConfigResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	license := []DiscoveredConfigResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeLicense, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	if computeConfigHash(base) == computeConfigHash(license) {
		t.Error("expected different hashes for different config types")
	}
}

func TestComputeConfigHash_EmptyConfigs(t *testing.T) {
	if got := computeConfigHash([]DiscoveredConfigResource{}); got != "" {
		t.Errorf("expected empty string for empty configs, got %q", got)
	}
}

func TestComputeConfigHash_NilConfigs(t *testing.T) {
	if got := computeConfigHash(nil); got != "" {
		t.Errorf("expected empty string for nil configs, got %q", got)
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

// --- buildAppliedConfigStatus ---

func TestBuildAppliedConfigStatus_ConfigMap(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "cm-1", IsSecret: false, ConfigType: ConfigTypeBase},
	}
	status := buildAppliedConfigStatus(configs, v1alpha1.ValidationStatusValidated)
	if len(status) != 1 {
		t.Fatalf("expected 1 status entry, got %d", len(status))
	}
	if status[0].Kind != "ConfigMap" {
		t.Errorf("expected Kind %q, got %q", "ConfigMap", status[0].Kind)
	}
	if status[0].ValidationStatus != v1alpha1.ValidationStatusValidated {
		t.Errorf("expected ValidationStatus %q, got %q", v1alpha1.ValidationStatusValidated, status[0].ValidationStatus)
	}
}

func TestBuildAppliedConfigStatus_Secret(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "sec-1", IsSecret: true, ConfigType: ConfigTypeLicense},
	}
	status := buildAppliedConfigStatus(configs, v1alpha1.ValidationStatusPending)
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

func TestBuildAppliedConfigStatus_EmptyConfigs(t *testing.T) {
	status := buildAppliedConfigStatus(nil, v1alpha1.ValidationStatusValidated)
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
	configs := []DiscoveredConfigResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"b.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for distinct keys, got %v", warnings)
	}
}

func TestDetectDuplicateKeys_SameKeyDifferentConfigType(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"foo.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeLicense, Data: map[string][]byte{"foo.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for same key different config type, got %v", warnings)
	}
}

func TestDetectDuplicateKeys_SameKeySameType(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"base-config.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"base-config.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "base-config.xml") || !strings.Contains(warnings[0], "ConfigMap/cm-a") || !strings.Contains(warnings[0], "ConfigMap/cm-b") {
		t.Errorf("warning should mention filename and both resources with kind, got: %s", warnings[0])
	}
}

func TestDetectDuplicateKeys_ConfigMapAndSecret(t *testing.T) {
	// Even though mountFilename() produces distinct paths (cm_* vs secret_*),
	// having the same data key in both a ConfigMap and Secret of the same
	// config type is likely a user mistake — it may produce unexpected merged
	// configuration. The warning is about user intent, not mount path collisions.
	configs := []DiscoveredConfigResource{
		{Name: "cm-a", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"config.xml": []byte("<a/>")}},
		{Name: "sec-a", IsSecret: true, ConfigType: ConfigTypeBase, Data: map[string][]byte{"config.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for CM+Secret same key, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "ConfigMap/cm-a") || !strings.Contains(warnings[0], "Secret/sec-a") {
		t.Errorf("warning should mention both resources with kind, got: %s", warnings[0])
	}
}

func TestDetectDuplicateKeys_SingleResourceMultipleKeys(t *testing.T) {
	// A single resource with multiple distinct keys should produce 0 warnings.
	configs := []DiscoveredConfigResource{
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
	configs := []DiscoveredConfigResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"shared.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"shared.xml": []byte("<b/>")}},
		{Name: "cm-c", ConfigType: ConfigTypeBase, Data: map[string][]byte{"shared.xml": []byte("<c/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "ConfigMap/cm-a") || !strings.Contains(warnings[0], "ConfigMap/cm-b") || !strings.Contains(warnings[0], "ConfigMap/cm-c") {
		t.Errorf("warning should list all three resources with kind, got: %s", warnings[0])
	}
}

func TestDetectDuplicateKeys_MultipleDistinctDuplicates(t *testing.T) {
	// Two different keys are each duplicated — should produce 2 sorted warnings.
	configs := []DiscoveredConfigResource{
		{Name: "cm-a", ConfigType: ConfigTypeBase, Data: map[string][]byte{"x.xml": []byte("<a/>"), "y.xml": []byte("<a/>")}},
		{Name: "cm-b", ConfigType: ConfigTypeBase, Data: map[string][]byte{"x.xml": []byte("<b/>"), "y.xml": []byte("<b/>")}},
	}
	warnings := detectDuplicateKeys(configs)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings for two distinct duplicated keys, got %d: %v", len(warnings), warnings)
	}
	// Warnings are sorted — "x.xml" before "y.xml".
	if !strings.Contains(warnings[0], "x.xml") {
		t.Errorf("first warning should be about x.xml, got: %s", warnings[0])
	}
	if !strings.Contains(warnings[1], "y.xml") {
		t.Errorf("second warning should be about y.xml, got: %s", warnings[1])
	}
}
