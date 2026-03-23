package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func newTestCluster() *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: "test-ns"},
		Spec: v1alpha1.IdentityServerClusterSpec{
			Version: "11.0",
		},
	}
}

func newTestNode(nodeType v1alpha1.NodeType) *v1alpha1.IdentityServerNode {
	return &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "test-ns"},
		Spec: v1alpha1.IdentityServerNodeSpec{
			Type:                     nodeType,
			Role:                     "test-role",
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "cluster-1"},
			Replicas:                 ptr.To(int32(1)),
		},
	}
}

func TestBuildDeployment_AdminArgs(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	deploy := buildDeployment(cluster, node)

	args := deploy.Spec.Template.Spec.Containers[0].Args
	assertContains(t, args, "--admin")
	assertContains(t, args, "-s")
	assertContains(t, args, "-N")
	assertNotContains(t, args, "--no-admin")
}

func TestBuildDeployment_RuntimeArgs(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)

	args := deploy.Spec.Template.Spec.Containers[0].Args
	assertContains(t, args, "--no-admin")
	assertNotContains(t, args, "--admin")
	assertNotContains(t, args, "-N")
}

func TestBuildDeployment_AdminPorts(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true}

	deploy := buildDeployment(cluster, node)
	ports := deploy.Spec.Template.Spec.Containers[0].Ports

	assertPortExists(t, ports, "config", portConfig)
	assertPortExists(t, ports, "ds-port", portDistributedService)
	assertPortExists(t, ports, "health-check", portHealthCheck)
	assertPortExists(t, ports, "metrics", portMetrics)
	assertPortExists(t, ports, "admin-ui", portAdminUI)
	assertPortNotExists(t, ports, "http")
}

func TestBuildDeployment_RuntimePorts(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	ports := deploy.Spec.Template.Spec.Containers[0].Ports

	assertPortExists(t, ports, "http", portHTTP)
	assertPortExists(t, ports, "health-check", portHealthCheck)
	assertPortExists(t, ports, "metrics", portMetrics)
	assertPortNotExists(t, ports, "config")
	assertPortNotExists(t, ports, "ds-port")
	assertPortNotExists(t, ports, "admin-ui")
}

func TestBuildDeployment_AdminReplicasForcedTo1(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.Replicas = ptr.To(int32(5))

	deploy := buildDeployment(cluster, node)

	if *deploy.Spec.Replicas != 1 {
		t.Errorf("expected admin replicas to be 1, got %d", *deploy.Spec.Replicas)
	}
}

func TestBuildDeployment_RuntimeReplicasFromSpec(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Replicas = ptr.To(int32(3))

	deploy := buildDeployment(cluster, node)

	if *deploy.Spec.Replicas != 3 {
		t.Errorf("expected runtime replicas to be 3, got %d", *deploy.Spec.Replicas)
	}
}

func TestBuildDeployment_DefaultImage(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	image := deploy.Spec.Template.Spec.Containers[0].Image

	expected := "curity.azurecr.io/curity/idsvr:11.0"
	if image != expected {
		t.Errorf("expected image %q, got %q", expected, image)
	}
}

func TestBuildDeployment_ImageOverride(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Image = "my-registry/curity:custom"
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	image := deploy.Spec.Template.Spec.Containers[0].Image

	if image != "my-registry/curity:custom" {
		t.Errorf("expected custom image, got %q", image)
	}
}

func TestBuildDeployment_ImagePullSecret(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.ImagePullSecret = "my-secret"
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)

	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 1 {
		t.Fatalf("expected 1 image pull secret, got %d", len(deploy.Spec.Template.Spec.ImagePullSecrets))
	}
	if deploy.Spec.Template.Spec.ImagePullSecrets[0].Name != "my-secret" {
		t.Errorf("expected secret name %q", "my-secret")
	}
}

func TestBuildDeployment_SecurityContext(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	podSC := deploy.Spec.Template.Spec.SecurityContext

	if *podSC.RunAsUser != 10001 {
		t.Errorf("expected runAsUser 10001, got %d", *podSC.RunAsUser)
	}
	if *podSC.RunAsGroup != 10000 {
		t.Errorf("expected runAsGroup 10000, got %d", *podSC.RunAsGroup)
	}
	if *podSC.FSGroup != 10000 {
		t.Errorf("expected fsGroup 10000, got %d", *podSC.FSGroup)
	}
}

