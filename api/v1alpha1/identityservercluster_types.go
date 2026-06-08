package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentityServerClusterSpec defines the desired state of IdentityServerCluster.
// It holds cluster-wide shared configuration that IdentityServerNode resources inherit.
// +kubebuilder:object:generate=true
type IdentityServerClusterSpec struct {
	// Version of the Curity Identity Server to deploy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version"`

	// Image overrides the default container image (e.g., for private mirrors).
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`

	// ImagePullSecret is the name of a Kubernetes Secret for pulling the container image.
	// +kubebuilder:validation:MaxLength=253
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// AdminCredentials references a Secret containing ADMIN_PASSWORD and
	// CONFIG_ENCRYPTION_KEY. The operator creates the secret with random
	// values if it does not exist.
	AdminCredentials *CredentialsSource `json:"adminCredentials,omitempty"`

	// Packages declares remote ZIP archives the operator downloads at
	// pod start and unpacks into every Curity container of every node
	// referencing this cluster. Each entry produces one init container
	// per pod. Removing an entry removes its init container and mount
	// on the next reconcile (which triggers a rolling restart).
	// MountPaths must be unique across the list — duplicates are
	// rejected at admission via CEL.
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:XValidation:rule="self.all(p1, self.exists_one(p2, p2.mountPath == p1.mountPath))",message="each package mountPath must be unique"
	Packages []PackageSpec `json:"packages,omitempty"`

	// Logging configures logging behavior.
	Logging *LoggingSpec `json:"logging,omitempty"`

	// PodAnnotations are annotations applied to all pods managed by nodes
	// referencing this cluster. Node-level annotations merge on top.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are labels applied to all pods managed by nodes referencing
	// this cluster. Node-level labels merge on top. Operator-owned keys are
	// rejected at admission (see CEL message for the list).
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['curity.io/owned-by', 'curity.io/cluster', 'curity.io/role']))",message="podLabels cannot include operator-owned curity.io/* keys: owned-by, cluster, role"
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Resources defines default CPU and memory requests/limits for pods.
	// Node-level resources override these.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Probes configures default liveness and readiness probes for pods.
	// Node-level probes override these.
	Probes *ProbeSpec `json:"probes,omitempty"`

	// Autoscaling configures horizontal pod autoscaling defaults.
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// PodDisruptionBudget configures the minimum available pods during disruptions.
	PodDisruptionBudget *PDBSpec `json:"podDisruptionBudget,omitempty"`

	// NodeSelector constrains pods to nodes with matching labels.
	// +kubebuilder:validation:MaxProperties=100
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow pods to schedule onto nodes with matching taints.
	// +kubebuilder:validation:MaxItems=100
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// TopologySpreadConstraints describe how pods should be spread across topology domains.
	// +kubebuilder:validation:MaxItems=32
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Affinity defines scheduling constraints for pods.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// InitContainers are user-defined init containers run after the operator's
	// package-fetcher init containers. They cannot override operator-managed
	// containers; a name colliding with an operator container is rejected.
	// Node-level value replaces this entirely.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(c, c.name != 'curity')",message="container name 'curity' is reserved by the operator"
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// ExtraContainers are user-defined sidecar containers run alongside the
	// Curity container and after the operator's log sidecars. Same naming
	// rules as initContainers. Node-level value replaces this entirely.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(c, c.name != 'curity')",message="container name 'curity' is reserved by the operator"
	ExtraContainers []corev1.Container `json:"extraContainers,omitempty"`

	// SecurityContext overrides the pod-level security context. Unset fields
	// inherit the operator defaults (runAsUser 10001, runAsGroup/fsGroup 10000).
	// Node-level value replaces this entirely.
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`

	// ContainerSecurityContext sets the security context of the main Curity
	// container. Node-level value replaces this entirely.
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// TerminationGracePeriodSeconds overrides the pod termination grace period
	// (default 30). Raise it (60-180 typical) for Curity's JVM shutdown under load.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// ImagePullPolicy overrides the pull policy of the main Curity container.
	// When unset it defaults like Kubernetes: Always for a :latest or untagged
	// image, otherwise IfNotPresent.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// NetworkPolicy, when set, makes the operator manage a NetworkPolicy that
	// restricts ingress to the admin node (mirrors the Helm chart's
	// release-level policy). Cluster-scoped: it protects the cluster's admin
	// node; runtime nodes are unaffected. Note: a NetworkPolicy only takes
	// effect on a cluster whose CNI implements NetworkPolicy enforcement;
	// otherwise it is silently ignored by Kubernetes.
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

// IdentityServerClusterStatus defines the observed state of IdentityServerCluster.
// +kubebuilder:object:generate=true
type IdentityServerClusterStatus struct {
	// Conditions represent the latest available observations of the cluster's state.
	// Condition types: Ready, Available, Progressing, Degraded.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// ArgoCD compares this to metadata.generation to detect stale status.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// NodeCount is the total number of IdentityServerNodes referencing this cluster.
	NodeCount int32 `json:"nodeCount,omitempty"`

	// ReadyNodes is the number of nodes with Ready=True condition.
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// Version is the currently observed Curity version from the spec.
	Version string `json:"version,omitempty"`

	// ClusterConfigSecretName is the name of the Secret containing cluster.xml.
	ClusterConfigSecretName string `json:"clusterConfigSecretName,omitempty"`

	// ManagedResourceIssueCount mirrors len(ManagedResourceIssues) for the
	// printer column (kubebuilder JSONPath has no length()). Writers must
	// keep both fields in sync in the same Status.Update.
	ManagedResourceIssueCount int `json:"managedResourceIssueCount,omitempty"`

	// ManagedResourceIssues lists problems on managed ConfigMaps/Secrets
	// (curity.io/managed=true) that affect this cluster's mount behavior.
	// Each entry is also emitted as a Warning Event on the resource itself.
	ManagedResourceIssues []ManagedResourceIssue `json:"managedResourceIssues,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=isc
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Nodes",type="integer",JSONPath=".status.nodeCount"
// +kubebuilder:printcolumn:name="Ready Nodes",type="integer",JSONPath=".status.readyNodes"
// +kubebuilder:printcolumn:name="Issues",type="integer",JSONPath=".status.managedResourceIssueCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="!self.metadata.name.contains('.')",message="cluster name must not contain '.'; the operator derives DNS-1035 Service names from it, and dots are not valid in DNS-1035 labels"

// IdentityServerCluster is the Schema for the identityserverclusters API.
// It defines cluster-wide shared configuration for Curity Identity Server deployments.
type IdentityServerCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdentityServerClusterSpec   `json:"spec,omitempty"`
	Status IdentityServerClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IdentityServerClusterList contains a list of IdentityServerCluster.
type IdentityServerClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdentityServerCluster `json:"items"`
}
