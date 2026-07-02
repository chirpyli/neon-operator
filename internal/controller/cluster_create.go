package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	pageserverspec "oltp.molnett.org/neon-operator/specs/pageserver"
	safekeeperspec "oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/specs/storagebroker"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

func (r *ClusterReconciler) createClusterResources(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
	log := logf.FromContext(ctx)

	log.Info("Reconciling JWT keys")
	if err := r.reconcileJWTKeys(ctx, cluster); err != nil {
		return err
	}

	log.Info("Reconciling storage controller")
	if err := r.reconcileStorageController(ctx, cluster); err != nil {
		return err
	}

	log.Info("Reconciling storage broker")
	if err := r.reconcileStorageBroker(ctx, cluster); err != nil {
		return err
	}

	log.Info("Reconciling safekeepers")
	if err := r.reconcileSafekeepers(ctx, cluster); err != nil {
		return err
	}

	log.Info("Reconciling pageservers")
	return r.reconcilePageservers(ctx, cluster)
}

// reconcileSafekeepers 确保集群的 Safekeeper CR 数量正确，
// 创建缺失的 CR 并删除多余的 CR。
//
// 注意：K8s 工作负载（StatefulSet、Service、PDB 等）的创建由
// Safekeeper Controller 负责，遵循设计文档中的微控制器模式。
func (r *ClusterReconciler) reconcileSafekeepers(
	ctx context.Context,
	cluster *neonv1alpha1.Cluster,
) error {
	desired := int(cluster.Spec.NumSafekeepers)

	// 列出此集群已拥有的 Safekeeper CR
	var existing neonv1alpha1.SafekeeperList
	if err := r.List(ctx, &existing,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			safekeeperspec.ClusterLabel: cluster.Name,
		},
	); err != nil {
		return fmt.Errorf("列出 safekeeper: %w", err)
	}

	// 建立已有 ID 的映射
	existingMap := make(map[uint32]*neonv1alpha1.Safekeeper)
	for i := range existing.Items {
		sk := &existing.Items[i]
		existingMap[sk.Spec.ID] = sk
	}

	// 同步节点故障恢复配置到已存在的 Safekeeper CR。
	// 这样在升级 operator 后，已有 Safekeeper 也能获得 NodeFailure 配置，
	// 无需手动重建 CR。
	var desiredNodeFailure *neonv1alpha1.NodeFailureRecoveryConfig
	if cluster.Spec.DefaultSafekeeperConfig != nil {
		desiredNodeFailure = cluster.Spec.DefaultSafekeeperConfig.NodeFailure
	}
	for _, sk := range existingMap {
		if sk.Spec.ID > uint32(desired) {
			continue // 跳过即将被删除的 CR
		}
		if !equality.Semantic.DeepEqual(sk.Spec.NodeFailure, desiredNodeFailure) {
			patched := sk.DeepCopy()
			patched.Spec.NodeFailure = desiredNodeFailure
			if err := r.Patch(ctx, patched, client.MergeFrom(sk)); err != nil {
				return fmt.Errorf("更新 safekeeper %d NodeFailure 配置: %w", sk.Spec.ID, err)
			}
			logf.FromContext(ctx).Info("已同步 Safekeeper NodeFailure 配置",
				"safekeeper", sk.Name, "nodeFailure", desiredNodeFailure)
		}
	}

	// 创建缺失的 Safekeeper CR（ID 从 1 开始）
	for id := uint32(1); id <= uint32(desired); id++ {
		if _, exists := existingMap[id]; !exists {
			if err := r.createSafekeeper(ctx, cluster, id); err != nil {
				return fmt.Errorf("创建 safekeeper %d: %w", id, err)
			}
		}
	}

	// 删除多余的 Safekeeper CR（缩容）
	for _, sk := range existingMap {
		if sk.Spec.ID > uint32(desired) {
			if err := r.deleteSafekeeper(ctx, sk); err != nil {
				return fmt.Errorf("删除 safekeeper %d: %w", sk.Spec.ID, err)
			}
		}
	}

	return nil
}

