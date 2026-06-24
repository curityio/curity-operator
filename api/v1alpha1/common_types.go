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
	// ConditionPackagesReady reports whether the init-container package
	// downloads succeeded (set only when spec.packages is non-empty).
	ConditionPackagesReady = "PackagesReady"
	// ConditionManagedConfigsValid surfaces dropped managed configs as a
	// condition: False when a managed ConfigMap/Secret is excluded from
	// mounting (e.g. a typo'd config-type or invalid logging config), so the
	// failure shows on dashboards that render only .status.conditions instead
	// of staying all-green. Advisory-only issues that still mount
	// (DuplicateConfigKey) stay in managedResourceIssues but do not flip this.
	ConditionManagedConfigsValid = "ManagedConfigsValid"
	// ConditionConvertKeystoreReady reports keystore-conversion state when
	// spec.convertKeystore is set: True once the env Secret is published, False
	// while a source Secret is missing or a conversion fails. Omitted entirely
	// when convertKeystore is empty.
	ConditionConvertKeystoreReady = "ConvertKeystoreReady"
	// ConditionComplete on an IdentityServerDatabase reports whether the
	// database-init Job has finished successfully.
	ConditionComplete = "Complete"
	// ConditionFailed on an IdentityServerDatabase reports whether the
	// database-init Job exhausted its retries without succeeding.
	ConditionFailed = "Failed"
)

// Reason values for IdentityServerDatabase conditions.
const (
	// ReasonJobCreated is set on Progressing when the database-init Job has just
	// been created (or recreated after an image/spec change).
	ReasonJobCreated = "JobCreated"
	// ReasonJobRunning is set on Progressing while the Job is running.
	ReasonJobRunning = "JobRunning"
	// ReasonJobComplete is set on Ready=True/Complete=True when the Job succeeded.
	ReasonJobComplete = "JobComplete"
	// ReasonJobFailed is set on Ready=False/Failed=True when the Job failed.
	ReasonJobFailed = "JobFailed"
	// ReasonJDBCURLMissing is set on Ready=False when connection.secretRef is the
	// only declared URL source but the referenced Secret has no JDBC_URL key.
	ReasonJDBCURLMissing = "JDBCURLMissing"
	// ReasonConnectionSecretMissing is set on Ready=False when connection.secretRef
	// points at a Secret that does not exist.
	ReasonConnectionSecretMissing = "ConnectionSecretMissing"
	// ReasonConnectionKeyMissing is set on Ready=False when a per-field
	// connection secretKeyRef (username/password) names a key absent from the
	// referenced Secret.
	ReasonConnectionKeyMissing = "ConnectionKeyMissing"
	// ReasonJobPodNotStarting is set on Progressing=True/Ready=False when the
	// Job's pod cannot start (unschedulable, image pull failure, or container
	// config error) — a state the Job itself never reports as complete or failed.
	ReasonJobPodNotStarting = "JobPodNotStarting"
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

	// ReasonPackagesNotReady is set on ConditionReady when ConditionPackagesReady
	// is False — the package-fetch failure delegates to the PackagesReady
	// condition for the detailed message.
	ReasonPackagesNotReady = "PackagesNotReady"

	// ReasonInvalidSpec is set on ConditionDegraded (True) and ConditionReady
	// (False) when the apiserver rejects a child-resource write with HTTP 422
	// (apierrors.IsInvalid). Only a CR spec edit can resolve this; the
	// reconciler does not requeue.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonAdminCredsSecretMissing is set on Degraded=True and Ready=False when a
	// user-provided adminCredentials Secret name points at a Secret that does not exist.
	ReasonAdminCredsSecretMissing = "AdminCredsSecretMissing"
)

