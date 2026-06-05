package controller

import (
	"crypto/sha256"
	"encoding/hex"
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
	"k8s.io/klog/v2"
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
	defaultProbeSuccess      = int32(1)

	// Liveness probes only accept SuccessThreshold=1.
	livenessSuccessThreshold = int32(1)

	// Kube apiserver default for --default-{not-ready,unreachable}-toleration-seconds.
	// Override at runtime via DEFAULT_TOLERATION_SECONDS env var (cmd/manager).
	defaultTolerationSeconds = int64(300)
)

// ownedResourceNameMaxLen caps the output of OwnedResourceName so that K8s's
// own pod-template-hash (~10 chars) and pod random suffix (5 chars) can still
// be appended without exceeding the 63-char DNS-1123 label limit on Pod names.
const ownedResourceNameMaxLen = 47

// OwnedResourceName returns the child-resource name for Deployments, Services,
// HPAs, and PDBs owned by a node. The 8-character SHA-256 suffix disambiguates
// (cluster, node) pairs that would otherwise produce the same name when the
// hyphen separator interacts with hyphens inside cluster or node names —
// without it, ("foo-bar","baz") and ("foo","bar-baz") both produce
// "foo-bar-baz". The output is capped at 47 characters so derived Pod names
// stay under the K8s 63-char DNS-1123 limit; the hash is computed over the
// FULL inputs before any truncation, so distinct (cluster, node) pairs still
// produce distinct names even when the human-readable prefix is truncated.
//
// Exported so test helpers in the controller_test and e2e packages compute
// the same name the production code uses, instead of reimplementing the
// algorithm.
func OwnedResourceName(clusterName, nodeName string) string {
	h := sha256.Sum256([]byte(clusterName + "\x00" + nodeName))
	suffix := "-" + hex.EncodeToString(h[:4])

	base := clusterName + "-" + nodeName
	if len(base)+len(suffix) > ownedResourceNameMaxLen {
		base = strings.TrimRight(base[:ownedResourceNameMaxLen-len(suffix)], "-")
	}
	return base + suffix
}

// buildDeployment constructs the desired Deployment for an IdentityServerNode.
// fetcherImage is the package init-container image (see ResolvePackageFetcherImage).
//
// Fields tagged "// apiserver-default" throughout this file are set explicitly
// to prevent the reconciler's wholesale `deploy.Spec = desiredDeploy.Spec`
// assignment from stripping them, causing a drift loop.
func buildDeployment(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode, configs []DiscoveredManagedResource, fetcherImage string) *appsv1.Deployment {
	labels := buildLabels(cluster, node)
	podAnnotations := mergeMaps(cluster.Spec.PodAnnotations, node.Spec.PodAnnotations)
	// Operator labels overlay last: collisions on operator-owned keys lose.
	podLabels := mergeMaps(mergeMaps(cluster.Spec.PodLabels, node.Spec.PodLabels), labels)

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
		// apiserver-default
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
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

	// Append package volumes and main-container mounts. Each package gets
	// an emptyDir at /pkg-<n>, surfaced to the main container at MountPath.
	if pkgVolumes := buildPackageVolumes(cluster.Spec.Packages); len(pkgVolumes) > 0 {
		volumes = append(volumes, pkgVolumes...)
		container.VolumeMounts = append(container.VolumeMounts, buildPackageVolumeMounts(cluster.Spec.Packages)...)
	}

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
			Name:      OwnedResourceName(cluster.Name, node.Name),
			Namespace: node.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: buildSelectorLabels(cluster, node),
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       ptr.To(intstr.FromString("25%")),
					MaxUnavailable: ptr.To(intstr.FromString("25%")),
				},
			},
			RevisionHistoryLimit:    ptr.To(int32(10)),
			ProgressDeadlineSeconds: ptr.To(int32(600)),
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
					InitContainers:                buildPackageInitContainers(cluster.Spec.Packages, fetcherImage),
					Containers:                    containers,
					Volumes:                       volumes,
					NodeSelector:                  resolveNodeSelector(cluster, node),
					Tolerations:                   mergeDefaultTolerations(resolveTolerations(cluster, node)),
					Affinity:                      resolveAffinity(cluster, node),
					TopologySpreadConstraints:     resolveTopologySpreadConstraints(cluster, node),
					DNSPolicy:                     corev1.DNSClusterFirst,
					RestartPolicy:                 corev1.RestartPolicyAlways,
					SchedulerName:                 corev1.DefaultSchedulerName,
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
					// WARNING: Priority is apiserver-derived from PriorityClassName.
					// If `priorityClassName` is ever exposed on the CRD, remove
					// this line — otherwise it stomps the resolved Priority and
					// reintroduces drift. apiserver-default.
					Priority:           ptr.To(int32(0)),
					EnableServiceLinks: ptr.To(true),
					PreemptionPolicy:   ptr.To(corev1.PreemptLowerPriority),
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
			Name:      OwnedResourceName(cluster.Name, node.Name),
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

