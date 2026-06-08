package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
			Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 8443},
		},
	}
}

func TestBuildDeployment_AdminArgs(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	args := deploy.Spec.Template.Spec.Containers[0].Args
	assertContains(t, args, "--admin")
	assertContains(t, args, "-s")
	assertContains(t, args, "-N")
	assertNotContains(t, args, "--no-admin")
}

func TestBuildDeployment_RuntimeArgs(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	args := deploy.Spec.Template.Spec.Containers[0].Args
	assertContains(t, args, "--no-admin")
	assertNotContains(t, args, "--admin")
	assertNotContains(t, args, "-N")
}

func TestBuildDeployment_AdminPorts(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	if *deploy.Spec.Replicas != 1 {
		t.Errorf("expected admin replicas to be 1, got %d", *deploy.Spec.Replicas)
	}
}

func TestBuildDeployment_RuntimeReplicasFromSpec(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Replicas = ptr.To(int32(3))

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	if *deploy.Spec.Replicas != 3 {
		t.Errorf("expected runtime replicas to be 3, got %d", *deploy.Spec.Replicas)
	}
}

func TestBuildDeployment_DefaultImage(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	image := deploy.Spec.Template.Spec.Containers[0].Image

	if image != "my-registry/curity:custom" {
		t.Errorf("expected custom image, got %q", image)
	}
}

func TestBuildDeployment_ImagePullSecret(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.ImagePullSecret = "my-secret"
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

// Selector must be exactly the private key — no app.kubernetes.io/*.
func TestBuildSelectorLabels_SinglePrivateKey(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	selector := buildSelectorLabels(cluster, node)

	if len(selector) != 1 {
		t.Errorf("selector must be exactly one key; got %d: %v", len(selector), selector)
	}
	want := OwnedResourceName(cluster.Name, node.Name)
	if got := selector["curity.io/owned-by"]; got != want {
		t.Errorf("selector[curity.io/owned-by] = %q; want %q", got, want)
	}
	for _, banned := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance"} {
		if _, present := selector[banned]; present {
			t.Errorf("selector must not include %q; got %v", banned, selector)
		}
	}
}

// Pod template carries both the informational instance key and the
// private selector key.
func TestBuildLabels_IncludesBothInformationalAndPrivateKeys(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	labels := buildLabels(cluster, node)
	want := OwnedResourceName(cluster.Name, node.Name)

	if got := labels["app.kubernetes.io/instance"]; got != want {
		t.Errorf("app.kubernetes.io/instance = %q; want %q", got, want)
	}
	if got := labels["curity.io/owned-by"]; got != want {
		t.Errorf("curity.io/owned-by = %q; want %q", got, want)
	}
}

// Operator wins on collisions; user-only keys survive the merge.
func TestBuildDeployment_OperatorLabelsOverlayUserPodLabelsLast(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.PodLabels = map[string]string{
		"app.kubernetes.io/instance": "hijacked-by-user",
		"curity.io/owned-by":         "hijacked-by-user",
		"app.kubernetes.io/version":  "user-pinned-version",
		"team":                       "platform",
		"environment":                "staging",
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	templateLabels := deploy.Spec.Template.Labels
	want := OwnedResourceName(cluster.Name, node.Name)

	if got := templateLabels["app.kubernetes.io/instance"]; got != want {
		t.Errorf("operator must win on app.kubernetes.io/instance; got %q want %q", got, want)
	}
	if got := templateLabels["curity.io/owned-by"]; got != want {
		t.Errorf("operator must win on curity.io/owned-by; got %q want %q", got, want)
	}
	if got := templateLabels["app.kubernetes.io/version"]; got != cluster.Spec.Version {
		t.Errorf("operator must win on app.kubernetes.io/version; got %q want %q", got, cluster.Spec.Version)
	}
	if got := templateLabels["team"]; got != "platform" {
		t.Errorf("user-only key 'team' lost; got %q want platform", got)
	}
	if got := templateLabels["environment"]; got != "staging" {
		t.Errorf("user-only key 'environment' lost; got %q want staging", got)
	}
}

// Catches a refactor that wires Spec.Selector from buildLabels instead
// of buildSelectorLabels.
func TestBuildDeployment_SelectorMatchLabelsExactly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.PodLabels = map[string]string{"app.kubernetes.io/instance": "hijacked"}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	selector := deploy.Spec.Selector
	if selector == nil {
		t.Fatal("Deployment.Spec.Selector must not be nil")
	}
	if got := len(selector.MatchLabels); got != 1 {
		t.Errorf("Deployment.Spec.Selector.MatchLabels: %d keys, want 1: %v", got, selector.MatchLabels)
	}
	want := OwnedResourceName(cluster.Name, node.Name)
	if got := selector.MatchLabels["curity.io/owned-by"]; got != want {
		t.Errorf("Deployment selector[curity.io/owned-by] = %q; want %q", got, want)
	}
	for _, banned := range []string{"app.kubernetes.io/instance", "app.kubernetes.io/name"} {
		if _, present := selector.MatchLabels[banned]; present {
			t.Errorf("Deployment selector must not include %q; got %v", banned, selector.MatchLabels)
		}
	}
}

func TestBuildPDB_SelectorMatchLabelsExactly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: ptr.To(intstr.FromInt(1))}

	pdb := buildPDB(cluster, node)
	if pdb == nil {
		t.Fatal("buildPDB must return non-nil when MinAvailable is set")
	}
	if got := len(pdb.Spec.Selector.MatchLabels); got != 1 {
		t.Errorf("PDB selector: %d keys, want 1: %v", got, pdb.Spec.Selector.MatchLabels)
	}
	want := OwnedResourceName(cluster.Name, node.Name)
	if got := pdb.Spec.Selector.MatchLabels["curity.io/owned-by"]; got != want {
		t.Errorf("PDB selector[curity.io/owned-by] = %q; want %q", got, want)
	}
}

func TestUserPodLabelsOverridden(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	if got := userPodLabelsOverridden(cluster, node); len(got) != 0 {
		t.Errorf("no user labels: want empty; got %v", got)
	}

	node.Spec.PodLabels = map[string]string{"team": "platform"}
	if got := userPodLabelsOverridden(cluster, node); len(got) != 0 {
		t.Errorf("non-colliding user labels: want empty; got %v", got)
	}

	// 2 cluster-level collisions + 1 node-level — all 3 reported sorted.
	cluster.Spec.PodLabels = map[string]string{
		"app.kubernetes.io/instance": "user-instance",
		"curity.io/owned-by":         "user-owned-by",
	}
	node.Spec.PodLabels = map[string]string{
		"curity.io/role": "user-role",
		"team":           "platform",
	}
	got := userPodLabelsOverridden(cluster, node)
	want := []string{"app.kubernetes.io/instance", "curity.io/owned-by", "curity.io/role"}
	if len(got) != len(want) {
		t.Fatalf("overridden count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("overridden[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestBuildDeployment_AnnotationMerging(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.PodAnnotations = map[string]string{"prometheus.io/scrape": "true", "team": "platform"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.PodAnnotations = map[string]string{"team": "identity"}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	cpu := deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]

	if cpu.String() != "500m" {
		t.Errorf("expected cluster resources, got %s", cpu.String())
	}
}

func TestBuildDeployment_ProbeDefaults(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	liveness := deploy.Spec.Template.Spec.Containers[0].LivenessProbe
	readiness := deploy.Spec.Template.Spec.Containers[0].ReadinessProbe

	if liveness.InitialDelaySeconds != 30 {
		t.Errorf("expected liveness initialDelay 30, got %d", liveness.InitialDelaySeconds)
	}
	if readiness.SuccessThreshold != 1 {
		t.Errorf("expected readiness successThreshold 1, got %d", readiness.SuccessThreshold)
	}
	if liveness.SuccessThreshold != 1 {
		t.Errorf("expected liveness successThreshold 1, got %d", liveness.SuccessThreshold)
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

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	liveness := deploy.Spec.Template.Spec.Containers[0].LivenessProbe

	if liveness.InitialDelaySeconds != 60 {
		t.Errorf("expected liveness initialDelay 60, got %d", liveness.InitialDelaySeconds)
	}
	// Other fields should keep defaults
	if liveness.PeriodSeconds != 10 {
		t.Errorf("expected liveness period 10, got %d", liveness.PeriodSeconds)
	}
}

// TestBuildDeployment_LivenessSuccessThresholdCoerced verifies that a
// user-supplied (or defaulted) SuccessThreshold > 1 on the liveness probe is
// silently coerced to 1. K8s rejects any other value at admission, so the
// operator pins this regardless of what the spec says.
func TestBuildDeployment_LivenessSuccessThresholdCoerced(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Probes = &v1alpha1.ProbeSpec{
		Liveness:  &v1alpha1.ProbeConfig{SuccessThreshold: ptr.To(int32(5))},
		Readiness: &v1alpha1.ProbeConfig{SuccessThreshold: ptr.To(int32(5))},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	liveness := deploy.Spec.Template.Spec.Containers[0].LivenessProbe
	readiness := deploy.Spec.Template.Spec.Containers[0].ReadinessProbe

	if liveness.SuccessThreshold != 1 {
		t.Errorf("liveness SuccessThreshold must be coerced to 1, got %d", liveness.SuccessThreshold)
	}
	// Readiness can legitimately take other values — user's 5 stands.
	if readiness.SuccessThreshold != 5 {
		t.Errorf("readiness SuccessThreshold honors spec, expected 5 got %d", readiness.SuccessThreshold)
	}
}

func TestBuildDeployment_UIEnabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(false)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	ports := deploy.Spec.Template.Spec.Containers[0].Ports
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertPortExists(t, ports, "admin-ui", portAdminUI)
	assertEnvVar(t, envVars, "ADMIN_UI_HTTP_MODE", "true")
}

// TestBuildDeployment_UIEnabledSecureNil exercises the nil-guard on UI.Secure
// at resources.go:289-297. Admission-time CEL enforces Secure non-nil when
// Enabled=true, but unit-constructed nodes can skip admission — the guard
// prevents a panic and falls through to the secure-HTTPS default.
func TestBuildDeployment_UIEnabledSecureNil(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: nil}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVar(t, envVars, "ADMIN_UI_HTTP_MODE", "false")
}

func TestBuildDeployment_UIDisabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: false, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	ports := deploy.Spec.Template.Spec.Containers[0].Ports
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertPortNotExists(t, ports, "admin-ui")

	// ADMIN_UI_HTTP_MODE must not be emitted when UI is disabled. Pins the
	// behavior change at resources.go:290 — prior code emitted whenever the
	// UI block existed regardless of Enabled.
	for _, e := range envVars {
		if e.Name == "ADMIN_UI_HTTP_MODE" {
			t.Errorf("ADMIN_UI_HTTP_MODE should not be set when UI is disabled, got %q", e.Value)
		}
	}
}

func TestBuildDeployment_EnvironmentVariables(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.EnvironmentVariables = []corev1.EnvVar{
		{Name: "CUSTOM_VAR", Value: "custom-value"},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVar(t, envVars, "STATUS_CMD_PORT", "4465")
	assertEnvVar(t, envVars, "LOGGING_LEVEL", "INFO")
	assertEnvVar(t, envVars, "CUSTOM_VAR", "custom-value")
}

func TestBuildDeployment_LoggingLevelDefault(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "INFO")
}

func TestBuildDeployment_LoggingLevelFromCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{Level: "DEBUG"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "DEBUG")
}

func TestBuildDeployment_LoggingLevelNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{Level: "DEBUG"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Level: "TRACE"}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "TRACE")
}

func TestBuildDeployment_LoggingLevelOff(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{Level: "OFF"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "OFF")
}

func TestBuildDeployment_LoggingOffSuppressesSidecars(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Level: "OFF", Logs: []string{"audit"}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container (no sidecars when OFF), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "log-volume" {
			t.Errorf("log-volume should not exist when level is OFF")
		}
	}
}

func TestBuildDeployment_SidecarImagePullPolicyDefaulted(t *testing.T) {
	// Log sidecars are operator-generated; every container the operator emits must
	// carry the apiserver-defaulted fields (e.g. ImagePullPolicy) or it drifts.
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Level: "INFO", Logs: []string{"audit", "request"}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected main + 2 sidecars, got %d containers", len(containers))
	}
	for _, c := range containers {
		if c.ImagePullPolicy == "" {
			t.Errorf("container %q missing ImagePullPolicy (apiserver defaults it → drift)", c.Name)
		}
		if c.TerminationMessagePath == "" || c.TerminationMessagePolicy == "" {
			t.Errorf("container %q missing terminationMessage defaults", c.Name)
		}
	}
	for _, c := range containers {
		if c.Name == "audit" || c.Name == "request" {
			if c.ImagePullPolicy != corev1.PullAlways {
				t.Errorf("sidecar %q: want Always (busybox:latest), got %q", c.Name, c.ImagePullPolicy)
			}
		}
	}
}

func TestBuildDeployment_LoggingOffClusterNodeLogsOverride(t *testing.T) {
	// Regression: cluster sets OFF, node overrides only Logs (not Level).
	// resolveLogging returns node spec (Level:""), resolveLoggingLevel returns "OFF".
	// Sidecars and log-volume must still be suppressed.
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{Level: "OFF"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Logs: []string{"audit"}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	assertEnvVar(t, deploy.Spec.Template.Spec.Containers[0].Env, "LOGGING_LEVEL", "OFF")
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container (no sidecars when cluster level is OFF), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "log-volume" {
			t.Errorf("log-volume should not exist when cluster level is OFF")
		}
	}
}

func TestBuildDeployment_LoggingUnsetDisabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	// logging unset → no log volume, no sidecars
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container (no sidecars), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "log-volume" {
			t.Errorf("log-volume should not exist when logging is unset")
		}
	}
}

func TestBuildDeployment_LoggingEmptyLogsDisabled(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	// empty logs list → disabled: no log volume, no sidecars
	node.Spec.Logging = &v1alpha1.LoggingSpec{Logs: []string{}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "log-volume" {
			t.Errorf("log-volume should not exist when logs is empty")
		}
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container (no sidecars for empty logs), got %d", len(deploy.Spec.Template.Spec.Containers))
	}
}

func TestBuildDeployment_LoggingSidecars(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Logs: []string{"audit", "request"},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

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
		Logs:  []string{"audit"},
		Image: "alpine:3.19",
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	sidecar := deploy.Spec.Template.Spec.Containers[1]
	if sidecar.Image != "alpine:3.19" {
		t.Errorf("expected custom image alpine:3.19, got %q", sidecar.Image)
	}
}

func TestBuildDeployment_LoggingResources(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Logs: []string{"audit"},
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10m"),
			},
		},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	sidecar := deploy.Spec.Template.Spec.Containers[1]
	cpu := sidecar.Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "10m" {
		t.Errorf("expected sidecar CPU 10m, got %s", cpu.String())
	}
}

func TestBuildDeployment_LoggingNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Logging = &v1alpha1.LoggingSpec{
		Logs: []string{"audit", "request"},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{
		Logs: []string{"cluster"},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

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
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)}

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
	node.Spec.Service = v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 9443}

	svc := buildService(cluster, node)

	assertServicePortExists(t, svc.Spec.Ports, "http", 9443)
}

