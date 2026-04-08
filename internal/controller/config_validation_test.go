package controller

import (
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func newValidationTestCluster() *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: "test-ns"},
		Spec: v1alpha1.IdentityServerClusterSpec{
			Version: "11.0",
		},
	}
}

func newValidationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	return s
}

func TestBuildConfigValidationJob_Name(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc123", nil)
	expected := "cluster-1-config-validation"
	if job.Name != expected {
		t.Errorf("expected job name %q, got %q", expected, job.Name)
	}
	if job.Namespace != "test-ns" {
		t.Errorf("expected namespace %q, got %q", "test-ns", job.Namespace)
	}
}

func TestBuildConfigValidationJob_Image(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	expected := "curity.azurecr.io/curity/idsvr:11.0"
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != expected {
		t.Errorf("expected image %q, got %q", expected, container.Image)
	}
}

func TestBuildConfigValidationJob_CustomImage(t *testing.T) {
	cluster := newValidationTestCluster()
	cluster.Spec.Image = "myregistry.io/idsvr:custom"
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "myregistry.io/idsvr:custom" {
		t.Errorf("expected custom image, got %q", container.Image)
	}
}

func TestBuildConfigValidationJob_SecurityContext(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil {
		t.Fatal("expected security context")
	}
	if *sc.RunAsUser != 10001 {
		t.Errorf("expected RunAsUser 10001, got %d", *sc.RunAsUser)
	}
	if *sc.RunAsGroup != 10000 {
		t.Errorf("expected RunAsGroup 10000, got %d", *sc.RunAsGroup)
	}
	if *sc.FSGroup != 10000 {
		t.Errorf("expected FSGroup 10000, got %d", *sc.FSGroup)
	}
}

func TestBuildConfigValidationJob_RestartPolicyAndBackoff(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicyNever, got %v", job.Spec.Template.Spec.RestartPolicy)
	}
	if *job.Spec.BackoffLimit != validationJobBackoffLimit {
		t.Errorf("expected backoff limit %d, got %d", validationJobBackoffLimit, *job.Spec.BackoffLimit)
	}
}

func TestBuildConfigValidationJob_ConfigHashAnnotation(t *testing.T) {
	cluster := newValidationTestCluster()
	hash := "sha256abc123"
	job, _ := buildConfigValidationJob(cluster, nil, hash, nil)
	got := job.Annotations[annotationConfigHash]
	if got != hash {
		t.Errorf("expected config-hash annotation %q, got %q", hash, got)
	}
}

func TestBuildConfigValidationJob_Labels(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	if job.Labels["curity.io/cluster"] != "cluster-1" {
		t.Errorf("expected cluster label, got %v", job.Labels)
	}
	if job.Labels["curity.io/component"] != "config-validation" {
		t.Errorf("expected component label, got %v", job.Labels)
	}
}

func TestBuildConfigValidationJob_MountsDiscoveredConfigs(t *testing.T) {
	cluster := newValidationTestCluster()
	configs := []DiscoveredConfigResource{
		{
			Name:       "base-config",
			IsSecret:   false,
			ConfigType: ConfigTypeBase,
			Data:       map[string][]byte{"config.xml": []byte("<config/>")},
		},
		{
			Name:       "license",
			IsSecret:   true,
			ConfigType: ConfigTypeLicense,
			Data:       map[string][]byte{"license.json": []byte(`{}`)},
		},
	}
	job, _ := buildConfigValidationJob(cluster, configs, "abc", nil)
	podSpec := job.Spec.Template.Spec

	// Should have discovered config volumes + tmp emptyDir (no cluster-config).
	if len(podSpec.Volumes) != 3 {
		t.Fatalf("expected 3 volumes (2 configs + tmp), got %d", len(podSpec.Volumes))
	}

	// Check ConfigMap volume.
	cmVol := podSpec.Volumes[0]
	if cmVol.ConfigMap == nil {
		t.Fatal("expected ConfigMap volume source for base-config")
	}
	if cmVol.ConfigMap.Name != "base-config" {
		t.Errorf("expected ConfigMap name %q, got %q", "base-config", cmVol.ConfigMap.Name)
	}

	// Check Secret volume.
	secVol := podSpec.Volumes[1]
	if secVol.Secret == nil {
		t.Fatal("expected Secret volume source for license")
	}
	if secVol.Secret.SecretName != "license" {
		t.Errorf("expected Secret name %q, got %q", "license", secVol.Secret.SecretName)
	}
}

func TestBuildConfigValidationJob_NoClusterConfigVolume(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "cluster-config" {
			t.Error("validation Job should not mount cluster-config volume")
		}
	}
}

func TestBuildConfigValidationJob_TmpEmptyDir(t *testing.T) {
	cluster := newValidationTestCluster()
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	podSpec := job.Spec.Template.Spec

	// Check tmp volume exists as emptyDir.
	var foundVol bool
	for _, vol := range podSpec.Volumes {
		if vol.Name == "tmp" {
			foundVol = true
			if vol.EmptyDir == nil {
				t.Error("expected emptyDir volume source for tmp")
			}
		}
	}
	if !foundVol {
		t.Error("expected tmp volume")
	}

	// Check tmp mount exists at /tmp.
	var foundMount bool
	for _, m := range podSpec.Containers[0].VolumeMounts {
		if m.Name == "tmp" && m.MountPath == "/tmp" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Error("expected /tmp volume mount")
	}
}

