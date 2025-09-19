package cluster

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxhelmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	fluxsourcev1 "github.com/fluxcd/source-controller/api/v1"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/controller-utils/pkg/collections"
	maputils "github.com/openmcp-project/controller-utils/pkg/collections/maps"
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
const defaultRequeueAfterDuration = 30 * time.Second

type ClusterReconciler struct {
	PlatformCluster   *clusters.Cluster
	eventRecorder     record.EventRecorder
	ProviderName      string
	ProviderNamespace string
	KnownClusters     map[types.NamespacedName]struct{}
	KnownClustersLock *sync.RWMutex
	FakeClientMapping map[string]client.Client // this must be nil except for unit tests
}

func NewClusterReconciler(platformCluster *clusters.Cluster, recorder record.EventRecorder, providerName, providerNamespace string) *ClusterReconciler {
	return &ClusterReconciler{
		PlatformCluster:   platformCluster,
		eventRecorder:     recorder,
		ProviderName:      providerName,
		ProviderNamespace: providerNamespace,
		KnownClusters:     map[types.NamespacedName]struct{}{},
		KnownClustersLock: &sync.RWMutex{},
	}
}

var _ reconcile.Reconciler = &ClusterReconciler{}

type ReconcileResult struct {
	// Result is the result to return from the Reconcile function.
	Result reconcile.Result
	// ReconcileError is the error to return from the Reconcile function, if any occurred.
	ReconcileError errutils.ReasonableError
	// Config is the selected configuration that was applied to the Cluster, if it could be determined.
	Config *dnsv1alpha1.ExternalDNSPurposeConfig
	// ConfigIndex is the index of the selected configuration in the DNSServiceConfig, or -1 if no configuration was selected.
	ConfigIndex int
	// SourceKind is the kind of Flux source that was deployed (HelmRepository, GitRepository, OCIRepository), if any.
	SourceKind string
	// AccessRequest is the AccessRequest that provides access to the Cluster, if access was successfully obtained.
	AccessRequest *clustersv1alpha1.AccessRequest
	// Message is an optional message to be printed in the generated event.
	Message string
	// ProviderConfig is the complete provider configuration.
	ProviderConfig *dnsv1alpha1.DNSServiceConfig
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logging.FromContextOrPanic(ctx).WithName(ControllerName)
	ctx = logging.NewContext(ctx, log)
	log.Info("Starting reconcile")

	// get Cluster resource
	c := &clustersv1alpha1.Cluster{}
	if err := r.PlatformCluster.Client().Get(ctx, req.NamespacedName, c); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Resource not found")
			r.removeKnownClusterRaw(req.Name, req.Namespace)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("unable to get resource '%s' from cluster: %w", req.String(), err)
	}

	rr := r.reconcile(ctx, c)

	// no status update, because the Cluster resource doesn't have status fields for DNS configuration
	// instead, output events for significant changes and errors
	if r.eventRecorder != nil {
		if rr.ReconcileError != nil {
			r.eventRecorder.Event(c, corev1.EventTypeWarning, rr.ReconcileError.Reason(), rr.ReconcileError.Error())
		} else if rr.Message != "" {
			r.eventRecorder.Event(c, corev1.EventTypeNormal, "Reconciled", rr.Message)
		}
	}

	return log.LogRequeue(rr.Result), rr.ReconcileError
}