func TestBuildService_LoadBalancerType(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Service = v1alpha1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Port: 8443}

	svc := buildService(cluster, node)

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("expected LoadBalancer type, got %s", svc.Spec.Type)
	}
}

func TestBuildService_SameName(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	svc := buildService(cluster, node)

	want := OwnedResourceName(cluster.Name, node.Name)
	if svc.Name != want {
		t.Errorf("expected service name %q, got %q", want, svc.Name)
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

func TestMergeManagedLabels(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		desired  map[string]string
		expected map[string]string
	}{
		{
			name:     "both nil",
			existing: nil,
			desired:  nil,
			expected: nil,
		},
		{
			name:     "nil existing, desired only",
			existing: nil,
			desired:  map[string]string{"app.kubernetes.io/name": "curity-identity-server"},
			expected: map[string]string{"app.kubernetes.io/name": "curity-identity-server"},
		},
		{
			name:     "foreign keys preserved",
			existing: map[string]string{"monitoring": "prometheus", "team": "identity"},
			desired:  map[string]string{"app.kubernetes.io/name": "curity-identity-server"},
			expected: map[string]string{
				"app.kubernetes.io/name": "curity-identity-server",
				"monitoring":             "prometheus",
				"team":                   "identity",
			},
		},
		{
			name: "operator keys overwritten, foreign preserved",
			existing: map[string]string{
				"app.kubernetes.io/version":   "10.0.0",
				"curity.io/role":              "stale",
				"argocd.argoproj.io/instance": "my-app",
			},
			desired: map[string]string{
				"app.kubernetes.io/version": "11.2.0",
				"curity.io/role":            "primary",
			},
			expected: map[string]string{
				"app.kubernetes.io/version":   "11.2.0",
				"curity.io/role":              "primary",
				"argocd.argoproj.io/instance": "my-app",
			},
		},
		{
			name:     "stale operator key dropped when not in desired",
			existing: map[string]string{"curity.io/legacy": "x", "foo": "bar"},
			desired:  map[string]string{"app.kubernetes.io/name": "curity-identity-server"},
			expected: map[string]string{
				"app.kubernetes.io/name": "curity-identity-server",
				"foo":                    "bar",
			},
		},
		{
			name:     "empty desired keeps only foreign existing",
			existing: map[string]string{"app.kubernetes.io/name": "x", "team": "id"},
			desired:  map[string]string{},
			expected: map[string]string{"team": "id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeManagedLabels(tt.existing, tt.desired)
			if tt.expected == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("length mismatch: expected %d (%v), got %d (%v)",
					len(tt.expected), tt.expected, len(got), got)
			}
			for k, v := range tt.expected {
				if got[k] != v {
					t.Errorf("key %q: expected %q, got %q", k, v, got[k])
				}
			}
		})
	}
}

func TestIsOperatorOwnedLabel(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"app.kubernetes.io/name", true},
		{"app.kubernetes.io/", true},
		{"curity.io/cluster", true},
		{"curity.io/", true},
		{"monitoring", false},
		{"argocd.argoproj.io/instance", false},
		{"app.kubernetes.iox/name", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := isOperatorOwnedLabel(tt.key); got != tt.want {
				t.Errorf("isOperatorOwnedLabel(%q) = %v, want %v", tt.key, got, tt.want)
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
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
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
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVarFromSecret(t, envVars, "PASSWORD", "admin-secret", "ADMIN_PASSWORD")
	assertEnvVarFromSecret(t, envVars, "CONFIG_ENCRYPTION_KEY", "admin-secret", "CONFIG_ENCRYPTION_KEY")
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
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	// Should use Path as-is, not rename to PASSWORD
	assertEnvVarFromSecret(t, envVars, "MY_CUSTOM_NAME", "admin-secret", "ADMIN_PASSWORD")
}

func TestBuildDeployment_RuntimeOmitsAdminPassword(t *testing.T) {
	// The admin password is the installer trigger and must not reach runtime
	// nodes; other credential keys still project there.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	envVars := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.Containers[0].Env

	assertNoEnvVar(t, envVars, "PASSWORD")
	assertEnvVarFromSecret(t, envVars, "CONFIG_ENCRYPTION_KEY", "admin-secret", "CONFIG_ENCRYPTION_KEY")
}

func TestBuildDeployment_RuntimeOmitsAdminPasswordCustomName(t *testing.T) {
	// Gating keys off the ADMIN_PASSWORD secret key, not its projected name, so
	// any custom env name is still dropped on runtime.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "MY_CUSTOM_NAME"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	envVars := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.Containers[0].Env

	assertNoEnvVar(t, envVars, "MY_CUSTOM_NAME")
}

func TestBuildDeployment_SkipInstallAdmin(t *testing.T) {
	// skipInstall passes SKIP_INSTALL=1; the password is still projected on the
	// admin (the image skips first-run setup whenever SKIP_INSTALL is set).
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.SkipInstall = ptr.To(true)

	envVars := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.Containers[0].Env

	assertEnvVar(t, envVars, "SKIP_INSTALL", "1")
	assertEnvVarFromSecret(t, envVars, "PASSWORD", "admin-secret", "ADMIN_PASSWORD")
	assertEnvVarFromSecret(t, envVars, "CONFIG_ENCRYPTION_KEY", "admin-secret", "CONFIG_ENCRYPTION_KEY")
}

func TestBuildDeployment_SkipInstallUnsetAdminKeepsPassword(t *testing.T) {
	// Without skipInstall, the admin keeps the password and gets no SKIP_INSTALL.
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
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	envVars := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.Containers[0].Env

	assertEnvVarFromSecret(t, envVars, "PASSWORD", "admin-secret", "ADMIN_PASSWORD")
	assertNoEnvVar(t, envVars, "SKIP_INSTALL")
}

// --- Admin UI PASSWORD auto-injection tests ---

func TestBuildDeployment_UIEnabled_AutoInjectsPassword(t *testing.T) {
	// When ui.enabled=true and adminCredentials has no PASSWORD mapping,
	// PASSWORD should be auto-injected from ADMIN_PASSWORD key.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "ADMIN_PASSWORD"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVarFromSecret(t, envVars, "PASSWORD", "admin-secret", "ADMIN_PASSWORD")
	// The explicit ADMIN_PASSWORD mapping should still be present
	assertEnvVarFromSecret(t, envVars, "ADMIN_PASSWORD", "admin-secret", "ADMIN_PASSWORD")
}

func TestBuildDeployment_UIEnabled_ExplicitPasswordMapping_NoDuplicate(t *testing.T) {
	// When adminCredentials already maps to PASSWORD, no duplicate should be added.
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
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	// Count PASSWORD env vars — should be exactly one
	count := 0
	for _, e := range envVars {
		if e.Name == "PASSWORD" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 PASSWORD env var, got %d", count)
	}
}

func TestBuildDeployment_UIEnabled_NoCredentials_NoPassword(t *testing.T) {
	// When ui.enabled=true but adminCredentials is nil, no PASSWORD should be injected.
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	for _, e := range envVars {
		if e.Name == "PASSWORD" {
			t.Errorf("PASSWORD should not be injected when adminCredentials is nil")
		}
	}
}

func TestBuildDeployment_UIDisabled_NoAutoInject(t *testing.T) {
	// When ui.enabled=false, PASSWORD should not be auto-injected even with credentials.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "ADMIN_PASSWORD"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: false, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	for _, e := range envVars {
		if e.Name == "PASSWORD" {
			t.Errorf("PASSWORD should not be auto-injected when UI is disabled")
		}
	}
}

func TestBuildDeployment_RuntimeNode_NoAutoInject(t *testing.T) {
	// PASSWORD auto-injection is only for admin nodes.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: "admin-secret",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "ADMIN_PASSWORD"},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	for _, e := range envVars {
		if e.Name == "PASSWORD" {
			t.Errorf("PASSWORD should not be auto-injected for runtime nodes")
		}
	}
}

// --- Default Admin Credentials Tests ---

func TestDefaultAdminCredentials_SecretName(t *testing.T) {
	creds := defaultAdminCredentials("my-cluster")
	if creds.ValueFrom.SecretKeyRef.Name != "my-cluster-admin-creds" {
		t.Errorf("expected 'my-cluster-admin-creds', got %q", creds.ValueFrom.SecretKeyRef.Name)
	}
}

func TestDefaultAdminCredentials_Items(t *testing.T) {
	creds := defaultAdminCredentials("test")
	items := creds.ValueFrom.SecretKeyRef.Items
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	expected := []v1alpha1.KeyToPath{
		{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
		{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
	}
	for i, item := range items {
		if item.Key != expected[i].Key || item.Path != expected[i].Path {
			t.Errorf("item[%d]: expected {%s, %s}, got {%s, %s}", i, expected[i].Key, expected[i].Path, item.Key, item.Path)
		}
	}
}

func TestBuildDeployment_DefaultedCredentials_InjectsEnvVars(t *testing.T) {
	// Simulates what the reconciler does: default credentials, then build deployment.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true, Secure: ptr.To(true)}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertEnvVarFromSecret(t, envVars, "PASSWORD", "cluster-1-admin-creds", "ADMIN_PASSWORD")
	assertEnvVarFromSecret(t, envVars, "CONFIG_ENCRYPTION_KEY", "cluster-1-admin-creds", "CONFIG_ENCRYPTION_KEY")
	assertNoEnvVar(t, envVars, "KEYSTORE_PASSWORD")
}

func TestBuildDeployment_DefaultedCredentials_RuntimeNode(t *testing.T) {
	// Runtime nodes get the encryption key but not the admin password — the
	// password is the installer trigger and belongs only on the admin.
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	envVars := deploy.Spec.Template.Spec.Containers[0].Env

	assertNoEnvVar(t, envVars, "PASSWORD")
	assertEnvVarFromSecret(t, envVars, "CONFIG_ENCRYPTION_KEY", "cluster-1-admin-creds", "CONFIG_ENCRYPTION_KEY")
	assertNoEnvVar(t, envVars, "KEYSTORE_PASSWORD")
}

// --- Cluster Config Builder Tests ---

func TestClusterConfigSecretName(t *testing.T) {
	name := clusterConfigSecretName("my-cluster")
	if name != "my-cluster-cluster-config" {
		t.Errorf("expected 'my-cluster-cluster-config', got %q", name)
	}
}

func TestFindAdminNodeName_NoAdmin(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}},
	}
	if got := findAdminNodeName(nodes); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFindAdminNodeName_WithAdmin(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "rt-1"}, Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}},
		{ObjectMeta: metav1.ObjectMeta{Name: "admin-1"}, Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeAdmin}},
		{ObjectMeta: metav1.ObjectMeta{Name: "rt-2"}, Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}},
	}
	if got := findAdminNodeName(nodes); got != "admin-1" {
		t.Errorf("expected 'admin-1', got %q", got)
	}
}

func TestFindAdminNodeName_Empty(t *testing.T) {
	if got := findAdminNodeName(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestIsClusterConfigReady_Placeholder(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{"cluster.xml": []byte("placeholder")}}
	if isClusterConfigReady(secret) {
		t.Error("placeholder should not be ready")
	}
}

func TestIsClusterConfigReady_RealData(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{"cluster.xml": []byte("<config>real xml</config>")}}
	if !isClusterConfigReady(secret) {
		t.Error("real data should be ready")
	}
}

func TestIsClusterConfigReady_MissingKey(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{"other-key": []byte("data")}}
	if isClusterConfigReady(secret) {
		t.Error("missing key should not be ready")
	}
}

func TestIsClusterConfigReady_EmptyData(t *testing.T) {
	secret := &corev1.Secret{Data: map[string][]byte{"cluster.xml": {}}}
	if isClusterConfigReady(secret) {
		t.Error("empty data should not be ready")
	}
}

func TestBuildClusterConfigSecret_Placeholder(t *testing.T) {
	cluster := newTestCluster()
	secret := buildClusterConfigSecret(cluster, nil, "admin-1", "abc123", "deadbeef")

	if secret.Name != "cluster-1-cluster-config" {
		t.Errorf("expected name 'cluster-1-cluster-config', got %q", secret.Name)
	}
	if secret.Namespace != "test-ns" {
		t.Errorf("expected namespace 'test-ns', got %q", secret.Namespace)
	}
	if string(secret.Data["cluster.xml"]) != "placeholder" {
		t.Errorf("expected placeholder data, got %q", secret.Data["cluster.xml"])
	}
	if secret.Labels["curity.io/cluster"] != "cluster-1" {
		t.Errorf("expected cluster label 'cluster-1', got %q", secret.Labels["curity.io/cluster"])
	}
	if secret.Labels["curity.io/component"] != "cluster-config" {
		t.Errorf("expected component label 'cluster-config'")
	}
	if secret.Annotations["curity.io/admin-node"] != "admin-1" {
		t.Errorf("expected admin-node annotation 'admin-1'")
	}
	if secret.Annotations["curity.io/encryption-key-hash"] != "abc123" {
		t.Errorf("expected encryption-key-hash annotation 'abc123'")
	}
	if secret.Annotations["curity.io/cluster-config-hash"] != "deadbeef" {
		t.Errorf("expected cluster-config-hash annotation 'deadbeef', got %q", secret.Annotations["curity.io/cluster-config-hash"])
	}
	if secret.Annotations["argocd.argoproj.io/compare-options"] != "IgnoreExtraneous" {
		t.Errorf("expected ArgoCD IgnoreExtraneous annotation")
	}
}

func TestBuildClusterConfigSecret_WithData(t *testing.T) {
	cluster := newTestCluster()
	xmlData := []byte("<config>test</config>")
	secret := buildClusterConfigSecret(cluster, xmlData, "admin-1", "", "")

	if string(secret.Data["cluster.xml"]) != "<config>test</config>" {
		t.Errorf("expected real data, got %q", secret.Data["cluster.xml"])
	}
	if _, present := secret.Annotations["curity.io/cluster-config-hash"]; present {
		t.Errorf("expected no cluster-config-hash annotation when configHash is empty")
	}
}

func TestBuildClusterConfigJob_BasicSpec(t *testing.T) {
	cluster := newTestCluster()
	job := buildClusterConfigJob(cluster, "admin-1", "")

	if job.Name != "cluster-1-cluster-config-job" {
		t.Errorf("expected job name 'cluster-1-cluster-config-job', got %q", job.Name)
	}
	if job.Namespace != "test-ns" {
		t.Errorf("expected namespace 'test-ns', got %q", job.Namespace)
	}
	if *job.Spec.BackoffLimit != jobBackoffLimit {
		t.Errorf("expected backoff limit %d, got %d", jobBackoffLimit, *job.Spec.BackoffLimit)
	}

	// Container spec
	container := job.Spec.Template.Spec.Containers[0]
	if container.Name != "genclust" {
		t.Errorf("expected container name 'genclust', got %q", container.Name)
	}
	if container.Image != "curity.azurecr.io/curity/idsvr:11.0" {
		t.Errorf("expected default image, got %q", container.Image)
	}

	// Env vars
	// CONFIG_SERVICE_HOST must equal the admin Service name produced by
	// ownedResourceName — runtime pods resolve this hostname via cluster DNS.
	assertEnvVar(t, container.Env, "CONFIG_SERVICE_HOST", OwnedResourceName("cluster-1", "admin-1"))
	assertEnvVar(t, container.Env, "CONFIG_SERVICE_PORT", "6789")

	// Security context
	sc := job.Spec.Template.Spec.SecurityContext
	if *sc.RunAsUser != 10001 {
		t.Errorf("expected runAsUser 10001, got %d", *sc.RunAsUser)
	}

	// No SA token
	if *job.Spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Error("expected automountServiceAccountToken=false")
	}

	// Restart policy
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicyNever")
	}
}

