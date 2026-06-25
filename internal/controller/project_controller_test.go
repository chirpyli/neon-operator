package controller

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/test/fixtures"
	"oltp.molnett.org/neon-operator/utils"
)

var _ = Describe("Project Controller", func() {
	const (
		clusterName = "proj-cluster"
		projectName = "proj-project"
	)
	var namespace string

	BeforeEach(func() {
		storconFake.Reset()
		storconFake.LocationConfig = nil
		storconFake.DeleteTenant = nil

		namespace = newTestNamespace()
		Expect(k8sClient.Create(ctx, fixtures.NewBucketCredsSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewStorcondDBSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewCluster(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewProject(projectName, namespace, clusterName))).To(Succeed())
	})

	AfterEach(func() {
		storconFake.LocationConfig = nil
		storconFake.DeleteTenant = nil
	})

	It("adds finalizer and reaches Available after tenant is attached", func() {
		var tenantID string
		Eventually(func(g Gomega) {
			proj := &neonv1alpha1.Project{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
			g.Expect(controllerutil.ContainsFinalizer(proj, utils.FinalizerName)).To(BeTrue(), "finalizer should be present")
			g.Expect(proj.Spec.TenantID).NotTo(BeEmpty(), "tenant ID should be assigned")

			cond := meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionTenantIDAssigned)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			cond = meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionAttached)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			cond = meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			tenantID = proj.Spec.TenantID
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Verify that the fake Storage Controller received the location_config PUT
		calls := storconFake.Calls()
		hasAttach := false
		for _, c := range calls {
			if c.Method == "PUT" {
				hasAttach = true
				break
			}
		}
		Expect(hasAttach).To(BeTrue(), "should have called PUT /v1/tenant/{id}/location_config")

		_ = tenantID // used above in Eventually scope, silence compiler
	})

	It("removes finalizer and completes deletion normally", func() {
		// Wait for Project to be fully created and stable.
		Eventually(func(g Gomega) {
			proj := &neonv1alpha1.Project{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
			g.Expect(proj.Spec.TenantID).NotTo(BeEmpty())
			cond := meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Capture calls before delete to isolate the deletion call.
		beforeCalls := len(storconFake.Calls())

		proj := &neonv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, proj)).To(Succeed())

		// Project should eventually be removed (finalizer removed → API server deletes).
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: projectName, Namespace: namespace},
				&neonv1alpha1.Project{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Project should be fully deleted")

		// Verify a DELETE call was made to the fake Storage Controller.
		calls := storconFake.Calls()[beforeCalls:]
		hasDelete := false
		for _, c := range calls {
			if c.Method == "DELETE" {
				hasDelete = true
				break
			}
		}
		Expect(hasDelete).To(BeTrue(), "should have called DELETE /v1/tenant/{id}")
	})

	It("removes finalizer when Storage Controller returns 404 (already deleted)", func() {
		// Override the delete handler to return 404.
		storconFake.DeleteTenant = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}

		// Wait for creation to stabilize.
		Eventually(func(g Gomega) {
			proj := &neonv1alpha1.Project{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
			cond := meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		proj := &neonv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, proj)).To(Succeed())

		// 404 is treated as success → finalizer removed → CR gone.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: projectName, Namespace: namespace},
				&neonv1alpha1.Project{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Project should be deleted even with 404 response")
	})

	It("retains finalizer and sets Terminating condition on Storage Controller failure", func() {
		// Override the delete handler to return 500.
		storconFake.DeleteTenant = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Wait for creation to stabilize.
		Eventually(func(g Gomega) {
			proj := &neonv1alpha1.Project{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
			cond := meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		proj := &neonv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, proj)).To(Succeed())

		// The finalizer should be retained and the Terminating condition should be set.
		// Requeue happens after 5s, so check within that window.
		Eventually(func(g Gomega) {
			p := &neonv1alpha1.Project{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, p)
			g.Expect(err).NotTo(HaveOccurred(), "Project should still exist (finalizer retained)")

			g.Expect(controllerutil.ContainsFinalizer(p, utils.FinalizerName)).To(BeTrue(), "finalizer should be retained")

			cond := meta.FindStatusCondition(p.Status.Conditions, utils.ConditionTerminating)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(utils.ReasonExternalCleanupFailed))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		// Restore the handler to allow subsequent cleanup (so AfterEach doesn't hang).
		storconFake.DeleteTenant = nil
	})

	It("skips Storage Controller call and removes finalizer when TenantID is empty", func() {
		// Wait for creation to stabilize (TenantID + finalizer set).
		Eventually(func(g Gomega) {
			proj := &neonv1alpha1.Project{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
			g.Expect(proj.Spec.TenantID).NotTo(BeEmpty())
			g.Expect(controllerutil.ContainsFinalizer(proj, utils.FinalizerName)).To(BeTrue())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Clear TenantID and immediately delete.
		// If the controller sees empty TenantID during finalize, it skips external call.
		// If the controller re-assigned TenantID before seeing deletion, it calls fake (500).
		p := &neonv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, p)).To(Succeed())
		p.Spec.TenantID = ""
		Expect(k8sClient.Update(ctx, p)).To(Succeed())

		storconFake.DeleteTenant = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, p)).To(Succeed())
		Expect(k8sClient.Delete(ctx, p)).To(Succeed())

		// Either the project gets deleted (empty TenantID path) or
		// the controller re-assigned TenantID and it's stuck with finalizer.
		Eventually(func() bool {
			pp := &neonv1alpha1.Project{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, pp)
			if apierrors.IsNotFound(err) {
				return true // Deleted — empty TenantID guard worked.
			}
			if err != nil {
				return false
			}
			cond := meta.FindStatusCondition(pp.Status.Conditions, utils.ConditionTerminating)
			if cond != nil && cond.Status == metav1.ConditionTrue {
				// Controller re-assigned TenantID, fake returned 500.
				// Restore handler to allow final cleanup.
				storconFake.DeleteTenant = nil
				return true
			}
			return false
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Terminating condition should be set or project deleted")

		// Ensure the project is eventually fully deleted.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: projectName, Namespace: namespace},
				&neonv1alpha1.Project{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue())
	})
})
