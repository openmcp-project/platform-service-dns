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

	// SecretsToCopy specifies secrets that should be copied to either the cluster's namespace on the platform cluster,
	// or the namespace on the target cluster where the helm chart will be installed into.
	// +optional
	SecretsToCopy *SecretsToCopy `json:"secretsToCopy,omitempty"`

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

type SecretsToCopy struct {
	// ToPlatformCluster lists secrets from the provider namespace that should be copied into the cluster's namespace on the platform cluster.
	// This is useful e.g. for pull secrets for the helm chart registry.
	// +optional
	ToPlatformCluster []SecretCopy `json:"toPlatformCluster,omitempty"`
	// ToTargetCluster lists secrets from the provider namespace that should be copied into the cluster's namespace on the target cluster.
	// This allows propagating secrets that are required by the helm chart to the target cluster.
	// +optional
	ToTargetCluster []SecretCopy `json:"toTargetCluster,omitempty"`
}

// ExternalDNSSource defines the source of the external-dns helm chart in form of a Flux source.
// Exactly one of 'HelmRepository', 'GitRepository' or 'OCIRepository' must be set.
// If 'copyAuthSecret' is set, the referenced source secret is copied into the namespace where the Flux resources are created with the specified target name.
// +kubebuilder:validation:ExactlyOneOf=helm;git;oci
type ExternalDNSSource struct {
	// ChartName specifies the name of the external-dns chart.
	// Depending on the source, this can also be a relative path within the repository.
	// When using a source that needs a version (helm or oci), append the version to the chart name using '@', e.g. 'external-dns@1.10.0' or omit for latest version.
	// +kubebuilder:validation:MinLength=1
	ChartName string                     `json:"chartName"`
	Helm      *fluxv1.HelmRepositorySpec `json:"helm,omitempty"`
	Git       *fluxv1.GitRepositorySpec  `json:"git,omitempty"`
	OCI       *fluxv1.OCIRepositorySpec  `json:"oci,omitempty"`
}

// SecretCopy defines the name of the secret to copy and the name of the copied secret.
// If target is nil or target.name is empty, the secret will be copied with the same name as the source secret.
type SecretCopy struct {
	// Source references the source secret to copy.
	// It has to be in the namespace the provider pod is running in.
	Source commonapi.LocalObjectReference `json:"source"`
	// Target is the name of the copied secret.
	// If not set, the secret will be copied with the same name as the source secret.
	// +optional
	Target *commonapi.LocalObjectReference `json:"target"`
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
	// There are a few special strings which will be replaced before creating the HelmRelease:
	// - <provider.name> will be replaced with the provider name resource.
	// - <provider.namespace> will be replaced with the namespace that hosts the platform service.
	// - <environment> will be replaced with the environment name of the operator.
	// - <cluster.name> will be replaced with the name of the reconciled Cluster.
	// - <cluster.namespace> will be replaced with the namespace of the reconciled Cluster.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	HelmValues *apiextensionsv1.JSON `json:"helmValues"`
}

// PurposeSelector is a selector to match against the list of purposes of a Cluster.
type PurposeSelector struct {
	PurposeSelectorRequirement `json:",inline"`
}

// PurposeSelectorRequirement is a selector to select purposes to apply the configuration to.
// The struct can be combined recursively using "and", "or" and "not" to build complex selectors.
// Exactly one of the fields must be set, otherwise only one of them is evaluated in the order: name, not, and, or.
// If name is set, the selector matches if the Cluster's purposes contain the given name.
// If and is set, the selector matches if all of the contained selectors match.
// If or is set, the selector matches if any of the contained selectors match.
// If not is set, the selector matches if the contained selector does not match.
type PurposeSelectorRequirement struct {
	// +kubebuilder:validation:items:Type=object
	// +optional
	And []PurposeSelectorRequirement `json:"and,omitempty"`
	// +kubebuilder:validation:items:Type=object
	// +optional
	Or []PurposeSelectorRequirement `json:"or,omitempty"`
	// +kubebuilder:validation:Type=object
	// +optional
	Not *PurposeSelectorRequirement `json:"not,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
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
	if ps == nil {
		return true
	}
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
	if r.Not != nil {
		return !requirementMatches(r.Not, purposes, seenRequirements)
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
	return true
}