// createSafekeeper 为指定集群和 ID 创建一个新的 Safekeeper CR。
func (r *ClusterReconciler) createSafekeeper(
	ctx context.Context,
	cluster *neonv1alpha1.Cluster,
	id uint32,
) error {
	log := logf.FromContext(ctx)

	storageConfig := neonv1alpha1.StorageConfig{Size: "10Gi"}
	if cluster.Spec.DefaultSafekeeperStorage != nil {
		storageConfig = *cluster.Spec.DefaultSafekeeperStorage
	}

	sk := &neonv1alpha1.Safekeeper{
		ObjectMeta: metav1.ObjectMeta{
			Name:      safekeeperName(cluster.Name, id),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				safekeeperspec.ClusterLabel: cluster.Name,
			},
		},
		Spec: neonv1alpha1.SafekeeperSpec{
			ID:            id,
			Cluster:       cluster.Name,
			StorageConfig: storageConfig,
		},
	}

	// 传递节点故障恢复配置
	if cluster.Spec.DefaultSafekeeperConfig != nil && cluster.Spec.DefaultSafekeeperConfig.NodeFailure != nil {
		sk.Spec.NodeFailure = cluster.Spec.DefaultSafekeeperConfig.NodeFailure
	}

	if err := ctrl.SetControllerReference(cluster, sk, r.Scheme); err != nil {
		return err
	}

	log.Info("正在创建 Safekeeper CR", "name", sk.Name, "id", id)
	return r.Create(ctx, sk)
}

// deleteSafekeeper 发起删除一个多余的 Safekeeper CR。
func (r *ClusterReconciler) deleteSafekeeper(
	ctx context.Context,
	sk *neonv1alpha1.Safekeeper,
) error {
	log := logf.FromContext(ctx)

	if sk.DeletionTimestamp != nil {
		return nil // 已在删除中
	}

	log.Info("正在删除多余的 Safekeeper CR", "name", sk.Name, "id", sk.Spec.ID)
	return r.Delete(ctx, sk)
}

// safekeeperName 返回 Safekeeper CR 的标准名称。
func safekeeperName(clusterName string, id uint32) string {
	return fmt.Sprintf("%s-safekeeper-%d", clusterName, id)
}

func (r *ClusterReconciler) reconcileJWTKeys(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
	log := logf.FromContext(ctx)

	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("cluster-%s-jwt", cluster.Name), Namespace: cluster.Namespace}, &secret)
	if err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	log.Info("正在为集群创建新的 JWT 密钥 Secret", "cluster", cluster.Name)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}

	privKeyVBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	privKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privKeyVBytes})

	pubKeyVBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	pubKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubKeyVBytes})

	jwtSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-%s-jwt", cluster.Name),
			Namespace: cluster.Namespace,
		},
		Data: map[string][]byte{
			"private.pem": privKeyPEM,
			"public.pem":  pubKeyPEM,
		},
	}

	if err := ctrl.SetControllerReference(cluster, jwtSecret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}
	return r.Create(ctx, jwtSecret)
}

