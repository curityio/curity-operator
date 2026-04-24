package controller

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

const (
	defaultImage = "curity.azurecr.io/curity/idsvr"

	portConfig             = 6789
	portDistributedService = 6790
	portHTTP               = 8443
	portHealthCheck        = 4465
	portMetrics            = 4466
	portAdminUI            = 6749

	containerName = "curity"

	defaultProbeInitialDelay = int32(30)
	defaultProbePeriod       = int32(10)
	defaultProbeTimeout      = int32(1)
	defaultProbeFailure      = int32(3)
	defaultProbeSuccess      = int32(3)
)

// ownedResourceName returns the child-resource name for Deployments, Services,
// HPAs, and PDBs owned by a node. Prefixing with the cluster name prevents
// collisions when two IdentityServerNode CRs in the same namespace share a
// node name but reference different clusters.
func ownedResourceName(clusterName, nodeName string) string {
	return clusterName + "-" + nodeName
}

// buildDeployment constructs the desired Deployment for an IdentityServerNode.
// configs contains the validated discovered ConfigMaps/Secrets to mount (may be nil).
func buildDeployment(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode, configs []DiscoveredConfigResource) *appsv1.Deployment {
	labels := buildLabels(cluster, node)
	podAnnotations := mergeMaps(cluster.Spec.PodAnnotations, node.Spec.PodAnnotations)
	podLabels := mergeMaps(labels, mergeMaps(cluster.Spec.PodLabels, node.Spec.PodLabels))

	replicas := resolveReplicas(cluster, node)
	resources := resolveResources(cluster, node)
	probes := resolveProbes(cluster, node)

	container := corev1.Container{
		Name:            containerName,
		Image:           buildImage(cluster),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            buildContainerArgs(node),
		Ports:           buildContainerPorts(node),
		Env:             buildEnvVars(cluster, node),
		LivenessProbe:   buildLivenessProbe(probes),
		ReadinessProbe:  buildReadinessProbe(probes),
		// TODO: Add container-level security context in Phase 2
		// SecurityContext: &corev1.SecurityContext{
		// 	RunAsNonRoot:             ptr.To(true),
		// 	AllowPrivilegeEscalation: ptr.To(false),
		// 	ReadOnlyRootFilesystem:   ptr.To(false),
		// 	Capabilities: &corev1.Capabilities{
		// 		Drop: []corev1.Capability{"ALL"},
		// 	},
		// },
	}

	if resources != nil {
		container.Resources = *resources
	}

	// Add volume mounts for configuration
	volumes, mounts := buildVolumes(cluster.Name, configs)
	container.VolumeMounts = mounts

	// Resolve logging config (node overrides cluster entirely)
	logging := resolveLogging(cluster, node)
	effectiveLevel := resolveLoggingLevel(cluster, node)

	// Add log volume mount to main container if stdout logging enabled and level is not OFF
	if logging != nil && logging.Stdout && effectiveLevel != "OFF" {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "log-volume",
			MountPath: "/opt/idsvr/var/log/",
		})
		volumes = append(volumes, corev1.Volume{
			Name: "log-volume",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	// Build sidecar log tailing containers (skip when effective level is OFF)
	var sidecars []corev1.Container
	if effectiveLevel != "OFF" {
		sidecars = buildLogSidecars(logging)
	}

	containers := []corev1.Container{container}
	containers = append(containers, sidecars...)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ownedResourceName(cluster.Name, node.Name),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: buildSelectorLabels(cluster, node),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  ptr.To(int64(10001)),
						RunAsGroup: ptr.To(int64(10000)),
						FSGroup:    ptr.To(int64(10000)),
					},
					Containers:                containers,
					Volumes:                   volumes,
					NodeSelector:              resolveNodeSelector(cluster, node),
					Tolerations:               resolveTolerations(cluster, node),
					Affinity:                  resolveAffinity(cluster, node),
					TopologySpreadConstraints: resolveTopologySpreadConstraints(cluster, node),
				},
			},
		},
	}

	if cluster.Spec.ImagePullSecret != "" {
		deploy.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: cluster.Spec.ImagePullSecret},
		}
	}

	return deploy
}