func TestBuildClusterConfigJob_PodFailurePolicyAndTerminationMessage(t *testing.T) {
	job := buildClusterConfigJob(newTestCluster(), "admin-1", "")

	if got := job.Spec.Template.Spec.Containers[0].TerminationMessagePolicy; got != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("expected terminationMessagePolicy FallbackToLogsOnError, got %q", got)
	}

	pfp := job.Spec.PodFailurePolicy
	if pfp == nil || len(pfp.Rules) != 2 {
		t.Fatalf("expected 2 podFailurePolicy rules, got %+v", pfp)
	}
	// Rule 0: Ignore infra disruptions (DisruptionTarget).
	if pfp.Rules[0].Action != batchv1.PodFailurePolicyActionIgnore ||
		len(pfp.Rules[0].OnPodConditions) != 1 ||
		pfp.Rules[0].OnPodConditions[0].Type != corev1.DisruptionTarget {
		t.Errorf("rule 0: expected Ignore on DisruptionTarget, got %+v", pfp.Rules[0])
	}
	// Rule 1: FailJob on genclust exit code 1.
	r1 := pfp.Rules[1]
	if r1.Action != batchv1.PodFailurePolicyActionFailJob || r1.OnExitCodes == nil ||
		r1.OnExitCodes.ContainerName == nil || *r1.OnExitCodes.ContainerName != "genclust" ||
		r1.OnExitCodes.Operator != batchv1.PodFailurePolicyOnExitCodesOpIn ||
		len(r1.OnExitCodes.Values) != 1 || r1.OnExitCodes.Values[0] != 1 {
		t.Errorf("rule 1: expected FailJob on genclust exit 1, got %+v", r1)
	}
}

func TestExtractGenclustFailureMessage(t *testing.T) {
	pod := func(csName string, exit int32, msg, reason string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  csName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg, Reason: reason}},
			}}},
		}
	}
	podAt := func(name string, ts int64, msg string) corev1.Pod {
		p := pod("genclust", 1, msg, "Error")
		p.Name = name
		p.CreationTimestamp = metav1.NewTime(time.Unix(ts, 0))
		return p
	}
	multiContainer := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mc"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "log-sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "genclust", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "boom"}}},
		}},
	}
	stackTail := "\t... 14 more\nCaused by: com.google.common.io.BaseEncoding$DecodingException: Invalid input length 65\n\tat com.google.Foo.bar(Foo.java:1)\n\t... 3 more"
	tests := []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{"exception line from stack tail", []corev1.Pod{pod("genclust", 1, stackTail, "Error")},
			"Invalid input length 65"},
		{"empty message falls back to reason", []corev1.Pod{pod("genclust", 1, "", "OOMKilled")}, "OOMKilled"},
		{"exit 0 ignored", []corev1.Pod{pod("genclust", 0, "x", "Completed")}, ""},
		{"non-genclust container ignored", []corev1.Pod{pod("other", 1, "boom Exception", "Error")}, ""},
		{"newest failed pod wins", []corev1.Pod{podAt("old", 100, "older cause"), podAt("new", 200, "newer cause")}, "newer cause"},
		{"skips non-genclust container in same pod", []corev1.Pod{multiContainer}, "boom"},
		{"no pods", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractGenclustFailureMessage(tt.pods); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSummarizeTerminationMessage(t *testing.T) {
	if got := summarizeTerminationMessage("plain first line\n\tat x\nboom Exception: bad key"); got != "boom Exception: bad key" {
		t.Errorf("exception line: got %q", got)
	}
	if got := summarizeTerminationMessage("just a plain message\n\tat frame(F.java:1)"); got != "just a plain message" {
		t.Errorf("fallback first non-frame line: got %q", got)
	}
	if got := summarizeTerminationMessage(strings.Repeat("x", 300)); len([]rune(got)) != 257 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncation (fallback line): runes=%d suffixOK=%v", len([]rune(got)), strings.HasSuffix(got, "…"))
	}
	// Java root cause: the last "Caused by:" wins over the outer exception line,
	// and the "Caused by:" + fully-qualified class prefix is stripped.
	root := "Exception in thread \"main\" java.lang.IllegalArgumentException: wrapper\n\tat a.B(C.java:1)\nCaused by: com.x.RootException: the real reason\n\t... 9 more"
	if got := summarizeTerminationMessage(root); got != "the real reason" {
		t.Errorf("caused-by preference: got %q", got)
	}
	// Truncation also applies on the matched exception-line path (not just fallback).
	if got := summarizeTerminationMessage("RootException: " + strings.Repeat("y", 300)); len([]rune(got)) != 257 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncation (matched line): runes=%d suffixOK=%v", len([]rune(got)), strings.HasSuffix(got, "…"))
	}
}

func TestHumanizeJavaMessage(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"caused-by + fqcn", "Caused by: com.google.common.io.BaseEncoding$DecodingException: Invalid input length 65", "Invalid input length 65"},
		{"keeps inner colon", "Caused by: com.x.BaseEncoding$DecodingException: Unrecognized character: Z", "Unrecognized character: Z"},
		{"fqcn without caused-by", "com.x.RootException: the real reason", "the real reason"},
		{"bare fqcn, no message", "java.lang.NullPointerException", "NullPointerException"},
		{"package-less class not stripped", "RootException: x", "RootException: x"},
		{"spaced head not stripped", "boom Exception: bad key", "boom Exception: bad key"},
		{"dotted non-throwable not stripped", "config.yaml: bad", "config.yaml: bad"},
		{"plain message untouched", "Invalid input length 65", "Invalid input length 65"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanizeJavaMessage(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChooseJobFailedMessage(t *testing.T) {
	jobFailed := func(msg string) *metav1.Condition {
		return &metav1.Condition{Type: "ClusterConfigReady", Status: metav1.ConditionFalse, Reason: "JobFailed", Message: msg}
	}
	tests := []struct {
		name       string
		podCause   string
		existing   *metav1.Condition
		jobCondMsg string
		want       string
	}{
		{"fresh pod cause wins", "Invalid input length 65", jobFailed("genclust failed: stale"), "backoff", "genclust failed: Invalid input length 65"},
		{"pod GC'd: preserve captured cause (no regress, no re-event)", "", jobFailed("genclust failed: Invalid input length 65"), "backoff", "genclust failed: Invalid input length 65"},
		{"first failure, no pod: generic", "", nil, "Job has reached the specified backoff limit", "genclust Job failed: Job has reached the specified backoff limit"},
		{"prior condition not JobFailed: generic", "", &metav1.Condition{Type: "ClusterConfigReady", Status: metav1.ConditionFalse, Reason: "JobRunning", Message: "running"}, "boom", "genclust Job failed: boom"},
		{"upgrade generic->cause when pod reappears", "real cause", jobFailed("genclust Job failed: backoff"), "backoff", "genclust failed: real cause"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseJobFailedMessage(tt.podCause, tt.existing, tt.jobCondMsg); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildClusterConfigJob_HostUsesPrefixedNameWithHyphens covers scenario E1:
// cluster and admin names that contain hyphens still produce a valid DNS-1123
// label as the genclust CONFIG_SERVICE_HOST, matching the admin Service name.
func TestBuildClusterConfigJob_HostUsesPrefixedNameWithHyphens(t *testing.T) {
	cluster := newTestCluster()
	cluster.Name = "prod-east"
	job := buildClusterConfigJob(cluster, "primary-admin", "")

	container := job.Spec.Template.Spec.Containers[0]
	assertEnvVar(t, container.Env, "CONFIG_SERVICE_HOST", OwnedResourceName("prod-east", "primary-admin"))

	// DNS-1123 label: lowercase alphanumerics and '-', must start/end with alphanumeric, max 63 chars.
	dns1123 := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, e := range container.Env {
		if e.Name == "CONFIG_SERVICE_HOST" {
			if len(e.Value) > 63 {
				t.Errorf("CONFIG_SERVICE_HOST %q exceeds DNS-1123 label length 63", e.Value)
			}
			if !dns1123.MatchString(e.Value) {
				t.Errorf("CONFIG_SERVICE_HOST %q is not a valid DNS-1123 label", e.Value)
			}
		}
	}
}

func TestBuildClusterConfigJob_ImagePullSecret(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.ImagePullSecret = "my-registry-secret"
	job := buildClusterConfigJob(cluster, "admin-1", "")

	if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 {
		t.Fatalf("expected 1 imagePullSecret, got %d", len(job.Spec.Template.Spec.ImagePullSecrets))
	}
	if job.Spec.Template.Spec.ImagePullSecrets[0].Name != "my-registry-secret" {
		t.Errorf("expected 'my-registry-secret', got %q", job.Spec.Template.Spec.ImagePullSecrets[0].Name)
	}
}

func TestBuildClusterConfigJob_NoImagePullSecret(t *testing.T) {
	cluster := newTestCluster()
	job := buildClusterConfigJob(cluster, "admin-1", "")

	if len(job.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("expected 0 imagePullSecrets, got %d", len(job.Spec.Template.Spec.ImagePullSecrets))
	}
}

func TestBuildClusterConfigJob_EncryptionKey(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.AdminCredentials = &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name:  "admin-secret",
				Items: []v1alpha1.KeyToPath{{Key: "ADMIN_PASSWORD", Path: "PASSWORD"}},
			},
		},
	}
	job := buildClusterConfigJob(cluster, "admin-1", "")

	container := job.Spec.Template.Spec.Containers[0]
	found := false
	for _, env := range container.Env {
		if env.Name == "CONFIG_ENCRYPTION_KEY" {
			found = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Error("expected secretKeyRef for CONFIG_ENCRYPTION_KEY")
			} else if env.ValueFrom.SecretKeyRef.Name != "admin-secret" {
				t.Errorf("expected secret name 'admin-secret', got %q", env.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	if !found {
		t.Error("expected CONFIG_ENCRYPTION_KEY env var")
	}
}

func TestBuildClusterConfigJob_SchedulingConstraints(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.NodeSelector = map[string]string{"disk": "ssd"}
	cluster.Spec.Tolerations = []corev1.Toleration{
		{Key: "special", Operator: corev1.TolerationOpExists},
	}

	job := buildClusterConfigJob(cluster, "admin-1", "")

	if job.Spec.Template.Spec.NodeSelector["disk"] != "ssd" {
		t.Error("expected nodeSelector to be inherited")
	}
	if len(job.Spec.Template.Spec.Tolerations) != 1 {
		t.Error("expected tolerations to be inherited")
	}
}

func TestBuildClusterConfigJob_CustomImage(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Image = "myregistry.io/curity:custom"
	job := buildClusterConfigJob(cluster, "admin-1", "")

	if job.Spec.Template.Spec.Containers[0].Image != "myregistry.io/curity:custom" {
		t.Errorf("expected custom image, got %q", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestBuildClusterConfigJob_Labels(t *testing.T) {
	cluster := newTestCluster()
	job := buildClusterConfigJob(cluster, "admin-1", "")

	if job.Labels["curity.io/cluster"] != "cluster-1" {
		t.Error("expected curity.io/cluster label")
	}
	if job.Labels["curity.io/component"] != "cluster-config" {
		t.Error("expected curity.io/component label")
	}
}

func TestBuildVolumes_OnlyClusterConfig(t *testing.T) {
	cluster := newTestCluster()
	volumes, mounts := buildVolumes(cluster.Name, nil)

	if len(volumes) != 1 {
		t.Fatalf("expected exactly 1 volume, got %d", len(volumes))
	}
	if volumes[0].Name != "cluster-config" {
		t.Errorf("expected first volume 'cluster-config', got %q", volumes[0].Name)
	}
	if volumes[0].Secret.SecretName != "cluster-1-cluster-config" {
		t.Errorf("expected secret name 'cluster-1-cluster-config', got %q", volumes[0].Secret.SecretName)
	}
	if *volumes[0].Secret.Optional != true {
		t.Error("expected cluster-config volume to be optional")
	}

	if len(mounts) != 1 {
		t.Fatalf("expected exactly 1 mount, got %d", len(mounts))
	}
	if mounts[0].MountPath != "/opt/idsvr/etc/init/cluster.xml" {
		t.Errorf("expected mount path '/opt/idsvr/etc/init/cluster.xml', got %q", mounts[0].MountPath)
	}
	if mounts[0].SubPath != "cluster.xml" {
		t.Errorf("expected subPath 'cluster.xml', got %q", mounts[0].SubPath)
	}
	if !mounts[0].ReadOnly {
		t.Error("expected cluster-config mount to be read-only")
	}
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

func containerNames(cs []corev1.Container) []string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return names
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

func assertNoEnvVar(t *testing.T, envVars []corev1.EnvVar, name string) {
	t.Helper()
	for _, e := range envVars {
		if e.Name == name {
			t.Errorf("expected env var %s to be absent", name)
			return
		}
	}
}

// --- Scheduling tests ---

func TestBuildDeployment_NodeSelectorClusterOnly(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.NodeSelector = map[string]string{"pool": "curity", "env": "prod"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	ns := deploy.Spec.Template.Spec.NodeSelector
	if ns["pool"] != "curity" || ns["env"] != "prod" {
		t.Errorf("expected cluster nodeSelector, got %v", ns)
	}
}

func TestBuildDeployment_NodeSelectorNodeOnly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.NodeSelector = map[string]string{"disk": "ssd"}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	ns := deploy.Spec.Template.Spec.NodeSelector
	if ns["disk"] != "ssd" {
		t.Errorf("expected node nodeSelector, got %v", ns)
	}
}

func TestBuildDeployment_NodeSelectorMerge(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.NodeSelector = map[string]string{"pool": "curity", "env": "prod"}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.NodeSelector = map[string]string{"pool": "gpu", "disk": "ssd"}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	ns := deploy.Spec.Template.Spec.NodeSelector
	if ns["pool"] != "gpu" {
		t.Errorf("expected node to win on conflict key 'pool', got %q", ns["pool"])
	}
	if ns["env"] != "prod" {
		t.Errorf("expected cluster key 'env' preserved, got %q", ns["env"])
	}
	if ns["disk"] != "ssd" {
		t.Errorf("expected node key 'disk' added, got %q", ns["disk"])
	}
}

func TestBuildDeployment_TolerationsClusterOnly(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Tolerations = []corev1.Toleration{
		{Key: "special", Operator: corev1.TolerationOpExists},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	// User toleration first, then 2 NoExecute defaults appended.
	tols := deploy.Spec.Template.Spec.Tolerations
	if len(tols) != 3 || tols[0].Key != "special" {
		t.Errorf("expected cluster toleration + 2 defaults, got %v", tols)
	}
}

func TestBuildDeployment_TolerationsNodeOnly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Tolerations = []corev1.Toleration{
		{Key: "gpu", Operator: corev1.TolerationOpEqual, Value: "true"},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	tols := deploy.Spec.Template.Spec.Tolerations
	if len(tols) != 3 || tols[0].Key != "gpu" {
		t.Errorf("expected node toleration + 2 defaults, got %v", tols)
	}
}

func TestBuildDeployment_TolerationsNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Tolerations = []corev1.Toleration{
		{Key: "old", Operator: corev1.TolerationOpExists},
		{Key: "another", Operator: corev1.TolerationOpExists},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Tolerations = []corev1.Toleration{
		{Key: "new", Operator: corev1.TolerationOpEqual, Value: "yes"},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	tols := deploy.Spec.Template.Spec.Tolerations
	// Node tolerations fully replace cluster tolerations; the two NoExecute
	// defaults are then appended unconditionally to avoid drift against the
	// DefaultTolerationSeconds admission controller.
	if len(tols) != 3 || tols[0].Key != "new" {
		t.Errorf("expected node tolerations + 2 defaults, got %v", tols)
	}
}

func TestBuildDeployment_AffinityClusterOnly(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "kubernetes.io/arch",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"amd64"},
					}},
				}},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	aff := deploy.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil {
		t.Fatal("expected cluster affinity to propagate")
	}
}

func TestBuildDeployment_AffinityNodeOnly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey: "kubernetes.io/hostname",
				},
			}},
		},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	aff := deploy.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAntiAffinity == nil {
		t.Fatal("expected node affinity to propagate")
	}
}

func TestBuildDeployment_AffinityNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	aff := deploy.Spec.Template.Spec.Affinity
	if aff.NodeAffinity != nil {
		t.Error("expected cluster affinity to be replaced, but NodeAffinity still present")
	}
	if aff.PodAntiAffinity == nil {
		t.Error("expected node affinity to win")
	}
}