func TestBuildDeployment_LabelMerging(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.PodLabels = map[string]string{"team": "platform", "shared": "cluster-val"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.PodLabels = map[string]string{"shared": "node-val", "custom": "true"}

	deploy := buildDeployment(cluster, node)
	labels := deploy.Spec.Template.Labels

	if labels["team"] != "platform" {
		t.Errorf("expected cluster label 'team' to be preserved")
	}
	if labels["shared"] != "node-val" {
		t.Errorf("expected node label to win on conflict, got %q", labels["shared"])
	}
	if labels["custom"] != "true" {
		t.Errorf("expected node-only label to be present")
	}
}

func TestBuildDeployment_AnnotationMerging(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.PodAnnotations = map[string]string{"prometheus.io/scrape": "true", "team": "platform"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.PodAnnotations = map[string]string{"team": "identity"}

	deploy := buildDeployment(cluster, node)
	annotations := deploy.Spec.Template.Annotations

	if annotations["prometheus.io/scrape"] != "true" {
		t.Errorf("expected cluster annotation preserved")
	}
	if annotations["team"] != "identity" {
		t.Errorf("expected node annotation to win on conflict")
	}
}

func TestBuildDeployment_NodeResourcesOverrideCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}

	deploy := buildDeployment(cluster, node)
	cpu := deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]

	if cpu.String() != "2" {
		t.Errorf("expected node resources to win, got %s", cpu.String())
	}
}

func TestBuildDeployment_ClusterResourcesUsedWhenNodeNil(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	cpu := deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]

	if cpu.String() != "500m" {
		t.Errorf("expected cluster resources, got %s", cpu.String())
	}
}

func TestBuildDeployment_ProbeDefaults(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	liveness := deploy.Spec.Template.Spec.Containers[0].LivenessProbe
	readiness := deploy.Spec.Template.Spec.Containers[0].ReadinessProbe

	if liveness.InitialDelaySeconds != 30 {
		t.Errorf("expected liveness initialDelay 30, got %d", liveness.InitialDelaySeconds)
	}
	if readiness.SuccessThreshold != 3 {
		t.Errorf("expected readiness successThreshold 3, got %d", readiness.SuccessThreshold)
	}
}

func TestBuildDeployment_ProbeOverrides(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Probes = &v1alpha1.ProbeSpec{
		Liveness: &v1alpha1.ProbeConfig{
			InitialDelaySeconds: ptr.To(int32(60)),
		},
	}

	deploy := buildDeployment(cluster, node)
	liveness := deploy.Spec.Template.Spec.Containers[0].LivenessProbe

	if liveness.InitialDelaySeconds != 60 {
		t.Errorf("expected liveness initialDelay 60, got %d", liveness.InitialDelaySeconds)
	}
	// Other fields should keep defaults
	if liveness.PeriodSeconds != 10 {
		t.Errorf("expected liveness period 10, got %d", liveness.PeriodSeconds)
	}
}

func TestBuildDeployment_UIEnabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(false)}

	deploy := buildDeployment(cluster, node)
	ports := deploy.Spec.Template.Spec.Containers[0].Ports
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertPortExists(t, ports, "admin-ui", portAdminUI)
	assertEnvVar(t, envVars, "ADMIN_UI_HTTP_MODE", "true")
}

func TestBuildDeployment_UIDisabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: false}

	deploy := buildDeployment(cluster, node)
	ports := deploy.Spec.Template.Spec.Containers[0].Ports

	assertPortNotExists(t, ports, "admin-ui")
}

func TestBuildDeployment_ConfigurationVolumes(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Configuration = &v1alpha1.ConfigurationSource{
		ValueFrom: v1alpha1.ConfigurationValueFrom{
			ConfigMapRef: &v1alpha1.ConfigMapRefSource{
				Name:  "test-config",
				Items: []v1alpha1.KeyToPath{{Key: "base.xml", Path: "base.xml"}},
			},
			SecretRef: &v1alpha1.SecretKeyRefSource{
				Name:  "test-secret-config",
				Items: []v1alpha1.KeyToPath{{Key: "ds.xml", Path: "ds.xml"}},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)

	if len(deploy.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(deploy.Spec.Template.Spec.Volumes))
	}
	if len(deploy.Spec.Template.Spec.Containers[0].VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(deploy.Spec.Template.Spec.Containers[0].VolumeMounts))
	}
}

func TestBuildDeployment_EnvironmentVariables(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.EnvironmentVariables = []corev1.EnvVar{
		{Name: "CUSTOM_VAR", Value: "custom-value"},
	}

	deploy := buildDeployment(cluster, node)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVar(t, envVars, "STATUS_CMD_PORT", "4465")
	assertEnvVar(t, envVars, "LOGGING_LEVEL", "INFO")
	assertEnvVar(t, envVars, "CUSTOM_VAR", "custom-value")
}

func TestBuildDeployment_LoggingLevelDefault(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "INFO")
}

func TestBuildDeployment_LoggingLevelFromCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{Level: "DEBUG"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "DEBUG")
}

