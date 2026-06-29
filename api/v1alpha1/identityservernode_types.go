package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentityServerNodeSpec defines the desired state of IdentityServerNode.
// Each node creates a Deployment and Service for a Curity Identity Server instance.
// Admin nodes are single-active by Curity design: replicas cannot be set and
// autoscaling must not be enabled — both enforced at admission.
// +kubebuilder:object:generate=true
// +kubebuilder:validation:XValidation:rule="self.type != 'admin' || !has(self.replicas)",message="replicas cannot be set on admin-type nodes (Curity admin is always single-active with 1 replica)"
// +kubebuilder:validation:XValidation:rule="self.type != 'admin' || !has(self.autoscaling) || !self.autoscaling.enabled",message="autoscaling cannot be enabled on admin-type nodes (Curity admin is single-active)"
// +kubebuilder:validation:XValidation:rule="self.type == 'admin' || !has(self.service) || (!has(self.service.distributedServicePort) && !has(self.service.uiPort))",message="service.distributedServicePort and service.uiPort are only valid on admin-type nodes"
// +kubebuilder:validation:XValidation:rule="self.type == 'admin' || !has(self.skipInstall)",message="skipInstall can only be set on admin-type nodes"
type IdentityServerNodeSpec struct {
	// Type determines whether this is an admin or runtime node.
	// Only one admin node is allowed per cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=admin;runtime
	Type NodeType `json:"type"`

	// Role is a unique identifier for this node within the cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	Role string `json:"role"`

	// UI configures the admin UI. Only applicable for admin-type nodes.
	UI *UISpec `json:"ui,omitempty"`

	// SkipInstall passes SKIP_INSTALL=1 to the admin container so it boots
	// without running first-run setup. Admin-only (CEL-enforced).
	SkipInstall *bool `json:"skipInstall,omitempty"`

	// IdentityServerClusterRef references the IdentityServerCluster managing this node.
	// +kubebuilder:validation:Required
	IdentityServerClusterRef ObjectReference `json:"identityServerClusterRef"`

	// Replicas is the number of pods (Deployment replicas) for runtime nodes.
	// Must not be set on admin nodes; the admin is always 1 (CEL-enforced).
	// Omitted runtime nodes default to 1 in the reconciler.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	Replicas *int32 `json:"replicas,omitempty"`

	// PodAnnotations are annotations applied to pods.
	// Merged with cluster-level annotations; node values win on conflict.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are labels applied to pods. Merged with cluster-level
	// labels; node values win on conflict. Operator-owned keys are
	// rejected at admission (see CEL message for the list).
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['curity.io/owned-by', 'curity.io/cluster', 'curity.io/role']))",message="podLabels cannot include operator-owned curity.io/* keys: owned-by, cluster, role"
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Probes configures liveness and readiness probes.
	// Overrides cluster-level probes when set.
	Probes *ProbeSpec `json:"probes,omitempty"`

	// Service tunes the node's Kubernetes Service. Optional — the operator always
	// creates the Service (inter-node DNS depends on it) with the full set of
	// role ports at their defaults; the fields override the Service type and the
	// individual port numbers.
	Service *ServiceSpec `json:"service,omitempty"`

	// EnvironmentVariables are additional environment variables passed to the
	// Curity container. Uses standard Kubernetes env var format with support
	// for valueFrom (secretKeyRef, configMapKeyRef).
	// +kubebuilder:validation:MaxItems=1000
	EnvironmentVariables []corev1.EnvVar `json:"environmentVariables,omitempty"`

	// Logging configures logging behavior.
	Logging *LoggingSpec `json:"logging,omitempty"`

	// Autoscaling configures horizontal pod autoscaling.
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// PodDisruptionBudget configures the minimum available pods during disruptions.
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`

	// CommonPodConfig holds the shared pod/scheduling knobs; node values override the cluster's.
	CommonPodConfig `json:",inline"`

	// InitContainers are user-defined init containers run after the operator's
	// package-fetcher init containers. They cannot override operator-managed
	// containers; a name colliding with an operator container is rejected.
	// Overrides the cluster-level value when set; set to [] to inherit none.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(c, c.name != 'curity')",message="container name 'curity' is reserved by the operator"
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// ExtraContainers are user-defined sidecar containers run alongside the
	// Curity container and after the operator's log sidecars. Same naming
	// rules as initContainers. Overrides the cluster value when set; [] to inherit none.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(c, c.name != 'curity')",message="container name 'curity' is reserved by the operator"
	ExtraContainers []corev1.Container `json:"extraContainers,omitempty"`
}

// IdentityServerNodeStatus defines the observed state of IdentityServerNode.
// Fields mirror Deployment status for ArgoCD health assessment.
// +kubebuilder:object:generate=true
type IdentityServerNodeStatus struct {
	// Conditions represent the latest available observations of the node's state.
	// Condition types: Ready, Available, Progressing, Degraded.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the desired number of pod replicas.
	Replicas int32 `json:"replicas,omitempty"`

	// UpdatedReplicas is the number of pods with the current spec applied.
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// ReadyReplicas is the number of pods that have passed the readiness probe.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of pods available to serve traffic.
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// UnavailableReplicas is the number of pods that are not yet available.
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`

	// DeploymentName is the name of the Deployment created by this node.
	DeploymentName string `json:"deploymentName,omitempty"`

	// ServiceName is the name of the Service created by this node.
	ServiceName string `json:"serviceName,omitempty"`

	// AppliedManagedResources lists the managed ConfigMaps and Secrets
	// (curity.io/managed=true) mounted on this node.
	AppliedManagedResources []AppliedManagedResource `json:"appliedManagedResources,omitempty"`

	// LastObservedPackageSecretsHash is a hash of the resourceVersions of
	// every Secret referenced by spec.packages. Used by the Secret-watch
	// recovery action to fire pod-deletion only once per Secret change
	// (instead of on every reconcile while the condition is in a
	// recoverable failure state — that would create an infinite
	// delete/recreate loop when the user's Secret value is still wrong).
	// Internal bookkeeping; not part of the user-facing API.
	LastObservedPackageSecretsHash string `json:"lastObservedPackageSecretsHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=isn
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Role",type="string",JSONPath=".spec.role"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.availableReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// IdentityServerNode is the Schema for the identityservernodes API.
// It defines a single Curity Identity Server node deployment.
type IdentityServerNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdentityServerNodeSpec   `json:"spec,omitempty"`
	Status IdentityServerNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IdentityServerNodeList contains a list of IdentityServerNode.
type IdentityServerNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdentityServerNode `json:"items"`
}
