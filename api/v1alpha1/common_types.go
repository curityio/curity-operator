// +kubebuilder:object:generate=true
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// NodeType defines whether a node is an admin or runtime node.
type NodeType string

const (
	// NodeTypeAdmin represents an admin node (max 1 per cluster).
	NodeTypeAdmin NodeType = "admin"
	// NodeTypeRuntime represents a runtime node.
	NodeTypeRuntime NodeType = "runtime"
)

// Condition type constants for status reporting.
const (
	ConditionReady              = "Ready"
	ConditionAvailable          = "Available"
	ConditionProgressing        = "Progressing"
	ConditionDegraded           = "Degraded"
	ConditionClusterConfigReady = "ClusterConfigReady"
)

// Finalizer names.
const (
	ClusterFinalizer = "curity.io/cluster-protection"
	NodeFinalizer    = "curity.io/node-protection"
)

// ObjectReference identifies a Kubernetes resource by name and namespace.
type ObjectReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`
}

// KeyToPath maps a key in a Secret or ConfigMap to a mount path.
type KeyToPath struct {
	// +kubebuilder:validation:Required
	Key string `json:"key"`

	// +kubebuilder:validation:Required
	Path string `json:"path"`
}

// SecretKeyRefSource references a Secret with selectable items.
type SecretKeyRefSource struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:MinItems=1
	Items []KeyToPath `json:"items"`
}

// ConfigMapRefSource references a ConfigMap with selectable items.
type ConfigMapRefSource struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:MinItems=1
	Items []KeyToPath `json:"items"`
}

// CredentialsSource specifies how to load admin credentials.
type CredentialsSource struct {
	ValueFrom CredentialsValueFrom `json:"valueFrom"`
}

// CredentialsValueFrom specifies the secret containing admin credentials.
type CredentialsValueFrom struct {
	SecretKeyRef SecretKeyRefSource `json:"secretKeyRef"`
}

// ConfigurationSource specifies how to load XML configuration files.
type ConfigurationSource struct {
	ValueFrom ConfigurationValueFrom `json:"valueFrom"`
}

// ConfigurationValueFrom specifies ConfigMap and Secret sources for configuration.
type ConfigurationValueFrom struct {
	ConfigMapRef *ConfigMapRefSource `json:"configMapRef,omitempty"`
	SecretRef    *SecretKeyRefSource `json:"secretRef,omitempty"`
}

// DataSourceSpec specifies database connection configuration.
type DataSourceSpec struct {
	// +kubebuilder:validation:Enum=postgres
	Type string `json:"type"`

	ValueFrom DataSourceValueFrom `json:"valueFrom"`
}

// DataSourceValueFrom specifies the secret containing database credentials.
type DataSourceValueFrom struct {
	SecretKeyRef SecretKeyRefSource `json:"secretKeyRef"`
}

// UISpec configures the admin UI (admin nodes only).
type UISpec struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Secure controls whether the admin UI uses HTTPS (true) or HTTP (false).
	// Defaults to true when not explicitly set.
	Secure *bool `json:"secure,omitempty"`
}

// ServiceSpec configures the Kubernetes Service for the node.
type ServiceSpec struct {
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer;NodePort
	Type corev1.ServiceType `json:"type,omitempty"`

	// +kubebuilder:default=8443
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// ProbeSpec configures liveness and readiness probes.
type ProbeSpec struct {
	Liveness  *ProbeConfig `json:"liveness,omitempty"`
	Readiness *ProbeConfig `json:"readiness,omitempty"`
}

// ProbeConfig holds tunable probe parameters.
type ProbeConfig struct {
	// +kubebuilder:default=30
	InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`

	// +kubebuilder:default=10
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`

	// +kubebuilder:default=1
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// +kubebuilder:default=3
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`

	// +kubebuilder:default=3
	SuccessThreshold *int32 `json:"successThreshold,omitempty"`
}

// AutoscalingSpec configures horizontal pod autoscaling.
type AutoscalingSpec struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:default=10
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// +kubebuilder:default=80
	TargetCPUUtilizationPercentage int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

// PDBSpec configures a PodDisruptionBudget.
type PDBSpec struct {
	// +kubebuilder:validation:Minimum=0
	MinAvailable *int32 `json:"minAvailable,omitempty"`
}

// LoggingSpec configures logging behavior.
// Matches the Helm chart's per-role logging configuration.
type LoggingSpec struct {
	// Level sets the Curity server log level.
	// Valid values: ERROR, WARN, INFO, DEBUG, TRACE.
	// Defaults to INFO if not set.
	// +kubebuilder:validation:Enum=ERROR;WARN;INFO;DEBUG;TRACE
	Level string `json:"level,omitempty"`

	// Stdout enables sidecar containers that tail Curity log files
	// to stdout, making them accessible via kubectl logs.
	// +kubebuilder:default=false
	Stdout bool `json:"stdout,omitempty"`

	// Logs is the list of Curity log files to stream when stdout is enabled.
	// Common values: audit, request, cluster, confsvc, confsvc-internal, post-commit-scripts.
	Logs []string `json:"logs,omitempty"`

	// Image for the sidecar log tailing containers.
	// Defaults to busybox:latest.
	Image string `json:"image,omitempty"`

	// Resources for the sidecar log tailing containers.
	// Node-level overrides cluster-level.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}
