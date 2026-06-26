package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/utils"
)

func (r *SafekeeperReconciler) createSafekeeperResources(ctx context.Context, sk *neonv1alpha1.Safekeeper) error {
	log := logf.FromContext(ctx)

	log.Info("调和 safekeeper Service")
	svc := safekeeper.Service(sk)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("调和 safekeeper Headless Service")
	headless := safekeeper.HeadlessService(sk)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, headless, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(headless.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("调和 safekeeper PDB")
	pdb := safekeeper.PodDisruptionBudget(sk)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, pdb, func(cur *policyv1.PodDisruptionBudget) bool {
		return !equality.Semantic.DeepDerivative(pdb.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("调和 safekeeper StatefulSet")
	var cluster neonv1alpha1.Cluster
	if err := r.Get(ctx, types.NamespacedName{Name: sk.Spec.Cluster, Namespace: sk.Namespace}, &cluster); err != nil {
		return fmt.Errorf("获取父集群失败: %w", err)
	}
	sts := safekeeper.StatefulSet(sk, cluster.Spec.NeonImage)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, sts, func(cur *appsv1.StatefulSet) bool {
		return !equality.Semantic.DeepDerivative(sts.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	// 向 Storage Controller 注册 safekeeper。
	// 尽力而为：如果失败，仅记录日志并在下次调和时重试。
	regErr := r.SCClient.RegisterSafekeeper(ctx, sk)
	if regErr != nil {
		log.Info("向 Storage Controller 注册 safekeeper 失败，将在下次调和时重试", "error", regErr)
	}
	// 更新 RegisteredWithSC 状态，方便用户通过 kubectl 查看注册状态。
	if patchErr := utils.PatchStatus(ctx, r.Client, sk, func(s *neonv1alpha1.Safekeeper) {
		s.Status.RegisteredWithSC = (regErr == nil)
	}); patchErr != nil {
		log.Error(patchErr, "更新 registeredWithSC 状态失败")
	}

	return nil
}