// buildService constructs the desired Service for an IdentityServerNode.
func buildService(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ownedResourceName(cluster.Name, node.Name),
			Namespace: node.Namespace,
			Labels:    buildLabels(cluster, node),
		},
		Spec: corev1.ServiceSpec{
			Type:     resolveServiceType(node),
			Selector: buildSelectorLabels(cluster, node),
			Ports:    buildServicePorts(node),
		},
	}
}

// buildImage returns the container image string.
func buildImage(cluster *v1alpha1.IdentityServerCluster) string {
	if cluster.Spec.Image != "" {
		return cluster.Spec.Image
	}
	return fmt.Sprintf("%s:%s", defaultImage, cluster.Spec.Version)
}

// buildContainerArgs returns the command arguments for the Curity container.
func buildContainerArgs(node *v1alpha1.IdentityServerNode) []string {
	if node.Spec.Type == v1alpha1.NodeTypeAdmin {
		return []string{
			"/opt/idsvr/bin/idsvr",
			"-s", node.Spec.Role,
			"-N", node.Name,
			"--admin",
		}
	}
	return []string{
		"/opt/idsvr/bin/idsvr",
		"-s", node.Spec.Role,
		"--no-admin",
	}
}

// buildContainerPorts returns the container ports based on node type.
func buildContainerPorts(node *v1alpha1.IdentityServerNode) []corev1.ContainerPort {
	ports := []corev1.ContainerPort{
		{Name: "health-check", ContainerPort: portHealthCheck, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: portMetrics, Protocol: corev1.ProtocolTCP},
	}

	if node.Spec.Type == v1alpha1.NodeTypeAdmin {
		ports = append(ports,
			corev1.ContainerPort{Name: "config", ContainerPort: portConfig, Protocol: corev1.ProtocolTCP},
			corev1.ContainerPort{Name: "ds-port", ContainerPort: portDistributedService, Protocol: corev1.ProtocolTCP},
		)
		if node.Spec.UI != nil && node.Spec.UI.Enabled {
			ports = append(ports, corev1.ContainerPort{Name: "admin-ui", ContainerPort: portAdminUI, Protocol: corev1.ProtocolTCP})
		}
	} else {
		ports = append(ports,
			corev1.ContainerPort{Name: "http", ContainerPort: portHTTP, Protocol: corev1.ProtocolTCP},
		)
	}

	return ports
}

// buildServicePorts returns the service ports based on node type.
func buildServicePorts(node *v1alpha1.IdentityServerNode) []corev1.ServicePort {
	ports := []corev1.ServicePort{
		{Name: "health-check", Port: portHealthCheck, TargetPort: intstr.FromString("health-check")},
		{Name: "metrics", Port: portMetrics, TargetPort: intstr.FromString("metrics")},
	}

	if node.Spec.Type == v1alpha1.NodeTypeAdmin {
		ports = append(ports,
			corev1.ServicePort{Name: "config", Port: portConfig, TargetPort: intstr.FromString("config")},
			corev1.ServicePort{Name: "ds-port", Port: portDistributedService, TargetPort: intstr.FromString("ds-port")},
		)
		if node.Spec.UI != nil && node.Spec.UI.Enabled {
			ports = append(ports, corev1.ServicePort{Name: "admin-ui", Port: portAdminUI, TargetPort: intstr.FromString("admin-ui")})
		}
	} else {
		servicePort := int32(portHTTP)
		if node.Spec.Service != nil && node.Spec.Service.Port > 0 {
			servicePort = node.Spec.Service.Port
		}
		ports = append(ports,
			corev1.ServicePort{Name: "http", Port: servicePort, TargetPort: intstr.FromString("http")},
		)
	}

	return ports
}