func TestBuildDeployment_TopologySpreadClusterOnly(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}}
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	tsc := deploy.Spec.Template.Spec.TopologySpreadConstraints
	if len(tsc) != 1 || tsc[0].TopologyKey != "topology.kubernetes.io/zone" {
		t.Errorf("expected cluster topology spread, got %v", tsc)
	}
}

func TestBuildDeployment_TopologySpreadNodeOnly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           2,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	tsc := deploy.Spec.Template.Spec.TopologySpreadConstraints
	if len(tsc) != 1 || tsc[0].TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("expected node topology spread, got %v", tsc)
	}
}

func TestBuildDeployment_TopologySpreadNodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{MaxSkew: 1, TopologyKey: "zone", WhenUnsatisfiable: corev1.ScheduleAnyway},
		{MaxSkew: 2, TopologyKey: "region", WhenUnsatisfiable: corev1.ScheduleAnyway},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{MaxSkew: 3, TopologyKey: "hostname", WhenUnsatisfiable: corev1.DoNotSchedule},
	}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	tsc := deploy.Spec.Template.Spec.TopologySpreadConstraints
	if len(tsc) != 1 || tsc[0].TopologyKey != "hostname" {
		t.Errorf("expected node topology spread to fully replace cluster, got %v", tsc)
	}
}

func TestBuildDeployment_SchedulingNilSafe(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	if deploy.Spec.Template.Spec.NodeSelector != nil {
		t.Error("expected nil nodeSelector")
	}
	// Tolerations always include the two NoExecute defaults that the
	// DefaultTolerationSeconds admission controller would otherwise add
	// (not-ready, unreachable). The operator sets them explicitly to avoid
	// drift on every reconcile.
	if got := len(deploy.Spec.Template.Spec.Tolerations); got != 2 {
		t.Errorf("expected 2 default tolerations when user has none, got %d: %+v",
			got, deploy.Spec.Template.Spec.Tolerations)
	}
	if deploy.Spec.Template.Spec.Affinity != nil {
		t.Error("expected nil affinity")
	}
	if deploy.Spec.Template.Spec.TopologySpreadConstraints != nil {
		t.Error("expected nil topologySpreadConstraints")
	}
}

func TestBuildClusterConfigJob_TopologySpreadConstraints(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}}

	job := buildClusterConfigJob(cluster, "admin-1", "")

	tsc := job.Spec.Template.Spec.TopologySpreadConstraints
	if len(tsc) != 1 || tsc[0].TopologyKey != "topology.kubernetes.io/zone" {
		t.Errorf("expected topology spread on job, got %v", tsc)
	}
}

// --- buildVolumes with discovered configs ---

func TestBuildVolumes_BaseConfigMap(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "base-cm", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"config.xml": []byte("<c/>")}},
	}
	volumes, mounts := buildVolumes("cluster-1", configs)
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes (cluster-config + base-cm), got %d", len(volumes))
	}
	if volumes[0].Name != "cluster-config" {
		t.Errorf("expected first volume to be cluster-config, got %q", volumes[0].Name)
	}
	vol := volumes[1]
	if vol.ConfigMap == nil || vol.ConfigMap.Name != "base-cm" {
		t.Errorf("expected ConfigMap volume for base-cm, got %v", vol)
	}
	found := false
	wantPath := MountPathBase + mountFilename(false, "base-cm", "config.xml")
	for _, m := range mounts {
		if m.SubPath == "config.xml" && m.MountPath == wantPath {
			found = true
		}
	}
	if !found {
		t.Errorf("expected mount at %s", wantPath)
	}
}

func TestVolumeDefaultMode(t *testing.T) {
	cases := []struct {
		name       string
		configType string
		isSecret   bool
		want       int32
	}{
		{"postCommitScript ConfigMap → 0755 (other-exec)", ConfigTypePostCommitScript, false, 0o755},
		{"postCommitScript Secret → 0555 (other-exec, no write)", ConfigTypePostCommitScript, true, 0o555},
		{"base ConfigMap keeps apiserver default", ConfigTypeBase, false, corev1.ConfigMapVolumeSourceDefaultMode},
		{"base Secret keeps apiserver default", ConfigTypeBase, true, corev1.SecretVolumeSourceDefaultMode},
		{"logging ConfigMap keeps apiserver default", ConfigTypeLogging, false, corev1.ConfigMapVolumeSourceDefaultMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := volumeDefaultMode(tc.configType, tc.isSecret)
			if got == nil || *got != tc.want {
				t.Errorf("volumeDefaultMode(%q, secret=%v) = %v, want %o", tc.configType, tc.isSecret, got, tc.want)
			}
		})
	}
}

func TestBuildVolumes_PostCommitScriptConfigMapIsExecutableDirMount(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "hooks", IsSecret: false, ConfigType: ConfigTypePostCommitScript,
			Data: map[string][]byte{"notify.sh": []byte("#!/bin/sh\n")}},
		// Regression guard: a base ConfigMap in the same call must keep 0644.
		{Name: "base-cm", IsSecret: false, ConfigType: ConfigTypeBase,
			Data: map[string][]byte{"config.xml": []byte("<c/>")}},
	}
	volumes, mounts := buildVolumes("cluster-1", configs)

	var scriptVol, baseVol *corev1.Volume
	for i := range volumes {
		switch volumes[i].Name {
		case configVolumeName(false, "hooks"):
			scriptVol = &volumes[i]
		case configVolumeName(false, "base-cm"):
			baseVol = &volumes[i]
		}
	}
	if scriptVol == nil || scriptVol.ConfigMap == nil {
		t.Fatalf("expected ConfigMap volume for postCommitScript; volumes=%v", volumes)
	}
	if scriptVol.ConfigMap.DefaultMode == nil || *scriptVol.ConfigMap.DefaultMode != 0o755 {
		t.Errorf("expected postCommitScript volume DefaultMode 0755, got %v", scriptVol.ConfigMap.DefaultMode)
	}
	// Regression: the per-type mode helper must not disturb base config.
	if baseVol == nil || baseVol.ConfigMap == nil ||
		baseVol.ConfigMap.DefaultMode == nil || *baseVol.ConfigMap.DefaultMode != corev1.ConfigMapVolumeSourceDefaultMode {
		t.Errorf("base ConfigMap DefaultMode must stay apiserver default, got %v", baseVol.ConfigMap.DefaultMode)
	}

	// Directory mount: per-key mangled filename under the post-commit-scripts dir.
	wantPath := MountPathPostCommitScript + mountFilename(false, "hooks", "notify.sh")
	var m *corev1.VolumeMount
	for i := range mounts {
		if mounts[i].MountPath == wantPath {
			m = &mounts[i]
			break
		}
	}
	if m == nil {
		t.Fatalf("expected mount at %s; mounts=%v", wantPath, mounts)
	}
	if m.SubPath != "notify.sh" {
		t.Errorf("expected SubPath notify.sh, got %q", m.SubPath)
	}
	if !m.ReadOnly {
		t.Error("expected ReadOnly mount")
	}
}

func TestBuildVolumes_PostCommitScriptSecretIs0555(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "sec-hooks", IsSecret: true, ConfigType: ConfigTypePostCommitScript,
			Data: map[string][]byte{"run.sh": []byte("#!/bin/sh\n")}},
	}
	volumes, _ := buildVolumes("cluster-1", configs)
	var vol *corev1.Volume
	for i := range volumes {
		if volumes[i].Name == configVolumeName(true, "sec-hooks") {
			vol = &volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatalf("expected Secret volume for postCommitScript; volumes=%v", volumes)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o555 {
		t.Errorf("expected postCommitScript Secret DefaultMode 0555, got %v", vol.Secret.DefaultMode)
	}
}

func TestBuildVolumes_LoggingConfigMapMountsAtLeafPath(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "my-log4j2", IsSecret: false, ConfigType: ConfigTypeLogging,
			Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")}},
	}
	volumes, mounts := buildVolumes("cluster-1", configs)
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes (cluster-config + logging), got %d", len(volumes))
	}
	vol := volumes[1]
	if vol.ConfigMap == nil || vol.ConfigMap.Name != "my-log4j2" {
		t.Errorf("expected ConfigMap volume for my-log4j2, got %v", vol)
	}

	// Find the logging mount and assert its leaf-path properties.
	var logMount *corev1.VolumeMount
	for i, m := range mounts {
		if m.MountPath == MountPathLogging {
			logMount = &mounts[i]
			break
		}
	}
	if logMount == nil {
		t.Fatalf("expected mount at %s; mounts=%v", MountPathLogging, mounts)
	}
	if logMount.SubPath != LoggingDataKey {
		t.Errorf("expected SubPath=%q, got %q", LoggingDataKey, logMount.SubPath)
	}
	// Assert NO mangled-path mount exists (regression guard against
	// accidentally falling through to the directory-base loop).
	mangled := MountPathBase + mountFilename(false, "my-log4j2", LoggingDataKey)
	for _, m := range mounts {
		if m.MountPath == mangled {
			t.Errorf("logging type must not produce mangled mount; found %q", mangled)
		}
	}
}

func TestBuildVolumes_LoggingMalformedResourceProducesNoOrphanVolume(t *testing.T) {
	// Defensive: if a leaf-mount config ever reaches buildVolumes with the
	// wrong number of keys (which applyLoggingValidations should prevent),
	// skip the resource entirely — no volume, no mount. Regression guard
	// against an orphan volume being appended without a matching mount.
	configs := []DiscoveredManagedResource{
		{Name: "malformed", ConfigType: ConfigTypeLogging,
			Data: map[string][]byte{"a.xml": []byte("<a/>"), "b.xml": []byte("<b/>")}},
	}
	volumes, mounts := buildVolumes("cluster-1", configs)
	for _, v := range volumes {
		if v.Name == "cfg-cm-malformed" {
			t.Errorf("malformed leaf-mount resource must not produce a volume; got %v", v)
		}
	}
	for _, m := range mounts {
		if m.Name == "cfg-cm-malformed" {
			t.Errorf("malformed leaf-mount resource must not produce a mount; got %v", m)
		}
	}
}

func TestBuildVolumes_LoggingSecretMountsAtLeafPath(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "log-sec", IsSecret: true, ConfigType: ConfigTypeLogging,
			Data: map[string][]byte{LoggingDataKey: []byte("<Configuration/>")}},
	}
	volumes, mounts := buildVolumes("cluster-1", configs)
	if vol := volumes[1]; vol.Secret == nil || vol.Secret.SecretName != "log-sec" {
		t.Errorf("expected Secret volume for log-sec, got %v", vol)
	}
	found := false
	for _, m := range mounts {
		if m.MountPath == MountPathLogging && m.SubPath == LoggingDataKey {
			found = true
		}
	}
	if !found {
		t.Errorf("expected leaf-path mount at %s for Secret", MountPathLogging)
	}
}

func TestBuildVolumes_BaseLicenseLoggingCoexist(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "base-cm", IsSecret: false, ConfigType: ConfigTypeBase,
			Data: map[string][]byte{LoggingDataKey: []byte("<base/>")}},
		{Name: "lic", IsSecret: true, ConfigType: ConfigTypeLicense,
			Data: map[string][]byte{"license.json": []byte("{}")}},
		{Name: "log", IsSecret: false, ConfigType: ConfigTypeLogging,
			Data: map[string][]byte{LoggingDataKey: []byte("<log/>")}},
	}
	_, mounts := buildVolumes("cluster-1", configs)

	wantBase := MountPathBase + mountFilename(false, "base-cm", LoggingDataKey)
	wantLicense := MountPathLicense + mountFilename(true, "lic", "license.json")
	wantLogging := MountPathLogging

	gotPaths := make(map[string]bool)
	for _, m := range mounts {
		gotPaths[m.MountPath] = true
	}
	for _, want := range []string{wantBase, wantLicense, wantLogging} {
		if !gotPaths[want] {
			t.Errorf("missing expected mount path %q; got paths %v", want, gotPaths)
		}
	}
}

func TestBuildVolumes_LicenseSecret(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "lic", IsSecret: true, ConfigType: ConfigTypeLicense, Data: map[string][]byte{"license.json": []byte("{}")}},
	}
	volumes, mounts := buildVolumes("cluster-1", configs)
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	vol := volumes[1]
	if vol.Secret == nil || vol.Secret.SecretName != "lic" {
		t.Errorf("expected Secret volume for lic, got %v", vol)
	}
	found := false
	wantPath := MountPathLicense + mountFilename(true, "lic", "license.json")
	for _, m := range mounts {
		if m.SubPath == "license.json" && m.MountPath == wantPath {
			found = true
		}
	}
	if !found {
		t.Errorf("expected mount at %s", wantPath)
	}
}

func TestBuildVolumes_MultipleDiscoveredConfigs(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm-1", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
		{Name: "sec-1", IsSecret: true, ConfigType: ConfigTypeLicense, Data: map[string][]byte{"b.json": []byte("{}")}},
	}
	volumes, _ := buildVolumes("cluster-1", configs)
	// 1 cluster-config + 2 discovered = 3
	if len(volumes) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(volumes))
	}
}