func (r *ClusterReconciler) reconcile(ctx context.Context, c *clustersv1alpha1.Cluster) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

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
	expectedLabels := map[string]string{
		openmcpconst.ManagedByLabel:      fmt.Sprintf("%s.%s", r.ProviderName, ControllerName),
		openmcpconst.ManagedPurposeLabel: c.Name,
	}

	// load DNSServiceConfig resource
	rr.ProviderConfig = &dnsv1alpha1.DNSServiceConfig{}
	rr.ProviderConfig.Name = r.ProviderName
	rr.ProviderConfig.Namespace = r.ProviderNamespace
	if err := r.PlatformCluster.Client().Get(ctx, client.ObjectKeyFromObject(rr.ProviderConfig), rr.ProviderConfig); err != nil {
		if apierrors.IsNotFound(err) {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("DNSServiceConfig '%s/%s' not found", rr.ProviderConfig.Namespace, rr.ProviderConfig.Name), clusterconst.ReasonConfigurationProblem)
		} else {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting DNSServiceConfig '%s/%s': %w", rr.ProviderConfig.Namespace, rr.ProviderConfig.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		}
		return rr
	}

	// iterate over configurations with purpose selectors and choose the first matching one
	for i, cfg := range rr.ProviderConfig.Spec.ExternalDNSForPurposes {
		if cfg.PurposeSelector.Matches(c.Spec.Purposes) {
			log.Info("Found configuration with matching purpose selector", "configName", cfg.Name, "configIndex", i)
			rr.Config = &rr.ProviderConfig.Spec.ExternalDNSForPurposes[i]
			rr.ConfigIndex = i
			break
		}
	}
	if rr.Config == nil {
		log.Info("No configuration with matching purpose selector found")
		rr.ConfigIndex = -1
	}

	if c.DeletionTimestamp.IsZero() && rr.Config != nil {
		// CREATE/UPDATE
		rr = r.handleCreateOrUpdate(ctx, c, expectedLabels, rr)
	} else {
		// DELETE
		rr = r.handleDelete(ctx, c, expectedLabels, rr)
	}

	return rr
}

func (r *ClusterReconciler) handleCreateOrUpdate(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)
	log.Info("Creating or updating DNS configuration for Cluster")

	// add finalizer to Cluster if not present
	old := c.DeepCopy()
	if controllerutil.AddFinalizer(c, dnsv1alpha1.ExternalDNSFinalizerOnCluster) {
		log.Info("Adding finalizer to Cluster", "finalizer", dnsv1alpha1.ExternalDNSFinalizerOnCluster)
		if err := r.PlatformCluster.Client().Patch(ctx, c, client.MergeFrom(old)); err != nil {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error adding finalizer to Cluster '%s/%s': %w", c.Namespace, c.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}
	}
	r.addKnownCluster(c)

	log.Info("Creating or updating AccessRequest to get access to Cluster")
	ar := &clustersv1alpha1.AccessRequest{}
	ar.SetName(accesslib.StableRequestNameFromLocalName(ControllerName, c.Name))
	ar.SetNamespace(c.Namespace)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.PlatformCluster.Client(), ar, func() error {
		if err := controllerutil.SetOwnerReference(c, ar, r.PlatformCluster.Scheme()); err != nil {
			return fmt.Errorf("error setting owner reference: %w", err)
		}
		ar.Labels = maputils.Merge(ar.Labels, expectedLabels)
		ar.Spec.ClusterRef = &commonapi.ObjectReference{
			Name:      c.Name,
			Namespace: c.Namespace,
		}
		ar.Spec.Token = &clustersv1alpha1.TokenConfig{
			Permissions: []clustersv1alpha1.PermissionsRequest{
				{
					Rules: []rbacv1.PolicyRule{ // TODO: restrict permissions
						{
							APIGroups: []string{"*"},
							Resources: []string{"*"},
							Verbs:     []string{"*"},
						},
					},
				},
			},
		}
		return nil
	}); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error creating or updating AccessRequest '%s/%s': %w", ar.Namespace, ar.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	if err := r.PlatformCluster.Client().Get(ctx, client.ObjectKeyFromObject(ar), ar); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting AccessRequest '%s/%s': %w", ar.Namespace, ar.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	if ar.Status.IsDenied() {
		rr.Message = fmt.Sprintf("AccessRequest '%s/%s' was denied, unable to proceed with deploying DNS configuration", ar.Namespace, ar.Name)
		return rr
	}
	if !ar.Status.IsGranted() {
		rr.Message = fmt.Sprintf("AccessRequest '%s/%s' is not yet granted, waiting for access to be granted", ar.Namespace, ar.Name)
		rr.Result.RequeueAfter = defaultRequeueAfterDuration
		return rr
	}
	rr.AccessRequest = ar

	rr = r.deployAuthSecret(ctx, c, expectedLabels, rr)
	if rr.ReconcileError != nil || rr.Result.RequeueAfter > 0 {
		return rr
	}

	rr = r.deployHelmChartSource(ctx, c, expectedLabels, rr)
	if rr.ReconcileError != nil || rr.Result.RequeueAfter > 0 {
		return rr
	}

	rr = r.deployHelmRelease(ctx, c, expectedLabels, rr)
	if rr.ReconcileError != nil || rr.Result.RequeueAfter > 0 {
		return rr
	}

	rr.Message = "Successfully triggered deployment of external-dns on Cluster"
	return rr
}