// buildEnvVars returns the environment variables for the container.
func buildEnvVars(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{Name: "STATUS_CMD_PORT", Value: strconv.Itoa(portHealthCheck)},
		{Name: "LOGGING_LEVEL", Value: resolveLoggingLevel(cluster, node)},
	}

	// Admin UI HTTP mode
	if node.Spec.Type == v1alpha1.NodeTypeAdmin && node.Spec.UI != nil {
		// Secure defaults to true if not explicitly set
		secure := node.Spec.UI.Secure == nil || *node.Spec.UI.Secure
		httpMode := "false"
		if !secure {
			httpMode = "true"
		}
		envVars = append(envVars, corev1.EnvVar{Name: "ADMIN_UI_HTTP_MODE", Value: httpMode})
	}

	// Admin credentials as env vars from secret
	if cluster.Spec.AdminCredentials != nil {
		secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
		for _, item := range cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Items {
			envVars = append(envVars, corev1.EnvVar{
				Name: item.Path,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  item.Key,
					},
				},
			})
		}
	}

	// Auto-inject PASSWORD for Curity's unattended installer when admin UI is enabled.
	// The installer checks for PASSWORD (not ADMIN_PASSWORD) to trigger first-run setup
	// which configures the admin-service XML and starts the UI on port 6749.
	if node.Spec.Type == v1alpha1.NodeTypeAdmin && node.Spec.UI != nil && node.Spec.UI.Enabled {
		if cluster.Spec.AdminCredentials != nil {
			hasPassword := false
			for _, item := range cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Items {
				if item.Path == "PASSWORD" {
					hasPassword = true
					break
				}
			}
			if !hasPassword {
				secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
				envVars = append(envVars, corev1.EnvVar{
					Name: "PASSWORD",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
							Key:                  "ADMIN_PASSWORD",
						},
					},
				})
			}
		}
	}

	// Append node-level environment variables last
	envVars = append(envVars, node.Spec.EnvironmentVariables...)

	return envVars
}

// buildVolumes returns volumes and mounts for the cluster-config (cluster.xml)
// and any discovered config resources. Cluster-config is always first.
func buildVolumes(clusterName string, configs []DiscoveredConfigResource) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := make([]corev1.Volume, 0, 1+len(configs))
	mounts := make([]corev1.VolumeMount, 0, 1+len(configs))

	// Cluster config (cluster.xml) — always mounted for inter-node TLS.
	// Optional so pods can start before the genclust Job completes.
	secretName := clusterName + "-cluster-config"
	volumes = append(volumes, corev1.Volume{
		Name: "cluster-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
				Items: []corev1.KeyToPath{
					{Key: "cluster.xml", Path: "cluster.xml"},
				},
				Optional: ptr.To(true),
			},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      "cluster-config",
		MountPath: "/opt/idsvr/etc/init/cluster.xml",
		SubPath:   "cluster.xml",
		ReadOnly:  true,
	})

	// Discovered config volumes.
	for _, cfg := range configs {
		volName := configVolumeName(cfg.IsSecret, cfg.Name)
		mountBase := mountPathForConfigType(cfg.ConfigType)

		if cfg.IsSecret {
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: cfg.Name,
					},
				},
			})
		} else {
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: cfg.Name},
					},
				},
			})
		}

		// Mount each data key as a SubPath mount.
		keys := make([]string, 0, len(cfg.Data))
		for k := range cfg.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: mountBase + mountFilename(cfg.IsSecret, cfg.Name, key),
				SubPath:   key,
				ReadOnly:  true,
			})
		}
	}

	return volumes, mounts
}

// buildLabels returns the standard Kubernetes labels for the resource.
// app.kubernetes.io/instance is {clusterName}-{nodeName} to keep selectors
// disjoint across clusters sharing a node name in the same namespace.
func buildLabels(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "curity-identity-server",
		"app.kubernetes.io/instance":   ownedResourceName(cluster.Name, node.Name),
		"app.kubernetes.io/managed-by": "curity-operator",
		"app.kubernetes.io/component":  string(node.Spec.Type),
		"app.kubernetes.io/version":    cluster.Spec.Version,
		"curity.io/cluster":            cluster.Name,
		"curity.io/role":               node.Spec.Role,
	}
}

// buildSelectorLabels returns the minimal labels used for pod selection.
func buildSelectorLabels(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "curity-identity-server",
		"app.kubernetes.io/instance": ownedResourceName(cluster.Name, node.Name),
	}
}