func TestBuildVolumes_ClusterConfigAlwaysFirst(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "aaa", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"a.xml": []byte("<a/>")}},
	}
	volumes, _ := buildVolumes("cluster-1", configs)
	if volumes[0].Name != "cluster-config" {
		t.Errorf("expected cluster-config first, got %q", volumes[0].Name)
	}
}

func TestBuildVolumes_VolumeNaming(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "foo", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"x.xml": []byte("<x/>")}},
		{Name: "foo", IsSecret: true, ConfigType: ConfigTypeBase, Data: map[string][]byte{"y.xml": []byte("<y/>")}},
	}
	volumes, _ := buildVolumes("cluster-1", configs)
	if len(volumes) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(volumes))
	}
	// ConfigMap and Secret with same name should have different volume names.
	if volumes[1].Name == volumes[2].Name {
		t.Errorf("expected different volume names for cm and secret, both got %q", volumes[1].Name)
	}
}

func TestBuildVolumes_DeterministicMountOrder(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{
			"z.xml": []byte("<z/>"),
			"a.xml": []byte("<a/>"),
		}},
	}
	_, mounts := buildVolumes("cluster-1", configs)
	// First mount is cluster-config, then sorted keys: a.xml before z.xml
	configMounts := mounts[1:]
	if len(configMounts) != 2 {
		t.Fatalf("expected 2 config mounts, got %d", len(configMounts))
	}
	if configMounts[0].SubPath != "a.xml" {
		t.Errorf("expected first config mount to be a.xml, got %q", configMounts[0].SubPath)
	}
	if configMounts[1].SubPath != "z.xml" {
		t.Errorf("expected second config mount to be z.xml, got %q", configMounts[1].SubPath)
	}
}

func TestBuildVolumes_DuplicateKeysDifferentResources(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "cm-a", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"base-config.xml": []byte("<a/>")}},
		{Name: "cm-b", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"base-config.xml": []byte("<b/>")}},
	}
	_, mounts := buildVolumes("cluster-1", configs)
	// cluster-config + 2 config mounts = 3
	if len(mounts) != 3 {
		t.Fatalf("expected 3 mounts, got %d", len(mounts))
	}
	// The two config mounts must have different MountPaths.
	if mounts[1].MountPath == mounts[2].MountPath {
		t.Errorf("expected different mount paths for duplicate keys, both got %q", mounts[1].MountPath)
	}
}

func TestBuildVolumes_ClusterConfigUnchanged(t *testing.T) {
	configs := []DiscoveredManagedResource{
		{Name: "my-cm", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"cluster.xml": []byte("<c/>")}},
	}
	_, mounts := buildVolumes("cluster-1", configs)
	// First mount is the operator's internal cluster.xml — must NOT be prefixed.
	if mounts[0].MountPath != "/opt/idsvr/etc/init/cluster.xml" {
		t.Errorf("operator cluster.xml mount changed: got %q", mounts[0].MountPath)
	}
	// Second mount is the user's config — must be prefixed, not colliding.
	wantPath := MountPathBase + mountFilename(false, "my-cm", "cluster.xml")
	if mounts[1].MountPath != wantPath {
		t.Errorf("user cluster.xml mount: got %q, want %q", mounts[1].MountPath, wantPath)
	}
}

func TestBuildDeployment_WithDiscoveredConfigs(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	configs := []DiscoveredManagedResource{
		{Name: "base-config", IsSecret: false, ConfigType: ConfigTypeBase, Data: map[string][]byte{"cfg.xml": []byte("<cfg/>")}},
	}
	deploy := buildDeployment(cluster, node, configs, DefaultPackageFetcherImage)
	container := deploy.Spec.Template.Spec.Containers[0]
	found := false
	wantPath := MountPathBase + mountFilename(false, "base-config", "cfg.xml")
	for _, m := range container.VolumeMounts {
		if m.SubPath == "cfg.xml" && m.MountPath == wantPath {
			found = true
		}
	}
	if !found {
		t.Errorf("expected discovered config mount at %s", wantPath)
	}
}

// --- resolveAutoscaling tests ---

func TestResolveAutoscaling_BothNil(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	result := resolveAutoscaling(cluster, node)
	if result != nil {
		t.Errorf("expected nil, got %+v", result)
	}
}

func TestResolveAutoscaling_ClusterOnly(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    3,
		MaxReplicas:                    8,
		TargetCPUUtilizationPercentage: 70,
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	result := resolveAutoscaling(cluster, node)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Enabled || result.MinReplicas != 3 || result.MaxReplicas != 8 || result.TargetCPUUtilizationPercentage != 70 {
		t.Errorf("expected cluster spec, got %+v", result)
	}
}

func TestResolveAutoscaling_NodeOnly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	result := resolveAutoscaling(cluster, node)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.MinReplicas != 1 || result.MaxReplicas != 5 {
		t.Errorf("expected node spec, got %+v", result)
	}
}

func TestResolveAutoscaling_NodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    5,
		MaxReplicas:                    20,
		TargetCPUUtilizationPercentage: 60,
	}
	result := resolveAutoscaling(cluster, node)
	if result.MinReplicas != 5 || result.MaxReplicas != 20 || result.TargetCPUUtilizationPercentage != 60 {
		t.Errorf("expected node to override cluster, got %+v", result)
	}
}

// --- resolveReplicas tests ---

func TestResolveReplicas_AdminForcedToOne(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 3}
	node := newTestNode(v1alpha1.NodeTypeAdmin)
	node.Spec.Replicas = ptr.To(int32(5))
	result := resolveReplicas(cluster, node)
	if result == nil || *result != 1 {
		t.Errorf("expected 1 for admin, got %v", result)
	}
}

func TestResolveReplicas_RuntimeFromSpec(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Replicas = ptr.To(int32(3))
	result := resolveReplicas(cluster, node)
	if result == nil || *result != 3 {
		t.Errorf("expected 3, got %v", result)
	}
}

func TestResolveReplicas_RuntimeDefaultsToOne(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Replicas = nil
	result := resolveReplicas(cluster, node)
	if result == nil || *result != 1 {
		t.Errorf("expected 1, got %v", result)
	}
}

func TestResolveReplicas_HPAEnabledReturnsMinReplicas(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Replicas = ptr.To(int32(5))
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 3}
	result := resolveReplicas(cluster, node)
	if result == nil || *result != 3 {
		t.Errorf("expected minReplicas=3, got %v", result)
	}
}

func TestResolveReplicas_ClusterLevelHPAReturnsMinReplicas(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 2}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Replicas = ptr.To(int32(3))
	result := resolveReplicas(cluster, node)
	if result == nil || *result != 2 {
		t.Errorf("expected cluster minReplicas=2, got %v", result)
	}
}

// --- buildHPA tests ---

func TestBuildHPA_NilAutoscaling(t *testing.T) {
	// Caller is responsible for nil-checking resolveAutoscaling before calling buildHPA.
	// Verify resolveAutoscaling returns nil when neither cluster nor node has autoscaling.
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	as := resolveAutoscaling(cluster, node)
	if as != nil {
		t.Errorf("expected nil autoscaling, got %+v", as)
	}
}

func TestBuildHPA_BasicCPU(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
	}
	hpa := buildHPA(cluster, node, resolveAutoscaling(cluster, node))
	want := OwnedResourceName(cluster.Name, node.Name)
	if hpa.Name != want {
		t.Errorf("expected name %s, got %q", want, hpa.Name)
	}
	if hpa.Namespace != "test-ns" {
		t.Errorf("expected namespace test-ns, got %q", hpa.Namespace)
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" {
		t.Errorf("expected target kind Deployment, got %q", hpa.Spec.ScaleTargetRef.Kind)
	}
	if hpa.Spec.ScaleTargetRef.Name != want {
		t.Errorf("expected target name %s, got %q", want, hpa.Spec.ScaleTargetRef.Name)
	}
	if hpa.Spec.ScaleTargetRef.APIVersion != "apps/v1" {
		t.Errorf("expected apiVersion apps/v1, got %q", hpa.Spec.ScaleTargetRef.APIVersion)
	}
	if *hpa.Spec.MinReplicas != 2 {
		t.Errorf("expected minReplicas 2, got %d", *hpa.Spec.MinReplicas)
	}
	if hpa.Spec.MaxReplicas != 10 {
		t.Errorf("expected maxReplicas 10, got %d", hpa.Spec.MaxReplicas)
	}
	if len(hpa.Spec.Metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(hpa.Spec.Metrics))
	}
	m := hpa.Spec.Metrics[0]
	if m.Type != autoscalingv2.ResourceMetricSourceType {
		t.Errorf("expected Resource metric type, got %q", m.Type)
	}
	if *m.Resource.Target.AverageUtilization != 80 {
		t.Errorf("expected 80%% CPU target, got %d", *m.Resource.Target.AverageUtilization)
	}
}

func TestBuildHPA_WithCustomMetrics(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
		CustomMetrics: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: "http_requests_per_second"},
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: ptr.To(resource.MustParse("1000")),
					},
				},
			},
		},
	}
	hpa := buildHPA(cluster, node, resolveAutoscaling(cluster, node))
	if len(hpa.Spec.Metrics) != 2 {
		t.Fatalf("expected 2 metrics (CPU + custom), got %d", len(hpa.Spec.Metrics))
	}
	if hpa.Spec.Metrics[0].Type != autoscalingv2.ResourceMetricSourceType {
		t.Errorf("expected first metric to be Resource (CPU), got %q", hpa.Spec.Metrics[0].Type)
	}
	if hpa.Spec.Metrics[1].Type != autoscalingv2.PodsMetricSourceType {
		t.Errorf("expected second metric to be Pods, got %q", hpa.Spec.Metrics[1].Type)
	}
}

func TestBuildHPA_PassesThroughSpecValues(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
	}
	hpa := buildHPA(cluster, node, resolveAutoscaling(cluster, node))
	if *hpa.Spec.MinReplicas != 2 {
		t.Errorf("expected default minReplicas 2, got %d", *hpa.Spec.MinReplicas)
	}
	if hpa.Spec.MaxReplicas != 10 {
		t.Errorf("expected default maxReplicas 10, got %d", hpa.Spec.MaxReplicas)
	}
}

func TestBuildHPA_Labels(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 10, TargetCPUUtilizationPercentage: 80}
	hpa := buildHPA(cluster, node, resolveAutoscaling(cluster, node))
	expectedLabels := map[string]string{
		"app.kubernetes.io/name":       "curity-identity-server",
		"app.kubernetes.io/instance":   OwnedResourceName("cluster-1", "node-1"),
		"app.kubernetes.io/managed-by": "curity-operator",
		"app.kubernetes.io/component":  "runtime",
		"app.kubernetes.io/version":    "11.0",
		"curity.io/cluster":            "cluster-1",
		"curity.io/role":               "test-role",
		"curity.io/owned-by":           OwnedResourceName("cluster-1", "node-1"),
	}
	for k, v := range expectedLabels {
		if hpa.Labels[k] != v {
			t.Errorf("label %q: expected %q, got %q", k, v, hpa.Labels[k])
		}
	}
}

func TestBuildHPA_NodeOverridesClusterValues(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    5,
		MaxReplicas:                    20,
		TargetCPUUtilizationPercentage: 60,
	}
	hpa := buildHPA(cluster, node, resolveAutoscaling(cluster, node))
	if *hpa.Spec.MinReplicas != 5 {
		t.Errorf("expected node minReplicas 5, got %d", *hpa.Spec.MinReplicas)
	}
	if hpa.Spec.MaxReplicas != 20 {
		t.Errorf("expected node maxReplicas 20, got %d", hpa.Spec.MaxReplicas)
	}
	if *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization != 60 {
		t.Errorf("expected node CPU target 60, got %d", *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	}
}

func TestBuildHPA_ClusterLevelCustomMetrics(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
		CustomMetrics: []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: "http_requests_per_second"},
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: ptr.To(resource.MustParse("500")),
					},
				},
			},
		},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	// Node has no autoscaling — should inherit cluster spec including custom metrics
	hpa := buildHPA(cluster, node, resolveAutoscaling(cluster, node))
	if len(hpa.Spec.Metrics) != 2 {
		t.Fatalf("expected 2 metrics (CPU + cluster custom), got %d", len(hpa.Spec.Metrics))
	}
	if hpa.Spec.Metrics[0].Type != autoscalingv2.ResourceMetricSourceType {
		t.Errorf("expected first metric CPU, got %q", hpa.Spec.Metrics[0].Type)
	}
	if hpa.Spec.Metrics[1].Type != autoscalingv2.PodsMetricSourceType {
		t.Errorf("expected second metric Pods, got %q", hpa.Spec.Metrics[1].Type)
	}
	if hpa.Spec.Metrics[1].Pods.Metric.Name != "http_requests_per_second" {
		t.Errorf("expected metric name http_requests_per_second, got %q", hpa.Spec.Metrics[1].Pods.Metric.Name)
	}
}

// --- buildDeployment + HPA integration tests ---

func TestBuildDeployment_HPAEnabled_ReplicasMatchMinReplicas(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    2,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: 80,
	}
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	if deploy.Spec.Replicas == nil {
		t.Fatal("expected non-nil replicas")
	}
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("expected replicas=minReplicas=2, got %d", *deploy.Spec.Replicas)
	}
}

func TestBuildDeployment_HPADisabled_ReplicasFromSpec(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{Enabled: false}
	node.Spec.Replicas = ptr.To(int32(3))
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 3 {
		t.Errorf("expected replicas=3, got %v", deploy.Spec.Replicas)
	}
}

// --- resolvePDB tests ---

func TestResolvePDB_BothNil(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	if got := resolvePDB(cluster, node); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestResolvePDB_ClusterOnly(t *testing.T) {
	cluster := newTestCluster()
	min := intstr.FromInt32(2)
	cluster.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	got := resolvePDB(cluster, node)
	if got != cluster.Spec.PodDisruptionBudget {
		t.Errorf("expected cluster's PDB pointer, got %p want %p", got, cluster.Spec.PodDisruptionBudget)
	}
}

func TestResolvePDB_NodeOnly(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	min := intstr.FromString("50%")
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}
	got := resolvePDB(cluster, node)
	if got != node.Spec.PodDisruptionBudget {
		t.Errorf("expected node's PDB pointer, got %p want %p", got, node.Spec.PodDisruptionBudget)
	}
}

func TestResolvePDB_NodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	clusterMin := intstr.FromInt32(1)
	cluster.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &clusterMin}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	nodeMin := intstr.FromInt32(5)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &nodeMin}
	got := resolvePDB(cluster, node)
	if got != node.Spec.PodDisruptionBudget {
		t.Errorf("expected node's PDB to win, got %p want %p", got, node.Spec.PodDisruptionBudget)
	}
}

// --- buildPDB tests ---

