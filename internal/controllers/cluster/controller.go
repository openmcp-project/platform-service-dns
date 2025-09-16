package cluster

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
		rr.AccessRequest = ar

		// inject labels into AccessRequest
		if err := ctrlutils.EnsureLabel(ctx, r.PlatformCluster.Client(), rr.AccessRequest, openmcpconst.ManagedByLabel, ControllerName, true, ctrlutils.OVERWRITE); err != nil {
			rr.ReconcileError = errutils.WithReason(fmt.Errorf("error ensuring labels on AccessRequest: %w", err), clusterconst.ReasonPlatformClusterInteractionProblem)
			return rr
		}

		rr = r.deployAuthSecret(ctx, c, expectedLabels, rr)
		if rr.ReconcileError != nil {
			return rr
		}

		rr = r.deployHelmChartSource(ctx, c, expectedLabels, rr)
		if rr.ReconcileError != nil {
			return rr
		}

		rr = r.deployHelmRelease(ctx, c, expectedLabels, rr)
		if rr.ReconcileError != nil {
			return rr
		}

		// TODO: wait for HelmRelease to be ready?
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

// deployAuthSecret copies the auth secret (for access to the helm chart source) into the Cluster namespace if configured.
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
	if r.Config.Spec.ExternalDNSSource.Helm != nil {
		fluxSource = &fluxsourcev1.HelmRepository{}
		setSpec = func(obj client.Object) error {
			helmRepo, ok := obj.(*fluxsourcev1.HelmRepository)
			if !ok {
				return fmt.Errorf("expected HelmRepository object, got %T", obj)
			}
			helmRepo.Spec = *r.Config.Spec.ExternalDNSSource.Helm.DeepCopy()
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
	} else if r.Config.Spec.ExternalDNSSource.Git != nil {
		fluxSource = &fluxsourcev1.GitRepository{}
		setSpec = func(obj client.Object) error {
			gitRepo, ok := obj.(*fluxsourcev1.GitRepository)
			if !ok {
				return fmt.Errorf("expected GitRepository object, got %T", obj)
			}
			gitRepo.Spec = *r.Config.Spec.ExternalDNSSource.Git.DeepCopy()
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
	} else if r.Config.Spec.ExternalDNSSource.OCI != nil {
		fluxSource = &fluxsourcev1.OCIRepository{}
		setSpec = func(obj client.Object) error {
			ociRepo, ok := obj.(*fluxsourcev1.OCIRepository)
			if !ok {
				return fmt.Errorf("expected OCIRepository object, got %T", obj)
			}
			ociRepo.Spec = *r.Config.Spec.ExternalDNSSource.OCI.DeepCopy()
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
		} else if r.Config.Spec.HelmReleaseReconciliationInterval != nil {
			hr.Spec.Interval = *r.Config.Spec.HelmReleaseReconciliationInterval
		} else {
			hr.Spec.Interval = metav1.Duration{Duration: 1 * time.Hour}
		}
		return nil
	}); err != nil {
		rr.ReconcileError = errutils.WithReason(fmt.Errorf("error creating or updating HelmRelease '%s/%s': %w", hr.Namespace, hr.Name, err), clusterconst.ReasonPlatformClusterInteractionProblem)
		return rr
	}

	return rr
}

// clusterBasedResourceName generates a name for secondary resources based on the cluster name.
// The name is guaranteed to be unique for each cluster and to not exceed the Kubernetes name length limit.
// It is deterministic, the same clusterName will always yield the same resource name.
func clusterBasedResourceName(clusterName string) string {
	suffix := ".external-dns"
	return ctrlutils.ShortenToXCharactersUnsafe(clusterName, ctrlutils.K8sMaxNameLength-len(suffix)) + suffix
}