// resolveReplicas returns the replica count for the Deployment.
// Admin nodes are always forced to 1 replica.
// Runtime nodes with HPA enabled return minReplicas as the initial baseline.
func resolveReplicas(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *int32 {
	if node.Spec.Type == v1alpha1.NodeTypeAdmin {
		return ptr.To(int32(1))
	}
	as := resolveAutoscaling(cluster, node)
	if as != nil && as.Enabled {
		return ptr.To(as.MinReplicas)
	}
	if node.Spec.Replicas != nil {
		return node.Spec.Replicas
	}
	return ptr.To(int32(1))
}

// resolveAutoscaling returns the effective autoscaling spec, preferring node over cluster entirely.
func resolveAutoscaling(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *v1alpha1.AutoscalingSpec {
	if node.Spec.Autoscaling != nil {
		return node.Spec.Autoscaling
	}
	return cluster.Spec.Autoscaling
}

// resolvePDB returns the effective PodDisruptionBudget spec. Node-level spec
// replaces cluster-level entirely — no field-level merge — so users who set
// PDBSpec on a node own its full configuration for that node.
func resolvePDB(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *v1alpha1.PDBSpec {
	if node.Spec.PodDisruptionBudget != nil {
		return node.Spec.PodDisruptionBudget
	}
	return cluster.Spec.PodDisruptionBudget
}

// resolveResources returns the resource requirements, preferring node over cluster.
func resolveResources(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *corev1.ResourceRequirements {
	if node.Spec.Resources != nil {
		return node.Spec.Resources
	}
	return cluster.Spec.Resources
}

// resolveProbes returns the probe spec, preferring node over cluster.
func resolveProbes(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *v1alpha1.ProbeSpec {
	if node.Spec.Probes != nil {
		return node.Spec.Probes
	}
	return cluster.Spec.Probes
}

// resolveLoggingLevel returns the logging level, preferring node over cluster, defaulting to INFO.
func resolveLoggingLevel(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) string {
	if node.Spec.Logging != nil && node.Spec.Logging.Level != "" {
		return node.Spec.Logging.Level
	}
	if cluster.Spec.Logging != nil && cluster.Spec.Logging.Level != "" {
		return cluster.Spec.Logging.Level
	}
	return "INFO"
}

// resolveLogging returns the effective logging spec, preferring node over cluster entirely.
func resolveLogging(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *v1alpha1.LoggingSpec {
	if node.Spec.Logging != nil {
		return node.Spec.Logging
	}
	return cluster.Spec.Logging
}

// resolveNodeSelector returns the effective node selector, merging cluster and node maps.
// Node values win on conflict keys, matching the mergeMaps pattern used for labels/annotations.
func resolveNodeSelector(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) map[string]string {
	return mergeMaps(cluster.Spec.NodeSelector, node.Spec.NodeSelector)
}

// resolveTolerations returns the effective tolerations, preferring node over cluster entirely.
func resolveTolerations(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) []corev1.Toleration {
	if len(node.Spec.Tolerations) > 0 {
		return node.Spec.Tolerations
	}
	return cluster.Spec.Tolerations
}

// resolveAffinity returns the effective affinity, preferring node over cluster entirely.
func resolveAffinity(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *corev1.Affinity {
	if node.Spec.Affinity != nil {
		return node.Spec.Affinity
	}
	return cluster.Spec.Affinity
}

// resolveTopologySpreadConstraints returns the effective topology spread constraints,
// preferring node over cluster entirely.
func resolveTopologySpreadConstraints(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) []corev1.TopologySpreadConstraint {
	if len(node.Spec.TopologySpreadConstraints) > 0 {
		return node.Spec.TopologySpreadConstraints
	}
	return cluster.Spec.TopologySpreadConstraints
}

const defaultLogImage = "busybox:latest"

// buildLogSidecars creates sidecar containers that tail Curity log files to stdout.
// One sidecar per log type (e.g., audit, request, cluster).
func buildLogSidecars(logging *v1alpha1.LoggingSpec) []corev1.Container {
	if logging == nil || !logging.Stdout || len(logging.Logs) == 0 {
		return nil
	}

	image := logging.Image
	if image == "" {
		image = defaultLogImage
	}

	var sidecars []corev1.Container
	for _, logName := range logging.Logs {
		sidecar := corev1.Container{
			Name:    logName,
			Image:   image,
			Command: []string{"tail", "-F", fmt.Sprintf("/log/%s.log", strings.ToLower(logName))},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "log-volume",
					MountPath: "/log",
					ReadOnly:  true,
				},
			},
		}
		if logging.Resources != nil {
			sidecar.Resources = *logging.Resources
		}
		sidecars = append(sidecars, sidecar)
	}

	return sidecars
}

// resolveServiceType returns the service type from the node spec or defaults to ClusterIP.
func resolveServiceType(node *v1alpha1.IdentityServerNode) corev1.ServiceType {
	if node.Spec.Service != nil && node.Spec.Service.Type != "" {
		return node.Spec.Service.Type
	}
	return corev1.ServiceTypeClusterIP
}

// buildLivenessProbe constructs the liveness probe with configurable or default values.
func buildLivenessProbe(probes *v1alpha1.ProbeSpec) *corev1.Probe {
	p := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(portHealthCheck),
			},
		},
		InitialDelaySeconds: defaultProbeInitialDelay,
		PeriodSeconds:       defaultProbePeriod,
		TimeoutSeconds:      defaultProbeTimeout,
		FailureThreshold:    defaultProbeFailure,
	}

	if probes != nil && probes.Liveness != nil {
		applyProbeConfig(p, probes.Liveness)
	}

	return p
}

// buildReadinessProbe constructs the readiness probe with configurable or default values.
func buildReadinessProbe(probes *v1alpha1.ProbeSpec) *corev1.Probe {
	p := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(portHealthCheck),
			},
		},
		InitialDelaySeconds: defaultProbeInitialDelay,
		PeriodSeconds:       defaultProbePeriod,
		TimeoutSeconds:      defaultProbeTimeout,
		FailureThreshold:    defaultProbeFailure,
		SuccessThreshold:    defaultProbeSuccess,
	}

	if probes != nil && probes.Readiness != nil {
		applyProbeConfig(p, probes.Readiness)
	}

	return p
}