func TestBuildPDB_MinAvailable_Integer(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	min := intstr.FromInt32(2)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}

	pdb := buildPDB(cluster, node)
	if pdb.Spec.MinAvailable == nil {
		t.Fatal("expected non-nil MinAvailable")
	}
	if pdb.Spec.MinAvailable.Type != intstr.Int {
		t.Errorf("expected MinAvailable type=Int, got %v", pdb.Spec.MinAvailable.Type)
	}
	if pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Errorf("expected MinAvailable=2, got %d", pdb.Spec.MinAvailable.IntValue())
	}
	if pdb.Spec.MaxUnavailable != nil {
		t.Errorf("expected nil MaxUnavailable, got %+v", pdb.Spec.MaxUnavailable)
	}
}

func TestBuildPDB_MinAvailable_Percentage(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	min := intstr.FromString("50%")
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}

	pdb := buildPDB(cluster, node)
	if pdb.Spec.MinAvailable == nil {
		t.Fatal("expected non-nil MinAvailable")
	}
	if pdb.Spec.MinAvailable.Type != intstr.String {
		t.Errorf("expected MinAvailable type=String, got %v", pdb.Spec.MinAvailable.Type)
	}
	if pdb.Spec.MinAvailable.StrVal != "50%" {
		t.Errorf("expected MinAvailable=\"50%%\", got %q", pdb.Spec.MinAvailable.StrVal)
	}
}

func TestBuildPDB_MaxUnavailable(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	max := intstr.FromInt32(1)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MaxUnavailable: &max}

	pdb := buildPDB(cluster, node)
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("expected MaxUnavailable=1, got %+v", pdb.Spec.MaxUnavailable)
	}
	if pdb.Spec.MinAvailable != nil {
		t.Errorf("MinAvailable must be unset when maxUnavailable is used, got %+v", pdb.Spec.MinAvailable)
	}
}

func TestBuildPDB_Labels(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	min := intstr.FromInt32(1)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}

	pdb := buildPDB(cluster, node)
	want := buildLabels(cluster, node)
	for k, v := range want {
		if pdb.Labels[k] != v {
			t.Errorf("label %q: want %q, got %q", k, v, pdb.Labels[k])
		}
	}
}

func TestBuildPDB_Name(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	min := intstr.FromInt32(1)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}

	pdb := buildPDB(cluster, node)
	want := OwnedResourceName(cluster.Name, node.Name)
	if pdb.Name != want {
		t.Errorf("expected name %q, got %q", want, pdb.Name)
	}
	if pdb.Namespace != node.Namespace {
		t.Errorf("expected namespace %q, got %q", node.Namespace, pdb.Namespace)
	}
}

func TestBuildPDB_Selector(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	min := intstr.FromInt32(1)
	node.Spec.PodDisruptionBudget = &v1alpha1.PDBSpec{MinAvailable: &min}

	pdb := buildPDB(cluster, node)
	if pdb.Spec.Selector == nil {
		t.Fatal("expected non-nil Selector")
	}
	want := buildSelectorLabels(cluster, node)
	for k, v := range want {
		if pdb.Spec.Selector.MatchLabels[k] != v {
			t.Errorf("selector label %q: want %q, got %q", k, v, pdb.Spec.Selector.MatchLabels[k])
		}
	}
}

// Bridges for the external controller_test package; not part of the public API.

func ComputeClusterConfigHashForTest(cluster *v1alpha1.IdentityServerCluster, adminNodeName string) string {
	return computeClusterConfigHash(cluster, adminNodeName)
}

func EncryptionKeyHashForTest(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	h := sha256.Sum256(key)
	return hex.EncodeToString(h[:])
}

// P1: same inputs always yield the same hash.
func TestComputeClusterConfigHash_Deterministic(t *testing.T) {
	cluster := newTestCluster()
	first := computeClusterConfigHash(cluster, "admin-1")
	for i := 0; i < 100; i++ {
		got := computeClusterConfigHash(cluster, "admin-1")
		if got != first {
			t.Fatalf("hash drifted on iteration %d: got %q, want %q", i, got, first)
		}
	}
}

// P2: changing any single input changes the hash.
func TestComputeClusterConfigHash_DiffersPerInput(t *testing.T) {
	base := newTestCluster()
	baseHash := computeClusterConfigHash(base, "admin-1")

	tests := []struct {
		name         string
		mutate       func() (*v1alpha1.IdentityServerCluster, string)
		shouldDiffer bool
	}{
		{
			name: "Version changes",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Version = "11.1"
				return c, "admin-1"
			},
			shouldDiffer: true,
		},
		{
			name: "Image changes",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Image = "private/idsvr:11.0"
				return c, "admin-1"
			},
			shouldDiffer: true,
		},
		{
			name: "ImagePullSecret changes",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.ImagePullSecret = "regcred"
				return c, "admin-1"
			},
			shouldDiffer: true,
		},
		{
			name: "adminNodeName changes",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				return base, "admin-2"
			},
			shouldDiffer: true,
		},
		{
			name: "Packages added (none -> one)",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Packages = []v1alpha1.PackageSpec{
					{Source: v1alpha1.PackageSource{URL: "https://example.test/plugin.zip"}, MountPath: "/opt/idsvr/plugins/p1"},
				}
				return c, "admin-1"
			},
			shouldDiffer: true,
		},
		{
			name: "Packages URL changes",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Packages = []v1alpha1.PackageSpec{
					{Source: v1alpha1.PackageSource{URL: "https://example.test/plugin.zip"}, MountPath: "/opt/idsvr/plugins/p1"},
				}
				return c, "admin-1"
			},
			shouldDiffer: true,
		},
		{
			name: "Packages reordered (order-sensitive)",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Packages = []v1alpha1.PackageSpec{
					{Source: v1alpha1.PackageSource{URL: "https://b.test/p.zip"}, MountPath: "/opt/idsvr/plugins/b"},
					{Source: v1alpha1.PackageSource{URL: "https://a.test/p.zip"}, MountPath: "/opt/idsvr/plugins/a"},
				}
				return c, "admin-1"
			},
			shouldDiffer: true,
		},
		{
			name: "Packages nil vs empty slice (both yield empty hash, no diff)",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Packages = []v1alpha1.PackageSpec{}
				return c, "admin-1"
			},
			shouldDiffer: false,
		},
		{
			name: "Tolerations change (not in hash by design)",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.Tolerations = []corev1.Toleration{{Key: "x"}}
				return c, "admin-1"
			},
			shouldDiffer: false,
		},
		{
			name: "NodeSelector changes (not in hash by design)",
			mutate: func() (*v1alpha1.IdentityServerCluster, string) {
				c := base.DeepCopy()
				c.Spec.NodeSelector = map[string]string{"zone": "us-east-1a"}
				return c, "admin-1"
			},
			shouldDiffer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, admin := tt.mutate()
			got := computeClusterConfigHash(c, admin)
			if (got != baseHash) != tt.shouldDiffer {
				t.Errorf("hash differs = %v, want %v (base=%q, got=%q)",
					got != baseHash, tt.shouldDiffer, baseHash, got)
			}
		})
	}
}

// P3: catches "developer added a new field and forgot to update the hash."
// New IdentityServerClusterSpec fields must be added to includedInHash (and
// to computeClusterConfigHash) or excludedFromHash with rationale.
func TestComputeClusterConfigHash_SpecFieldCoverage(t *testing.T) {
	includedInHash := map[string]bool{
		"Version":         true, // via buildImage(cluster)
		"Image":           true, // via buildImage(cluster)
		"ImagePullSecret": true,
		// Packages flows into the hash via computePackagesHash (conditional
		// append — nil/empty packages leave the hash unchanged). Each package
		// edit forces a genclust Job re-run so plugin-contributed config types
		// are loaded when cluster.xml is generated.
		"Packages": true,
	}
	excludedFromHash := map[string]bool{
		// AdminCredentials is excluded: handled by curity.io/encryption-key-hash
		// with empty-guards that tolerate the credentials-Secret cache miss on
		// first reconcile.
		"AdminCredentials":          true,
		"Logging":                   true,
		"PodAnnotations":            true,
		"PodLabels":                 true,
		"Resources":                 true,
		"Probes":                    true,
		"Autoscaling":               true,
		"PodDisruptionBudget":       true,
		"NodeSelector":              true,
		"Tolerations":               true,
		"TopologySpreadConstraints": true,
		"Affinity":                  true,
		// Pod-shape escape hatches + NetworkPolicy: none affect cluster.xml
		// generation, so they must not trigger a genclust Job re-run.
		"InitContainers":                true,
		"ExtraContainers":               true,
		"SecurityContext":               true,
		"ContainerSecurityContext":      true,
		"TerminationGracePeriodSeconds": true,
		"ImagePullPolicy":               true,
		"NetworkPolicy":                 true,
	}

	specType := reflect.TypeOf(v1alpha1.IdentityServerClusterSpec{})
	for i := 0; i < specType.NumField(); i++ {
		f := specType.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if !includedInHash[name] && !excludedFromHash[name] {
			t.Errorf("spec field %q must be in includedInHash or excludedFromHash", name)
		}
	}
}

// N4: hash is non-empty even when every input is empty.
func TestComputeClusterConfigHash_NonEmptyForEmptyInputs(t *testing.T) {
	c := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec:       v1alpha1.IdentityServerClusterSpec{},
	}
	got := computeClusterConfigHash(c, "")
	if got == "" {
		t.Fatal("hash must be non-empty even for empty inputs")
	}
	if len(got) != 64 { // sha256 hex = 64 chars
		t.Errorf("expected 64-char hex hash, got %d chars: %q", len(got), got)
	}
}

// E1: NUL separator prevents distinct field assignments from colliding.
func TestComputeClusterConfigHash_DistinctFieldsDistinctHashes(t *testing.T) {
	a := newTestCluster()
	a.Spec.Image = "ab"
	a.Spec.ImagePullSecret = "cd"

	b := newTestCluster()
	b.Spec.Image = "abcd"
	b.Spec.ImagePullSecret = ""

	if computeClusterConfigHash(a, "admin") == computeClusterConfigHash(b, "admin") {
		t.Error("distinct field assignments must produce distinct hashes")
	}
}

// E2: hash works when all optional fields are empty.
func TestComputeClusterConfigHash_AllOptionalFieldsEmpty(t *testing.T) {
	c := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
	}
	got := computeClusterConfigHash(c, "admin-1")
	if got == "" {
		t.Fatal("hash must be non-empty when only Version + adminNodeName are set")
	}
}

// E3: long combined names don't panic or silently truncate.
func TestComputeClusterConfigHash_LongCombinedNames(t *testing.T) {
	c := newTestCluster()
	c.Name = strings.Repeat("a", 200)
	got := computeClusterConfigHash(c, strings.Repeat("b", 200))
	if got == "" {
		t.Fatal("hash must be non-empty for long names")
	}
}

func TestEncryptionKeyHashForTest(t *testing.T) {
	tests := []struct {
		name     string
		key      []byte
		wantHash bool
	}{
		{"empty bytes returns empty string", nil, false},
		{"zero-length bytes returns empty string", []byte{}, false},
		{"single byte produces 64-char hex", []byte{0}, true},
		{"32-byte key produces 64-char hex", make([]byte, 32), true},
		{"long key produces 64-char hex", make([]byte, 1024), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EncryptionKeyHashForTest(tt.key)
			if tt.wantHash {
				if len(got) != 64 {
					t.Errorf("expected 64-char hex hash, got %d chars: %q", len(got), got)
				}
			} else {
				if got != "" {
					t.Errorf("expected empty string, got %q", got)
				}
			}
		})
	}

	key := []byte("smoke-test-key")
	first := EncryptionKeyHashForTest(key)
	for i := 0; i < 100; i++ {
		if got := EncryptionKeyHashForTest(key); got != first {
			t.Fatalf("hash drifted on iteration %d", i)
		}
	}

	if EncryptionKeyHashForTest([]byte("a")) == EncryptionKeyHashForTest([]byte("b")) {
		t.Error("different keys must produce different hashes")
	}
}

// dns1123LabelRegex is the K8s constraint applied to Service / Deployment /
// HPA / PDB names: lowercase alphanumeric plus hyphens, no leading or trailing
// hyphen, length 1-63.
var dns1123LabelRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestOwnedResourceName_Deterministic(t *testing.T) {
	first := OwnedResourceName("prod", "admin")
	for i := 0; i < 100; i++ {
		if got := OwnedResourceName("prod", "admin"); got != first {
			t.Fatalf("ownedResourceName drifted on iteration %d: %q != %q", i, got, first)
		}
	}
}

func TestOwnedResourceName_AmbiguousPair_NoCollision(t *testing.T) {
	a := OwnedResourceName("foo-bar", "baz")
	b := OwnedResourceName("foo", "bar-baz")
	if a == b {
		t.Errorf("ambiguous pairs must produce distinct names; got %q == %q", a, b)
	}
}

func TestOwnedResourceName_FormatValid(t *testing.T) {
	cases := []struct {
		cluster, node string
	}{
		{"prod", "admin"},
		{"prod", "runtime"},
		{"foo-bar", "baz"},
		{"foo", "bar-baz"},
		{"acme-prod", "admin"},
		{"stage-eu", "runtime-1"},
		{"a", "b"},
		{"123", "456"},
		{"long-cluster-name-with-many-hyphens", "node"},
		{"c", "long-node-name-with-many-hyphens-too"},
	}
	for _, tc := range cases {
		got := OwnedResourceName(tc.cluster, tc.node)
		if !dns1123LabelRegex.MatchString(got) {
			t.Errorf("OwnedResourceName(%q, %q) = %q does not match DNS-1123 label regex", tc.cluster, tc.node, got)
		}
		if len(got) > 63 {
			t.Errorf("OwnedResourceName(%q, %q) = %q exceeds 63 chars (got %d)", tc.cluster, tc.node, got, len(got))
		}
	}
}

func TestOwnedResourceName_LengthCap47(t *testing.T) {
	// Inputs whose combined length exceeds the cap should produce a truncated
	// output ending with the 9-char hash suffix.
	long := strings.Repeat("a", 30) // very long cluster name
	got := OwnedResourceName(long, "node")
	if len(got) > ownedResourceNameMaxLen {
		t.Errorf("output exceeds %d-char cap: got %q (len %d)", ownedResourceNameMaxLen, got, len(got))
	}
	// Hash suffix is the trailing 9 chars (`-<8hex>`); must always be present.
	if len(got) < 9 {
		t.Fatalf("output too short to contain hash suffix: %q", got)
	}
	suffix := got[len(got)-9:]
	if suffix[0] != '-' {
		t.Errorf("expected suffix to start with '-', got %q", suffix)
	}
}

func TestOwnedResourceName_HashOverFullInputs(t *testing.T) {
	// Two pairs whose 47-char-truncated prefix would coincide must still
	// produce different hashes (and therefore different full names) because
	// the hash is computed over the FULL inputs, not the truncated prefix.
	a := OwnedResourceName("very-long-cluster-name-aaaaa", "differing-suffix-one")
	b := OwnedResourceName("very-long-cluster-name-aaaaa", "differing-suffix-two")
	if a == b {
		t.Errorf("hash-over-full-inputs must differentiate: got %q == %q", a, b)
	}
}

