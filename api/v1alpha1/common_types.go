// +kubebuilder:object:generate=true
package v1alpha1

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	// ConditionClusterReady on an IdentityServerNode reports whether its
	// referenced IdentityServerCluster exists and the node has been adopted
	// via controller OwnerReferences.
	ConditionClusterReady = "ClusterReady"
)

// Reason values used across multiple condition types. Each constant names
// the condition(s) it is set on; do not assume from the block.
const (
	// ReasonClusterFound / ReasonClusterNotFound are set on
	// ConditionClusterReady (IdentityServerNode) when the referenced cluster
	// is or is not found. ReasonClusterNotFound is also reused on Degraded
	// and Ready while the orphan state persists.
	ReasonClusterFound    = "ClusterFound"
	ReasonClusterNotFound = "ClusterNotFound"
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
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer;NodePort;ExternalName
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
// +kubebuilder:validation:XValidation:rule="self.minReplicas <= self.maxReplicas",message="minReplicas must be less than or equal to maxReplicas"
type AutoscalingSpec struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetCPUUtilizationPercentage int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// CustomMetrics defines additional HPA metrics beyond CPU utilization.
	// A CPU utilization metric is always included automatically; do not duplicate it here.
	// Uses the standard Kubernetes MetricSpec from autoscaling/v2.
	// +kubebuilder:validation:MaxItems=100
	CustomMetrics []autoscalingv2.MetricSpec `json:"customMetrics,omitempty"`
}

// PDBSpec configures a PodDisruptionBudget for runtime pods.
// Admin nodes do not participate in PDB reconciliation; if set there, the
// field is ignored and a Warning event is emitted (see the node reconciler).
type PDBSpec struct {
	// MinAvailable is the minimum number of pods that must remain available
	// during voluntary disruption. Accepts an integer (e.g., 2) or a
	// percentage string (e.g., "50%"). Matches upstream
	// policy/v1.PodDisruptionBudgetSpec.MinAvailable.
	// +kubebuilder:validation:XIntOrString
	MinAvailable *intstr.IntOrString `json:"minAvailable,omitempty"`
}

// AppliedManagedResource describes a managed ConfigMap or Secret
// (curity.io/managed=true) that the operator has mounted on a node.
// +kubebuilder:object:generate=true
type AppliedManagedResource struct {
	// Name is the name of the ConfigMap or Secret.
	Name string `json:"name"`

	// Kind is "ConfigMap" or "Secret".
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	Kind string `json:"kind"`

	// ConfigType is the curity.io/config-type annotation value ("base", "license", or "logging").
	// +kubebuilder:validation:Enum=base;license;logging
	ConfigType string `json:"configType"`
}

