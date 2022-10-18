//go:build !ignore_uncovered
// +build !ignore_uncovered

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Important: Run "make" to regenerate code after modifying this file

// +kubebuilder:object:root=true

// ClusterSubnetState is the Schema for the ClusterSubnetState API
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:resource:shortName=css
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Exhausted",type=string,JSONPath=`.status.exhausted`
// +kubebuilder:printcolumn:name="Updated",type=string,JSONPath=`.spec.timestamp`
type ClusterSubnetState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSubnetStateSpec   `json:"spec,omitempty"`
	Status ClusterSubnetStateStatus `json:"status,omitempty"`
}

// Scaler groups IP request params together
type Scaler struct {
	Batch  int64   `json:"batch,omitempty"`
	Buffer float64 `json:"buffer,omitempty"`
}

type ClusterSubnetStateSpec struct {
	Scaler Scaler `json:"scaler,omitempty"`
}

// ClusterSubnetStateStatus defines the observed state of ClusterSubnetState
type ClusterSubnetStateStatus struct {
	Exhausted bool   `json:"exhausted,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Scaler    Scaler `json:"scaler,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterSubnetStateList contains a list of ClusterSubnetState
type ClusterSubnetStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterSubnetState `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterSubnetState{}, &ClusterSubnetStateList{})
}
