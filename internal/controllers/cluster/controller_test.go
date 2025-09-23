//nolint:goconst
package cluster_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fluxhelmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxsourcev1 "github.com/fluxcd/source-controller/api/v1"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	testutils "github.com/openmcp-project/controller-utils/pkg/testing"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	commonapi "github.com/openmcp-project/openmcp-operator/api/common"
	openmcpconst "github.com/openmcp-project/openmcp-operator/api/constants"
	accesslib "github.com/openmcp-project/openmcp-operator/lib/clusteraccess"

	dnsv1alpha1 "github.com/openmcp-project/platform-service-dns/api/dns/v1alpha1"
	"github.com/openmcp-project/platform-service-dns/api/install"
	"github.com/openmcp-project/platform-service-dns/internal/controllers/cluster"
)

const (
	platformCluster = "platform"

	providerName      = "dns-service"
	providerNamespace = "test"
	managedByValue    = providerName + "." + cluster.ControllerName
)

var platformScheme = install.InstallOperatorAPIsPlatform(runtime.NewScheme())

func defaultTestSetup(testDirPathSegments ...string) *testutils.Environment {
	env := testutils.NewEnvironmentBuilder().
		WithFakeClient(platformScheme).
		WithInitObjectPath(testDirPathSegments...).
		WithDynamicObjectsWithStatus(&clustersv1alpha1.AccessRequest{}).
		WithReconcilerConstructor(func(c client.Client) reconcile.Reconciler {
			cRec := cluster.NewClusterReconciler(clusters.NewTestClusterFromClient(platformCluster, c), nil, providerName, providerNamespace)
			cRec.FakeClientMapping = map[string]client.Client{
				"cluster-01": nil,
				"cluster-02": nil,
				"cluster-03": nil,
			}
			return cRec
		}).
		Build()

	return env
}

