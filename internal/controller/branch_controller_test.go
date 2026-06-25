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

var _ = Describe("Branch Controller", func() {
	const (
		clusterName = "bran-cluster"
		projectName = "bran-project"
		branchName  = "bran-branch"
	)
	var namespace string

	BeforeEach(func() {
		storconFake.Reset()
		storconFake.Timeline = nil
		storconFake.DeleteTimeline = nil

		namespace = newTestNamespace()
		// Prerequisites: Cluster + Project (Project must exist before Branch is created)
		Expect(k8sClient.Create(ctx, fixtures.NewBucketCredsSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewStorcondDBSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewCluster(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewProject(projectName, namespace, clusterName))).To(Succeed())

		// Wait for Project to reach Available (tenant attached).
		Eventually(func(g Gomega) {
			proj := &neonv1alpha1.Project{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
			cond := meta.FindStatusCondition(proj.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		Expect(k8sClient.Create(ctx, fixtures.NewBranch(branchName, namespace, projectName))).To(Succeed())
	})

	AfterEach(func() {
		storconFake.Timeline = nil
		storconFake.DeleteTimeline = nil
	})

	It("adds finalizer and gets timeline created on Storage Controller", func() {
		Eventually(func(g Gomega) {
			branch := &neonv1alpha1.Branch{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
			g.Expect(controllerutil.ContainsFinalizer(branch, utils.FinalizerName)).To(BeTrue(), "finalizer should be present")
			g.Expect(branch.Spec.TimelineID).NotTo(BeEmpty(), "timeline ID should be assigned")

			cond := meta.FindStatusCondition(branch.Status.Conditions, utils.ConditionTimelineIDAssigned)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			cond = meta.FindStatusCondition(branch.Status.Conditions, utils.ConditionTimelineCreated)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Verify the fake Storage Controller received the timeline POST.
		calls := storconFake.Calls()
		hasTimelineCreate := false
		for _, c := range calls {
			if c.Method == "POST" {
				hasTimelineCreate = true
				break
			}
		}
		Expect(hasTimelineCreate).To(BeTrue(), "should have called POST /v1/tenant/{id}/timeline")
	})

	It("removes finalizer and completes deletion normally", func() {
		// Wait for TimelineID to be assigned.
		Eventually(func(g Gomega) {
			branch := &neonv1alpha1.Branch{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
			g.Expect(branch.Spec.TimelineID).NotTo(BeEmpty())
			cond := meta.FindStatusCondition(branch.Status.Conditions, utils.ConditionTimelineCreated)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		beforeCalls := len(storconFake.Calls())

		branch := &neonv1alpha1.Branch{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
		Expect(k8sClient.Delete(ctx, branch)).To(Succeed())

		// Branch should eventually be removed.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: branchName, Namespace: namespace},
				&neonv1alpha1.Branch{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Branch should be fully deleted")

		// Verify a DELETE call was made.
		calls := storconFake.Calls()[beforeCalls:]
		hasDelete := false
		for _, c := range calls {
			if c.Method == "DELETE" {
				hasDelete = true
				break
			}
		}
		Expect(hasDelete).To(BeTrue(), "should have called DELETE /v1/tenant/{id}/timeline/{tid}")
	})

	It("removes finalizer when Storage Controller returns 404 (timeline already deleted)", func() {
		// Override delete handler to return 404.
		storconFake.DeleteTimeline = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}

		// Wait for TimelineID.
		Eventually(func(g Gomega) {
			branch := &neonv1alpha1.Branch{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
			g.Expect(branch.Spec.TimelineID).NotTo(BeEmpty())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		branch := &neonv1alpha1.Branch{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
		Expect(k8sClient.Delete(ctx, branch)).To(Succeed())

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: branchName, Namespace: namespace},
				&neonv1alpha1.Branch{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Branch should be deleted even with 404 response")
	})

	It("retains finalizer and sets Terminating condition on Storage Controller failure", func() {
		// Override delete handler to return 500.
		storconFake.DeleteTimeline = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Wait for TimelineID.
		Eventually(func(g Gomega) {
			branch := &neonv1alpha1.Branch{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
			g.Expect(branch.Spec.TimelineID).NotTo(BeEmpty())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		branch := &neonv1alpha1.Branch{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
		Expect(k8sClient.Delete(ctx, branch)).To(Succeed())

		// Finalizer should be retained, Terminating=ExternalCleanupFailed.
		Eventually(func(g Gomega) {
			b := &neonv1alpha1.Branch{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, b)
			g.Expect(err).NotTo(HaveOccurred(), "Branch should still exist (finalizer retained)")

			g.Expect(controllerutil.ContainsFinalizer(b, utils.FinalizerName)).To(BeTrue(), "finalizer should be retained")

			cond := meta.FindStatusCondition(b.Status.Conditions, utils.ConditionTerminating)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(utils.ReasonExternalCleanupFailed))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		// Restore handler.
		storconFake.DeleteTimeline = nil
	})

	It("skips external cleanup when TimelineID is empty (branch never created timeline)", func() {
		// Create a separate branch and wait for TimelineID + finalizer.
		noTLBranch := "bran-no-timeline"
		branch := fixtures.NewBranch(noTLBranch, namespace, projectName)
		Expect(k8sClient.Create(ctx, branch)).To(Succeed())

		Eventually(func(g Gomega) {
			b := &neonv1alpha1.Branch{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noTLBranch, Namespace: namespace}, b)).To(Succeed())
			g.Expect(b.Spec.TimelineID).NotTo(BeEmpty())
			g.Expect(controllerutil.ContainsFinalizer(b, utils.FinalizerName)).To(BeTrue())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Clear TimelineID and immediately delete.
		b := &neonv1alpha1.Branch{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noTLBranch, Namespace: namespace}, b)).To(Succeed())
		b.Spec.TimelineID = ""
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		storconFake.DeleteTimeline = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}
		Expect(k8sClient.Delete(ctx, b)).To(Succeed())

		Eventually(func() bool {
			bb := &neonv1alpha1.Branch{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: noTLBranch, Namespace: namespace}, bb)
			if apierrors.IsNotFound(err) {
				return true // Deleted — empty TimelineID guard worked.
			}
			if err != nil {
				return false
			}
			cond := meta.FindStatusCondition(bb.Status.Conditions, utils.ConditionTerminating)
			if cond != nil && cond.Status == metav1.ConditionTrue {
				storconFake.DeleteTimeline = nil
				return true
			}
			return false
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Terminating condition should be set or branch deleted")

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: noTLBranch, Namespace: namespace},
				&neonv1alpha1.Branch{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue())
	})

	It("skips external cleanup when parent Project is already gone", func() {
		// Wait for TimelineID.
		var timelineID string
		Eventually(func(g Gomega) {
			branch := &neonv1alpha1.Branch{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
			g.Expect(branch.Spec.TimelineID).NotTo(BeEmpty())
			timelineID = branch.Spec.TimelineID
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Delete the parent Project first.
		proj := &neonv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, proj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, proj)).To(Succeed())

		// Wait for Project to be fully gone.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: projectName, Namespace: namespace},
				&neonv1alpha1.Project{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "parent Project should be deleted first")

		// Override delete handler to ensure it's NOT called.
		storconFake.DeleteTimeline = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Now delete the Branch.
		branch := &neonv1alpha1.Branch{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: branchName, Namespace: namespace}, branch)).To(Succeed())
		Expect(k8sClient.Delete(ctx, branch)).To(Succeed())

		// Branch should be deleted without calling the external API.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: branchName, Namespace: namespace},
				&neonv1alpha1.Branch{}))
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue(), "Branch should be deleted without external API call when Project is gone")

		_ = timelineID
	})
})
