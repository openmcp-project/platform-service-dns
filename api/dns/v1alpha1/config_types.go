package v1alpha1

import (
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fluxv1 "github.com/fluxcd/source-controller/api/v1"

	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
)

// DNSServiceConfigSpec defines the desired state of DNSServiceConfig
type DNSServiceConfigSpec struct {
	// ExternalDNSSource is the source of the external-dns helm chart.
	ExternalDNSSource ExternalDNSSource `json:"externalDNSSource"`

	// HelmReleaseReconciliationInterval is the interval at which the HelmRelease for external-dns is reconciled.
	// The value can be overwritten for specific purposes using ExternalDNSForPurposes.
	// If not set, a default of 1h is used.
	// +optional
	HelmReleaseReconciliationInterval *metav1.Duration `json:"helmReleaseReconciliationInterval,omitempty"`

	// ExternalDNSForPurposes is a list of DNS configurations in combination with purpose selectors.
	// The first matching purpose selector will be applied to the Cluster.
	// If no selector matches, no configuration will be applied.
	// +optional
	ExternalDNSForPurposes []ExternalDNSPurposeConfig `json:"externalDNSForPurposes,omitempty"`
}

// ExternalDNSSource defines the source of the external-dns helm chart in form of a Flux source.
// Exactly one of 'HelmRepository', 'GitRepository' or 'OCIRepository' must be set.
// If 'copyAuthSecret' is set, the referenced source secret is copied into the namespace where the Flux resources are created with the specified target name.
// +kubebuilder:validation:XValidation:rule=`size(self.filter(property, (property != "copyAuthSecret") && (size(self[property]) > 0))) == 1`, message="Exactly one of 'helm', 'git', or 'oci' must be set"
type ExternalDNSSource struct {
	Helm           *fluxv1.HelmRepositorySpec `json:"helm,omitempty"`
	Git            *fluxv1.GitRepositorySpec  `json:"git,omitempty"`
	OCI            *fluxv1.OCIRepositorySpec  `json:"oci,omitempty"`
	CopyAuthSecret *SecretCopy                `json:"copyAuthSecret,omitempty"`
}

// SecretCopy defines the name of the secret to copy and the name of the copied secret.
// If target is nil or target.name is empty, the secret will be copied with the same name as the source secret.
type SecretCopy struct {
	Source commonapi.ObjectReference  `json:"source"`
	Target *commonapi.ObjectReference `json:"target"`
}

// ExternalDNSPurposeConfig holds a purpose selector and the DNS configuration to apply if the selector matches.
type ExternalDNSPurposeConfig struct {
	// Name is an optional name.
	// It can be set to more easily identify the configuration in logs and events.
	// +optional
	Name string `json:"name,omitempty"`

	// PurposeSelector is a selector to match against the list of purposes of a Cluster.
	// If not set, all Clusters are matched.
	// +optional
	PurposeSelector *PurposeSelector `json:"purposeSelector,omitempty"`

	// HelmReleaseReconciliationInterval is the interval at which the HelmRelease for external-dns is reconciled.
	// If not set, the global HelmReleaseReconciliationInterval is used.
	// +optional
	HelmReleaseReconciliationInterval *metav1.Duration `json:"helmReleaseReconciliationInterval,omitempty"`

	// HelmValues are the helm values to deploy external-dns with, if the purpose selector matches.
	// +kubebuilder:validation:Schemaless
	HelmValues *apiextensionsv1.JSON `json:"helmValues"`
}

// PurposeSelector is a selector to match against the list of purposes of a Cluster.
type PurposeSelector struct {
	PurposeSelectorRequirement `json:",inline"`
}

// PurposeSelectorRequirement is a selector to select purposes to apply the configuration to.
// The struct can be combined recursively using "and", "or" and "not" to build complex selectors.
// Exactly one of the fields must be set.
// If name is set, the selector matches if the Cluster's purposes contain the given name.
// If and is set, the selector matches if all of the contained selectors match.
// If or is set, the selector matches if any of the contained selectors match.
// If not is set, the selector matches if the contained selector does not match.
// +kubebuilder:validation:XValidation:rule=`size(self.filter(property, size(self[property]) > 0)) == 1`, message="Exactly one of 'and', 'or', 'not' or 'name' must be set"
type PurposeSelectorRequirement struct {
	And  []PurposeSelectorRequirement `json:"and,omitempty"`
	Or   []PurposeSelectorRequirement `json:"or,omitempty"`
	Not  *PurposeSelectorRequirement  `json:"not,omitempty"`
	Name string                       `json:"name,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:metadata:labels="openmcp.cloud/cluster=platform"
// +kubebuilder:resource:scope=Cluster,shortName=dnscfg

// DNSServiceConfig is the Schema for the DNS PlatformService configuration API
type DNSServiceConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DNSServiceConfigSpec `json:"spec,omitempty"`
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

// Matches returns true if the selector matches the given list of purposes.
func (ps *PurposeSelector) Matches(purposes []string) bool {
	return requirementMatches(&ps.PurposeSelectorRequirement, purposes, map[*PurposeSelectorRequirement]empty{})
}

type empty struct{}

func requirementMatches(r *PurposeSelectorRequirement, purposes []string, seenRequirements map[*PurposeSelectorRequirement]empty) bool {
	if r == nil {
		return true
	}
	if _, ok := seenRequirements[r]; ok {
		panic("circular reference in PurposeSelectorRequirement")
	}
	seenRequirements[r] = empty{}
	defer delete(seenRequirements, r)

	if r.Name != "" {
		return slices.Contains(purposes, r.Name)
	}
	if len(r.And) > 0 {
		for i := range r.And {
			if !requirementMatches(&r.And[i], purposes, seenRequirements) {
				return false
			}
		}
		return true
	}
	if len(r.Or) > 0 {
		for i := range r.Or {
			if requirementMatches(&r.Or[i], purposes, seenRequirements) {
				return true
			}
		}
		return false
	}
	if r.Not != nil {
		return !requirementMatches(r.Not, purposes, seenRequirements)
	}
	return false
}