// Reason values for ConditionPackagesReady (set by the pre-check leg or the
// pod-watch translator on IdentityServerNode, mirrored to IdentityServerCluster).
const (
	// ReasonAllPackagesFetched is set on ConditionPackagesReady=True when every
	// package-fetch init container across the current ReplicaSet's pods exited 0.
	ReasonAllPackagesFetched = "AllPackagesFetched"

	// ReasonPackagesPending means a new package set has been applied but the
	// new ReplicaSet's pods have not yet reported init-container status.
	// Set preemptively when the operator is about to stamp a Deployment
	// with new package init containers, so the Ready overlay flips False
	// immediately — closes the rolling-update Ready=True false-positive
	// window that the prior PackagesReady=True (from the old, still-serving
	// pod) would otherwise leak through.
	ReasonPackagesPending = "PackagesPending"

	// ReasonPackagePodCreationFailed means the Deployment's ReplicaSet
	// controller cannot create pods for the package-bearing spec (SCC
	// rejection, admission webhook denial, ResourceQuota exhausted,
	// invalid pod spec). Without this, a packages rollout that blocks at
	// pod-creation time produces no pod-watch events and PackagesReady
	// stays PackagesPending indefinitely. The condition message carries
	// the Deployment's verbatim ReplicaFailure text so users see the
	// actual K8s diagnostic on `kubectl describe`.
	ReasonPackagePodCreationFailed = "PackagePodCreationFailed"

	// ReasonPackageSecretMissing means a referenced Secret does not exist.
	ReasonPackageSecretMissing = "PackageSecretMissing"

	// ReasonPackageSecretKeyMissing means the referenced Secret exists but
	// the named key is absent.
	ReasonPackageSecretKeyMissing = "PackageSecretKeyMissing"

	// ReasonPackageImagePullFailed means the fetcher init-container image
	// could not be pulled.
	ReasonPackageImagePullFailed = "PackageImagePullFailed"

	// ReasonPackageTLSVerifyFailed means the package download failed TLS
	// certificate verification against the configured CA bundle.
	ReasonPackageTLSVerifyFailed = "PackageTLSVerifyFailed"

	// ReasonPackageClientCertInvalid means the configured mTLS client cert
	// or private key could not be used for the package download.
	ReasonPackageClientCertInvalid = "PackageClientCertInvalid"

	// ReasonPackageHTTPError means the package URL returned an HTTP 4xx/5xx.
	ReasonPackageHTTPError = "PackageHTTPError"

	// ReasonPackageInvalidArchive means the downloaded artifact could not
	// be unpacked as a ZIP archive.
	ReasonPackageInvalidArchive = "PackageInvalidArchive"

	// ReasonPackageFetchFailed is the open-set catch-all for any definitive
	// package-fetch failure that does not match a more specific reason.
	// The condition message carries the verbatim diagnostic signal.
	ReasonPackageFetchFailed = "PackageFetchFailed"
)

// Finalizer names.
const (
	ClusterFinalizer = "curity.io/cluster-protection"
	NodeFinalizer    = "curity.io/node-protection"
)

// ObjectReference identifies a Kubernetes resource by name. The referent
// must live in the same namespace as the referring object: K8s GC treats
// OwnerReference as namespace-local, so a cross-namespace controller
// ownerRef silently cascade-deletes the dependent.
// +kubebuilder:validation:XValidation:rule="!has(self.__namespace__) || size(self.__namespace__) == 0",message="cross-namespace references are not supported; omit this field"
type ObjectReference struct {
	// Name must be a valid DNS-1123 subdomain.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`
}