// buildServicePorts returns the service ports based on node type. Protocol
// is set explicitly on every port (apiserver-default; see buildDeployment).
func buildServicePorts(node *v1alpha1.IdentityServerNode) []corev1.ServicePort {
	ports := []corev1.ServicePort{
		{Name: "health-check", Port: portHealthCheck, TargetPort: intstr.FromString("health-check"), Protocol: corev1.ProtocolTCP},
		{Name: "metrics", Port: portMetrics, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP},
	}

	if node.Spec.Type == v1alpha1.NodeTypeAdmin {
		ports = append(ports,
			corev1.ServicePort{Name: "config", Port: portConfig, TargetPort: intstr.FromString("config"), Protocol: corev1.ProtocolTCP},
			corev1.ServicePort{Name: "ds-port", Port: portDistributedService, TargetPort: intstr.FromString("ds-port"), Protocol: corev1.ProtocolTCP},
		)
		if node.Spec.UI != nil && node.Spec.UI.Enabled {
			ports = append(ports, corev1.ServicePort{Name: "admin-ui", Port: portAdminUI, TargetPort: intstr.FromString("admin-ui"), Protocol: corev1.ProtocolTCP})
		}
	} else {
		ports = append(ports,
			corev1.ServicePort{Name: "http", Port: node.Spec.Service.Port, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
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

	isAdmin := node.Spec.Type == v1alpha1.NodeTypeAdmin
	// skipInstall is admin-only; guard the type for in-process paths that bypass CEL.
	skipInstall := isAdmin && node.Spec.SkipInstall != nil && *node.Spec.SkipInstall

	// Admin UI HTTP mode. nil Secure falls through to the secure-HTTPS default
	// (a guard for in-process construction paths that bypass admission).
	if isAdmin && node.Spec.UI != nil && node.Spec.UI.Enabled {
		httpMode := "false"
		if node.Spec.UI.Secure != nil && !*node.Spec.UI.Secure {
			httpMode = "true"
		}
		envVars = append(envVars, corev1.EnvVar{Name: "ADMIN_UI_HTTP_MODE", Value: httpMode})
	}

	// Admin credentials as env vars. The admin password is the installer
	// trigger, so it goes only on the admin node.
	if cluster.Spec.AdminCredentials != nil {
		secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
		for _, item := range cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Items {
			if item.Key == "ADMIN_PASSWORD" && !isAdmin {
				continue
			}
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

	if skipInstall {
		envVars = append(envVars, corev1.EnvVar{Name: "SKIP_INSTALL", Value: "1"})
	}

	// Auto-inject PASSWORD for Curity's unattended installer when admin UI is enabled.
	// The installer checks for PASSWORD (not ADMIN_PASSWORD) to trigger first-run setup
	// which configures the admin-service XML and starts the UI on port 6749.
	if isAdmin && node.Spec.UI != nil && node.Spec.UI.Enabled {
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
func buildVolumes(clusterName string, configs []DiscoveredManagedResource) ([]corev1.Volume, []corev1.VolumeMount) {
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
				// apiserver-default
				DefaultMode: ptr.To(corev1.SecretVolumeSourceDefaultMode),
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
		mountPath, isLeaf := mountPathForConfigType(cfg.ConfigType)

		keys := make([]string, 0, len(cfg.Data))
		for k := range cfg.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		// applyLoggingValidations guarantees len(keys)==1 for leaf mounts;
		// skip the entire volume if a malformed resource ever reaches here.
		if isLeaf && len(keys) != 1 {
			continue
		}

		// apiserver-default
		volName := configVolumeName(cfg.IsSecret, cfg.Name)
		if cfg.IsSecret {
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  cfg.Name,
						DefaultMode: ptr.To(corev1.SecretVolumeSourceDefaultMode),
					},
				},
			})
		} else {
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: cfg.Name},
						DefaultMode:          ptr.To(corev1.ConfigMapVolumeSourceDefaultMode),
					},
				},
			})
		}

		if isLeaf {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: mountPath,
				SubPath:   keys[0],
				ReadOnly:  true,
			})
			continue
		}

		for _, key := range keys {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: mountPath + mountFilename(cfg.IsSecret, cfg.Name, key),
				SubPath:   key,
				ReadOnly:  true,
			})
		}
	}

	return volumes, mounts
}

// buildLabels returns the standard Kubernetes labels for the resource.
// `curity.io/owned-by` is the operator's private selector key (see
// buildSelectorLabels); the `app.kubernetes.io/*` keys are informational.
func buildLabels(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "curity-identity-server",
		"app.kubernetes.io/instance":   OwnedResourceName(cluster.Name, node.Name),
		"app.kubernetes.io/managed-by": "curity-operator",
		"app.kubernetes.io/component":  string(node.Spec.Type),
		"app.kubernetes.io/version":    cluster.Spec.Version,
		"curity.io/cluster":            cluster.Name,
		"curity.io/role":               node.Spec.Role,
		"curity.io/owned-by":           OwnedResourceName(cluster.Name, node.Name),
	}
}