func (r *ClusterReconciler) handleDelete(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)
	// check if the Cluster has a finalizer, otherwise we don't have to do anything
	if !slices.Contains(c.Finalizers, dnsv1alpha1.ExternalDNSFinalizerOnCluster) {
		log.Debug("Cluster does not have finalizer, no cleanup required", "finalizer", dnsv1alpha1.ExternalDNSFinalizerOnCluster)
		r.removeKnownCluster(c)
		return rr
	}

	log.Info("Cleaning up DNS configuration for Cluster, either because it is being deleted or no configuration matches anymore")

	rr = r.undeployHelmRelease(ctx, c, expectedLabels, rr)
	if rr.ReconcileError != nil || rr.Result.RequeueAfter > 0 {
		return rr
	}

	rr = r.undeployHelmChartSource(ctx, c, expectedLabels, rr)
	if rr.ReconcileError != nil || rr.Result.RequeueAfter > 0 {
		return rr
	}

	rr = r.undeployAuthSecret(ctx, c, expectedLabels, rr)
	if rr.ReconcileError != nil || rr.Result.RequeueAfter > 0 {
		return rr
	}

	// delete AccessRequest
	ar := &clustersv1alpha1.AccessRequest{}
	ar.Name = accesslib.StableRequestNameFromLocalName(strings.ToLower(ControllerName), c.Name)
	ar.Namespace = c.Namespace
	if err := r.PlatformCluster.Client().Delete(ctx, ar); err != nil {
		if !apierrors.IsNotFound(err) {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error deleting AccessRequest '%s/%s': %w", ar.Namespace, ar.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}
	}

	// remove finalizer from Cluster
	old := c.DeepCopy()
	if controllerutil.RemoveFinalizer(c, dnsv1alpha1.ExternalDNSFinalizerOnCluster) {
		log.Info("Removing finalizer from Cluster", "finalizer", dnsv1alpha1.ExternalDNSFinalizerOnCluster)
		if err := r.PlatformCluster.Client().Patch(ctx, c, client.MergeFrom(old)); err != nil {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error removing finalizer from Cluster '%s/%s': %w", c.Namespace, c.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}
	}
	r.removeKnownCluster(c)

	rr.Message = "Successfully removed external-dns from Cluster"
	return rr
}

// deployAuthSecret copies the auth secret (for access to the helm chart source) into the Cluster namespace if configured.
func (r *ClusterReconciler) deployAuthSecret(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// copy secret if configured
	if rr.ProviderConfig.Spec.ExternalDNSSource.CopyAuthSecret != nil {
		source := &corev1.Secret{}
		source.Name = rr.ProviderConfig.Spec.ExternalDNSSource.CopyAuthSecret.Source.Name
		source.Namespace = r.ProviderNamespace
		log.Debug("Auth secret copying configured, getting source secret", "sourceNamespace", source.Namespace, "sourceName", source.Name)
		if err := r.PlatformCluster.Client().Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting source secret '%s/%s': %w", source.Namespace, source.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}

		// check if target secret already exists
		target := &corev1.Secret{}
		target.Name = source.Name
		if rr.ProviderConfig.Spec.ExternalDNSSource.CopyAuthSecret.Target != nil && rr.ProviderConfig.Spec.ExternalDNSSource.CopyAuthSecret.Target.Name != "" {
			target.Name = rr.ProviderConfig.Spec.ExternalDNSSource.CopyAuthSecret.Target.Name
		}
		target.Namespace = c.Namespace
		targetExists := true
		if err := r.PlatformCluster.Client().Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
			if !apierrors.IsNotFound(err) {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting target secret '%s/%s': %w", target.Namespace, target.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
				return rr
			}
			targetExists = false
		}
		if targetExists {
			// if target secret exists, verify that it is managed by us
			log.Debug("Target secret already exists", "targetNamespace", target.Namespace, "targetName", target.Name)
			for k, v := range expectedLabels {
				if v2, ok := target.Labels[k]; !ok || v2 != v {
					rr.ReconcileError = errutils.WithReason(fmt.Errorf("target secret '%s/%s' already exists and is not managed by %s controller", target.Namespace, target.Name, ControllerName), clusterconst.ReasonConfigurationProblem)
					return rr
				}
			}
		}
		log.Debug("Creating or updating target secret", "targetNamespace", target.Namespace, "targetName", target.Name)
		if _, err := controllerutil.CreateOrUpdate(ctx, r.PlatformCluster.Client(), target, func() error {
			if err := controllerutil.SetOwnerReference(c, target, r.PlatformCluster.Scheme()); err != nil {
				return fmt.Errorf("error setting owner reference on target secret '%s/%s': %w", target.Namespace, target.Name, err)
			}
			target.Labels = maputils.Merge(target.Labels, source.Labels, expectedLabels)
			target.Annotations = maputils.Merge(target.Annotations, source.Annotations)
			target.Data = make(map[string][]byte, len(source.Data))
			maps.Copy(target.Data, source.Data)
			target.Type = source.Type
			return nil
		}); err != nil {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error creating or updating target secret '%s/%s': %w", target.Namespace, target.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}

		rr.Message = "Successfully copied auth secret into Cluster namespace."
	}

	return rr
}

// deployHelmChartSource deploys the configured Flux source (HelmRepository, GitRepository, OCIRepository) into the Cluster namespace.
// It sets 'SourceKind' in the ReconcileResult.
func (r *ClusterReconciler) deployHelmChartSource(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// deploy Flux Source
	var fluxSource client.Object
	var setSpec func(obj client.Object) error
	sourceName := clusterBasedResourceName(c.Name)
	// list existing Flux sources to detect obsolete ones
	existingHelm := &fluxsourcev1.HelmRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingHelm, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing HelmRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	existingGit := &fluxsourcev1.GitRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingGit, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing GitRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	existingOCI := &fluxsourcev1.OCIRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingOCI, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing OCIRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	toBeDeleted := []client.Object{}
	// determine which type of source to create and which existing sources to delete
	if rr.ProviderConfig.Spec.ExternalDNSSource.Helm != nil {
		fluxSource = &fluxsourcev1.HelmRepository{}
		setSpec = func(obj client.Object) error {
			helmRepo, ok := obj.(*fluxsourcev1.HelmRepository)
			if !ok {
				return fmt.Errorf("expected HelmRepository object, got %T", obj)
			}
			helmRepo.Spec = *rr.ProviderConfig.Spec.ExternalDNSSource.Helm.DeepCopy()
			return nil
		}
		rr.SourceKind = "HelmRepository"
		for i := range existingHelm.Items {
			obj := &existingHelm.Items[i]
			if obj.GetName() != sourceName {
				toBeDeleted = append(toBeDeleted, obj)
			}
		}
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingGit.Items, func(obj fluxsourcev1.GitRepository) client.Object { return &obj })...)
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingOCI.Items, func(obj fluxsourcev1.OCIRepository) client.Object { return &obj })...)
	} else if rr.ProviderConfig.Spec.ExternalDNSSource.Git != nil {
		fluxSource = &fluxsourcev1.GitRepository{}
		setSpec = func(obj client.Object) error {
			gitRepo, ok := obj.(*fluxsourcev1.GitRepository)
			if !ok {
				return fmt.Errorf("expected GitRepository object, got %T", obj)
			}
			gitRepo.Spec = *rr.ProviderConfig.Spec.ExternalDNSSource.Git.DeepCopy()
			return nil
		}
		rr.SourceKind = "GitRepository"
		for i := range existingGit.Items {
			obj := &existingGit.Items[i]
			if obj.GetName() != sourceName {
				toBeDeleted = append(toBeDeleted, obj)
			}
		}
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingHelm.Items, func(obj fluxsourcev1.HelmRepository) client.Object { return &obj })...)
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingOCI.Items, func(obj fluxsourcev1.OCIRepository) client.Object { return &obj })...)
	} else if rr.ProviderConfig.Spec.ExternalDNSSource.OCI != nil {
		fluxSource = &fluxsourcev1.OCIRepository{}
		setSpec = func(obj client.Object) error {
			ociRepo, ok := obj.(*fluxsourcev1.OCIRepository)
			if !ok {
				return fmt.Errorf("expected OCIRepository object, got %T", obj)
			}
			ociRepo.Spec = *rr.ProviderConfig.Spec.ExternalDNSSource.OCI.DeepCopy()
			return nil
		}
		rr.SourceKind = "OCIRepository"
		for i := range existingOCI.Items {
			obj := &existingOCI.Items[i]
			if obj.GetName() != sourceName {
				toBeDeleted = append(toBeDeleted, obj)
			}
		}
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingHelm.Items, func(obj fluxsourcev1.HelmRepository) client.Object { return &obj })...)
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingGit.Items, func(obj fluxsourcev1.GitRepository) client.Object { return &obj })...)
	} else {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("no flux source configured"), clusterconst.ReasonConfigurationProblem)
		return rr
	}
	fluxSource.SetName(sourceName)
	fluxSource.SetNamespace(c.Namespace)
	log.Info("Creating or updating Flux source", "kind", rr.SourceKind, "sourceName", fluxSource.GetName(), "sourceNamespace", fluxSource.GetNamespace())
	if _, err := controllerutil.CreateOrUpdate(ctx, r.PlatformCluster.Client(), fluxSource, func() error {
		if err := controllerutil.SetOwnerReference(c, fluxSource, r.PlatformCluster.Scheme()); err != nil {
			return fmt.Errorf("error setting owner reference on %s '%s/%s': %w", rr.SourceKind, fluxSource.GetNamespace(), fluxSource.GetName(), err)
		}
		fluxSource.SetLabels(maputils.Merge(fluxSource.GetLabels(), expectedLabels))
		return setSpec(fluxSource)
	}); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error creating or updating %s '%s/%s': %w", rr.SourceKind, fluxSource.GetNamespace(), fluxSource.GetName(), err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	// delete obsolete sources
	for _, obj := range toBeDeleted {
		log.Info("Deleting obsolete Flux source", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
		if err := r.PlatformCluster.Client().Delete(ctx, obj); err != nil {
			if !apierrors.IsNotFound(err) {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("error deleting obsolete %s '%s/%s': %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err), clusterconst.ReasonPlatformClusterInteractionProblem)
				return rr
			}
		}
	}

	rr.Message = fmt.Sprintf("Successfully created or updated helm chart source (%s).", rr.SourceKind)
	return rr
}

// deployHelmRelease deploys the HelmRelease to install external-dns onto the Cluster.
// It expects 'Config', 'AccessRequest', and 'SourceKind' to be set in the given ReconcileResult.
func (r *ClusterReconciler) deployHelmRelease(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	hr := &fluxhelmv2.HelmRelease{}
	hr.Name = clusterBasedResourceName(c.Name)
	hr.Namespace = c.Namespace

	log.Info("Creating or updating HelmRelease", "resourceName", hr.Name, "resourceNamespace", hr.Namespace)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.PlatformCluster.Client(), hr, func() error {
		// owner reference
		if err := controllerutil.SetOwnerReference(c, hr, r.PlatformCluster.Scheme()); err != nil {
			return fmt.Errorf("error setting owner reference on HelmRelease '%s/%s': %w", hr.Namespace, hr.Name, err)
		}
		// labels
		hr.Labels = maputils.Merge(hr.Labels, expectedLabels)
		// chart
		hr.Spec.Chart = nil
		hr.Spec.ChartRef = &fluxhelmv2.CrossNamespaceSourceReference{
			APIVersion: fluxsourcev1.SchemeBuilder.GroupVersion.String(),
			Kind:       rr.SourceKind,
			Name:       hr.Name,
			Namespace:  hr.Namespace,
		}
		// release information
		hr.Spec.ReleaseName = "external-dns"
		hr.Spec.TargetNamespace = "external-dns"
		// values
		hr.Spec.Values = rr.Config.HelmValues.DeepCopy()
		// install configuration
		if hr.Spec.Install == nil {
			hr.Spec.Install = &fluxhelmv2.Install{}
		}
		hr.Spec.Install.CRDs = fluxhelmv2.CreateReplace
		hr.Spec.Install.CreateNamespace = true
		if hr.Spec.Install.Remediation == nil {
			hr.Spec.Install.Remediation = &fluxhelmv2.InstallRemediation{}
		}
		hr.Spec.Install.Remediation.Retries = 3
		// upgrade configuration
		if hr.Spec.Upgrade == nil {
			hr.Spec.Upgrade = &fluxhelmv2.Upgrade{}
		}
		hr.Spec.Upgrade.CRDs = fluxhelmv2.CreateReplace
		if hr.Spec.Upgrade.Remediation == nil {
			hr.Spec.Upgrade.Remediation = &fluxhelmv2.UpgradeRemediation{}
		}
		hr.Spec.Upgrade.Remediation.Retries = 3
		// reference Cluster kubeconfig
		hr.Spec.KubeConfig = &fluxmeta.KubeConfigReference{
			SecretRef: fluxmeta.SecretKeyReference{
				Name: rr.AccessRequest.Status.SecretRef.Name,
				Key:  clustersv1alpha1.SecretKeyKubeconfig,
			},
		}
		// deploy interval
		if rr.Config.HelmReleaseReconciliationInterval != nil {
			hr.Spec.Interval = *rr.Config.HelmReleaseReconciliationInterval
		} else if rr.ProviderConfig.Spec.HelmReleaseReconciliationInterval != nil {
			hr.Spec.Interval = *rr.ProviderConfig.Spec.HelmReleaseReconciliationInterval
		} else {
			hr.Spec.Interval = metav1.Duration{Duration: 1 * time.Hour}
		}
		return nil
	}); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error creating or updating HelmRelease '%s/%s': %w", hr.Namespace, hr.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}

	rr.Message = "Successfully created or updated HelmRelease to install external-dns."
	return rr
}

