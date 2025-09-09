package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
)

// DNSServiceConfigSpec defines the desired state of DNSServiceConfig
type DNSServiceConfigSpec struct {
	// TODO
}

// DNSServiceConfigStatus defines the observed state of DNSServiceConfig
type DNSServiceConfigStatus struct {
	commonapi.Status `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=platform"
// +kubebuilder:selectablefield:JSONPath=".spec.profile"
// +kubebuilder:printcolumn:JSONPath=`.status.phase`,name="Phase",type=string
// +kubebuilder:resource:scope=Cluster,shortName=dnscfg

// DNSServiceConfig is the Schema for the DNS PlatformService configuration API
type DNSServiceConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSServiceConfigSpec   `json:"spec,omitempty"`
	Status DNSServiceConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DNSServiceConfigList contains a list of DNSServiceConfig
type DNSServiceConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSServiceConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSServiceConfig{}, &DNSServiceConfigList{})
}
