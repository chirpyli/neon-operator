package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/compute"
	"oltp.molnett.org/neon-operator/utils"
)

func (r *EndpointReconciler) createEndpointResources(ctx context.Context, endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	log.Info("Reconciling endpoint ConfigMap")
	if err := r.reconcileEndpointConfigMap(ctx, endpoint, branch, project); err != nil {
		return err
	}

	log.Info("Reconciling endpoint Deployment")
	if err := r.reconcileEndpointDeployment(ctx, endpoint, branch, project); err != nil {
		return err
	}

	log.Info("Reconciling endpoint admin Service")
	if err := r.reconcileEndpointAdminService(ctx, endpoint, branch, project); err != nil {
		return err
	}

	log.Info("Reconciling endpoint postgres Service")
	if err := r.reconcileEndpointPostgresService(ctx, endpoint, branch, project); err != nil {
		return err
	}

	return nil
}

func (r *EndpointReconciler) reconcileEndpointConfigMap(ctx context.Context, endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	cluster, err := r.getCluster(ctx, project.Spec.ClusterName, branch.Namespace)
	if err != nil {
		return err
	}

	var jwkSecret corev1.Secret
	err = r.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("cluster-%s-jwt", cluster.Name), Namespace: cluster.Namespace}, &jwkSecret)
	if err != nil {
		return err
	}

	intendedConfigMap, err := compute.EndpointConfigMap(ctx, r.Client, endpoint, branch, project, jwkSecret)
	if err != nil {
		return err
	}

	var currentConfigMap corev1.ConfigMap
	getErr := r.Get(ctx, types.NamespacedName{Name: intendedConfigMap.Name, Namespace: endpoint.Namespace}, &currentConfigMap)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("failed to get endpoint ConfigMap: %w", getErr)
	}

	err = ctrl.SetControllerReference(endpoint, intendedConfigMap, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set controller reference for endpoint ConfigMap: %w", err)
	}

	if apierrors.IsNotFound(getErr) {
		if err := r.Create(ctx, intendedConfigMap, &client.CreateOptions{
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to create endpoint ConfigMap: %w", err)
		}
		log.Info("Endpoint ConfigMap created", "name", endpoint.Name)
		return nil
	}

	configMapChanged := !equality.Semantic.DeepDerivative(intendedConfigMap.Data, currentConfigMap.Data)

	if configMapChanged {
		if err := r.Patch(ctx, intendedConfigMap, client.Apply, &client.PatchOptions{
			Force:        ptr.To(true),
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to update endpoint ConfigMap: %w", err)
		}
		log.Info("Endpoint ConfigMap updated", "name", endpoint.Name)

		// [Phase 2.5] ConfigMap 更新后尝试热推送到 running Pod
		if err := r.tryHotReload(ctx, endpoint); err != nil {
			log.Info("Hot reload skipped (pod will pick up config on restart)",
				"endpoint", endpoint.Name, "reason", err)
		}
		return nil
	}

	return nil
}

func (r *EndpointReconciler) reconcileEndpointDeployment(ctx context.Context, endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	// Compute 镜像使用独立的 compute-node 镜像（含 compute_ctl），
	// 与 cluster.Spec.NeonImage（含 pageserver/safekeeper 等）不同。
	// 优先使用 Cluster.Spec.ComputeImage；为空时从 NeonImage 提取 registry 前缀，
	// 优先从本地 registry 拉取，避免依赖 Docker Hub。
	cluster, err := r.getCluster(ctx, project.Spec.ClusterName, branch.Namespace)
	if err != nil {
		return err
	}
	computeImage := deriveComputeImage(cluster.Spec.ComputeImage, cluster.Spec.NeonImage, branch.Spec.PGVersion)
	intendedDeployment := compute.EndpointDeployment(endpoint, branch, project, computeImage)

	// 应用资源配置：Endpoint.Spec.Resources > Project.DefaultEndpointSettings > 默认
	resources := getEffectiveResources(endpoint, project)
	if resources != nil {
		intendedDeployment.Spec.Template.Spec.Containers[0].Resources = *resources
	}

	// 读取当前 spec ConfigMap 并计算 checksum，注入到 PodTemplate annotation。
	// 当 ConfigMap 变更（如 role/database 的 SCRAM 密钥就绪后更新）时，
	// Deployment Spec 也会变化，从而触发 Kubernetes 滚动重启 Pod。
	cmName := fmt.Sprintf("endpoint-%s-spec", endpoint.Name)
	var specCM corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: endpoint.Namespace}, &specCM); err == nil {
		hash := configMapDataChecksum(specCM.Data)
		if intendedDeployment.Spec.Template.ObjectMeta.Annotations == nil {
			intendedDeployment.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
		}
		intendedDeployment.Spec.Template.ObjectMeta.Annotations["neon.oltp.molnett.org/spec-checksum"] = hash
	}

	var currentDeployment appsv1.Deployment
	getErr := r.Get(ctx, types.NamespacedName{Name: intendedDeployment.Name, Namespace: endpoint.Namespace}, &currentDeployment)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("failed to get endpoint Deployment: %w", getErr)
	}

	err = ctrl.SetControllerReference(endpoint, intendedDeployment, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set controller reference for endpoint Deployment: %w", err)
	}

	if apierrors.IsNotFound(getErr) {
		if err := r.Create(ctx, intendedDeployment, &client.CreateOptions{
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to create endpoint Deployment: %w", err)
		}
		log.Info("Endpoint Deployment created", "name", endpoint.Name)
		return nil
	}

	if !equality.Semantic.DeepDerivative(intendedDeployment.Spec, currentDeployment.Spec) {
		// 如果 Deployment 尚未 Available 且 spec 差异仅在于 checksum annotation，
		// 则跳过更新，避免不必要的滚动重启（首次创建时 SCRAM 异步就绪触发）。
		if !utils.IsDeploymentAvailable(&currentDeployment) {
			intendedAnnotations := intendedDeployment.Spec.Template.ObjectMeta.Annotations
			currentAnnotations := currentDeployment.Spec.Template.ObjectMeta.Annotations

			var intendedChecksum, currentChecksum string
			if intendedAnnotations != nil {
				intendedChecksum = intendedAnnotations["neon.oltp.molnett.org/spec-checksum"]
			}
			if currentAnnotations != nil {
				currentChecksum = currentAnnotations["neon.oltp.molnett.org/spec-checksum"]
			}

			// 将 intended 的 checksum 临时替换为 current 的，重新比较
			if intendedAnnotations != nil {
				if currentChecksum == "" {
					delete(intendedAnnotations, "neon.oltp.molnett.org/spec-checksum")
				} else {
					intendedAnnotations["neon.oltp.molnett.org/spec-checksum"] = currentChecksum
				}
			}

			if equality.Semantic.DeepDerivative(intendedDeployment.Spec, currentDeployment.Spec) {
				log.Info("Skipping Deployment update: not yet Available, only checksum differs",
					"name", endpoint.Name)
				return nil
			}

			// 恢复 intended checksum，继续正常更新流程
			if intendedAnnotations != nil {
				if intendedChecksum == "" {
					delete(intendedAnnotations, "neon.oltp.molnett.org/spec-checksum")
				} else {
					intendedAnnotations["neon.oltp.molnett.org/spec-checksum"] = intendedChecksum
				}
			}
		}

		// 如果 Deployment 已 Available 但正处于滚动更新中（Progressing=NewReplicaSetCreated），
		// 且仅 spec-checksum annotation 不同，则跳过本次更新。
		// 这避免了 Role/Database 集中就绪期间，每个 reconcile 都触发新的滚动更新，
		// 导致 Pod 被反复杀死重建（观察到的 11 个 ReplicaSet 级联现象）。
		// 等当前滚动更新完成后，下次 reconcile 会自动应用最新的 checksum。
		if utils.IsDeploymentAvailable(&currentDeployment) && utils.IsDeploymentRollingOut(&currentDeployment) {
			intendedAnnotations := intendedDeployment.Spec.Template.ObjectMeta.Annotations
			currentAnnotations := currentDeployment.Spec.Template.ObjectMeta.Annotations

			var intendedChecksum, currentChecksum string
			if intendedAnnotations != nil {
				intendedChecksum = intendedAnnotations["neon.oltp.molnett.org/spec-checksum"]
			}
			if currentAnnotations != nil {
				currentChecksum = currentAnnotations["neon.oltp.molnett.org/spec-checksum"]
			}

			// 将 intended 的 checksum 临时替换为 current 的，重新比较
			if intendedAnnotations != nil {
				if currentChecksum == "" {
					delete(intendedAnnotations, "neon.oltp.molnett.org/spec-checksum")
				} else {
					intendedAnnotations["neon.oltp.molnett.org/spec-checksum"] = currentChecksum
				}
			}

			onlyChecksumDiff := equality.Semantic.DeepDerivative(intendedDeployment.Spec, currentDeployment.Spec)

			// 恢复 intended checksum
			if intendedAnnotations != nil {
				if intendedChecksum == "" {
					delete(intendedAnnotations, "neon.oltp.molnett.org/spec-checksum")
				} else {
					intendedAnnotations["neon.oltp.molnett.org/spec-checksum"] = intendedChecksum
				}
			}

			if onlyChecksumDiff {
				log.Info("Skipping Deployment update: active rollout in progress, only checksum differs",
					"name", endpoint.Name)
				return nil
			}
		}

		if err := r.Patch(ctx, intendedDeployment, client.Apply, &client.PatchOptions{
			Force:        ptr.To(true),
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to update endpoint Deployment: %w", err)
		}
		log.Info("Endpoint Deployment updated", "name", endpoint.Name)
		return nil
	}

	return nil
}

func (r *EndpointReconciler) reconcileEndpointAdminService(ctx context.Context, endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	intendedService := compute.EndpointAdminService(endpoint, branch, project)

	var currentService corev1.Service
	getErr := r.Get(ctx, types.NamespacedName{Name: intendedService.Name, Namespace: endpoint.Namespace}, &currentService)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("failed to get endpoint admin Service: %w", getErr)
	}

	err := ctrl.SetControllerReference(endpoint, intendedService, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set controller reference for endpoint admin Service: %w", err)
	}

	if apierrors.IsNotFound(getErr) {
		if err := r.Create(ctx, intendedService, &client.CreateOptions{
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to create endpoint admin Service: %w", err)
		}
		log.Info("Endpoint admin Service created", "name", endpoint.Name)
		return nil
	}

	if !equality.Semantic.DeepDerivative(intendedService.Spec, currentService.Spec) {
		if err := r.Patch(ctx, intendedService, client.Apply, &client.PatchOptions{
			Force:        ptr.To(true),
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to update endpoint admin Service: %w", err)
		}
		log.Info("Endpoint admin Service updated", "name", endpoint.Name)
		return nil
	}

	return nil
}

func (r *EndpointReconciler) reconcileEndpointPostgresService(ctx context.Context, endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	cluster, err := r.getCluster(ctx, project.Spec.ClusterName, branch.Namespace)
	if err != nil {
		return err
	}

	// 读取 ServiceExposure 配置：Endpoint.Spec.Exposure > Cluster.Spec.PostgresExposure
	var exposure *neonv1alpha1.ServiceExposure
	if endpoint.Spec.Exposure != nil {
		exposure = endpoint.Spec.Exposure
	} else if cluster.Spec.PostgresExposure != nil {
		exposure = cluster.Spec.PostgresExposure
	}

	intendedService := compute.EndpointPostgresService(endpoint, branch, project, exposure)

	var currentService corev1.Service
	getErr := r.Get(ctx, types.NamespacedName{Name: intendedService.Name, Namespace: endpoint.Namespace}, &currentService)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("failed to get endpoint postgres Service: %w", getErr)
	}

	err = ctrl.SetControllerReference(endpoint, intendedService, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set controller reference for endpoint postgres Service: %w", err)
	}

	if apierrors.IsNotFound(getErr) {
		if err := r.Create(ctx, intendedService, &client.CreateOptions{
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to create endpoint postgres Service: %w", err)
		}
		log.Info("Endpoint postgres Service created", "name", endpoint.Name)
		return nil
	}

	if !equality.Semantic.DeepDerivative(intendedService.Spec, currentService.Spec) {
		if err := r.Patch(ctx, intendedService, client.Apply, &client.PatchOptions{
			Force:        ptr.To(true),
			FieldManager: utils.FieldManager,
		}); err != nil {
			return fmt.Errorf("failed to update endpoint postgres Service: %w", err)
		}
		log.Info("Endpoint postgres Service updated", "name", endpoint.Name)
		return nil
	}

	return nil
}

// getEffectiveResources 合并 Endpoint.Spec.Resources 和 Project.DefaultEndpointSettings.Resources。
// Endpoint 级别覆盖 Project 级别。
func getEffectiveResources(endpoint *neonv1alpha1.Endpoint, project *neonv1alpha1.Project) *corev1.ResourceRequirements {
	if endpoint.Spec.Resources != nil {
		return endpoint.Spec.Resources
	}
	if project.Spec.DefaultEndpointSettings != nil && project.Spec.DefaultEndpointSettings.Resources != nil {
		return project.Spec.DefaultEndpointSettings.Resources
	}
	return nil
}

func (r *EndpointReconciler) getCluster(ctx context.Context, clusterName, namespace string) (*neonv1alpha1.Cluster, error) {
	cluster := &neonv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, cluster); err != nil {
		return nil, fmt.Errorf("failed to get cluster %s: %w", clusterName, err)
	}
	return cluster, nil
}

func endpointDeploymentName(endpoint *neonv1alpha1.Endpoint) string {
	return fmt.Sprintf("endpoint-%s", endpoint.Name)
}

func ptrToInt32(i int32) *int32 { return &i }

// configMapDataChecksum 计算 ConfigMap Data 的 SHA-256 checksum，
// 用作 Deployment PodTemplate 的 annotation，确保 ConfigMap 变更触发 Pod 滚动重启。
func configMapDataChecksum(data map[string]string) string {
	h := sha256.New()
	for k, v := range data {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// deriveComputeImage 确定 Compute 容器镜像，优先使用本地 registry。
//
// 策略：
//  1. computeImage 非空 → 直接使用（用户显式指定）
//  2. neonImage 含 registry 前缀（如 192.168.232.128:5000/neondatabase/neon:8463）
//     → 提取 registry 前缀，构造 {registry}/neondatabase/compute-node-v{PGVersion}
//  3. neonImage 无 registry 前缀（如 neondatabase/neon:8463）
//     → 退化为 neondatabase/compute-node-v{PGVersion}（Docker Hub）
func deriveComputeImage(computeImage, neonImage string, pgVersion int) string {
	if computeImage != "" {
		return computeImage
	}

	// 从 neonImage 提取 registry 前缀
	// 例: "192.168.232.128:5000/neondatabase/neon:8463" → parts = ["192.168.232.128:5000", "neondatabase", "neon:8463"]
	// 例: "neondatabase/neon:8463"              → parts = ["neondatabase", "neon:8463"]
	parts := strings.SplitN(neonImage, "/", 3)
	if len(parts) >= 3 {
		// 有 registry 前缀，复用同一个 registry
		return fmt.Sprintf("%s/%s/compute-node-v%d", parts[0], parts[1], pgVersion)
	}

	// 无 registry 前缀，退化为 Docker Hub 默认
	return fmt.Sprintf("neondatabase/compute-node-v%d", pgVersion)
}