func TestBuildConfigValidationJob_AdminCredentials(t *testing.T) {
	cluster := newValidationTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "my-admin-creds",
			},
		},
	}
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	envs := job.Spec.Template.Spec.Containers[0].Env
	if len(envs) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(envs))
	}
	if envs[0].Name != "ADMIN_PASSWORD" {
		t.Errorf("expected ADMIN_PASSWORD env var, got %q", envs[0].Name)
	}
	if envs[0].ValueFrom.SecretKeyRef.Name != "my-admin-creds" {
		t.Errorf("expected secret name %q, got %q", "my-admin-creds", envs[0].ValueFrom.SecretKeyRef.Name)
	}
	if envs[1].Name != "CONFIG_ENCRYPTION_KEY" {
		t.Errorf("expected CONFIG_ENCRYPTION_KEY env var, got %q", envs[1].Name)
	}
}

func TestBuildConfigValidationJob_ImagePullSecret(t *testing.T) {
	cluster := newValidationTestCluster()
	cluster.Spec.ImagePullSecret = "my-pull-secret"
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	secrets := job.Spec.Template.Spec.ImagePullSecrets
	if len(secrets) != 1 || secrets[0].Name != "my-pull-secret" {
		t.Errorf("expected ImagePullSecret %q, got %v", "my-pull-secret", secrets)
	}
}

func TestBuildConfigValidationJob_SchedulingConstraints(t *testing.T) {
	cluster := newValidationTestCluster()
	cluster.Spec.NodeSelector = map[string]string{"disk": "ssd"}
	cluster.Spec.Tolerations = []corev1.Toleration{{Key: "key1", Effect: corev1.TaintEffectNoSchedule}}
	job, _ := buildConfigValidationJob(cluster, nil, "abc", nil)
	if job.Spec.Template.Spec.NodeSelector["disk"] != "ssd" {
		t.Error("expected NodeSelector inherited from cluster")
	}
	if len(job.Spec.Template.Spec.Tolerations) != 1 {
		t.Error("expected Tolerations inherited from cluster")
	}
}

func TestBuildConfigValidationJob_OwnerReference(t *testing.T) {
	cluster := newValidationTestCluster()
	scheme := newValidationScheme(t)
	job, err := buildConfigValidationJob(cluster, nil, "abc", scheme)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(job.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(job.OwnerReferences))
	}
	if job.OwnerReferences[0].Name != "cluster-1" {
		t.Errorf("expected owner name %q, got %q", "cluster-1", job.OwnerReferences[0].Name)
	}
}

// --- buildValidationVolumes ---

func TestBuildValidationVolumes_EmptyConfigs(t *testing.T) {
	volumes, mounts := buildValidationVolumes(nil)
	if len(volumes) != 0 {
		t.Fatalf("expected 0 volumes for nil configs, got %d", len(volumes))
	}
	if len(mounts) != 0 {
		t.Fatalf("expected 0 mounts for nil configs, got %d", len(mounts))
	}
}

func TestBuildValidationVolumes_ConfigMapVolume(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "base-cm", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"config.xml": []byte("<c/>")}},
	}
	volumes, mounts := buildValidationVolumes(configs)
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(volumes))
	}
	vol := volumes[0]
	if vol.ConfigMap == nil {
		t.Fatal("expected ConfigMap volume source")
	}
	if vol.ConfigMap.Name != "base-cm" {
		t.Errorf("expected ConfigMap name %q, got %q", "base-cm", vol.ConfigMap.Name)
	}

	// Find the mount for the config key.
	found := false
	for _, m := range mounts {
		if m.Name == vol.Name && m.SubPath == "config.xml" {
			found = true
			if m.MountPath != MountPathBase+"config.xml" {
				t.Errorf("expected mount path %q, got %q", MountPathBase+"config.xml", m.MountPath)
			}
		}
	}
	if !found {
		t.Error("expected mount for config.xml not found")
	}
}

func TestBuildValidationVolumes_SecretVolume(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "lic", IsSecret: true, ConfigType: ConfigTypeLicense, Data: map[string][]byte{"license.json": []byte("{}")}},
	}
	volumes, mounts := buildValidationVolumes(configs)
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(volumes))
	}
	vol := volumes[0]
	if vol.Secret == nil {
		t.Fatal("expected Secret volume source")
	}
	if vol.Secret.SecretName != "lic" {
		t.Errorf("expected Secret name %q, got %q", "lic", vol.Secret.SecretName)
	}

	found := false
	for _, m := range mounts {
		if m.Name == vol.Name && m.SubPath == "license.json" {
			found = true
			if m.MountPath != MountPathLicense+"license.json" {
				t.Errorf("expected mount path %q, got %q", MountPathLicense+"license.json", m.MountPath)
			}
		}
	}
	if !found {
		t.Error("expected mount for license.json not found")
	}
}

func TestBuildValidationVolumes_MixedConfigs(t *testing.T) {
	configs := []DiscoveredConfigResource{
		{Name: "cm-1", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
		{Name: "sec-1", IsSecret: true, ConfigType: ConfigTypeLicense, Data: map[string][]byte{"b.json": []byte("{}")}},
	}
	volumes, _ := buildValidationVolumes(configs)
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
}