// applyProbeConfig overrides probe defaults with user-specified values.
func applyProbeConfig(probe *corev1.Probe, config *v1alpha1.ProbeConfig) {
	if config.InitialDelaySeconds != nil {
		probe.InitialDelaySeconds = *config.InitialDelaySeconds
	}
	if config.PeriodSeconds != nil {
		probe.PeriodSeconds = *config.PeriodSeconds
	}
	if config.TimeoutSeconds != nil {
		probe.TimeoutSeconds = *config.TimeoutSeconds
	}
	if config.FailureThreshold != nil {
		probe.FailureThreshold = *config.FailureThreshold
	}
	if config.SuccessThreshold != nil {
		probe.SuccessThreshold = *config.SuccessThreshold
	}
}

// mergeMaps merges two maps, with values from the override map taking precedence.
func mergeMaps(base, override map[string]string) map[string]string {
	if base == nil && override == nil {
		return nil
	}
	result := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		result[k] = v
	}
	return result
}

// buildHPA constructs the desired HorizontalPodAutoscaler for a runtime node.
// The as parameter must be non-nil; callers must guard with a nil/enabled check.
func buildHPA(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode, as *v1alpha1.AutoscalingSpec) *autoscalingv2.HorizontalPodAutoscaler {

	metrics := []autoscalingv2.MetricSpec{
		{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: ptr.To(as.TargetCPUUtilizationPercentage),
				},
			},
		},
	}

	metrics = append(metrics, as.CustomMetrics...)

	owned := ownedResourceName(cluster.Name, node.Name)
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owned,
			Namespace: node.Namespace,
			Labels:    buildLabels(cluster, node),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       owned,
			},
			MinReplicas: ptr.To(as.MinReplicas),
			MaxReplicas: as.MaxReplicas,
			Metrics:     metrics,
		},
	}
}

// buildPDB constructs the desired PodDisruptionBudget for a runtime node.
// Caller must verify that resolvePDB(cluster, node) is non-nil and its
// MinAvailable field is non-nil before invoking this function.
func buildPDB(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) *policyv1.PodDisruptionBudget {
	pdb := resolvePDB(cluster, node)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ownedResourceName(cluster.Name, node.Name),
			Namespace: node.Namespace,
			Labels:    buildLabels(cluster, node),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: pdb.MinAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: buildSelectorLabels(cluster, node),
			},
		},
	}
}