// userPodLabelsOverridden returns the sorted user-podLabels keys that the
// operator-overlay-last merge will silently drop. Drives the
// PodLabelsIgnored event; also covers the +2 app.kubernetes.io/{name,
// instance} keys the CRD CEL doesn't block.
func userPodLabelsOverridden(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) []string {
	operatorKeys := buildLabels(cluster, node)
	userKeys := mergeMaps(cluster.Spec.PodLabels, node.Spec.PodLabels)
	var overridden []string
	for k := range userKeys {
		if _, isOperator := operatorKeys[k]; isOperator {
			overridden = append(overridden, k)
		}
	}
	sort.Strings(overridden)
	return overridden
}

// buildSelectorLabels returns the single private key used for pod
// selection. Unreachable from user podLabels (CRD CEL + merge order).
func buildSelectorLabels(cluster *v1alpha1.IdentityServerCluster, node *v1alpha1.IdentityServerNode) map[string]string {
	return map[string]string{
		"curity.io/owned-by": OwnedResourceName(cluster.Name, node.Name),
	}
}

// resolveReplicas returns the replica count for the Deployment.
// Admin nodes always get 1 (enforced at admission by CEL). Runtime nodes
// with HPA enabled return minReplicas as the initial baseline.
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

// DefaultTolerationSeconds is the runtime-overridable seconds value for the
// not-ready/unreachable defaults. cmd/manager reassigns it from the
// DEFAULT_TOLERATION_SECONDS env var. Must match the cluster's apiserver
// flag or the drift loop returns.
var DefaultTolerationSeconds = defaultTolerationSeconds

// mergeDefaultTolerations appends the two NoExecute tolerations the kube
// `DefaultTolerationSeconds` admission controller would otherwise add.
// Matching uses `Toleration.ToleratesTaint` so a user `{Operator: Exists}`
// is recognised as already covering the defaults (no duplicate appended).
func mergeDefaultTolerations(user []corev1.Toleration) []corev1.Toleration {
	defaults := []corev1.Taint{
		{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute},
		{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute},
	}
	logger := klog.Background()
	out := append([]corev1.Toleration(nil), user...)
	for i := range defaults {
		taint := &defaults[i]
		tolerated := false
		for j := range user {
			if user[j].ToleratesTaint(logger, taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			out = append(out, corev1.Toleration{
				Key:               taint.Key,
				Operator:          corev1.TolerationOpExists,
				Effect:            taint.Effect,
				TolerationSeconds: ptr.To(DefaultTolerationSeconds),
			})
		}
	}
	return out
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
			// apiserver-default
			TerminationMessagePath:   corev1.TerminationMessagePathDefault,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		}
		if logging.Resources != nil {
			sidecar.Resources = *logging.Resources
		}
		sidecars = append(sidecars, sidecar)
	}

	return sidecars
}

// resolveServiceType returns the service type from the node spec. Service is
// Required at admission, so the field is always populated.
func resolveServiceType(node *v1alpha1.IdentityServerNode) corev1.ServiceType {
	return node.Spec.Service.Type
}

// buildLivenessProbe constructs the liveness probe with configurable or default values.
func buildLivenessProbe(probes *v1alpha1.ProbeSpec) *corev1.Probe {
	p := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(portHealthCheck),
				// apiserver-default
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: defaultProbeInitialDelay,
		PeriodSeconds:       defaultProbePeriod,
		TimeoutSeconds:      defaultProbeTimeout,
		FailureThreshold:    defaultProbeFailure,
		SuccessThreshold:    livenessSuccessThreshold,
	}

	if probes != nil && probes.Liveness != nil {
		applyProbeConfig(p, probes.Liveness)
	}
	// Pin to 1 — K8s rejects any other livenessProbe.successThreshold at
	// admission. Shared ProbeConfig allows user values for readiness's sake.
	p.SuccessThreshold = livenessSuccessThreshold

	return p
}

// buildReadinessProbe constructs the readiness probe with configurable or default values.
func buildReadinessProbe(probes *v1alpha1.ProbeSpec) *corev1.Probe {
	p := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(portHealthCheck),
				// apiserver-default
				Scheme: corev1.URISchemeHTTP,
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

var operatorLabelPrefixes = []string{
	"app.kubernetes.io/",
	"curity.io/",
}

// mergeManagedLabels returns desired overlaid onto existing, preserving any
// existing keys whose prefix is not operator-owned. Use in CreateOrUpdate
// mutators so user- or GitOps-added labels survive reconciles.
func mergeManagedLabels(existing, desired map[string]string) map[string]string {
	if existing == nil && desired == nil {
		return nil
	}
	out := make(map[string]string, len(existing)+len(desired))
	for k, v := range existing {
		if !isOperatorOwnedLabel(k) {
			out[k] = v
		}
	}
	for k, v := range desired {
		out[k] = v
	}
	return out
}

func isOperatorOwnedLabel(key string) bool {
	for _, p := range operatorLabelPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
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

	owned := OwnedResourceName(cluster.Name, node.Name)
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
			Name:      OwnedResourceName(cluster.Name, node.Name),
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