func TestOwnedResourceName_HashStable(t *testing.T) {
	// Locks the algorithm + encoding choice. If this fails, someone changed
	// the hashing recipe and existing deployments would see resource renames.
	want := "prod-admin-65e81f35"
	if got := OwnedResourceName("prod", "admin"); got != want {
		t.Errorf("OwnedResourceName(\"prod\", \"admin\") = %q, want %q", got, want)
	}
}

func TestOwnedResourceName_NullByteSeparator(t *testing.T) {
	// The null byte inside the hash input ensures ("a","bc") and ("ab","c")
	// hash differently — defensive against future format changes that might
	// otherwise let two distinct pairs share a hash input.
	a := OwnedResourceName("a", "bc")
	b := OwnedResourceName("ab", "c")
	// Visible names also differ (different prefixes), but the hash portion
	// must independently differ to prove the null-byte separator works.
	hashA := a[len(a)-8:]
	hashB := b[len(b)-8:]
	if hashA == hashB {
		t.Errorf("hashes for ambiguous pairs must differ: %q vs %q", hashA, hashB)
	}
	// And the actual SHA digests should reflect the null-byte input.
	rawA := sha256.Sum256([]byte("a\x00bc"))
	rawB := sha256.Sum256([]byte("ab\x00c"))
	if hex.EncodeToString(rawA[:4]) != hashA || hex.EncodeToString(rawB[:4]) != hashB {
		t.Errorf("hash inputs do not include null-byte separator")
	}
}

func TestOwnedResourceName_NoTrailingDash(t *testing.T) {
	// Truncation must never leave a trailing '-' before the hash suffix.
	// Construct an input whose truncated prefix would naturally end in '-':
	// 30 chars of "a" + "-" + many "x" → after slicing to 38 chars, the
	// boundary may land mid-separator.
	cases := [][2]string{
		{strings.Repeat("a", 30), strings.Repeat("x", 30)},
		{strings.Repeat("a", 38), "y"},
		{strings.Repeat("a", 37) + "-", "z"},
	}
	for _, tc := range cases {
		got := OwnedResourceName(tc[0], tc[1])
		if !dns1123LabelRegex.MatchString(got) {
			t.Errorf("invalid DNS-1123 label for (%q, %q): %q", tc[0], tc[1], got)
		}
		// The hash suffix is the last 9 chars; the char immediately before it
		// must not be '-' (that would mean we have "--<hash>" mid-name).
		if len(got) >= 10 && got[len(got)-10] == '-' {
			t.Errorf("found double dash before hash suffix in (%q, %q) = %q", tc[0], tc[1], got)
		}
	}
}

// TestMergeDefaultTolerations exercises the two NoExecute defaults injected
// by mergeDefaultTolerations against the cases the kube apiserver's
// DefaultTolerationSeconds admission controller cares about. The matching
// rule must agree with the apiserver's, otherwise we duplicate-write or
// strip-and-rewrite on every reconcile and reintroduce the drift loop.
func TestMergeDefaultTolerations(t *testing.T) {
	notReady := corev1.TaintNodeNotReady
	unreachable := corev1.TaintNodeUnreachable
	noExec := corev1.TaintEffectNoExecute

	hasToleration := func(out []corev1.Toleration, key string, effect corev1.TaintEffect) bool {
		for _, tol := range out {
			if tol.Key == key && tol.Effect == effect {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name           string
		in             []corev1.Toleration
		wantHasDefault map[string]bool // key -> should a default for this key be in the output
		wantUserKeysIn []string        // user keys that must still be present
		wantLen        int
	}{
		{
			name:           "nil_input_appends_both_defaults",
			in:             nil,
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantLen:        2,
		},
		{
			name:           "empty_slice_appends_both_defaults",
			in:             []corev1.Toleration{},
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantLen:        2,
		},
		{
			name: "user_has_not_ready_already_partial_overlap",
			in: []corev1.Toleration{
				{Key: notReady, Operator: corev1.TolerationOpExists, Effect: noExec, TolerationSeconds: ptr.To(int64(600))},
			},
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantUserKeysIn: []string{notReady},
			wantLen:        2, // user's not-ready preserved; default unreachable appended
		},
		{
			name: "user_has_unreachable_already_partial_overlap",
			in: []corev1.Toleration{
				{Key: unreachable, Operator: corev1.TolerationOpExists, Effect: noExec, TolerationSeconds: ptr.To(int64(900))},
			},
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantUserKeysIn: []string{unreachable},
			wantLen:        2,
		},
		{
			name: "user_has_both_no_defaults_appended",
			in: []corev1.Toleration{
				{Key: notReady, Operator: corev1.TolerationOpExists, Effect: noExec, TolerationSeconds: ptr.To(int64(60))},
				{Key: unreachable, Operator: corev1.TolerationOpExists, Effect: noExec, TolerationSeconds: ptr.To(int64(60))},
			},
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantLen:        2, // both preserved; nothing appended
		},
		{
			name: "operator_exists_with_no_key_tolerates_everything",
			in: []corev1.Toleration{
				// Empty Key + Operator=Exists matches all taints regardless of effect.
				{Operator: corev1.TolerationOpExists},
			},
			wantLen: 1, // user's universal toleration covers both; nothing appended
		},
		{
			name: "operator_exists_no_key_with_noexecute_effect",
			in: []corev1.Toleration{
				// Empty Key + Operator=Exists + Effect=NoExecute matches all NoExecute taints.
				{Operator: corev1.TolerationOpExists, Effect: noExec},
			},
			wantLen: 1,
		},
		{
			name: "mismatched_effect_does_not_cover_default",
			in: []corev1.Toleration{
				// User tolerates not-ready as NoSchedule, not NoExecute — our default
				// is NoExecute and is NOT tolerated by this entry.
				{Key: notReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantUserKeysIn: []string{notReady},
			wantLen:        3, // user's NoSchedule entry + 2 NoExecute defaults
		},
		{
			name: "unrelated_user_toleration_preserved",
			in: []corev1.Toleration{
				{Key: "gpu", Operator: corev1.TolerationOpEqual, Value: "true"},
			},
			wantHasDefault: map[string]bool{notReady: true, unreachable: true},
			wantUserKeysIn: []string{"gpu"},
			wantLen:        3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeDefaultTolerations(tc.in)
			if len(got) != tc.wantLen {
				t.Errorf("len=%d, want %d, got=%+v", len(got), tc.wantLen, got)
			}
			for key, wantPresent := range tc.wantHasDefault {
				if hasToleration(got, key, noExec) != wantPresent {
					t.Errorf("expected default for key=%q effect=NoExecute present=%v, got %+v", key, wantPresent, got)
				}
			}
			for _, key := range tc.wantUserKeysIn {
				found := false
				for _, tol := range got {
					if tol.Key == key {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected user-supplied key %q to survive merge, got %+v", key, got)
				}
			}
		})
	}

	// Idempotency: running mergeDefaultTolerations on its own output produces
	// the same list (no growth, no reordering). Critical: the drift loop
	// returns the moment this property breaks.
	t.Run("idempotent_on_own_output", func(t *testing.T) {
		in := []corev1.Toleration{
			{Key: "gpu", Operator: corev1.TolerationOpEqual, Value: "true"},
		}
		once := mergeDefaultTolerations(in)
		twice := mergeDefaultTolerations(once)
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("not idempotent:\n  once=%+v\n twice=%+v", once, twice)
		}
	})

	// DefaultTolerationSeconds package-level variable is honored on each
	// appended toleration. cmd/manager/main.go sets this from the
	// DEFAULT_TOLERATION_SECONDS env var; the operator must emit whatever
	// the cluster's apiserver flags are tuned to or drift will return.
	t.Run("respects_DefaultTolerationSeconds_package_var", func(t *testing.T) {
		orig := DefaultTolerationSeconds
		defer func() { DefaultTolerationSeconds = orig }()
		DefaultTolerationSeconds = int64(900)
		got := mergeDefaultTolerations(nil)
		for _, tol := range got {
			if tol.TolerationSeconds == nil || *tol.TolerationSeconds != 900 {
				t.Errorf("default toleration seconds not honored: got %+v", tol.TolerationSeconds)
			}
		}
	})
}

// --- Pod-customization escape hatches + NetworkPolicy ---

func TestResolveImagePullPolicy(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	if got := resolveImagePullPolicy(cluster, node); got != corev1.PullIfNotPresent {
		t.Errorf("default: want IfNotPresent, got %q", got)
	}
	cluster.Spec.ImagePullPolicy = corev1.PullNever
	if got := resolveImagePullPolicy(cluster, node); got != corev1.PullNever {
		t.Errorf("cluster value: want Never, got %q", got)
	}
	node.Spec.ImagePullPolicy = corev1.PullAlways
	if got := resolveImagePullPolicy(cluster, node); got != corev1.PullAlways {
		t.Errorf("node overrides cluster: want Always, got %q", got)
	}

	// Unset policy is tag-aware on the resolved image (matches the apiserver
	// and the user-container default), not a flat IfNotPresent.
	latest := newTestCluster()
	latest.Spec.Image = "myreg/idsvr:latest"
	if got := resolveImagePullPolicy(latest, newTestNode(v1alpha1.NodeTypeRuntime)); got != corev1.PullAlways {
		t.Errorf(":latest image, unset policy: want Always, got %q", got)
	}
	pinned := newTestCluster()
	pinned.Spec.Image = "myreg/idsvr:1.2.3"
	if got := resolveImagePullPolicy(pinned, newTestNode(v1alpha1.NodeTypeRuntime)); got != corev1.PullIfNotPresent {
		t.Errorf("pinned image, unset policy: want IfNotPresent, got %q", got)
	}
}

func TestResolveTerminationGracePeriodSeconds(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)

	if got := resolveTerminationGracePeriodSeconds(cluster, node); got == nil || *got != 30 {
		t.Errorf("default: want 30, got %v", got)
	}
	cluster.Spec.TerminationGracePeriodSeconds = ptr.To(int64(90))
	if got := resolveTerminationGracePeriodSeconds(cluster, node); *got != 90 {
		t.Errorf("cluster value: want 90, got %d", *got)
	}
	node.Spec.TerminationGracePeriodSeconds = ptr.To(int64(120))
	if got := resolveTerminationGracePeriodSeconds(cluster, node); *got != 120 {
		t.Errorf("node overrides cluster: want 120, got %d", *got)
	}
}

func TestResolveContainers_NodeOverridesCluster(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	cluster.Spec.InitContainers = []corev1.Container{{Name: "init-cluster"}}
	cluster.Spec.ExtraContainers = []corev1.Container{{Name: "sc-cluster"}}

	if got := resolveInitContainers(cluster, node); len(got) != 1 || got[0].Name != "init-cluster" {
		t.Errorf("init unset: want cluster value, got %v", got)
	}
	if got := resolveExtraContainers(cluster, node); len(got) != 1 || got[0].Name != "sc-cluster" {
		t.Errorf("extra unset: want cluster value, got %v", got)
	}
	node.Spec.InitContainers = []corev1.Container{{Name: "init-node"}}
	node.Spec.ExtraContainers = []corev1.Container{{Name: "sc-node"}}
	if got := resolveInitContainers(cluster, node); len(got) != 1 || got[0].Name != "init-node" {
		t.Errorf("init set: want node value, got %v", got)
	}
	if got := resolveExtraContainers(cluster, node); len(got) != 1 || got[0].Name != "sc-node" {
		t.Errorf("extra set: want node value, got %v", got)
	}
}

func TestMergePodSecurityContext(t *testing.T) {
	base := mergePodSecurityContext(nil)
	if *base.RunAsUser != 10001 || *base.RunAsGroup != 10000 || *base.FSGroup != 10000 {
		t.Fatalf("nil user: want 10001/10000/10000, got %+v", base)
	}

	policy := corev1.FSGroupChangeOnRootMismatch
	merged := mergePodSecurityContext(&corev1.PodSecurityContext{FSGroupChangePolicy: &policy})
	if *merged.RunAsUser != 10001 || *merged.RunAsGroup != 10000 || *merged.FSGroup != 10000 {
		t.Errorf("partial override dropped base UID/GID: %+v", merged)
	}
	if merged.FSGroupChangePolicy == nil || *merged.FSGroupChangePolicy != policy {
		t.Errorf("partial override dropped the user field")
	}

	over := mergePodSecurityContext(&corev1.PodSecurityContext{RunAsUser: ptr.To(int64(2000))})
	if *over.RunAsUser != 2000 {
		t.Errorf("explicit override: want runAsUser 2000, got %d", *over.RunAsUser)
	}
	if *over.RunAsGroup != 10000 || *over.FSGroup != 10000 {
		t.Errorf("explicit override dropped other base fields: %+v", over)
	}
}

func TestApplyContainerDefaults(t *testing.T) {
	c := &corev1.Container{
		Name:  "bare",
		Image: "busybox:1.36",
		Ports: []corev1.ContainerPort{{ContainerPort: 9000}},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)},
		}},
		Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/drain", Port: intstr.FromInt32(8080)},
		}},
	}
	applyContainerDefaults(c)

	if c.TerminationMessagePath != corev1.TerminationMessagePathDefault {
		t.Errorf("terminationMessagePath not defaulted")
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("terminationMessagePolicy not defaulted")
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("tagged image: want IfNotPresent, got %q", c.ImagePullPolicy)
	}
	if c.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("port protocol not defaulted to TCP")
	}
	if c.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("probe httpGet scheme not defaulted to HTTP")
	}
	// The apiserver fills these probe fields, so the builder must too or it drifts.
	p := c.LivenessProbe
	if p.TimeoutSeconds != 1 || p.PeriodSeconds != 10 || p.SuccessThreshold != 1 || p.FailureThreshold != 3 {
		t.Errorf("probe threshold/period not defaulted: timeout=%d period=%d success=%d failure=%d",
			p.TimeoutSeconds, p.PeriodSeconds, p.SuccessThreshold, p.FailureThreshold)
	}
	if c.Lifecycle.PreStop.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("lifecycle preStop httpGet scheme not defaulted to HTTP")
	}
}

func TestApplyContainerDefaultsAll_DoesNotMutateInput(t *testing.T) {
	in := []corev1.Container{{Name: "x", Image: "busybox:1.36"}}
	_ = ApplyContainerDefaultsAll(in)
	if in[0].TerminationMessagePath != "" || in[0].ImagePullPolicy != "" {
		t.Errorf("ApplyContainerDefaultsAll mutated the input (must deep-copy the CR spec)")
	}
}

func TestDefaultPullPolicy(t *testing.T) {
	cases := map[string]corev1.PullPolicy{
		"busybox:1.36":            corev1.PullIfNotPresent,
		"redis:latest":            corev1.PullAlways,
		"alpine":                  corev1.PullAlways,
		"registry.io:5000/app:v1": corev1.PullIfNotPresent,
		"registry.io:5000/app":    corev1.PullAlways,
		"repo@sha256:abc123":      corev1.PullAlways,
	}
	for img, want := range cases {
		if got := defaultPullPolicy(img); got != want {
			t.Errorf("defaultPullPolicy(%q): want %q, got %q", img, want, got)
		}
	}
}