func TestBuildDeployment_LoggingLevelNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{Level: "DEBUG"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Level: "TRACE"}

	deploy := buildDeployment(cluster, node)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "TRACE")
}

func TestBuildDeployment_LoggingStdoutDisabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	// stdout=false (default) → no log volume, no sidecars
	deploy := buildDeployment(cluster, node)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container (no sidecars), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "log-volume" {
			t.Errorf("log-volume should not exist when stdout=false")
		}
	}
}

func TestBuildDeployment_LoggingStdoutEnabledNoLogs(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Stdout: true, Logs: []string{}}

	deploy := buildDeployment(cluster, node)

	// Volume should exist (stdout=true) but no sidecars (logs=[])
	foundLogVol := false
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "log-volume" {
			foundLogVol = true
		}
	}
	if !foundLogVol {
		t.Errorf("log-volume should exist when stdout=true")
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container (no sidecars for empty logs), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
}

func TestBuildDeployment_LoggingSidecars(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Stdout: true,
		Logs:   []string{"audit", "request"},
	}

	deploy := buildDeployment(cluster, node)

	// 1 main + 2 sidecars
	if len(deploy.Spec.Template.Spec.Containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(deploy.Spec.Template.Spec.Containers))
	}

	// Check sidecar names and commands
	audit := deploy.Spec.Template.Spec.Containers[1]
	if audit.Name != "audit" {
		t.Errorf("expected sidecar name 'audit', got %q", audit.Name)
	}
	if audit.Command[2] != "/log/audit.log" {
		t.Errorf("expected audit log path, got %q", audit.Command[2])
	}
	if audit.Image != "busybox:latest" {
		t.Errorf("expected default image busybox:latest, got %q", audit.Image)
	}

	request := deploy.Spec.Template.Spec.Containers[2]
	if request.Name != "request" {
		t.Errorf("expected sidecar name 'request', got %q", request.Name)
	}

	// Check volume mount on sidecar
	if len(audit.VolumeMounts) != 1 || audit.VolumeMounts[0].Name != "log-volume" {
		t.Errorf("sidecar should mount log-volume")
	}
	if !audit.VolumeMounts[0].ReadOnly {
		t.Errorf("sidecar mount should be read-only")
	}
}

func TestBuildDeployment_LoggingCustomImage(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Stdout: true,
		Logs:   []string{"audit"},
		Image:  "alpine:3.19",
	}

	deploy := buildDeployment(cluster, node)
	sidecar := deploy.Spec.Template.Spec.Containers[1]
	if sidecar.Image != "alpine:3.19" {
		t.Errorf("expected custom image alpine:3.19, got %q", sidecar.Image)
	}
}

func TestBuildDeployment_LoggingResources(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Stdout: true,
		Logs:   []string{"audit"},
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10m"),
			},
		},
	}

	deploy := buildDeployment(cluster, node)
	sidecar := deploy.Spec.Template.Spec.Containers[1]
	cpu := sidecar.Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "10m" {
		t.Errorf("expected sidecar CPU 10m, got %s", cpu.String())
	}
}

func TestBuildDeployment_LoggingNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{
		Stdout: true,
		Logs:   []string{"audit", "request"},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Stdout: true,
		Logs:   []string{"cluster"},
	}

	deploy := buildDeployment(cluster, node)

	// Node overrides entirely: only 1 sidecar (cluster), not 2 (audit, request)
	if len(deploy.Spec.Template.Spec.Containers) != 2 {
		t.Errorf("expected 2 containers (main + 1 sidecar), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	if deploy.Spec.Template.Spec.Containers[1].Name != "cluster" {
		t.Errorf("expected sidecar name 'cluster', got %q", deploy.Spec.Template.Spec.Containers[1].Name)
	}
}

func TestBuildService_AdminPorts(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true}

	svc := buildService(cluster, node)

	assertServicePortExists(t, svc.Spec.Ports, "config", portConfig)
	assertServicePortExists(t, svc.Spec.Ports, "ds-port", portDistributedService)
	assertServicePortExists(t, svc.Spec.Ports, "health-check", portHealthCheck)
	assertServicePortExists(t, svc.Spec.Ports, "admin-ui", portAdminUI)
}

func TestBuildService_RuntimePorts(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	svc := buildService(cluster, node)

	assertServicePortExists(t, svc.Spec.Ports, "http", portHTTP)
	assertServicePortExists(t, svc.Spec.Ports, "health-check", portHealthCheck)
	assertServicePortExists(t, svc.Spec.Ports, "metrics", portMetrics)
}

func TestBuildService_CustomPort(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Service = &v1alpha1.ServiceSpec{Port: 9443}

	svc := buildService(cluster, node)

	assertServicePortExists(t, svc.Spec.Ports, "http", 9443)
}