// ManagedResourceIssue describes a problem on a managed ConfigMap or Secret
// (curity.io/managed=true) that affects the cluster carrying this status —
// either the resource was skipped from mounting (UnknownConfigType) or it
// shares a data key with another applicable resource (DuplicateConfigKey).
// Scope-only issues (UnknownClusterInScope, EmptyClusterScope) are not
// represented here; they surface only as Warning Events on the resource
// itself, since they do not change what mounts on this cluster.
// +kubebuilder:object:generate=true
type ManagedResourceIssue struct {
	// Kind is "ConfigMap" or "Secret".
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	Kind string `json:"kind"`

	// Name is the name of the ConfigMap or Secret.
	Name string `json:"name"`

	// Reason is a stable machine-readable identifier for the kind of issue.
	// +kubebuilder:validation:Enum=UnknownConfigType;DuplicateConfigKey;LoggingConfigInvalid;DuplicateLoggingConfig
	Reason string `json:"reason"`

	// Message is a byte-stable human-readable description of the issue.
	Message string `json:"message"`
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
	// Allowed values: audit, request, cluster, confsvc, confsvc-internal, post-commit-scripts.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Enum=audit;request;cluster;confsvc;confsvc-internal;post-commit-scripts
	Logs []string `json:"logs,omitempty"`

	// Image for the sidecar log tailing containers.
	// Defaults to busybox:latest.
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`

	// Resources for the sidecar log tailing containers.
	// Node-level overrides cluster-level.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PackageSpec declares one remote ZIP archive that the operator downloads
// at pod start and unpacks into the Curity container at MountPath.
// Each entry produces one init container per pod. Removing an entry
// removes its init container and mount on the next reconcile.
type PackageSpec struct {
	// Source describes where to fetch the archive from.
	// +kubebuilder:validation:Required
	Source PackageSource `json:"source"`

	// MountPath is the absolute path inside the Curity container where
	// the archive contents are unpacked. Each package needs a distinct path.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:MaxLength=1024
	MountPath string `json:"mountPath"`
}

// PackageSource describes the source of a package archive.
// When auth is set, exactly one of basicAuth or bearerToken must be
// configured — `auth: {}` (empty) is rejected so the API surface is
// unambiguous (an empty auth block would silently fall back to
// unauthenticated, which is confusing). Omit the auth field entirely
// for unauthenticated downloads.
// +kubebuilder:validation:XValidation:rule="!has(self.auth) || ((has(self.auth.basicAuth) ? 1 : 0) + (has(self.auth.bearerToken) ? 1 : 0) == 1)",message="exactly one of auth.basicAuth or auth.bearerToken must be set when auth is configured"
type PackageSource struct {
	// URL is the full URL of the ZIP archive to fetch. HTTPS is strongly
	// recommended; the pattern allows http:// only because the proposal
	// schema does — production deployments should set NetworkPolicy to
	// block plain-HTTP egress.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// TLS configures the download connection's TLS behavior. Omit to
	// accept the system trust store with default verification.
	TLS *PackageTLSSpec `json:"tls,omitempty"`

	// Auth carries credentials for authenticated endpoints. Set at most
	// one of BasicAuth or BearerToken.
	Auth *PackageAuthSpec `json:"auth,omitempty"`
}

// PackageTLSSpec configures TLS for a package download. When Enabled is
// false (default), the system trust store is used and the rest of this
// block is ignored. When Enabled is true, SkipVerify / CA / ClientCert apply.
type PackageTLSSpec struct {
	// Enabled gates the rest of this TLS block. When false (default),
	// the system trust store is used.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// SkipVerify disables certificate verification. Use only in
	// non-production environments. When true, CA is ignored.
	// +kubebuilder:default=false
	SkipVerify bool `json:"skipVerify,omitempty"`

	// CA is a Secret holding a PEM bundle used as the trust root for
	// this download. Ignored when SkipVerify is true.
	CA *PackageSecretKeyRef `json:"ca,omitempty"`

	// ClientCert is a Secret holding the cert (and matching private key)
	// for mutual TLS.
	ClientCert *PackageClientCertRef `json:"clientCert,omitempty"`
}

// PackageAuthSpec carries credentials for an authenticated download.
// Set at most one of BasicAuth or BearerToken.
type PackageAuthSpec struct {
	// BasicAuth provides HTTP Basic Authentication credentials.
	BasicAuth *PackageBasicAuthRef `json:"basicAuth,omitempty"`

	// BearerToken provides a bearer token sent as `Authorization: Bearer <token>`.
	BearerToken *PackageSecretKeyRef `json:"bearerToken,omitempty"`
}

// PackageSecretKeyRef references a single key in a Secret. Used for
// bearer tokens, CA bundles, and client cert files.
type PackageSecretKeyRef struct {
	// +kubebuilder:validation:Required
	SecretRef PackageSecretKeySelector `json:"secretRef"`
}

// PackageSecretKeySelector identifies one key inside a Secret.
type PackageSecretKeySelector struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// PackageClientCertRef references the Secret holding a client certificate
// and matching private key for mTLS.
type PackageClientCertRef struct {
	// +kubebuilder:validation:Required
	SecretRef PackageClientCertSelector `json:"secretRef"`
}

// PackageClientCertSelector identifies the Secret holding the client
// certificate. The Secret must contain both `tls.crt` and `tls.key` keys
// (the K8s TLS Secret convention). Key names the cert entry; the matching
// private key is read from the sibling key derived by replacing a trailing
// "crt" with "key" (so `tls.crt` → `tls.key`).
type PackageClientCertSelector struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// PackageBasicAuthRef references credentials for HTTP basic auth.
type PackageBasicAuthRef struct {
	// +kubebuilder:validation:Required
	SecretRef PackageBasicAuthSelector `json:"secretRef"`
}

// PackageBasicAuthSelector identifies the Secret holding basic-auth
// credentials and the keys within it for username and password.
type PackageBasicAuthSelector struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UsernameKey is the key in the Secret that holds the username.
	// +kubebuilder:validation:Required
	UsernameKey string `json:"usernameKey"`

	// PasswordKey is the key in the Secret that holds the password.
	// +kubebuilder:validation:Required
	PasswordKey string `json:"passwordKey"`
}