// KeyToPath maps a Secret key to the env var name projected into the
// container. Path is the env var Name (not a filesystem path), so it must be
// a valid POSIX env var identifier: starts with letter or underscore, then
// alphanumerics or underscore.
type KeyToPath struct {
	// +kubebuilder:validation:Required
	Key string `json:"key"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Path string `json:"path"`
}

// ConvertKeystoreItem is one keystore-conversion request.
type ConvertKeystoreItem struct {
	// +kubebuilder:validation:Required
	SourceTLS ConvertKeystoreSource `json:"sourceTls"`
}

// ConvertKeystoreSource describes a TLS Secret to convert and the env vars to
// publish the result under.
// +kubebuilder:validation:XValidation:rule="!has(self.cert) || self.cert != self.keyName",message="cert must differ from keyName (they become two distinct environment variables)"
type ConvertKeystoreSource struct {
	// KeyName is the environment variable the converted keystore is published
	// under (e.g. SSL_SERVER_KEY). It must also be referenced from the Curity
	// base configuration (the ssl-server-keystore element). Must be a valid
	// environment-variable name: a letter or underscore, then alphanumerics or
	// underscores.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	KeyName string `json:"keyName"`

	// FromSecretRef is the source kubernetes.io/tls Secret (tls.crt + tls.key) in
	// the same namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	FromSecretRef string `json:"fromSecretRef"`

	// Cert, when set, additionally publishes the raw PEM certificate (multi-line,
	// not base64) under this environment-variable name (e.g. SSL_SERVER_CERT), for
	// Curity config that references the cert directly. A cert chain is a few KB,
	// well under env-size limits.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Cert string `json:"cert,omitempty"`
}

// SecretKeyRefSource references the admin-credentials Secret. Items must
// contain exactly one entry each for ADMIN_PASSWORD and CONFIG_ENCRYPTION_KEY —
// these specific keys are what the operator projects into Curity pods.
type SecretKeyRefSource struct {
	// Name must be a valid DNS-1123 subdomain Secret name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.exists_one(i, i.key == 'ADMIN_PASSWORD') && self.exists_one(i, i.key == 'CONFIG_ENCRYPTION_KEY')",message="items must contain exactly one entry each for ADMIN_PASSWORD and CONFIG_ENCRYPTION_KEY"
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

// UISpec configures the admin UI (admin nodes only). Enabled is required;
// secure is required only when enabled is true.
// +kubebuilder:validation:XValidation:rule="!self.enabled || has(self.secure)",message="secure is required when enabled is true"
type UISpec struct {
	// +kubebuilder:validation:Required
	Enabled bool `json:"enabled"`

	// Secure controls whether the admin UI uses HTTPS (true) or HTTP (false).
	// Required when enabled=true.
	Secure *bool `json:"secure,omitempty"`
}

// ServiceSpec tunes the node's single Kubernetes Service. Every field is
// optional and defaults per node role — set only the ports you want to
// override; the Service is always created with the full set of role ports.
type ServiceSpec struct {
	// Type is the Service type for the whole Service (every port shares it).
	// Defaults to ClusterIP.
	// +optional
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer;NodePort
	Type corev1.ServiceType `json:"type,omitempty"`

	// Port is the node's primary Service port: http on runtime (default 8443),
	// config on admin (default 6789). Changing the admin config port reruns
	// genclust and rolling-restarts the cluster.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// DistributedServicePort overrides the admin node's distributed-service
	// port (default 6790). Admin-only.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	DistributedServicePort int32 `json:"distributedServicePort,omitempty"`

	// UIPort exposes the admin UI on the Service at this port (typically 6749,
	// Curity's UI port). Setting it opts into exposure; omit it to run the UI
	// (ui.enabled) without surfacing it — the operator never auto-exposes the
	// UI. Admin-only.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	UIPort int32 `json:"uiPort,omitempty"`
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

	// Default 1 — K8s rejects livenessProbe.successThreshold != 1.
	// +kubebuilder:default=1
	SuccessThreshold *int32 `json:"successThreshold,omitempty"`
}

// AutoscalingSpec configures horizontal pod autoscaling. All four primary
// fields are required when the block is present; omit the block entirely
// to disable autoscaling.
// +kubebuilder:validation:XValidation:rule="self.minReplicas <= self.maxReplicas",message="minReplicas must be less than or equal to maxReplicas"
type AutoscalingSpec struct {
	// +kubebuilder:validation:Required
	Enabled bool `json:"enabled"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxReplicas int32 `json:"maxReplicas"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetCPUUtilizationPercentage int32 `json:"targetCPUUtilizationPercentage"`

	// CustomMetrics defines additional HPA metrics beyond CPU utilization.
	// A CPU utilization metric is always included automatically; do not duplicate it here.
	// Uses the standard Kubernetes MetricSpec from autoscaling/v2.
	// +kubebuilder:validation:MaxItems=100
	CustomMetrics []autoscalingv2.MetricSpec `json:"customMetrics,omitempty"`
}

// PDBSpec configures a PodDisruptionBudget for runtime pods.
// Admin nodes do not participate in PDB reconciliation; if set there, the
// field is ignored and a Warning event is emitted (see the node reconciler).
// A PodDisruptionBudget accepts exactly one of minAvailable/maxUnavailable.
// +kubebuilder:validation:XValidation:rule="(has(self.minAvailable) ? 1 : 0) + (has(self.maxUnavailable) ? 1 : 0) == 1",message="exactly one of minAvailable or maxUnavailable must be set"
type PDBSpec struct {
	// MinAvailable is the minimum number of pods that must remain available
	// during voluntary disruption. Accepts an integer (e.g., 2) or a
	// percentage string (e.g., "50%"). Matches upstream
	// policy/v1.PodDisruptionBudgetSpec.MinAvailable. Mutually exclusive with
	// maxUnavailable (set exactly one).
	// Pattern catches invalid string forms; CEL catches negative integers
	// (Pattern does not apply to the int variant of x-kubernetes-int-or-string).
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:Pattern=`^([0-9]+|[0-9]+%)$`
	// +kubebuilder:validation:XValidation:rule="!(type(self) == int && self < 0)",message="minAvailable must be non-negative"
	MinAvailable *intstr.IntOrString `json:"minAvailable,omitempty"`

	// MaxUnavailable is the maximum number of pods that may be unavailable
	// during voluntary disruption. Accepts an integer (e.g., 1) or a
	// percentage string (e.g., "50%"). Matches upstream
	// policy/v1.PodDisruptionBudgetSpec.MaxUnavailable. Mutually exclusive with
	// minAvailable (set exactly one).
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:Pattern=`^([0-9]+|[0-9]+%)$`
	// +kubebuilder:validation:XValidation:rule="!(type(self) == int && self < 0)",message="maxUnavailable must be non-negative"
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
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

	// ConfigType is the curity.io/config-type annotation value ("base", "license", "logging", "postCommitScript", or "env").
	// +kubebuilder:validation:Enum=base;license;logging;postCommitScript;env
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

	// Logs are the Curity log files to tail to stdout via sidecar containers (one
	// per stream); a non-empty list enables them, empty/omitted or Level: OFF disables.
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
	// URL is the full HTTP or HTTPS URL of the ZIP archive to fetch.
	// Both schemes are supported; use NetworkPolicy at the cluster level
	// to restrict plain-HTTP egress if your deployment requires it.
	// The pattern also rejects URLs with embedded credentials
	// (userinfo@host) — use the structured `auth` field instead — and
	// disallows whitespace or control characters anywhere in the URL.
	// The tail accepts `/path`, `?query`, or `#fragment` after the host,
	// so bare-host, query-only and fragment-only URLs all parse.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://[^/@\s]+([/?#][^\s]*)?$`
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
//
// SkipVerify is mutually exclusive with CA and ClientCert: the operator's
// curl invocation under SkipVerify=true is just ` -k ` (no --cacert, no
// --cert/--key), so combining them would silently drop the trust bundle
// and any client cert. The API rejects the combination instead of silently
// dropping it.
// +kubebuilder:validation:XValidation:rule="!self.skipVerify || (!has(self.ca) && !has(self.clientCert))",message="tls.skipVerify cannot be combined with tls.ca or tls.clientCert; skipVerify=true silently drops both, so the API rejects the combination"
type PackageTLSSpec struct {
	// Enabled gates the rest of this TLS block. When false (default),
	// the system trust store is used.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// SkipVerify disables certificate verification. Use only in
	// non-production environments. Mutually exclusive with CA and
	// ClientCert (enforced by CRD validation).
	// +kubebuilder:default=false
	SkipVerify bool `json:"skipVerify,omitempty"`

	// CA is a Secret holding a PEM bundle used as the trust root for
	// this download. Mutually exclusive with SkipVerify (enforced by
	// CRD validation).
	CA *PackageSecretKeyRef `json:"ca,omitempty"`

	// ClientCert is a Secret holding the cert (and matching private key)
	// for mutual TLS. Mutually exclusive with SkipVerify (enforced by
	// CRD validation).
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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// UsernameKey is the key in the Secret that holds the username.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	UsernameKey string `json:"usernameKey"`

	// PasswordKey is the key in the Secret that holds the password.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PasswordKey string `json:"passwordKey"`
}

// NetworkPolicySpec configures the operator-managed NetworkPolicy that
// restricts ingress to the cluster's admin node. Mirrors the upstream Helm
// chart's release-level policy: ingress is allowed from same-cluster runtime
// pods on the config and distributed-service ports, plus optionally from an
// API-gateway namespace to the admin-UI port. Setting this field (even empty)
// enables the policy; leaving it nil disables it.
type NetworkPolicySpec struct {
	// APIGatewayNamespace, when set and the admin UI is enabled, allows ingress
	// to the admin-UI port from pods in this namespace. Omit to skip the UI rule.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	APIGatewayNamespace string `json:"apiGatewayNamespace,omitempty"`
}

// ValueSource provides a string value either inline or from a Secret key.
// Exactly one of value/secretKeyRef must be set. A non-secret value belongs in
// value; secretKeyRef is for secrets like a JDBC password. (Unlike a raw env
// var there is no fieldRef/resourceFieldRef — pod metadata and resource
// quantities are meaningless for a connection setting.)
// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.secretKeyRef)",message="exactly one of value or secretKeyRef must be set"
type ValueSource struct {
	// Value is the literal value. Mutually exclusive with secretKeyRef.
	// +kubebuilder:validation:MaxLength=2048
	Value string `json:"value,omitempty"`

	// SecretKeyRef names the Secret and key holding the value. Mutually
	// exclusive with value.
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// JDBCConnection describes how the database-init Job obtains its JDBC
// connection settings. Each setting maps onto an environment variable read by
// idsvr: url -> JDBC_URL, username -> JDBC_USERNAME, password -> JDBC_PASSWORD.
//
// Settings may be provided two ways, in increasing precedence:
//  1. secretRef — a Secret whose JDBC_URL / JDBC_USERNAME / JDBC_PASSWORD keys
//     are loaded as env vars (envFrom, optional so missing keys are tolerated).
//  2. url/username/password — per-field inline value or secretKeyRef.
//     These are appended after envFrom, so an explicit field overrides the
//     matching key from secretRef.
//
// JDBC_URL must come from somewhere: if secretRef is unset, url is required.
// CEL enforces that a source is declared; whether a secretRef Secret actually
// carries a JDBC_URL key is verified by the controller at reconcile time.
// +kubebuilder:validation:XValidation:rule="has(self.secretRef) || has(self.url)",message="connection.url is required unless connection.secretRef is set (JDBC_URL must come from somewhere)"
type JDBCConnection struct {
	// SecretRef is the name of a Secret in the same namespace whose
	// JDBC_URL / JDBC_USERNAME / JDBC_PASSWORD keys are loaded into the Job's
	// container as environment variables. Use it to keep the whole connection
	// in one Secret. Missing keys are tolerated (envFrom optional).
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	SecretRef string `json:"secretRef,omitempty"`

	// URL sets JDBC_URL. Required unless secretRef is set; overrides a
	// JDBC_URL key from secretRef when both are present.
	URL *ValueSource `json:"url,omitempty"`

	// Username sets JDBC_USERNAME. Optional; overrides secretRef's JDBC_USERNAME.
	Username *ValueSource `json:"username,omitempty"`

	// Password sets JDBC_PASSWORD. Optional; overrides secretRef's JDBC_PASSWORD.
	// Prefer secretKeyRef over an inline value.
	Password *ValueSource `json:"password,omitempty"`
}

// DatabaseJobTemplate carries the standard Kubernetes Job/Pod knobs that may be
// configured on the database-init Job. Every field is optional; unset fields
// fall back to operator defaults (and, for image pull settings, to the
// referenced cluster). It is intentionally close to the pod-level fields on
// IdentityServerClusterSpec so the two read alike.
type DatabaseJobTemplate struct {
	// BackoffLimit is the number of retries before the Job is marked failed.
	// Defaults to 3.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// ActiveDeadlineSeconds bounds the Job's running time; it fails if it runs
	// longer. Unset means no deadline.
	// +kubebuilder:validation:Minimum=1
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// TTLSecondsAfterFinished lets Kubernetes garbage-collect the finished Job
	// (and its pods) after this many seconds. Unset means the Job is retained
	// until its owning IdentityServerDatabase is deleted.
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// ServiceAccountName runs the Job's pod under this ServiceAccount. Unset
	// uses the namespace default; the operator does not mount a token unless
	// automountServiceAccountToken is explicitly true.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// AutomountServiceAccountToken controls whether the SA token is mounted.
	// Defaults to false (the Job needs no Kubernetes API access).
	AutomountServiceAccountToken *bool `json:"automountServiceAccountToken,omitempty"`

	// Resources sets CPU/memory requests and limits on the idsvr container.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// SecurityContext sets the pod-level security context. Unset fields inherit
	// the operator defaults (runAsUser 10001, runAsGroup/fsGroup 10000).
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`

	// ContainerSecurityContext sets the security context of the idsvr container.
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// ImagePullPolicy overrides the pull policy of the idsvr container. Unset
	// defaults like Kubernetes (Always for :latest/untagged, else IfNotPresent).
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecret names a Secret for pulling the image. Unset falls back to
	// the referenced cluster's imagePullSecret.
	// +kubebuilder:validation:MaxLength=253
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// Env appends extra environment variables to the idsvr container, after the
	// JDBC_* variables. Names colliding with JDBC_URL/JDBC_USERNAME/JDBC_PASSWORD
	// are rejected at admission.
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:XValidation:rule="self.all(e, !(e.name in ['JDBC_URL','JDBC_USERNAME','JDBC_PASSWORD']))",message="env must not redefine JDBC_URL, JDBC_USERNAME or JDBC_PASSWORD; use spec.connection instead"
	Env []corev1.EnvVar `json:"env,omitempty"`

	// NodeSelector constrains the pod to nodes with matching labels.
	// +kubebuilder:validation:MaxProperties=100
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow the pod to schedule onto nodes with matching taints.
	// +kubebuilder:validation:MaxItems=100
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// TopologySpreadConstraints describe how the pod spreads across topology domains.
	// +kubebuilder:validation:MaxItems=32
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Affinity defines scheduling constraints for the pod.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// PriorityClassName sets the pod's PriorityClass.
	// +kubebuilder:validation:MaxLength=253
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// TerminationGracePeriodSeconds overrides the pod termination grace period.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// PodAnnotations are annotations applied to the Job's pod template.
	// +kubebuilder:validation:MaxProperties=100
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are labels applied to the Job's pod template. Operator-owned
	// keys (curity.io/cluster, curity.io/component, curity.io/database) are rejected.
	// +kubebuilder:validation:MaxProperties=100
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['curity.io/cluster', 'curity.io/component', 'curity.io/database']))",message="podLabels cannot include operator-owned curity.io/* keys: cluster, component, database"
	PodLabels map[string]string `json:"podLabels,omitempty"`
}
