package cluster

import (
	"context"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	ctrlutils "github.com/openmcp-project/controller-utils/pkg/controller"
	errutils "github.com/openmcp-project/controller-utils/pkg/errors"
	"github.com/openmcp-project/controller-utils/pkg/logging"

	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	clusterconst "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1/constants"
	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	openmcpconst "github.com/openmcp-project/openmcp-operator/api/constants"
	accesslib "github.com/openmcp-project/openmcp-operator/lib/clusteraccess"

	dnsv1alpha1 "github.com/openmcp-project/platform-service-dns/api/dns/v1alpha1"
)

const ControllerName = "DNSCluster"

type ClusterReconciler struct {
	PlatformCluster *clusters.Cluster
	Config          *dnsv1alpha1.DNSServiceConfig
	eventRecorder   record.EventRecorder
	ProviderName    string
}

func NewClusterReconciler(platformCluster *clusters.Cluster, recorder record.EventRecorder, cfg *dnsv1alpha1.DNSServiceConfig, providerName string) *ClusterReconciler {
	return &ClusterReconciler{
		PlatformCluster: platformCluster,
		eventRecorder:   recorder,
		ProviderName:    providerName,
	}
}

var _ reconcile.Reconciler = &ClusterReconciler{}

type ReconcileResult struct {
	Result         reconcile.Result
	ReconcileError errutils.ReasonableError
	Config         *dnsv1alpha1.ExternalDNSPurposeConfig
	ConfigIndex    int
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logging.FromContextOrPanic(ctx).WithName(ControllerName)
	ctx = logging.NewContext(ctx, log)
	log.Info("Starting reconcile")
	rr := r.reconcile(ctx, req)

	// no status update, because the Cluster resource doesn't have status fields for DNS configuration
	// instead, output events for significant changes
	// TODO

	return rr.Result, rr.ReconcileError
}

func (r *ClusterReconciler) reconcile(ctx context.Context, req reconcile.Request) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// get Cluster resource
	c := &clustersv1alpha1.Cluster{}
	if err := r.PlatformCluster.Client().Get(ctx, req.NamespacedName, c); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Resource not found")
			return ReconcileResult{}
		}
		return ReconcileResult{ReconcileError: errutils.WithReason(fmt.Errorf("unable to get resource '%s' from cluster: %w", req.String(), err), clusterconst.ReasonPlatformClusterInteractionProblem)}
	}

	// handle operation annotation
	if c.GetAnnotations() != nil {
		op, ok := c.GetAnnotations()[dnsv1alpha1.OperationAnnotation]
		if !ok {
			// only evaluate the generic operation annotation if no DNS-specific one is set
			op, ok = c.GetAnnotations()[openmcpconst.OperationAnnotation]
		}
		if ok {
			switch op {
			case openmcpconst.OperationAnnotationValueIgnore:
				log.Info("Ignoring resource due to ignore operation annotation")
				return ReconcileResult{}
			case openmcpconst.OperationAnnotationValueReconcile:
				log.Debug("Removing reconcile operation annotation from resource")
				if err := ctrlutils.EnsureAnnotation(ctx, r.PlatformCluster.Client(), c, openmcpconst.OperationAnnotation, "", true, ctrlutils.DELETE); err != nil {
					return ReconcileResult{ReconcileError: errutils.WithReason(fmt.Errorf("error removing operation annotation: %w", err), clusterconst.ReasonPlatformClusterInteractionProblem)}
				}
			}
		}
	}

	rr := ReconcileResult{}

	// iterate over configurations with purpose selectors and choose the first matching one
	for i, cfg := range r.Config.Spec.ExternalDNSForPurposes {
		if cfg.PurposeSelector == nil || cfg.PurposeSelector.Matches(c.Spec.Purposes) {
			log.Info("Found configuration with matching purpose selector", "configName", cfg.Name, "configIndex", i)
			rr.Config = &r.Config.Spec.ExternalDNSForPurposes[i]
			rr.ConfigIndex = i
			break
		}
	}
	if rr.Config == nil {
		log.Info("No configuration with matching purpose selector found")
		rr.ConfigIndex = -1
		return rr
	}

	// get access to the Cluster
	accessMgr := accesslib.NewClusterAccessManager(r.PlatformCluster.Client(), strings.ToLower(ControllerName), c.Namespace)
	localName := c.Name
	if len(ControllerName+"--"+localName) > 63 {
		localName = ctrlutils.K8sNameUUIDUnsafe(c.Name)
	}
	access, ar, err := accessMgr.WaitForClusterAccess(ctx, localName, nil, &commonapi.ObjectReference{
		Name:      c.Name,
		Namespace: c.Namespace,
	}, accesslib.ReferenceToCluster, []clustersv1alpha1.PermissionsRequest{
		{
			Rules: []rbacv1.PolicyRule{ // TODO: restrict permissions
				{
					APIGroups: []string{"*"},
					Resources: []string{"*"},
					Verbs:     []string{"*"},
				},
			},
		},
	})
	if err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting access to Cluster '%s/%s': %w", c.Namespace, c.Name, err), clusterconst.ReasonInternalError)
		return rr
	}

	// inject labels into AccessRequest
	if err := ctrlutils.EnsureLabel(ctx, r.PlatformCluster.Client(), ar, openmcpconst.ManagedByLabel, ControllerName, true, ctrlutils.OVERWRITE); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error ensuring labels on AccessRequest: %w", err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}

	// TODO: deploy external-dns with selected config onto Cluster
	// access to Cluster: access.Client()
	// selected config: rr.Config
	return rr
}