func (r *ClusterReconciler) reconcileStorageController(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
	// 读取 JWT Secret，提取公钥 PEM 和组件 token。
	// SC 二进制通过环境变量读取密钥（PUBLIC_KEY、PAGESERVER_JWT_TOKEN 等）。
	var jwtSecret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{
		Name:      utils.JWTSecretName(cluster.Name),
		Namespace: cluster.Namespace,
	}, &jwtSecret); err != nil {
		return err
	}
	jm, err := utils.NewJWTManagerFromSecret(&jwtSecret)
	if err != nil {
		return fmt.Errorf("create JWT manager: %w", err)
	}

	// 去除换行符，以便安全地通过环境变量传递
	publicKeyPEM := strings.ReplaceAll(string(jwtSecret.Data["public.pem"]), "\n", "")
	publicKeyPEM = strings.ReplaceAll(publicKeyPEM, "\r", "")

	// 获取或生成组件 JWT token（持久化到 Secret 中，避免重复生成导致抖动）。
	tokens, err := r.ensureComponentTokens(ctx, cluster, jm, &jwtSecret)
	if err != nil {
		return fmt.Errorf("ensure component tokens: %w", err)
	}

	dep := storagecontroller.Deployment(cluster, publicKeyPEM, tokens.pageserver, tokens.controlPlane, tokens.safekeeper)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, dep, func(cur *appsv1.Deployment) bool {
		return !equality.Semantic.DeepDerivative(dep.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	pdb := storagecontroller.PodDisruptionBudget(cluster.Name, cluster.Namespace)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, pdb, func(cur *policyv1.PodDisruptionBudget) bool {
		return !equality.Semantic.DeepDerivative(pdb.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	svc := storagecontroller.Service(cluster)
	return utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	})
}

type componentTokens struct {
	pageserver, controlPlane, safekeeper string
}

// ensureComponentTokens 从 Secret 中读取组件 JWT token，如果不存在则生成并持久化。
// 避免每次 reconcile 都重新签发 token，导致 Deployment 频繁更新、ReplicaSet 大量堆积。
func (r *ClusterReconciler) ensureComponentTokens(
	ctx context.Context,
	cluster *neonv1alpha1.Cluster,
	jm *utils.JWTManager,
	secret *corev1.Secret,
) (*componentTokens, error) {
	log := logf.FromContext(ctx)

	tokens := &componentTokens{
		pageserver:   string(secret.Data["pageserver_token"]),
		controlPlane: string(secret.Data["control_plane_token"]),
		safekeeper:   string(secret.Data["safekeeper_token"]),
	}

	// 检查已有 token 是否过期，过期则需要重新签发。
	// GenerateScopeToken 通过 SHA256(key+cluster+scope) 确定性计算 exp，
	// 部分 hash 组合可能导致 exp 已经过去，必须检测并重新生成。
	tokenExpired := func(tokenStr string) bool {
		return tokenStr != "" && jm.IsTokenExpired(tokenStr)
	}
	anyExpired := tokenExpired(tokens.pageserver) || tokenExpired(tokens.controlPlane) || tokenExpired(tokens.safekeeper)

	// 如果所有 token 已存在且未过期，直接复用以保证 Deployment spec 稳定不变。
	if !anyExpired && tokens.pageserver != "" && tokens.controlPlane != "" && tokens.safekeeper != "" {
		return tokens, nil
	}

	if anyExpired {
		log.Info("检测到 JWT token 已过期，重新生成",
			"pageserver_expired", tokenExpired(tokens.pageserver),
			"controlPlane_expired", tokenExpired(tokens.controlPlane),
			"safekeeper_expired", tokenExpired(tokens.safekeeper))
	} else {
		log.Info("正在生成新的组件 JWT token 并持久化到 Secret")
	}

	var err error
	tokens.pageserver, err = jm.GenerateScopeToken(cluster.Name, utils.ScopePageServerAPI, utils.TokenDefaultLifetime)
	if err != nil {
		return nil, fmt.Errorf("generate pageserver token: %w", err)
	}
	tokens.controlPlane, err = jm.GenerateScopeToken(cluster.Name, utils.ScopeAdmin, utils.TokenDefaultLifetime)
	if err != nil {
		return nil, fmt.Errorf("generate control plane token: %w", err)
	}
	tokens.safekeeper, err = jm.GenerateScopeToken(cluster.Name, utils.ScopeSafekeeperData, utils.TokenDefaultLifetime)
	if err != nil {
		return nil, fmt.Errorf("generate safekeeper token: %w", err)
	}

	// 持久化到 Secret，后续 reconcile 直接复用。
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data["pageserver_token"] = []byte(tokens.pageserver)
	secret.Data["control_plane_token"] = []byte(tokens.controlPlane)
	secret.Data["safekeeper_token"] = []byte(tokens.safekeeper)

	if err := r.Update(ctx, secret); err != nil {
		return nil, fmt.Errorf("persist component tokens to secret: %w", err)
	}

	return tokens, nil
}

func (r *ClusterReconciler) reconcileStorageBroker(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
	dep := storagebroker.Deployment(cluster)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, dep, func(cur *appsv1.Deployment) bool {
		return !equality.Semantic.DeepDerivative(dep.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	pdb := storagebroker.PodDisruptionBudget(cluster.Name, cluster.Namespace)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, pdb, func(cur *policyv1.PodDisruptionBudget) bool {
		return !equality.Semantic.DeepDerivative(pdb.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	svc := storagebroker.Service(cluster)
	return utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	})
}

// reconcilePageservers 确保集群的 Pageserver CR 数量正确，
// 创建缺失的 CR 并删除多余的 CR（缩容时执行安全 drain）。
func (r *ClusterReconciler) reconcilePageservers(
	ctx context.Context,
	cluster *neonv1alpha1.Cluster,
) error {
	desired := int(cluster.Spec.NumPageservers)

	// 列出此集群已拥有的 Pageserver CR
	var existing neonv1alpha1.PageserverList
	if err := r.List(ctx, &existing,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			pageserverspec.ClusterLabel: cluster.Name,
		},
	); err != nil {
		return fmt.Errorf("列出 pageserver: %w", err)
	}

	// 建立已有 ID 的映射
	existingMap := make(map[uint64]*neonv1alpha1.Pageserver)
	for i := range existing.Items {
		ps := &existing.Items[i]
		existingMap[ps.Spec.ID] = ps
	}

	// 创建缺失的 Pageserver CR（ID 从 1 开始）
	for id := uint64(1); id <= uint64(desired); id++ {
		if _, exists := existingMap[id]; !exists {
			if err := r.createPageserver(ctx, cluster, id); err != nil {
				return fmt.Errorf("创建 pageserver %d: %w", id, err)
			}
		}
	}

	// 删除多余的 Pageserver CR（缩容，按 ID 降序）
	for _, ps := range existingMap {
		if ps.Spec.ID > uint64(desired) {
			if err := r.deletePageserver(ctx, ps); err != nil {
				return fmt.Errorf("删除 pageserver %d: %w", ps.Spec.ID, err)
			}
		}
	}

	return nil
}

// createPageserver 为指定集群和 ID 创建一个新的 Pageserver CR。
func (r *ClusterReconciler) createPageserver(
	ctx context.Context,
	cluster *neonv1alpha1.Cluster,
	id uint64,
) error {
	log := logf.FromContext(ctx)

	storageSize := "100Gi"
	initialSchedulingPolicy := "Active"
	if cluster.Spec.DefaultPageserverConfig != nil {
		if cluster.Spec.DefaultPageserverConfig.StorageSize != "" {
			storageSize = cluster.Spec.DefaultPageserverConfig.StorageSize
		}
		if cluster.Spec.DefaultPageserverConfig.InitialSchedulingPolicy != "" {
			initialSchedulingPolicy = cluster.Spec.DefaultPageserverConfig.InitialSchedulingPolicy
		}
	}

	ps := &neonv1alpha1.Pageserver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pageserverName(cluster.Name, id),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				pageserverspec.ClusterLabel: cluster.Name,
			},
		},
		Spec: neonv1alpha1.PageserverSpec{
			ID:                      id,
			Cluster:                 cluster.Name,
			InitialSchedulingPolicy: initialSchedulingPolicy,
			BucketCredentialsSecret: cluster.Spec.BucketCredentialsSecret,
			StorageConfig: neonv1alpha1.StorageConfig{
				Size: storageSize,
				// StorageClass 由 PS Controller 使用默认值或从集群配置获取
			},
		},
	}

	// 传递资源限制配置
	if cluster.Spec.DefaultPageserverConfig != nil && cluster.Spec.DefaultPageserverConfig.Resources != nil {
		ps.Spec.Resources = cluster.Spec.DefaultPageserverConfig.Resources
	}

	// 传递节点故障恢复配置
	if cluster.Spec.DefaultPageserverConfig != nil && cluster.Spec.DefaultPageserverConfig.NodeFailure != nil {
		ps.Spec.NodeFailure = cluster.Spec.DefaultPageserverConfig.NodeFailure
	}

	if err := ctrl.SetControllerReference(cluster, ps, r.Scheme); err != nil {
		return err
	}

	log.Info("正在创建 Pageserver CR", "name", ps.Name, "id", id)
	return r.Create(ctx, ps)
}

// deletePageserver 发起删除一个多余的 Pageserver CR。
// 缩容时 Cluster Controller 发起删除后，PS Controller 的 finalizer
// 会先执行 SC drain 再移除 finalizer（安全下线）。
func (r *ClusterReconciler) deletePageserver(
	ctx context.Context,
	ps *neonv1alpha1.Pageserver,
) error {
	log := logf.FromContext(ctx)

	if ps.DeletionTimestamp != nil {
		return nil // 已在删除中
	}

	log.Info("正在删除多余的 Pageserver CR（将触发安全下线）", "name", ps.Name, "id", ps.Spec.ID)
	return r.Delete(ctx, ps)
}

// pageserverName 返回 Pageserver CR 的标准名称。
func pageserverName(clusterName string, id uint64) string {
	return fmt.Sprintf("%s-pageserver-%d", clusterName, id)
}
