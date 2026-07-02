/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	safekeeperspec "oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/utils"
)

type SafekeeperReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// SCClient 用于与 Storage Controller 的管理 API 通信，
	// 负责 JWT 认证和重定向跟随。
	SCClient *SCClient
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func (r *SafekeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("调和循环开始", "request", req)
	defer func() {
		log.Info("调和循环结束", "request", req)
	}()

	safekeeper, err := r.getSafekeeper(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if safekeeper == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.SafekeeperNameKey, safekeeper.Name)

	// === 删除路径：先在 SC 中标记 Decomissioned，再移除 Finalizer ===
	if !safekeeper.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, safekeeper)
	}

	// === 创建/更新路径：确保 Finalizer 存在 ===
	if !controllerutil.ContainsFinalizer(safekeeper, utils.FinalizerName) {
		controllerutil.AddFinalizer(safekeeper, utils.FinalizerName)
		if err := r.Update(ctx, safekeeper); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Safekeeper Finalizer 已添加，直接继续调和")
		// 不依赖 Requeue 返回，而是直接 fall-through 继续后续调和逻辑。
		// 这样可以减少一次不必要的队列往返，提高批量创建场景的调和效率。
	}

	if requeue, err := r.handleNodeFailure(ctx, safekeeper); requeue || err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	result, err := r.reconcile(ctx, safekeeper)
	if errors.Is(err, ErrRequeueAfterChange) {
		return result, nil
	} else if err != nil {
		log.Error(err, "Reconcile failed")
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *SafekeeperReconciler) getSafekeeper(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Safekeeper, error) {
	log := logf.FromContext(ctx)
	safekeeper := &neonv1alpha1.Safekeeper{}
	if err := r.Get(ctx, req.NamespacedName, safekeeper); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Safekeeper 已被删除")
			return nil, nil
		}

		return nil, fmt.Errorf("无法获取资源: %w", err)
	}
	return safekeeper, nil
}