// undeployHelmRelease deletes the HelmRelease.
// It requeues the Cluster until the HelmRelease is fully deleted.
func (r *ClusterReconciler) undeployHelmRelease(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	hr := &fluxhelmv2.HelmRelease{}
	hr.Name = clusterBasedResourceName(c.Name)
	hr.Namespace = c.Namespace

	if err := r.PlatformCluster.Client().Get(ctx, client.ObjectKeyFromObject(hr), hr); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("HelmRelease not found, nothing to do", "resourceName", hr.Name, "resourceNamespace", hr.Namespace)
			return rr
		} else {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting HelmRelease '%s/%s': %w", hr.Namespace, hr.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}
	}

	// check if HelmRelease is marked for deletion
	if !hr.DeletionTimestamp.IsZero() {
		log.Info("HelmRelease already marked for deletion, waiting for its removal", "resourceName", hr.Name, "resourceNamespace", hr.Namespace)
	} else {
		// verify that the HelmRelease is managed by us
		for k, v := range expectedLabels {
			if v2, ok := hr.Labels[k]; !ok || v2 != v {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("HelmRelease '%s/%s' exists but is missing expected label (label '%s', expected to have value '%s', actually has '%s')", hr.Namespace, hr.Name, k, v, v2), clusterconst.ReasonInternalError)
				return rr
			}
		}

		// delete HelmRelease
		log.Info("Deleting HelmRelease", "resourceName", hr.Name, "resourceNamespace", hr.Namespace)
		if err := r.PlatformCluster.Client().Delete(ctx, hr); err != nil {
			if !apierrors.IsNotFound(err) {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("error deleting HelmRelease '%s/%s': %w", hr.Namespace, hr.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
				return rr
			}
		}
	}

	// requeue to verify deletion
	rr.Message = "Waiting for HelmRelease to be deleted."
	rr.Result.RequeueAfter = defaultRequeueAfterDuration
	return rr
}

