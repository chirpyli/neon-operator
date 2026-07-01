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
	"oltp.molnett.org/neon-operator/specs/pageserver"
	"oltp.molnett.org/neon-operator/utils"
)

func (r *PageserverReconciler) createPageserverResources(ctx context.Context, ps *neonv1alpha1.Pageserver) error {
	log := logf.FromContext(ctx)

	// 获取或生成 PS 的 JWT token（PS → SC upcall 和 PS → SK WAL 认证使用）。
	// Token 持久化到 JWT Secret，避免每次 reconcile 重新生成导致 StatefulSet 频繁滚动更新。
	controlPlaneAPIToken, safekeeperAuthToken, err := r.ensurePSAuthTokens(ctx, ps)
	if err != nil {
		return err
	}

	log.Info("Reconciling pageserver ConfigMap")
	if err := r.reconcileConfigMap(ctx, ps, controlPlaneAPIToken); err != nil {
		return err
	}

	log.Info("Reconciling pageserver Service")
	svc := pageserver.Service(ps)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, ps, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("Reconciling pageserver headless Service")
	headless := pageserver.HeadlessService(ps)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, ps, headless, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(headless.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("Reconciling pageserver PDB")
	pdb := pageserver.PodDisruptionBudget(ps)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, ps, pdb, func(cur *policyv1.PodDisruptionBudget) bool {
		return !equality.Semantic.DeepDerivative(pdb.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("Reconciling pageserver StatefulSet")
	var cluster neonv1alpha1.Cluster
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Spec.Cluster, Namespace: ps.Namespace}, &cluster); err != nil {
		return fmt.Errorf("failed to get parent cluster: %w", err)
	}
	sts := pageserver.StatefulSet(ps, cluster.Spec.NeonImage, safekeeperAuthToken)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, ps, sts, func(cur *appsv1.StatefulSet) bool {
		return !equality.Semantic.DeepDerivative(sts.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	// 向 Storage Controller 同步 pageserver 状态（如已注册）
	r.syncSCState(ctx, ps)

	return nil
}

func (r *PageserverReconciler) reconcileConfigMap(ctx context.Context, ps *neonv1alpha1.Pageserver, controlPlaneAPIToken string) error {
	var bucketSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Spec.BucketCredentialsSecret.Name, Namespace: ps.Namespace}, &bucketSecret); err != nil {
		return fmt.Errorf("failed to get bucket credentials secret: %w", err)
	}

	cm := pageserver.ConfigMap(ps, &bucketSecret, controlPlaneAPIToken)
	return utils.ReconcileSSA(ctx, r.Client, r.Scheme, ps, cm, func(cur *corev1.ConfigMap) bool {
		return !equality.Semantic.DeepDerivative(cm.Data, cur.Data)
	})
}

// ensurePSAuthTokens 从 JWT Secret 中读取 pageserver 专用的两个 token，
// 如果不存在则生成并持久化。避免每次 reconcile 重新签发，
// 导致 ConfigMap 和 StatefulSet 频繁更新，引发 pod 反复重建。
//   - controlPlaneAPIToken（generations_api scope）：PS → SC upcall
//   - safekeeperAuthToken（safekeeperdata scope）：PS → SK WAL 认证
func (r *PageserverReconciler) ensurePSAuthTokens(ctx context.Context, ps *neonv1alpha1.Pageserver) (string, string, error) {
	log := logf.FromContext(ctx)

	secretName := utils.JWTSecretName(ps.Spec.Cluster)
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ps.Namespace}, &secret); err != nil {
		log.Info("JWT Secret 尚未就绪，等下次 reconcile 重试",
			"secret", secretName, "error", err)
		return "", "", nil
	}

	// 从 Secret 中读取已持久化的 token
	controlPlaneAPIToken := string(secret.Data["pageserver_control_plane_token"])
	safekeeperAuthToken := string(secret.Data["pageserver_safekeeper_token"])

	// 如果两个 token 都已存在，直接复用，保持 ConfigMap/StatefulSet spec 稳定。
	if controlPlaneAPIToken != "" && safekeeperAuthToken != "" {
		return controlPlaneAPIToken, safekeeperAuthToken, nil
	}

	log.Info("正在生成新的 pageserver JWT token 并持久化到 Secret")

	jm, err := utils.NewJWTManagerFromSecret(&secret)
	if err != nil {
		log.Error(err, "从 Secret 创建 JWT manager 失败", "secret", secretName)
		return "", "", nil
	}

	// 生成 control_plane_api_token（generations_api scope）
	if controlPlaneAPIToken == "" {
		controlPlaneAPIToken, err = utils.GenerateUpcallToken(jm, ps.Spec.Cluster)
		if err != nil {
			log.Error(err, "生成 control_plane_api_token 失败")
			return "", "", nil
		}
	}

	// 生成 safekeeper auth token（safekeeperdata scope）
	if safekeeperAuthToken == "" {
		safekeeperAuthToken, err = utils.GenerateSafekeeperToken(jm, ps.Spec.Cluster)
		if err != nil {
			log.Error(err, "生成 safekeeper auth token 失败")
			return "", "", nil
		}
	}

	// 持久化到 Secret，后续 reconcile 直接复用。
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data["pageserver_control_plane_token"] = []byte(controlPlaneAPIToken)
	secret.Data["pageserver_safekeeper_token"] = []byte(safekeeperAuthToken)

	if err := r.Update(ctx, &secret); err != nil {
		return "", "", fmt.Errorf("持久化 pageserver token 到 Secret 失败: %w", err)
	}

	return controlPlaneAPIToken, safekeeperAuthToken, nil
}
