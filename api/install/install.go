package install

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	fluxhelmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxsourcev1 "github.com/fluxcd/source-controller/api/v1"

	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	providerv1alpha1 "github.com/openmcp-project/openmcp-operator/api/provider/v1alpha1"

	dnsv1alpha1 "github.com/openmcp-project/platform-service-dns/api/dns/v1alpha1"
)

// InstallCRDAPIs installs the CRD APIs in the scheme.
// This is used for the init subcommand.
func InstallCRDAPIs(scheme *runtime.Scheme) *runtime.Scheme {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextv1.AddToScheme(scheme))

	return scheme
}

func InstallOperatorAPIsPlatform(scheme *runtime.Scheme) *runtime.Scheme {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dnsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clustersv1alpha1.AddToScheme(scheme))
	utilruntime.Must(providerv1alpha1.AddToScheme(scheme))
	utilruntime.Must(fluxsourcev1.AddToScheme(scheme))
	utilruntime.Must(fluxhelmv2.AddToScheme(scheme))

	return scheme
}