// undeployHelmChartSource deletes all Flux sources where the labels indicate they were created by this controller for the given Cluster.
// It does not wait for their deletion.
func (r *ClusterReconciler) undeployHelmChartSource(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// list existing Flux sources to detect obsolete ones
	existingHelm := &fluxsourcev1.HelmRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingHelm, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing HelmRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	existingGit := &fluxsourcev1.GitRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingGit, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing GitRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	existingOCI := &fluxsourcev1.OCIRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingOCI, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing OCIRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	toBeDeleted := []client.Object{}
	toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingHelm.Items, func(obj fluxsourcev1.HelmRepository) client.Object {
		if obj.GetObjectKind().GroupVersionKind().Kind == "" {
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: fluxsourcev1.GroupVersion.Group, Version: fluxsourcev1.GroupVersion.Version, Kind: "HelmRepository"})
		}
		return &obj
	})...)
	toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingGit.Items, func(obj fluxsourcev1.GitRepository) client.Object {
		if obj.GetObjectKind().GroupVersionKind().Kind == "" {
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: fluxsourcev1.GroupVersion.Group, Version: fluxsourcev1.GroupVersion.Version, Kind: "GitRepository"})
		}
		return &obj
	})...)
	toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingOCI.Items, func(obj fluxsourcev1.OCIRepository) client.Object {
		if obj.GetObjectKind().GroupVersionKind().Kind == "" {
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: fluxsourcev1.GroupVersion.Group, Version: fluxsourcev1.GroupVersion.Version, Kind: "OCIRepository"})
		}
		return &obj
	})...)

	for _, obj := range toBeDeleted {
		log.Info("Deleting Flux source", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "resourceName", obj.GetName(), "resourceNamespace", obj.GetNamespace())
		if err := r.PlatformCluster.Client().Delete(ctx, obj); err != nil {
			if !apierrors.IsNotFound(err) {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("error deleting %s '%s/%s': %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err), clusterconst.ReasonPlatformClusterInteractionProblem)
				return rr
			}
		}
	}

	rr.Message = "Deleted all helm chart sources for Cluster."
	return rr
}