//nolint:unparam
func (r *SafekeeperReconciler) reconcile(ctx context.Context, safekeeper *neonv1alpha1.Safekeeper) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	createErr := r.createSafekeeperResources(ctx, safekeeper)
	if createErr != nil {
		log.Error(createErr, "创建 safekeeper 资源时出错")
	}

	stsName := safekeeperspec.Name(safekeeper)
	if err := utils.UpdateSTSBackedStatus(ctx, r.Client, safekeeper, stsName, "Safekeeper", createErr); err != nil {
		log.Error(err, "更新 safekeeper 状态失败")
		return ctrl.Result{}, err
	}

	if createErr != nil {
		return ctrl.Result{}, fmt.Errorf("无法创建 safekeeper 资源: %w", createErr)
	}

	// 向 Storage Controller 注册 safekeeper 失败时，accelerated requeue，
	// 而不是等待下一次 cache resync（默认 5 分钟）。
	// SC 注册是创建流程的最后一步；如果此处为 false，说明刚刚的
	// createSafekeeperResources 中 RegisterSafekeeper 返回了错误。
	if !safekeeper.Status.RegisteredWithSC {
		log.Info("Storage Controller 注册待重试", "safekeeper", safekeeper.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// finalize 处理 Safekeeper 的删除逻辑。
//
// 在移除 Finalizer 之前，向 Storage Controller 发送 Decomissioned 调度策略请求。
// SC 会停止该 safekeeper 的 reconciler 和心跳监控，将其从调度中移除。
//
// v1.6: SC 不可达时不再直接移除 finalizer。检查 Cluster CR 是否存在来决定行为：
//   - Cluster 仍存在 → SC 临时不可达 → Requeue 重试
//   - Cluster 已删除 → SC 已不可恢复 → 移除 finalizer（Cluster finalizer 的 cleanupSafekeeperNodes 已兜底处理）
func (r *SafekeeperReconciler) finalize(ctx context.Context, sk *neonv1alpha1.Safekeeper) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(sk, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("正在终止 Safekeeper 删除，向 Storage Controller 发送 Decomissioned 请求",
		"safekeeper", sk.Name, "id", sk.Spec.ID)

	// 步骤1：在 SC 中标记 Decomissioned
	if err := r.SCClient.DecommissionSafekeeper(ctx, sk); err != nil {
		// SC 不可达时，判断 Cluster CR 是否存在
		clusterCR := &neonv1alpha1.Cluster{}
		crErr := r.Get(ctx, types.NamespacedName{Name: sk.Spec.Cluster, Namespace: sk.Namespace}, clusterCR)
		if crErr != nil && apierrors.IsNotFound(crErr) {
			// Cluster 已删除，SC 不再存在，无法执行任何 SC 操作。
			// Cluster finalizer 的 cleanupSafekeeperNodes 应在删除前已完成 decommission。
			log.Info("Cluster 已删除，SC 不可达，跳过 Safekeeper Decommission", "error", err)
		} else {
			// SC 存在但暂时不可达：保留 finalizer，稍后重试。
			// 不再像旧代码那样直接跳过（会导致 scheduling_policy 保持 Active）。
			log.Info("无法连接 SC 进行 Safekeeper Decommission，保留 finalizer 稍后重试", "error", err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// 步骤2：移除 Finalizer，允许 K8s 删除资源
	controllerutil.RemoveFinalizer(sk, utils.FinalizerName)
	if err := r.Update(ctx, sk); err != nil {
		log.Error(err, "移除 Finalizer 失败")
		return ctrl.Result{}, fmt.Errorf("移除 finalizer: %w", err)
	}

	log.Info("Finalizer 已移除，Safekeeper 将由 APIServer 删除",
		"safekeeper", sk.Name)
	return ctrl.Result{}, nil
}

// safekeeperLabelKey 是打在 Pod/StatefulSet 上用于反向映射到 Safekeeper CR 的 label。
const safekeeperLabelKey = "molnett.org/safekeeper"

func (r *SafekeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Safekeeper{}).
		// 监听 Cluster 变化，将属于该 Cluster 的所有 Safekeeper 加入调和队列。
		// 作为 For() 的补充触发路径，当 Cluster 状态发生变化（如 numSafekeepers
		// 变更、Status 更新等）时，确保所有关联 Safekeeper 都能被重新检查。
		Watches(&neonv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.mapClusterToSafekeepers)).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// 监听属于 safekeeper 的 Pod 状态变化（phase、conditions、deletionTimestamp 等），
		// 以便在节点故障导致 Pod 进入 Terminating/Ready=False 时及时触发故障恢复。
		// Pod 的直接 Owner 是 StatefulSet 而非 Safekeeper CR，无法用 Owns() 自动关联，
		// 因此通过 pod label "molnett.org/safekeeper" 反向映射到对应的 Safekeeper CR。
		// mapPodToSafekeeper 内部会过滤掉不含该 label 的 Pod，避免无关事件触发调和。
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToSafekeeper)).
		Named("safekeeper").
		Complete(r)
}

// mapPodToSafekeeper 将 safekeeper Pod 的事件映射为对应 Safekeeper CR 的调和请求。
// 通过 pod label "molnett.org/safekeeper" 获取 CR 名称。
func (r *SafekeeperReconciler) mapPodToSafekeeper(_ context.Context, obj client.Object) []reconcile.Request {
	skName := obj.GetLabels()[safekeeperLabelKey]
	if skName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      skName,
		},
	}}
}

// mapClusterToSafekeepers 列出属于指定 Cluster 的所有 Safekeeper CR，
// 并将其加入调和队列。使用标准缓存 client，与 Informer 共享同一缓存。
func (r *SafekeeperReconciler) mapClusterToSafekeepers(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*neonv1alpha1.Cluster)
	if !ok {
		return nil
	}

	var safekeepers neonv1alpha1.SafekeeperList
	if err := r.List(ctx, &safekeepers,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			safekeeperspec.ClusterLabel: cluster.Name,
		},
	); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(safekeepers.Items))
	for _, sk := range safekeepers.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: sk.Namespace,
				Name:      sk.Name,
			},
		})
	}
	return requests
}

