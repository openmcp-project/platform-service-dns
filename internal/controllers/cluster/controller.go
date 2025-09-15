package cluster

import (
	"context"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxv1 "github.com/fluxcd/source-controller/api/v1"

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

type ClusterReconciler struct {
	PlatformCluster   *clusters.Cluster
	Config            *dnsv1alpha1.DNSServiceConfig
	eventRecorder     record.EventRecorder
	ProviderName      string
	ProviderNamespace string
}

func NewClusterReconciler(platformCluster *clusters.Cluster, recorder record.EventRecorder, cfg *dnsv1alpha1.DNSServiceConfig, providerName, providerNamespace string) *ClusterReconciler {
	return &ClusterReconciler{
		PlatformCluster:   platformCluster,
		eventRecorder:     recorder,
		ProviderName:      providerName,
		ProviderNamespace: providerNamespace,
		Config:            cfg,
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
	expectedLabels := map[string]string{
		openmcpconst.ManagedByLabel:      ControllerName,
		openmcpconst.ManagedPurposeLabel: c.Name,
	}

	if c.DeletionTimestamp.IsZero() {
		// CREATE/UPDATE
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
		// TODO: use access
		_, ar, err := accessMgr.WaitForClusterAccess(ctx, localName, nil, &commonapi.ObjectReference{
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

		rr = r.deployAuthSecret(ctx, c, expectedLabels, rr)
		if rr.ReconcileError != nil {
			return rr
		}

		rr = r.deployFluxSource(ctx, c, expectedLabels, rr)
		if rr.ReconcileError != nil {
			return rr
		}

		// TODO: deploy Flux Kustomization to deploy external-dns with selected config onto Cluster
	} else {
		// DELETE
		log.Info("Cluster marked for deletion, cleaning up DNS configuration")

		// TODO: clean up deployed resources by removing Flux resources with matching labels

		// TODO: clean up copied auth secret if it was copied

		// remove finalizer from Cluster
		old := c.DeepCopy()
		if controllerutil.RemoveFinalizer(c, dnsv1alpha1.ExternalDNSFinalizerOnCluster) {
			log.Info("Removing finalizer from Cluster", "finalizer", dnsv1alpha1.ExternalDNSFinalizerOnCluster)
			if err := r.PlatformCluster.Client().Patch(ctx, c, client.MergeFrom(old)); err != nil {
				rr.ReconcileError = errutils.WithReason(fmt.Errorf("error removing finalizer from Cluster '%s/%s': %w", c.Namespace, c.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
				return rr
			}
		}
	}

	return rr
}

func (r *ClusterReconciler) deployAuthSecret(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// copy secret if configured
	if r.Config.Spec.ExternalDNSSource.CopyAuthSecret != nil {
		source := &corev1.Secret{}
		source.Name = r.Config.Spec.ExternalDNSSource.CopyAuthSecret.Source.Name
		source.Namespace = r.ProviderNamespace
		log.Debug("Auth secret copying configured, getting source secret", "sourceNamespace", source.Namespace, "sourceName", source.Name)
		if err := r.PlatformCluster.Client().Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error getting source secret '%s/%s': %w", source.Namespace, source.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}

		// check if target secret already exists
		target := &corev1.Secret{}
		target.Name = source.Name
		if r.Config.Spec.ExternalDNSSource.CopyAuthSecret.Target != nil && r.Config.Spec.ExternalDNSSource.CopyAuthSecret.Target.Name != "" {
			target.Name = r.Config.Spec.ExternalDNSSource.CopyAuthSecret.Target.Name
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
	}

	return rr
}

func (r *ClusterReconciler) deployFluxSource(ctx context.Context, c *clustersv1alpha1.Cluster, expectedLabels map[string]string, rr ReconcileResult) ReconcileResult {
	log := logging.FromContextOrPanic(ctx)

	// deploy Flux Source
	var fluxSource client.Object
	var setSpec func(obj client.Object) error
	var kind string
	sourceName := c.Name + ".external-dns" // TODO: shorten if too long
	// list existing Flux sources to detect obsolete ones
	existingHelm := &fluxv1.HelmRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingHelm, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing HelmRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	existingGit := &fluxv1.GitRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingGit, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing GitRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	existingOCI := &fluxv1.OCIRepositoryList{}
	if err := r.PlatformCluster.Client().List(ctx, existingOCI, client.InNamespace(c.Namespace), client.MatchingLabels(expectedLabels)); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error listing existing OCIRepository resources in target namespace '%s': %w", c.Namespace, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}
	toBeDeleted := []client.Object{}
	// determine which type of source to create and which existing sources to delete
	if r.Config.Spec.ExternalDNSSource.Helm != nil {
		fluxSource = &fluxv1.HelmRepository{}
		setSpec = func(obj client.Object) error {
			helmRepo, ok := obj.(*fluxv1.HelmRepository)
			if !ok {
				return fmt.Errorf("expected HelmRepository object, got %T", obj)
			}
			helmRepo.Spec = *r.Config.Spec.ExternalDNSSource.Helm.DeepCopy()
			return nil
		}
		kind = "HelmRepository"
		for i := range existingHelm.Items {
			obj := &existingHelm.Items[i]
			if obj.GetName() != sourceName {
				toBeDeleted = append(toBeDeleted, obj)
			}
		}
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingGit.Items, func(obj fluxv1.GitRepository) client.Object { return &obj })...)
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingOCI.Items, func(obj fluxv1.OCIRepository) client.Object { return &obj })...)
	} else if r.Config.Spec.ExternalDNSSource.Git != nil {
		fluxSource = &fluxv1.GitRepository{}
		setSpec = func(obj client.Object) error {
			gitRepo, ok := obj.(*fluxv1.GitRepository)
			if !ok {
				return fmt.Errorf("expected GitRepository object, got %T", obj)
			}
			gitRepo.Spec = *r.Config.Spec.ExternalDNSSource.Git.DeepCopy()
			return nil
		}
		kind = "GitRepository"
		for i := range existingGit.Items {
			obj := &existingGit.Items[i]
			if obj.GetName() != sourceName {
				toBeDeleted = append(toBeDeleted, obj)
			}
		}
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingHelm.Items, func(obj fluxv1.HelmRepository) client.Object { return &obj })...)
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingOCI.Items, func(obj fluxv1.OCIRepository) client.Object { return &obj })...)
	} else if r.Config.Spec.ExternalDNSSource.OCI != nil {
		fluxSource = &fluxv1.OCIRepository{}
		setSpec = func(obj client.Object) error {
			ociRepo, ok := obj.(*fluxv1.OCIRepository)
			if !ok {
				return fmt.Errorf("expected OCIRepository object, got %T", obj)
			}
			ociRepo.Spec = *r.Config.Spec.ExternalDNSSource.OCI.DeepCopy()
			return nil
		}
		kind = "OCIRepository"
		for i := range existingOCI.Items {
			obj := &existingOCI.Items[i]
			if obj.GetName() != sourceName {
				toBeDeleted = append(toBeDeleted, obj)
			}
		}
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingHelm.Items, func(obj fluxv1.HelmRepository) client.Object { return &obj })...)
		toBeDeleted = append(toBeDeleted, collections.ProjectSliceToSlice(existingGit.Items, func(obj fluxv1.GitRepository) client.Object { return &obj })...)
	} else {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("no flux source configured"), clusterconst.ReasonConfigurationProblem)
		return rr
	}
	fluxSource.SetName(sourceName)
	fluxSource.SetNamespace(c.Namespace)
	log.Info("Creating or updating Flux source", "kind", kind, "sourceName", fluxSource.GetName(), "sourceNamespace", fluxSource.GetNamespace())
	if _, err := controllerutil.CreateOrUpdate(ctx, r.PlatformCluster.Client(), fluxSource, func() error {
		fluxSource.SetLabels(maputils.Merge(fluxSource.GetLabels(), expectedLabels))
		return setSpec(fluxSource)
	}); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error creating or updating %s '%s/%s': %w", kind, fluxSource.GetNamespace(), fluxSource.GetName(), err), clusterconst.ReasonPlatformClusterInteractionProblem)
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

	return rr
}