func TestBuildDeployment_UserExtraContainersDefaultedAndAppended(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.ExtraContainers = []corev1.Container{{Name: "sidecar", Image: "fluent/fluent-bit:3.0"}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	containers := deploy.Spec.Template.Spec.Containers

	if containers[0].Name != containerName {
		t.Fatalf("main container displaced from index 0: %q", containers[0].Name)
	}
	last := containers[len(containers)-1]
	if last.Name != "sidecar" {
		t.Errorf("extra container not appended last: %q", last.Name)
	}
	if last.TerminationMessagePath != corev1.TerminationMessagePathDefault {
		t.Errorf("extra container not defaulted (terminationMessagePath)")
	}
	if last.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("extra container pull policy: want IfNotPresent for tagged image, got %q", last.ImagePullPolicy)
	}
}

func TestBuildDeployment_UserInitContainersDefaulted(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.InitContainers = []corev1.Container{{Name: "wait-for-db", Image: "busybox:1.36"}}

	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	inits := deploy.Spec.Template.Spec.InitContainers
	if len(inits) == 0 || inits[len(inits)-1].Name != "wait-for-db" {
		t.Fatalf("user init container not present/last: %+v", inits)
	}
	if inits[len(inits)-1].TerminationMessagePath != corev1.TerminationMessagePathDefault {
		t.Errorf("user init container not defaulted")
	}
}

func TestBuildDeployment_ContainerSecurityContextAndPullPolicy(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.ContainerSecurityContext = &corev1.SecurityContext{RunAsNonRoot: ptr.To(true)}
	node.Spec.ImagePullPolicy = corev1.PullAlways

	main := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.Containers[0]
	if main.SecurityContext == nil || main.SecurityContext.RunAsNonRoot == nil || !*main.SecurityContext.RunAsNonRoot {
		t.Errorf("container securityContext not applied to main container")
	}
	if main.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("imagePullPolicy override not applied: %q", main.ImagePullPolicy)
	}
}

func TestBuildDeployment_PodSecurityContextMergePreservesBase(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	policy := corev1.FSGroupChangeOnRootMismatch
	node.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroupChangePolicy: &policy}

	sc := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.SecurityContext
	if *sc.RunAsUser != 10001 || *sc.FSGroup != 10000 {
		t.Errorf("merge dropped required base UID/GID: %+v", sc)
	}
	if sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != policy {
		t.Errorf("user securityContext field not merged")
	}
}

func TestBuildDeployment_TerminationGracePeriodOverride(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.TerminationGracePeriodSeconds = ptr.To(int64(120))

	got := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.TerminationGracePeriodSeconds
	if got == nil || *got != 120 {
		t.Errorf("terminationGracePeriodSeconds override not applied: %v", got)
	}
}

func TestBuildDeployment_ClusterLevelEscapeHatchesApply(t *testing.T) {
	// Escape hatches set on the cluster must apply to a node that overrides none
	// of them — the other tests only exercise the node-level path.
	cluster := newTestCluster()
	cluster.Spec.InitContainers = []corev1.Container{{Name: "cluster-init", Image: "busybox:1.36"}}
	cluster.Spec.ExtraContainers = []corev1.Container{{Name: "cluster-sidecar", Image: "busybox:1.36"}}
	cluster.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000))}
	cluster.Spec.ContainerSecurityContext = &corev1.SecurityContext{RunAsNonRoot: ptr.To(true)}
	cluster.Spec.TerminationGracePeriodSeconds = ptr.To(int64(90))
	cluster.Spec.ImagePullPolicy = corev1.PullAlways

	node := newTestNode(v1alpha1.NodeTypeRuntime) // overrides none of the above

	spec := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec
	assertContains(t, containerNames(spec.InitContainers), "cluster-init")
	assertContains(t, containerNames(spec.Containers), "cluster-sidecar")
	if spec.SecurityContext == nil || spec.SecurityContext.FSGroup == nil || *spec.SecurityContext.FSGroup != 2000 {
		t.Errorf("cluster securityContext FSGroup not applied: %+v", spec.SecurityContext)
	}
	if sc := spec.Containers[0].SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("cluster containerSecurityContext not applied to main container")
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 90 {
		t.Errorf("cluster terminationGracePeriodSeconds not applied: %v", spec.TerminationGracePeriodSeconds)
	}
	if spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("cluster imagePullPolicy not applied: %q", spec.Containers[0].ImagePullPolicy)
	}
}

func TestBuildDeployment_NodeOverridesClusterEscapeHatches(t *testing.T) {
	// When both levels set a field, the node wins entirely (no field-level merge
	// across levels — the documented "node overrides cluster when set" contract).
	cluster := newTestCluster()
	cluster.Spec.InitContainers = []corev1.Container{{Name: "cluster-init", Image: "busybox:1.36"}}
	cluster.Spec.TerminationGracePeriodSeconds = ptr.To(int64(90))
	cluster.Spec.ImagePullPolicy = corev1.PullNever

	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.InitContainers = []corev1.Container{{Name: "node-init", Image: "busybox:1.36"}}
	node.Spec.TerminationGracePeriodSeconds = ptr.To(int64(45))
	node.Spec.ImagePullPolicy = corev1.PullAlways

	spec := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec
	names := containerNames(spec.InitContainers)
	assertContains(t, names, "node-init")
	assertNotContains(t, names, "cluster-init")
	if *spec.TerminationGracePeriodSeconds != 45 {
		t.Errorf("node TGPS should win: got %d", *spec.TerminationGracePeriodSeconds)
	}
	if spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("node imagePullPolicy should win: got %q", spec.Containers[0].ImagePullPolicy)
	}
}

func TestResolveContainers_NodeEmptyListClearsClusterValue(t *testing.T) {
	// nil node list inherits the cluster's; an explicit [] clears it. The
	// apiserver preserves the empty array, so the two are distinguishable.
	cluster := newTestCluster()
	cluster.Spec.InitContainers = []corev1.Container{{Name: "cluster-init", Image: "busybox:1.36"}}
	cluster.Spec.ExtraContainers = []corev1.Container{{Name: "cluster-sidecar", Image: "busybox:1.36"}}

	inherit := newTestNode(v1alpha1.NodeTypeRuntime) // both lists nil
	if got := resolveInitContainers(cluster, inherit); len(got) != 1 || got[0].Name != "cluster-init" {
		t.Errorf("nil node initContainers must inherit cluster's: %v", containerNames(got))
	}

	clear := newTestNode(v1alpha1.NodeTypeRuntime)
	clear.Spec.InitContainers = []corev1.Container{}
	clear.Spec.ExtraContainers = []corev1.Container{}
	if got := resolveInitContainers(cluster, clear); len(got) != 0 {
		t.Errorf("explicit [] node initContainers must clear cluster's, got %v", containerNames(got))
	}
	if got := resolveExtraContainers(cluster, clear); len(got) != 0 {
		t.Errorf("explicit [] node extraContainers must clear cluster's, got %v", containerNames(got))
	}
}

func TestResolveSchedulingSlices_NodeEmptyListClearsClusterValue(t *testing.T) {
	// tolerations + topologySpreadConstraints use the same nil-inherits / []-clears
	// contract as init/extra containers.
	cluster := newTestCluster()
	cluster.Spec.Tolerations = []corev1.Toleration{{Key: "k"}}
	cluster.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{TopologyKey: "zone"}}

	inherit := newTestNode(v1alpha1.NodeTypeRuntime)
	if len(resolveTolerations(cluster, inherit)) != 1 || len(resolveTopologySpreadConstraints(cluster, inherit)) != 1 {
		t.Errorf("nil node lists must inherit cluster's")
	}

	clear := newTestNode(v1alpha1.NodeTypeRuntime)
	clear.Spec.Tolerations = []corev1.Toleration{}
	clear.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{}
	if got := resolveTolerations(cluster, clear); len(got) != 0 {
		t.Errorf("explicit [] node tolerations must clear cluster's, got %v", got)
	}
	if got := resolveTopologySpreadConstraints(cluster, clear); len(got) != 0 {
		t.Errorf("explicit [] node topologySpreadConstraints must clear cluster's, got %v", got)
	}
}

func TestDetectContainerNameConflict(t *testing.T) {
	cluster := newTestCluster()

	// User extraContainer named after a log stream collides with the log sidecar.
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.Logging = &v1alpha1.LoggingSpec{Level: "INFO", Logs: []string{"request"}}
	node.Spec.ExtraContainers = []corev1.Container{{Name: "request", Image: "busybox:1.36"}}
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	msg, conflict := detectContainerNameConflict(deploy, cluster, node)
	if !conflict {
		t.Fatal("expected a container-name conflict")
	}
	if !strings.Contains(msg, `"request"`) || !strings.Contains(msg, "log sidecar") {
		t.Errorf("message should name the request log sidecar source: %q", msg)
	}

	// Distinct names: no conflict.
	clean := newTestNode(v1alpha1.NodeTypeRuntime)
	clean.Spec.ExtraContainers = []corev1.Container{{Name: "audit-shipper", Image: "busybox:1.36"}}
	if _, c := detectContainerNameConflict(buildDeployment(cluster, clean, nil, DefaultPackageFetcherImage), cluster, clean); c {
		t.Errorf("distinct container names must not conflict")
	}
}

func TestBuildDeployment_InitContainerOrdering(t *testing.T) {
	// Package fetchers must run BEFORE user init containers — the operator's
	// download-and-unpack has to finish before any user-supplied setup runs.
	cluster := newTestCluster()
	cluster.Spec.Packages = []v1alpha1.PackageSpec{
		{Source: v1alpha1.PackageSource{URL: "https://example.test/p.zip"}, MountPath: "/opt/idsvr/plugins/p1"},
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	node.Spec.InitContainers = []corev1.Container{{Name: "user-init", Image: "busybox:1.36"}}

	inits := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage).Spec.Template.Spec.InitContainers
	if len(inits) != 2 {
		t.Fatalf("want 2 init containers (1 fetcher + 1 user), got %d: %v", len(inits), containerNames(inits))
	}
	if inits[len(inits)-1].Name != "user-init" {
		t.Errorf("user init must be last (after fetchers); order: %v", containerNames(inits))
	}
	if !strings.HasPrefix(inits[0].Name, packageFetchContainerNamePrefix) {
		t.Errorf("first init container must be a package fetcher, got %q", inits[0].Name)
	}
}

func TestBuildNetworkPolicy_Structure(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.NetworkPolicy = &v1alpha1.NetworkPolicySpec{}
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	np := buildNetworkPolicy(cluster, node)

	if np.Spec.PodSelector.MatchLabels["curity.io/owned-by"] != OwnedResourceName(cluster.Name, node.Name) {
		t.Errorf("podSelector is not the private owned-by key: %v", np.Spec.PodSelector.MatchLabels)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes not [Ingress] (drift): %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("UI off: want 2 ingress rules (runtime + genclust), got %d", len(np.Spec.Ingress))
	}
	from := np.Spec.Ingress[0].From[0].PodSelector.MatchLabels
	if from["curity.io/cluster"] != cluster.Name || from["app.kubernetes.io/component"] != string(v1alpha1.NodeTypeRuntime) {
		t.Errorf("ingress from-selector wrong: %v", from)
	}
	ports := np.Spec.Ingress[0].Ports
	if len(ports) != 2 {
		t.Fatalf("want 2 ports (config+ds), got %d", len(ports))
	}
	for _, p := range ports {
		if p.Protocol == nil || *p.Protocol != corev1.ProtocolTCP {
			t.Errorf("port protocol not explicit TCP (drift): %+v", p)
		}
	}
	// genclust rule: the config Job must be an allowed peer on the config port.
	gc := np.Spec.Ingress[1].From[0].PodSelector.MatchLabels
	if gc["curity.io/cluster"] != cluster.Name || gc["curity.io/component"] != "cluster-config" {
		t.Errorf("genclust from-selector wrong: %v", gc)
	}
	if gcPorts := np.Spec.Ingress[1].Ports; len(gcPorts) != 1 || gcPorts[0].Port.IntValue() != portConfig {
		t.Errorf("genclust rule must allow only the config port, got %+v", gcPorts)
	}
}

func TestBuildNetworkPolicy_UIRuleConditional(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeAdmin)

	// Base rules (runtime + genclust) are always present; the UI rule is the +1.
	cluster.Spec.NetworkPolicy = &v1alpha1.NetworkPolicySpec{APIGatewayNamespace: "edge"}
	node.Spec.UI = &v1alpha1.UISpec{Enabled: true}
	if np := buildNetworkPolicy(cluster, node); len(np.Spec.Ingress) != 3 {
		t.Errorf("UI on + gateway ns: want 3 ingress rules (runtime + genclust + UI), got %d", len(np.Spec.Ingress))
	}

	cluster.Spec.NetworkPolicy = &v1alpha1.NetworkPolicySpec{}
	if np := buildNetworkPolicy(cluster, node); len(np.Spec.Ingress) != 2 {
		t.Errorf("UI on, no gateway ns: want 2 ingress rules (runtime + genclust), got %d", len(np.Spec.Ingress))
	}

	cluster.Spec.NetworkPolicy = &v1alpha1.NetworkPolicySpec{APIGatewayNamespace: "edge"}
	node.Spec.UI = &v1alpha1.UISpec{Enabled: false}
	if np := buildNetworkPolicy(cluster, node); len(np.Spec.Ingress) != 2 {
		t.Errorf("UI off: want 2 ingress rules (runtime + genclust), got %d", len(np.Spec.Ingress))
	}
}

func TestBuildNetworkPolicy_SelectorsMatchRealPodLabels(t *testing.T) {
	// Cross-check the NP selectors against the labels pods actually carry: if
	// buildNetworkPolicy and buildDeployment diverge, a real CNI silently blocks
	// runtime→admin traffic (invisible in Kind/envtest, which don't enforce).
	cluster := newTestCluster()
	cluster.Spec.NetworkPolicy = &v1alpha1.NetworkPolicySpec{}
	admin := newTestNode(v1alpha1.NodeTypeAdmin)

	np := buildNetworkPolicy(cluster, admin)

	runtime := newTestNode(v1alpha1.NodeTypeRuntime)
	runtime.Name = "runtime-1"
	runtimePodLabels := buildDeployment(cluster, runtime, nil, DefaultPackageFetcherImage).Spec.Template.Labels
	for k, v := range np.Spec.Ingress[0].From[0].PodSelector.MatchLabels {
		if got := runtimePodLabels[k]; got != v {
			t.Errorf("ingress selector %s=%q unmatched by runtime pod label (got %q) — CNI would block runtime→admin", k, v, got)
		}
	}

	adminPodLabels := buildDeployment(cluster, admin, nil, DefaultPackageFetcherImage).Spec.Template.Labels
	for k, v := range np.Spec.PodSelector.MatchLabels {
		if got := adminPodLabels[k]; got != v {
			t.Errorf("NP podSelector %s=%q does not match admin pod label (got %q) — policy applies to no pod", k, v, got)
		}
	}
}