// handleNodeFailure 检测并处理节点故障导致的 Pod 不可用问题。
// 返回 (requeue, error)。支持以下场景：
// 1. Pod 因 volume node affinity conflict 处于 Pending 状态超过阈值
// 2. Pod 处于 Running 状态但 Ready=False，且所在节点 NotReady 超过阈值
// 3. Pod 处于 Terminating 状态超过阈值（节点不可达导致删除卡住）
// 4. Pod 处于 Failed 状态
// 5. Pod 处于 Unknown 状态且所在节点 NotReady 超过阈值
// 触发恢复时，删除 PVC 和 Pod 以触发 StatefulSet 重建。
//
// 注意：Pod 进入 Terminating 状态时 status.phase 仍保持为 Running（或 Pending），
// 仅 metadata.deletionTimestamp 被设置。因此必须先检查 deletionTimestamp，
// 再按 phase 分发，否则 Terminating 的 Pod 会错误地走 Running/Pending 分支。
func (r *SafekeeperReconciler) handleNodeFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper) (bool, error) {
	if sk.Spec.NodeFailure == nil || !sk.Spec.NodeFailure.AutoRecover {
		return false, nil
	}

	threshold := nodeFailurePendingThreshold
	if sk.Spec.NodeFailure.MaxPendingDuration != nil {
		threshold = sk.Spec.NodeFailure.MaxPendingDuration.Duration
	}

	podName := safekeeperspec.Name(sk) + "-0"
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: sk.Namespace}, pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("获取 pod: %w", err)
	}

	// Terminating 检查必须先于 phase 分发：deletionTimestamp 被设置后，
	// Pod 的 status.phase 不会改变（仍为 Running/Pending），若先按 phase
	// 分发，Terminating 的 Pod 会被错误地路由到 handleRunningPodFailure，
	// 导致 handleTerminatingPodFailure 永远无法被执行。
	if pod.DeletionTimestamp != nil {
		return r.handleTerminatingPodFailure(ctx, sk, pod, podName, threshold)
	}

	switch pod.Status.Phase {
	case corev1.PodPending:
		return r.handlePendingPodFailure(ctx, sk, pod, podName, threshold)
	case corev1.PodRunning:
		return r.handleRunningPodFailure(ctx, sk, pod, podName, threshold)
	case corev1.PodFailed:
		return r.handleFailedPodFailure(ctx, sk, pod, podName)
	case corev1.PodUnknown:
		return r.handleUnknownPodFailure(ctx, sk, pod, podName, threshold)
	}

	return false, nil
}

// handlePendingPodFailure 处理 Pending 状态的 Pod 故障（volume node affinity conflict）
func (r *SafekeeperReconciler) handlePendingPodFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
	log := logf.FromContext(ctx)

	hasVolumeConflict := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason == "" {
					hasVolumeConflict = true
				}
			}
			if c.Message != "" {
				hasVolumeConflict = true
			}
		}
	}

	if hasVolumeConflict {
		pendingTime := time.Since(pod.CreationTimestamp.Time)
		if pendingTime < threshold {
			return false, nil
		}

		log.Info("Pod 因 volume node affinity 长时间 Pending，触发自动恢复",
			"pod", podName, "pending", pendingTime, "threshold", threshold)

		return r.triggerRecovery(ctx, sk, pod, podName, "VolumeNodeAffinityConflict",
			fmt.Sprintf("Pod 因 volume node affinity Pending %v，正在删除 PVC 以触发重建", pendingTime))
	}

	return false, nil
}

// handleRunningPodFailure 处理 Running 状态但节点故障的 Pod
// 当 Pod Ready=False 且所在节点 NotReady 超过阈值时触发恢复
func (r *SafekeeperReconciler) handleRunningPodFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
	log := logf.FromContext(ctx)

	podReady := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			podReady = c.Status == corev1.ConditionTrue
			break
		}
	}

	if podReady {
		return false, nil
	}

	if pod.Spec.NodeName == "" {
		return false, nil
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		log.Info("无法获取节点状态", "node", pod.Spec.NodeName, "error", err)
		return false, nil
	}

	nodeReady := false
	var nodeNotReadySince time.Time
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			nodeReady = c.Status == corev1.ConditionTrue
			if !nodeReady && c.LastTransitionTime.IsZero() {
				nodeNotReadySince = pod.CreationTimestamp.Time
			} else if !nodeReady {
				nodeNotReadySince = c.LastTransitionTime.Time
			}
			break
		}
	}

	if nodeReady {
		return false, nil
	}

	nodeNotReadyDuration := time.Since(nodeNotReadySince)
	if nodeNotReadyDuration < threshold {
		log.Info("节点 NotReady 时间未超过阈值", "node", pod.Spec.NodeName,
			"duration", nodeNotReadyDuration, "threshold", threshold)
		return false, nil
	}

	log.Info("Pod Running 但 Ready=False，节点 NotReady 超过阈值，触发自动恢复",
		"pod", podName, "node", pod.Spec.NodeName, "duration", nodeNotReadyDuration, "threshold", threshold)

	return r.triggerRecovery(ctx, sk, pod, podName, "NodeNotReady",
		fmt.Sprintf("节点 %s NotReady %v，Pod Ready=False，正在删除 PVC 以触发重建", pod.Spec.NodeName, nodeNotReadyDuration))
}

