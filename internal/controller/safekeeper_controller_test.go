package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/test/fixtures"
	"oltp.molnett.org/neon-operator/utils"
)

var _ = Describe("Safekeeper Controller", func() {
	const (
		clusterName    = "sk-suite"
		safekeeperName = "sk-suite-sk0"
		safekeeperID   = uint32(1)
	)
	var (
		namespace string
		stsName   = clusterName + "-safekeeper-1"
	)

	BeforeEach(func() {
		storconFake.Reset()
		storconFake.RegisterSafekeeper = nil
		storconFake.DecommissionSafekeeper = nil

		namespace = newTestNamespace()
		Expect(k8sClient.Create(ctx, fixtures.NewBucketCredsSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewCluster(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewSafekeeper(safekeeperName, namespace, clusterName, safekeeperID))).To(Succeed())
	})

	AfterEach(func() {
		storconFake.RegisterSafekeeper = nil
		storconFake.DecommissionSafekeeper = nil
	})

	It("creates StatefulSet and services owned by the Safekeeper CR", func() {
		Eventually(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, sts)).To(Succeed())
			g.Expect(sts.OwnerReferences).To(HaveLen(1))
			g.Expect(sts.OwnerReferences[0].Kind).To(Equal("Safekeeper"))
			g.Expect(sts.Spec.ServiceName).To(Equal(stsName + "-headless"))
			g.Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))

			svc := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, svc)).To(Succeed())
			g.Expect(svc.Spec.ClusterIP).NotTo(Equal(corev1.ClusterIPNone))

			headless := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stsName + "-headless", Namespace: namespace}, headless)).To(Succeed())
			g.Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("flips Available to True once the StatefulSet reports ready replicas", func() {
		sts := &appsv1.StatefulSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, sts)
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			sk := &neonv1alpha1.Safekeeper{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
			cond := meta.FindStatusCondition(sk.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())

		patch := sts.DeepCopy()
		patch.Status.ObservedGeneration = sts.Generation
		patch.Status.Replicas = 1
		patch.Status.ReadyReplicas = 1
		patch.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Patch(ctx, patch, client.MergeFrom(sts))).To(Succeed())

		Eventually(func(g Gomega) {
			sk := &neonv1alpha1.Safekeeper{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
			cond := meta.FindStatusCondition(sk.Status.Conditions, utils.ConditionAvailable)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("removes finalizer and completes deletion normally", func() {
		// Wait for Safekeeper to be created and stable.
		Eventually(func(g Gomega) {
			sk := &neonv1alpha1.Safekeeper{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
			g.Expect(controllerutil.ContainsFinalizer(sk, utils.FinalizerName)).To(BeTrue())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		sk := &neonv1alpha1.Safekeeper{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
		Expect(k8sClient.Delete(ctx, sk)).To(Succeed())

		// Safekeeper should eventually be removed.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: safekeeperName, Namespace: namespace},
				&neonv1alpha1.Safekeeper{}))
		}, 15*time.Second, 200*time.Millisecond).Should(BeTrue(), "Safekeeper should be fully deleted")
	})

	It("registers safekeeper with Storage Controller and sets RegisteredWithSC", func() {
		// Wait for safekeeper reconciliation to register with SC.
		Eventually(func(g Gomega) {
			sk := &neonv1alpha1.Safekeeper{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
			g.Expect(sk.Status.RegisteredWithSC).To(BeTrue())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Verify the SC Register endpoint was called.
		calls := storconFake.Calls()
		var found bool
		for _, c := range calls {
			if c.Method == "POST" && c.Path == "/control/v1/safekeeper/1" {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "Expected RegisterSafekeeper call to SC was not made")
	})

	It("calls DecommissionSafekeeper in Storage Controller on deletion", func() {
		// Wait for safekeeper to be stable.
		Eventually(func(g Gomega) {
			sk := &neonv1alpha1.Safekeeper{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
			g.Expect(controllerutil.ContainsFinalizer(sk, utils.FinalizerName)).To(BeTrue())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		// Reset calls so we only see the decommission call.
		storconFake.Reset()

		sk := &neonv1alpha1.Safekeeper{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: safekeeperName, Namespace: namespace}, sk)).To(Succeed())
		Expect(k8sClient.Delete(ctx, sk)).To(Succeed())

		// Verify DecommissionSafekeeper was called with correct body.
		Eventually(func() bool {
			for _, c := range storconFake.Calls() {
				if c.Method == "POST" && c.Path == "/control/v1/safekeeper/1/scheduling_policy" {
					return true
				}
			}
			return false
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue(),
			"Expected DecommissionSafekeeper call to SC was not made")
	})

})
