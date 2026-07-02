package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IdentityServerDatabaseSpec defines the desired state of IdentityServerDatabase.
// It triggers a run-once Job that initializes the Curity Identity Server
// database schema (idsvr -I) against a JDBC database, using the same container
// image as the referenced IdentityServerCluster.
// +kubebuilder:object:generate=true
type IdentityServerDatabaseSpec struct {
	// IdentityServerClusterRef references the IdentityServerCluster (in the same
	// namespace) whose image the schema-management Job runs. The Job re-runs when
	// the resolved image, spec.connection, or spec.jobTemplate changes.
	// +kubebuilder:validation:Required
	IdentityServerClusterRef ObjectReference `json:"identityServerClusterRef"`

	// Connection configures the JDBC settings passed to idsvr as JDBC_URL,
	// JDBC_USERNAME and JDBC_PASSWORD. Values may be inline or sourced from a
	// Secret. JDBC_URL is required unless connection.secretRef provides it.
	// +kubebuilder:validation:Required
	Connection JDBCConnection `json:"connection"`

	// JobTemplate carries the standard Kubernetes Job/Pod settings for the
	// schema-management Job (resources, securityContext, containerSecurityContext,
	// serviceAccountName, scheduling, etc.). All fields are optional.
	JobTemplate *DatabaseJobTemplate `json:"jobTemplate,omitempty"`
}

// IdentityServerDatabaseStatus defines the observed state of IdentityServerDatabase.
// +kubebuilder:object:generate=true
type IdentityServerDatabaseStatus struct {
	// Conditions represent the latest available observations of the Job's state.
	// Condition types: Ready, Progressing, Complete, Failed.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// JobName is the name of the schema-management Job the controller manages.
	JobName string `json:"jobName,omitempty"`

	// ObservedImage is the Curity image the most recent Job ran with, resolved
	// from the referenced cluster. A change here re-triggers the Job.
	ObservedImage string `json:"observedImage,omitempty"`

	// LastCompletedHash is the trigger hash of the last Job that completed
	// successfully. Controller-internal bookkeeping, opaque to clients; it lets
	// the controller tell a Job cleaned up by ttlSecondsAfterFinished apart from
	// one that never ran, so a finished init is not re-run.
	LastCompletedHash string `json:"lastCompletedHash,omitempty"`

	// CompletionTime is when the Job finished successfully, if it has.
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=isdb
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.identityServerClusterRef.name"
// +kubebuilder:printcolumn:name="Complete",type="string",JSONPath=".status.conditions[?(@.type==\"Complete\")].status"
// +kubebuilder:printcolumn:name="Failed",type="string",JSONPath=".status.conditions[?(@.type==\"Failed\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// IdentityServerDatabase is the Schema for the identityserverdatabases API.
// It manages the Curity Identity Server database schema via a run-once Job.
type IdentityServerDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdentityServerDatabaseSpec   `json:"spec,omitempty"`
	Status IdentityServerDatabaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IdentityServerDatabaseList contains a list of IdentityServerDatabase.
type IdentityServerDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdentityServerDatabase `json:"items"`
}