// handleTerminatingPodFailure 处理 Terminating 状态的 Pod（节点不可达导致删除卡住）
func (r *SafekeeperReconciler) handleTerminatingPodFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
	log := logf.FromContext(ctx)

	terminatingDuration := time.Since(pod.DeletionTimestamp.Time)
	if terminatingDuration < threshold {
		log.Info("Pod Terminating 时间未超过阈值", "pod", podName, "duration", terminatingDuration)
		return false, nil
	}

	if pod.Spec.NodeName != "" {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err == nil {
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionFalse {
					log.Info("Pod Terminating 且节点 NotReady，触发自动恢复",
						"pod", podName, "node", pod.Spec.NodeName)
					return r.triggerRecovery(ctx, sk, pod, podName, "NodeNotReady",
						fmt.Sprintf("节点 %s NotReady，Pod Terminating %v", pod.Spec.NodeName, terminatingDuration))
				}
			}
		}
	}

	log.Info("Pod 卡在 Terminating 状态超过阈值，触发自动恢复",
		"pod", podName, "duration", terminatingDuration)

	return r.triggerRecovery(ctx, sk, pod, podName, "PodTerminatingStuck",
		fmt.Sprintf("Pod Terminating %v，正在删除 PVC 以触发重建", terminatingDuration))
}

// handleFailedPodFailure 处理 Failed 状态的 Pod
func (r *SafekeeperReconciler) handleFailedPodFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper, pod *corev1.Pod, podName string) (bool, error) {
	log := logf.FromContext(ctx)

	log.Info("Pod 处于 Failed 状态，触发自动恢复", "pod", podName)

	return r.triggerRecovery(ctx, sk, pod, podName, "PodFailed",
		fmt.Sprintf("Pod %s Failed，正在删除 PVC 以触发重建", podName))
}

// handleUnknownPodFailure 处理 Unknown 状态的 Pod（通常表示节点不可达）
func (r *SafekeeperReconciler) handleUnknownPodFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
	log := logf.FromContext(ctx)

	if pod.Spec.NodeName == "" {
		return false, nil
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		log.Info("无法获取节点状态", "node", pod.Spec.NodeName)
		return false, nil
	}

	nodeReady := false
	var nodeNotReadySince time.Time
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			nodeReady = c.Status == corev1.ConditionTrue
			if !nodeReady && c.LastTransitionTime.IsZero() {
				nodeNotReadySince = pod.CreationTimestamp.Time
			} else if !nodeReady {
				nodeNotReadySince = c.LastTransitionTime.Time
			}
			break
		}
	}

	if nodeReady {
		return false, nil
	}

	nodeNotReadyDuration := time.Since(nodeNotReadySince)
	if nodeNotReadyDuration < threshold {
		log.Info("节点 NotReady 时间未超过阈值", "node", pod.Spec.NodeName,
			"duration", nodeNotReadyDuration)
		return false, nil
	}

	log.Info("Pod Unknown 且节点 NotReady 超过阈值，触发自动恢复",
		"pod", podName, "node", pod.Spec.NodeName, "duration", nodeNotReadyDuration)

	return r.triggerRecovery(ctx, sk, pod, podName, "NodeNotReady",
		fmt.Sprintf("节点 %s NotReady %v，Pod Unknown，正在删除 PVC 以触发重建", pod.Spec.NodeName, nodeNotReadyDuration))
}

// triggerRecovery 执行故障恢复：删除 Pod 和 PVC，触发 StatefulSet 重建
func (r *SafekeeperReconciler) triggerRecovery(ctx context.Context, sk *neonv1alpha1.Safekeeper, pod *corev1.Pod, podName, reason, message string) (bool, error) {
	log := logf.FromContext(ctx)

	_ = utils.PatchStatus(ctx, r.Client, sk, func(s *neonv1alpha1.Safekeeper) {
		utils.SetCondition(s, s.StatusConditions(), utils.ConditionNodeRecoveryInProgress,
			metav1.ConditionTrue, reason, message)
	})

	deleteOptions := &client.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	}
	if err := r.Delete(ctx, pod, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "删除 Pod 失败")
		return true, err
	}

	pvcName := safekeeperspec.Name(sk) + "-safekeeper-storage-0"
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: sk.Namespace}, pvc); err == nil {
		if err := r.Delete(ctx, pvc); err != nil {
			log.Error(err, "删除 PVC 失败")
			return true, err
		}
		log.Info("已删除 PVC，StatefulSet 将创建新 PVC 在新节点上", "pvc", pvcName)
	}

	return true, nil
}