var _ = Describe("ClusterReconciler", func() {

	It("should fail if no DNSServiceConfig exists", func() {
		env := defaultTestSetup("testdata", "test-01")

		// delete any existing DNSServiceConfig
		Expect(env.Client().DeleteAllOf(env.Ctx, &dnsv1alpha1.DNSServiceConfig{})).To(Succeed())

		c := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c)).To(Succeed())
		env.ShouldNotReconcile(testutils.RequestFromObject(c))
	})

	It("should correctly match configs to clusters and create the flux resources", func() {
		env := defaultTestSetup("testdata", "test-01")

		cfg := &dnsv1alpha1.DNSServiceConfig{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: providerName}, cfg)).To(Succeed())

		c1 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c1)).To(Succeed())
		rr := env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c1)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify that the correct resources were created
		expectedLabels := map[string]string{
			openmcpconst.ManagedByLabel:      managedByValue,
			openmcpconst.ManagedPurposeLabel: c1.Name,
		}
		// flux source
		srcs := &fluxsourcev1.OCIRepositoryList{}
		Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(srcs.Items).To(HaveLen(1))
		Expect(srcs.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c1.Name),
		})))
		Expect(srcs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"URL": Equal("oci://example.org/repo/charts"),
		}))
		// flux helm release
		hrs := &fluxhelmv2.HelmReleaseList{}
		Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(hrs.Items).To(HaveLen(1))
		Expect(hrs.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c1.Name),
		})))
		Expect(hrs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"ReleaseName":     Equal("external-dns"),
			"TargetNamespace": Equal("external-dns"),
			"Values":          BeEquivalentTo(cfg.Spec.ExternalDNSForPurposes[0].HelmValues),
			"Interval":        Equal(metav1.Duration{Duration: 1 * time.Hour}),
		}))
		// AccessRequest
		ars := &clustersv1alpha1.AccessRequestList{}
		Expect(env.Client().List(env.Ctx, ars, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(ars.Items).To(HaveLen(1))
		Expect(ars.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c1.Name),
		})))
		Expect(ars.Items[0].Spec.ClusterRef).To(PointTo(MatchFields(IgnoreExtras, Fields{
			"Name":      Equal(c1.Name),
			"Namespace": Equal("foo"),
		})))
		// copied secrets (including deletion of the obsolete one)
		ss := &corev1.SecretList{}
		Expect(env.Client().List(env.Ctx, ss, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(ss.Items).To(ConsistOf(
			MatchFields(IgnoreExtras, Fields{
				"ObjectMeta": MatchFields(IgnoreExtras, Fields{
					"Name": Equal("my-auth-copy"),
				}),
				"Data": Equal(map[string][]byte{"key": []byte("value")}),
			}),
			MatchFields(IgnoreExtras, Fields{
				"ObjectMeta": MatchFields(IgnoreExtras, Fields{
					"Name": Equal("my-other-secret"),
				}),
				"Data": Equal(map[string][]byte{"foo": []byte("bar")}),
			}),
		))
		for _, s := range ss.Items {
			Expect(s.OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
				"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
				"Kind":       Equal("Cluster"),
				"Name":       Equal(c1.Name),
			})), "secret '%s/%s' does not have the expected owner reference", s.Namespace, s.Name)
		}

		c2 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-02", Namespace: "bar"}, c2)).To(Succeed())
		rr = env.ShouldReconcile(testutils.RequestFromObject(c2))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c2)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c2))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify that the correct flux resources were created
		expectedLabels[openmcpconst.ManagedPurposeLabel] = c2.Name
		// flux source
		srcs = &fluxsourcev1.OCIRepositoryList{}
		Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c2.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(srcs.Items).To(HaveLen(1))
		Expect(srcs.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c2.Name),
		})))
		Expect(srcs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"URL": Equal("oci://example.org/repo/charts"),
		}))
		// flux helm release
		hrs = &fluxhelmv2.HelmReleaseList{}
		Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c2.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(hrs.Items).To(HaveLen(1))
		Expect(hrs.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c2.Name),
		})))
		Expect(hrs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"ReleaseName":     Equal("external-dns"),
			"TargetNamespace": Equal("external-dns"),
			"Values":          BeEquivalentTo(cfg.Spec.ExternalDNSForPurposes[1].HelmValues),
			"Interval":        Equal(metav1.Duration{Duration: 1 * time.Hour}),
		}))
		// AccessRequest
		ars = &clustersv1alpha1.AccessRequestList{}
		Expect(env.Client().List(env.Ctx, ars, client.InNamespace(c2.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(ars.Items).To(HaveLen(1))
		Expect(ars.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c2.Name),
		})))
		Expect(ars.Items[0].Spec.ClusterRef).To(PointTo(MatchFields(IgnoreExtras, Fields{
			"Name":      Equal(c2.Name),
			"Namespace": Equal("bar"),
		})))
		// copied secrets
		Expect(env.Client().List(env.Ctx, ss, client.InNamespace(c2.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(ss.Items).To(ConsistOf(
			MatchFields(IgnoreExtras, Fields{
				"ObjectMeta": MatchFields(IgnoreExtras, Fields{
					"Name": Equal("my-auth-copy"),
				}),
				"Data": Equal(map[string][]byte{"key": []byte("value")}),
			}),
			MatchFields(IgnoreExtras, Fields{
				"ObjectMeta": MatchFields(IgnoreExtras, Fields{
					"Name": Equal("my-other-secret"),
				}),
				"Data": Equal(map[string][]byte{"foo": []byte("bar")}),
			}),
		))
		for _, s := range ss.Items {
			Expect(s.OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
				"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
				"Kind":       Equal("Cluster"),
				"Name":       Equal(c2.Name),
			})), "secret '%s/%s' does not have the expected owner reference", s.Namespace, s.Name)
		}
	})

	It("should correctly match complex purpose selectors and don't create resources if no purpose selector matches", func() {
		env := defaultTestSetup("testdata", "test-02")

		cfg := &dnsv1alpha1.DNSServiceConfig{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: providerName}, cfg)).To(Succeed())

		c1 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c1)).To(Succeed())
		rr := env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c1)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify that the correct HelmRelease was created
		expectedLabels := map[string]string{
			openmcpconst.ManagedByLabel:      managedByValue,
			openmcpconst.ManagedPurposeLabel: "cluster-01",
		}
		// cluster-01 has purposes foo, bar, and foobar so it matches the second configuration
		hrs := &fluxhelmv2.HelmReleaseList{}
		Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(hrs.Items).To(HaveLen(1))
		Expect(hrs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"Values": BeEquivalentTo(cfg.Spec.ExternalDNSForPurposes[1].HelmValues),
		}))

		c2 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-02", Namespace: "bar"}, c2)).To(Succeed())
		rr = env.ShouldReconcile(testutils.RequestFromObject(c2))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c2)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c2))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify that the correct HelmRelease was created
		expectedLabels[openmcpconst.ManagedPurposeLabel] = "cluster-02"
		// cluster-02 has purpose bar, so it matches the first configuration
		hrs = &fluxhelmv2.HelmReleaseList{}
		Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c2.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(hrs.Items).To(HaveLen(1))
		Expect(hrs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"Values": BeEquivalentTo(cfg.Spec.ExternalDNSForPurposes[0].HelmValues),
		}))

		c3 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-03", Namespace: "baz"}, c3)).To(Succeed())
		env.ShouldReconcile(testutils.RequestFromObject(c3))

		// verify that the correct HelmRelease was created
		expectedLabels[openmcpconst.ManagedPurposeLabel] = "cluster-03"
		// cluster-03 has purposes foo and bar, so it does not match any configuration,
		// as the first one requires either foo or bar, but not both, and the second one requires foo and foobar
		// this means that no resources should be created
		srcs := &fluxsourcev1.OCIRepositoryList{}
		Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c3.Namespace))).To(Succeed())
		Expect(srcs.Items).To(BeEmpty())
		hrs = &fluxhelmv2.HelmReleaseList{}
		Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c3.Namespace))).To(Succeed())
		Expect(hrs.Items).To(BeEmpty())
		ars := &clustersv1alpha1.AccessRequestList{}
		Expect(env.Client().List(env.Ctx, ars, client.InNamespace(c3.Namespace))).To(Succeed())
		Expect(ars.Items).To(BeEmpty())
	})

	It("should use finalizers and remove resources when the Cluster is being deleted", func() {
		env := defaultTestSetup("testdata", "test-01")

		cfg := &dnsv1alpha1.DNSServiceConfig{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: providerName}, cfg)).To(Succeed())

		c1 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c1)).To(Succeed())
		rr := env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c1)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify finalizer on Cluster
		Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(c1), c1)).To(Succeed())
		Expect(c1.Finalizers).To(ContainElement(dnsv1alpha1.ExternalDNSFinalizerOnCluster))

		// verify that the flux resources were created
		expectedLabels := map[string]string{
			openmcpconst.ManagedByLabel:      managedByValue,
			openmcpconst.ManagedPurposeLabel: c1.Name,
		}
		// flux source
		srcs := &fluxsourcev1.OCIRepositoryList{}
		Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(srcs.Items).To(HaveLen(1))
		// flux helm release
		hrs := &fluxhelmv2.HelmReleaseList{}
		Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(hrs.Items).To(HaveLen(1))
		// AccessRequest
		ars := &clustersv1alpha1.AccessRequestList{}
		Expect(env.Client().List(env.Ctx, ars, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(ars.Items).To(HaveLen(1))
		// copied secrets
		ss := &corev1.SecretList{}
		Expect(env.Client().List(env.Ctx, ss, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(ss.Items).To(HaveLen(2))

		// delete Cluster
		Expect(env.Client().Delete(env.Ctx, c1)).To(Succeed())
		// cluster should still exist because of finalizer
		Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(c1), c1)).To(Succeed())
		Expect(c1.DeletionTimestamp).ToNot(BeNil())

		// wrapped in Eventually because it may take multiple reconciliations until all resources are deleted
		Eventually(func(g Gomega) {
			// reconcile again, this should remove the resources and the finalizer
			env.ShouldReconcile(testutils.RequestFromObject(c1))

			// verify that the flux resources were deleted
			// flux source
			srcs = &fluxsourcev1.OCIRepositoryList{}
			g.Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
			g.Expect(srcs.Items).To(BeEmpty())
			// flux helm release
			hrs = &fluxhelmv2.HelmReleaseList{}
			g.Expect(env.Client().List(env.Ctx, hrs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
			g.Expect(hrs.Items).To(BeEmpty())
			// auth secret
			ss := &corev1.SecretList{}
			g.Expect(env.Client().List(env.Ctx, ss, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
			Expect(ss.Items).To(BeEmpty())

			// verify that finalizer was removed and Cluster deleted
			g.Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(c1), c1)).To(MatchError(apierrors.IsNotFound, "IsNotFound"))
		}).Should(Succeed())
	})

	It("should delete obsolete flux sources", func() {
		env := defaultTestSetup("testdata", "test-01")

		cfg := &dnsv1alpha1.DNSServiceConfig{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: providerName}, cfg)).To(Succeed())

		// create dummy flux sources
		expectedLabels := map[string]string{
			openmcpconst.ManagedByLabel:      managedByValue,
			openmcpconst.ManagedPurposeLabel: "cluster-01",
		}
		// helm repo
		helmSource := &fluxsourcev1.HelmRepository{}
		helmSource.Name = "dummy"
		helmSource.Namespace = "foo"
		helmSource.Labels = expectedLabels
		Expect(env.Client().Create(env.Ctx, helmSource)).To(Succeed())
		// oci repo
		ociSource := &fluxsourcev1.OCIRepository{}
		ociSource.Name = "dummy"
		ociSource.Namespace = "foo"
		ociSource.Labels = expectedLabels
		Expect(env.Client().Create(env.Ctx, ociSource)).To(Succeed())
		// git repo
		gitSource := &fluxsourcev1.GitRepository{}
		gitSource.Name = "dummy"
		gitSource.Namespace = "foo"
		gitSource.Labels = expectedLabels
		Expect(env.Client().Create(env.Ctx, gitSource)).To(Succeed())

		c1 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c1)).To(Succeed())
		rr := env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c1)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeZero())

		// all three dummy resources should be deleted
		Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(helmSource), helmSource)).To(MatchError(apierrors.IsNotFound, "IsNotFound"))
		Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(ociSource), ociSource)).To(MatchError(apierrors.IsNotFound, "IsNotFound"))
		Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(gitSource), gitSource)).To(MatchError(apierrors.IsNotFound, "IsNotFound"))
	})

	It("should create a GitRepository if configured", func() {
		env := defaultTestSetup("testdata", "test-03")

		cfg := &dnsv1alpha1.DNSServiceConfig{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: providerName}, cfg)).To(Succeed())

		c1 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c1)).To(Succeed())
		rr := env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c1)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify that the correct resources were created
		expectedLabels := map[string]string{
			openmcpconst.ManagedByLabel:      managedByValue,
			openmcpconst.ManagedPurposeLabel: c1.Name,
		}
		// flux source
		srcs := &fluxsourcev1.GitRepositoryList{}
		Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(srcs.Items).To(HaveLen(1))
		Expect(srcs.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c1.Name),
		})))
		Expect(srcs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"URL": Equal("https://example.org/repo/charts"),
		}))
	})

	It("should create a HelmRepository if configured", func() {
		env := defaultTestSetup("testdata", "test-04")

		cfg := &dnsv1alpha1.DNSServiceConfig{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: providerName}, cfg)).To(Succeed())

		c1 := &clustersv1alpha1.Cluster{}
		Expect(env.Client().Get(env.Ctx, client.ObjectKey{Name: "cluster-01", Namespace: "foo"}, c1)).To(Succeed())
		rr := env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeNumerically(">", 0))
		fakeAccessRequestReadiness(env, c1)
		rr = env.ShouldReconcile(testutils.RequestFromObject(c1))
		Expect(rr.RequeueAfter).To(BeZero())

		// verify that the correct resources were created
		expectedLabels := map[string]string{
			openmcpconst.ManagedByLabel:      managedByValue,
			openmcpconst.ManagedPurposeLabel: c1.Name,
		}
		// flux source
		srcs := &fluxsourcev1.HelmRepositoryList{}
		Expect(env.Client().List(env.Ctx, srcs, client.InNamespace(c1.Namespace), client.MatchingLabels(expectedLabels))).To(Succeed())
		Expect(srcs.Items).To(HaveLen(1))
		Expect(srcs.Items[0].OwnerReferences).To(ContainElements(MatchFields(IgnoreExtras, Fields{
			"APIVersion": Equal(clustersv1alpha1.GroupVersion.String()),
			"Kind":       Equal("Cluster"),
			"Name":       Equal(c1.Name),
		})))
		Expect(srcs.Items[0].Spec).To(MatchFields(IgnoreExtras, Fields{
			"URL": Equal("https://example.org/repo/charts"),
		}))
	})

})

func fakeAccessRequestReadiness(env *testutils.Environment, c *clustersv1alpha1.Cluster) {
	ar := &clustersv1alpha1.AccessRequest{}
	ar.SetName(accesslib.StableRequestNameFromLocalName(cluster.ControllerName, c.Name))
	ar.SetNamespace(c.Namespace)
	Expect(env.Client().Get(env.Ctx, client.ObjectKeyFromObject(ar), ar)).To(Succeed())
	old := ar.DeepCopy()
	ar.Status.Phase = clustersv1alpha1.REQUEST_GRANTED
	ar.Status.SecretRef = &commonapi.ObjectReference{
		Name:      ar.Name,
		Namespace: ar.Namespace,
	}
	Expect(env.Client().Status().Patch(env.Ctx, ar, client.MergeFrom(old))).To(Succeed())
	sec := &corev1.Secret{}
	sec.Name = ar.Status.SecretRef.Name
	sec.Namespace = ar.Status.SecretRef.Namespace
	sec.Data = map[string][]byte{
		clustersv1alpha1.SecretKeyKubeconfig: []byte("fake"),
	}
	Expect(env.Client().Create(env.Ctx, sec)).To(Succeed())
}
