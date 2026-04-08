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
	ConditionReady                 = "Ready"
	ConditionAvailable             = "Available"
	ConditionProgressing           = "Progressing"
	ConditionDegraded              = "Degraded"
	ConditionClusterConfigReady    = "ClusterConfigReady"
	ConditionConfigValidationReady = "ConfigValidationReady"
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

// CredentialsSource specifies how to load admin credentials.
type CredentialsSource struct {
	ValueFrom CredentialsValueFrom `json:"valueFrom"`
}

// CredentialsValueFrom specifies the secret containing admin credentials.
type CredentialsValueFrom struct {
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

// Validation status values for AppliedConfigStatus.
const (
	ValidationStatusValidated = "Validated"
	ValidationStatusPending   = "Pending"
	ValidationStatusFailed    = "Failed"
)

// AppliedConfigStatus represents the status of a discovered configuration resource.
// +kubebuilder:object:generate=true
type AppliedConfigStatus struct {
	// Name is the name of the ConfigMap or Secret.
	Name string `json:"name"`

	// Kind is "ConfigMap" or "Secret".
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	Kind string `json:"kind"`

	// ConfigType is the curity.io/config-type annotation value ("base" or "license").
	// +kubebuilder:validation:Enum=base;license
	ConfigType string `json:"configType"`

	// ValidationStatus is "Validated", "Pending", or "Failed".
	// +kubebuilder:validation:Enum=Validated;Pending;Failed
	ValidationStatus string `json:"validationStatus"`
}

// LoggingSpec configures logging behavior.
// Matches the Helm chart's per-role logging configuration.
type LoggingSpec struct {
	// Level sets the Curity server log level.
	// Valid values: ERROR, WARN, INFO, DEBUG, TRACE, OFF.
	// When set to OFF, log sidecar containers and the shared log volume are suppressed.
	// Defaults to INFO if not set.
	// +kubebuilder:validation:Enum=ERROR;WARN;INFO;DEBUG;TRACE;OFF
	Level string `json:"level,omitempty"`

	// Stdout enables sidecar containers that tail Curity log files
	// to stdout, making them accessible via kubectl logs.
	// Sidecars are suppressed when Level is OFF, even if Stdout is true.
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