// undeployAuthSecret removes all secrets from the Cluster namespace where the labels indicate they were created by this controller for the given Cluster.
// It does not wait for their deletion.
func (r *ClusterReconciler) undeployAuthSecret(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// list existing secrets to detect obsolete ones
	existingSecrets := &corev1.SecretList{}
	if err := r.PlatformCluster.Client().List(ctx, existingSecrets, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing Secret resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}

	for i := range existingSecrets.Items {
		obj := &existingSecrets.Items[i]
		log.Info("Deleting auth secret", "resourceName", obj.GetName(), "resourceNamespace", obj.GetNamespace())
		if err := r.PlatformCluster.Client().Delete(ctx, obj); err != nil {
			if !apierrors.IsNotFound(err) {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("error deleting Secret '%s/%s': %w", obj.GetNamespace(), obj.GetName(), err), clusterconst.ReasonPlatformClusterInteractionProblem)
				return rr
			}
		}
	}

	rr.Message = "Deleted all auth secrets for Cluster."
	return rr
}

// clusterBasedResourceName generates a name for secondary resources based on the cluster name.
// The name is guaranteed to be unique for each cluster and to not exceed the Kubernetes name length limit.
// It is deterministic, the same clusterName will always yield the same resource name.
func clusterBasedResourceName(clusterName string) string {
	suffix := ".external-dns"
	return ctrlutils.ShortenToXCharactersUnsafe(clusterName, ctrlutils.K8sMaxNameLength-len(suffix)) + suffix
}

