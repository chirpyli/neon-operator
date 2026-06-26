package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
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
	return r.reconcileSafekeepers(ctx, cluster)
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
	dep := storagecontroller.Deployment(cluster)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, dep, func(cur *appsv1.Deployment) bool {
		return !equality.Semantic.DeepDerivative(dep.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	svc := storagecontroller.Service(cluster)
	return utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	})
}

func (r *ClusterReconciler) reconcileStorageBroker(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
	dep := storagebroker.Deployment(cluster)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, dep, func(cur *appsv1.Deployment) bool {
		return !equality.Semantic.DeepDerivative(dep.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	svc := storagebroker.Service(cluster)
	return utils.ReconcileSSA(ctx, r.Client, r.Scheme, cluster, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	})
}
