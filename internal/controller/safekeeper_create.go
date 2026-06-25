package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

func (r *SafekeeperReconciler) createSafekeeperResources(ctx context.Context, sk *neonv1alpha1.Safekeeper) error {
	log := logf.FromContext(ctx)

	log.Info("Reconciling safekeeper Service")
	svc := safekeeper.Service(sk)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, svc, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(svc.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("Reconciling safekeeper headless Service")
	headless := safekeeper.HeadlessService(sk)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, headless, func(cur *corev1.Service) bool {
		return !equality.Semantic.DeepDerivative(headless.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	log.Info("Reconciling safekeeper StatefulSet")
	var cluster neonv1alpha1.Cluster
	if err := r.Get(ctx, types.NamespacedName{Name: sk.Spec.Cluster, Namespace: sk.Namespace}, &cluster); err != nil {
		return fmt.Errorf("failed to get parent cluster: %w", err)
	}
	sts := safekeeper.StatefulSet(sk, cluster.Spec.NeonImage)
	if err := utils.ReconcileSSA(ctx, r.Client, r.Scheme, sk, sts, func(cur *appsv1.StatefulSet) bool {
		return !equality.Semantic.DeepDerivative(sts.Spec, cur.Spec)
	}); err != nil {
		return err
	}

	// 向 storage controller 注册 safekeeper。
	// 这是尽力而为的操作：如果失败，记录日志并在下次调和时重试。
	if err := r.registerSafekeeperWithStorageController(ctx, sk); err != nil {
		log.Info("Failed to register safekeeper with storage controller, will retry", "error", err)
	}

	return nil
}

// registerSafekeeperWithStorageController 向 storage controller 的管理 API 注册
// safekeeper 实例，使其可被发现并用于 timeline 分配。
//
// safekeeper ID 直接透传 —— operator 和 storage controller 均要求 ID >= 1。
func (r *SafekeeperReconciler) registerSafekeeperWithStorageController(
	ctx context.Context,
	sk *neonv1alpha1.Safekeeper,
) error {
	log := logf.FromContext(ctx)

	base := r.StorageControllerBaseURL
	if base == "" {
		base = storagecontroller.URL(sk.Spec.Cluster)
	}

	url := fmt.Sprintf("%s/control/v1/safekeeper/%d", base, sk.Spec.ID)

	host := fmt.Sprintf("%s.%s", safekeeper.Name(sk), sk.Namespace)

	body := map[string]any{
		"id":                   sk.Spec.ID,
		"region_id":            "se-ume", // fixme: 暂时硬编码，后续需要支持从集群配置文件中读取
		"version":              1,
		"host":                 host,
		"port":                 5454,
		"http_port":            7676,
		"availability_zone_id": "se-ume", // fixme: 暂时硬编码，后续需要支持从集群配置文件中读取
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal safekeeper registration body: %w", err)
	}

	log.Info("Registering safekeeper with storage controller",
		"url", url, "id", sk.Spec.ID, "host", host)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to register safekeeper with storage controller: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error(err, "failed to close response body")
		}
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Info("Successfully registered safekeeper with storage controller",
			"id", sk.Spec.ID)
		return nil
	}

	return fmt.Errorf("storage controller returned status %d for safekeeper registration", resp.StatusCode)
}