func TestBuildService_LoadBalancerType(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Service = &v1alpha1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}

	svc := buildService(cluster, node)

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("expected LoadBalancer type, got %s", svc.Spec.Type)
	}
}

func TestBuildService_SameName(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	svc := buildService(cluster, node)

	if svc.Name != node.Name {
		t.Errorf("expected service name %q, got %q", node.Name, svc.Name)
	}
}

func TestMergeMaps(t *testing.T) {
	tests := []struct {
		name     string
		base     map[string]string
		override map[string]string
		expected map[string]string
	}{
		{"both nil", nil, nil, nil},
		{"base only", map[string]string{"a": "1"}, nil, map[string]string{"a": "1"}},
		{"override only", nil, map[string]string{"b": "2"}, map[string]string{"b": "2"}},
		{"merge with conflict", map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "3", "c": "4"}, map[string]string{"a": "1", "b": "3", "c": "4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeMaps(tt.base, tt.override)
			if tt.expected == nil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}
			for k, v := range tt.expected {
				if result[k] != v {
					t.Errorf("key %q: expected %q, got %q", k, v, result[k])
				}
			}
		})
	}
}

func TestBuildDeployment_AdminCredentialsEnvVarUsesPath(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	// Env var name must come from Path, not from a hardcoded mapping
	assertEnvVarFromSecret(t, envVars, "PASSWORD", "admin-secret", "ADMIN_PASSWORD")
}

func TestBuildDeployment_AdminCredentialsMultipleItems(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
					{Key: "KEYSTORE_PASSWORD", Path: "KEYSTORE_PASSWORD"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	deploy := buildDeployment(cluster, node)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVarFromSecret(t, envVars, "PASSWORD", "admin-secret", "ADMIN_PASSWORD")
	assertEnvVarFromSecret(t, envVars, "CONFIG_ENCRYPTION_KEY", "admin-secret", "CONFIG_ENCRYPTION_KEY")
	assertEnvVarFromSecret(t, envVars, "KEYSTORE_PASSWORD", "admin-secret", "KEYSTORE_PASSWORD")
}

func TestBuildDeployment_AdminCredentialsNoSpecialMapping(t *testing.T) {
	// Verify that Path is always used as-is with no implicit renaming
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "MY_CUSTOM_NAME"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	// Should use Path as-is, not rename to PASSWORD
	assertEnvVarFromSecret(t, envVars, "MY_CUSTOM_NAME", "admin-secret", "ADMIN_PASSWORD")
}

// --- test helpers ---

func assertContains(t *testing.T, slice []string, item string) {
	t.Helper()
	for _, s := range slice {
		if s == item {
			return
		}
	}
	t.Errorf("expected slice to contain %q, got %v", item, slice)
}

func assertNotContains(t *testing.T, slice []string, item string) {
	t.Helper()
	for _, s := range slice {
		if s == item {
			t.Errorf("expected slice not to contain %q", item)
			return
		}
	}
}

func assertPortExists(t *testing.T, ports []corev1.ContainerPort, name string, port int32) {
	t.Helper()
	for _, p := range ports {
		if p.Name == name && p.ContainerPort == port {
			return
		}
	}
	t.Errorf("expected port %s:%d to exist", name, port)
}

func assertPortNotExists(t *testing.T, ports []corev1.ContainerPort, name string) {
	t.Helper()
	for _, p := range ports {
		if p.Name == name {
			t.Errorf("expected port %s not to exist", name)
			return
		}
	}
}

func assertServicePortExists(t *testing.T, ports []corev1.ServicePort, name string, port int32) {
	t.Helper()
	for _, p := range ports {
		if p.Name == name && p.Port == port {
			return
		}
	}
	t.Errorf("expected service port %s:%d to exist", name, port)
}

func assertEnvVar(t *testing.T, envVars []corev1.EnvVar, name, value string) {
	t.Helper()
	for _, e := range envVars {
		if e.Name == name {
			if e.Value != value {
				t.Errorf("env %s: expected %q, got %q", name, value, e.Value)
			}
			return
		}
	}
	t.Errorf("expected env var %s to exist", name)
}

func assertEnvVarFromSecret(t *testing.T, envVars []corev1.EnvVar, envName, secretName, secretKey string) {
	t.Helper()
	for _, e := range envVars {
		if e.Name == envName {
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("env %s: expected secretKeyRef, got literal value", envName)
				return
			}
			if e.ValueFrom.SecretKeyRef.Name != secretName {
				t.Errorf("env %s: expected secret %q, got %q", envName, secretName, e.ValueFrom.SecretKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef.Key != secretKey {
				t.Errorf("env %s: expected key %q, got %q", envName, secretKey, e.ValueFrom.SecretKeyRef.Key)
			}
			return
		}
	}
	t.Errorf("expected env var %s to exist", envName)
}
