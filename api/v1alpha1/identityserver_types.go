package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type IdentityServerSpec struct {
	Replicas *int32 `json:"replicas,omitempty"`
}

type IdentityServerStatus struct {
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	ReadyReplicas int32              `json:"readyReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=idsvr
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type IdentityServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdentityServerSpec   `json:"spec,omitempty"`
	Status IdentityServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IdentityServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdentityServer `json:"items"`
}