func (r *ClusterReconciler) addKnownCluster(c *clustersv1alpha1.Cluster) {
	nn := types.NamespacedName{Namespace: c.Namespace, Name: c.Name}
	r.KnownClustersLock.Lock()
	defer r.KnownClustersLock.Unlock()
	r.KnownClusters[nn] = struct{}{}
}

func (r *ClusterReconciler) removeKnownCluster(c *clustersv1alpha1.Cluster) {
	r.removeKnownClusterRaw(c.Name, c.Namespace)
}

func (r *ClusterReconciler) removeKnownClusterRaw(name, namespace string) {
	nn := types.NamespacedName{Namespace: namespace, Name: name}
	r.KnownClustersLock.Lock()
	defer r.KnownClustersLock.Unlock()
	delete(r.KnownClusters, nn)
}

func (r *ClusterReconciler) listKnownClusters() []types.NamespacedName {
	r.KnownClustersLock.RLock()
	defer r.KnownClustersLock.RUnlock()
	result := make([]types.NamespacedName, 0, len(r.KnownClusters))
	for nn := range r.KnownClusters {
		result = append(result, nn)
	}
	return result
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// watch Cluster resources
		For(&clustersv1alpha1.Cluster{}).
		WithEventFilter(predicate.And(
			predicate.Or(
				predicate.GenerationChangedPredicate{},
				ctrlutils.DeletionTimestampChangedPredicate{},
				ctrlutils.GotAnnotationPredicate(openmcpconst.OperationAnnotation, openmcpconst.OperationAnnotationValueReconcile),
				ctrlutils.LostAnnotationPredicate(openmcpconst.OperationAnnotation, openmcpconst.OperationAnnotationValueIgnore),
				ctrlutils.GotAnnotationPredicate(dnsv1alpha1.OperationAnnotation, openmcpconst.OperationAnnotationValueReconcile),
				ctrlutils.LostAnnotationPredicate(dnsv1alpha1.OperationAnnotation, openmcpconst.OperationAnnotationValueIgnore),
			),
			predicate.Not(
				predicate.Or(
					ctrlutils.HasAnnotationPredicate(openmcpconst.OperationAnnotation, openmcpconst.OperationAnnotationValueIgnore),
					ctrlutils.HasAnnotationPredicate(dnsv1alpha1.OperationAnnotation, openmcpconst.OperationAnnotationValueIgnore),
				),
			),
		)).
		// watch DNSServiceConfig resource and reconcile all Clusters that are known to have external-dns deployed if it changes
		Watches(&dnsv1alpha1.DNSServiceConfig{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []ctrl.Request {
			return collections.ProjectSliceToSlice(r.listKnownClusters(), func(nn types.NamespacedName) ctrl.Request {
				return ctrl.Request{NamespacedName: nn}
			})
		}), builder.WithPredicates(predicate.And(
			predicate.GenerationChangedPredicate{},
			ctrlutils.ExactNamePredicate(r.ProviderName, r.ProviderNamespace),
		))).
		Complete(r)
}
