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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	pageserverspec "oltp.molnett.org/neon-operator/specs/pageserver"
	"oltp.molnett.org/neon-operator/utils"
)

// drainPollInterval is the interval between drain progress checks.
const drainPollInterval = 5 * time.Second

// drainTimeout is the maximum time to wait for drain to complete.
const drainTimeout = 30 * time.Minute

// nodeFailurePendingThreshold is the default threshold for detecting a stuck pod
// pending due to volume node affinity conflict.
const nodeFailurePendingThreshold = 5 * time.Minute

type PageserverReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	SCClient *SCClient
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func (r *PageserverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	pageserver, err := r.getPageserver(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pageserver == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.PageserverNameKey, pageserver.Name)

	// === 删除路径：执行外部资源清理（Drain → 移除 Finalizer） ===
	if !pageserver.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, pageserver)
	}

	// === 创建/更新路径：确保 Finalizer 存在 ===
	if !controllerutil.ContainsFinalizer(pageserver, utils.FinalizerName) {
		controllerutil.AddFinalizer(pageserver, utils.FinalizerName)
		if err := r.Update(ctx, pageserver); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Pageserver Finalizer 已添加，直接继续调和")
	}

	// 检查节点故障恢复
	if requeue, err := r.handleNodeFailure(ctx, pageserver); requeue || err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	result, err := r.reconcile(ctx, pageserver)
	if errors.Is(err, ErrRequeueAfterChange) {
		return result, nil
	} else if err != nil {
		log.Error(err, "Reconcile failed")
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *PageserverReconciler) getPageserver(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Pageserver, error) {
	log := logf.FromContext(ctx)
	pageserver := &neonv1alpha1.Pageserver{}
	if err := r.Get(ctx, req.NamespacedName, pageserver); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Pageserver has been deleted")
			return nil, nil
		}

		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return pageserver, nil
}

//nolint:unparam
func (r *PageserverReconciler) reconcile(ctx context.Context, pageserver *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	createErr := r.createPageserverResources(ctx, pageserver)
	if createErr != nil {
		log.Error(createErr, "error while creating pageserver resources")
	}

	stsName := pageserverspec.Name(pageserver)
	if err := utils.UpdateSTSBackedStatus(ctx, r.Client, pageserver, stsName, "Pageserver", createErr); err != nil {
		log.Error(err, "failed to update pageserver status")
		return ctrl.Result{}, err
	}

	if createErr != nil {
		return ctrl.Result{}, fmt.Errorf("not able to create pageserver resources: %w", createErr)
	}
	return ctrl.Result{}, nil
}

// finalize 处理 Pageserver 的安全下线。
//
// 流程：
//
//	Phase 0: 准入检查（确认有其他可调度节点）
//	Phase 1: 启动 SC Drain
//	Phase 2: 轮询监控 Drain 进度（检查 attached shard count）
//	Phase 3: Drain 完成 → 设 PauseForRestart + 可选 Delete → 移除 Finalizer
func (r *PageserverReconciler) finalize(ctx context.Context, ps *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(ps, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Finalizing Pageserver deletion (drain phase)", "pageserver", ps.Name)

	// 更新状态，标记正在进行终止清理
	_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
		p.Status.ObservedGeneration = p.Generation
		utils.SetCondition(p, p.StatusConditions(), utils.ConditionTerminating,
			metav1.ConditionTrue, utils.ReasonTerminating,
			"Pageserver 正在安全下线（Drain）")
	})

	if r.SCClient == nil {
		log.Info("SCClient 未配置，跳过 drain，直接移除 finalizer")
		return r.removePageserverFinalizer(ctx, ps)
	}

	clusterName := ps.Spec.Cluster
	nodeID := ps.Spec.ID

	// Phase 0: 准入检查 — 检查节点状态 + 是否有其他可调度节点
	allNodes, err := r.SCClient.ListNodeNodes(ctx, clusterName, ps.Namespace)
	if err != nil {
		log.Info("无法连接 SC 进行准入检查，跳过 drain 继续删除", "error", err)
		return r.removePageserverFinalizer(ctx, ps)
	}

	var currentScheduling string
	schedulableCount := 0
	for _, n := range allNodes {
		if n.ID == nodeID {
			currentScheduling = n.Scheduling
		}
		if n.ID != nodeID && n.Availability == "Active" {
			if n.Scheduling == "Active" || n.Scheduling == "Filling" {
				schedulableCount++
			}
		}
	}

	// 如果节点已经不在 SC 中（已删除或从未注册），直接移除 finalizer
	if currentScheduling == "" && len(allNodes) > 0 {
		log.Info("节点未在 SC 中注册，跳过 drain，直接移除 finalizer",
			"nodeID", nodeID)
		return r.removePageserverFinalizer(ctx, ps)
	}

	// 如果节点已经在 Draining 状态，跳到 Phase 2
	if currentScheduling == "Draining" {
		log.Info("节点已在 Draining，跳过 Phase 1，直接进入监控",
			"nodeID", nodeID)
		return r.monitorDrainProgress(ctx, ps)
	}

	// 检查是否单节点集群（无法 drain）
	if schedulableCount == 0 {
		log.Info("无其他可调度节点，无法 drain。如需强制删除，请添加 force-delete annotation",
			"nodeID", nodeID)
		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
				metav1.ConditionFalse, utils.ReasonNoSchedulableNodes,
				"无其他可调度节点，无法 drain")
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Phase 1: 启动 Drain
	if currentScheduling == "Active" {
		log.Info("启动 SC Drain", "nodeID", nodeID)
		if err := r.SCClient.StartNodeDrain(ctx, clusterName, ps.Namespace, nodeID); err != nil {
			log.Error(err, "启动 drain 失败")
			_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
				utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
					metav1.ConditionFalse, utils.ReasonDrainFailed,
					fmt.Sprintf("启动 drain 失败: %v", err))
			})
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
				metav1.ConditionTrue, utils.ReasonDrainStarted,
				"Drain 已启动，正在迁移 shard")
		})
		log.Info("Drain 已启动，进入监控阶段", "nodeID", nodeID)
	}

	// Phase 2: 监控 Drain 进度
	return r.monitorDrainProgress(ctx, ps)
}

// monitorDrainProgress 轮询 SC 检查 drain 进度，直到所有 attached shard 迁移完成。
func (r *PageserverReconciler) monitorDrainProgress(ctx context.Context, ps *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	clusterName := ps.Spec.Cluster
	nodeID := ps.Spec.ID

	// 查询节点上的 shard 列表
	shards, err := r.SCClient.GetNodeShards(ctx, clusterName, ps.Namespace, nodeID)
	if err != nil {
		log.Info("查询 SC shard 状态失败，稍后重试", "error", err, "nodeID", nodeID)
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil
	}

	// 计算 attached shard 数量
	attachedCount := 0
	for _, s := range shards {
		if s.Attached {
			attachedCount++
		}
	}

	log.Info("Drain 进度", "nodeID", nodeID, "attachedCount", attachedCount)

	// 更新 Status 中的 shard 计数
	_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
		p.Status.SCAttachedShardCount = int32(attachedCount)
		p.Status.SCTotalShardCount = int32(len(shards))
	})

	if attachedCount == 0 {
		// Phase 3: Drain 完成
		log.Info("Drain 完成，所有 attached shard 已迁移", "nodeID", nodeID)

		// 显式设置 PauseForRestart（SC 不会自动切换）
		pauseScheduling := "PauseForRestart"
		if err := r.SCClient.ConfigureNode(ctx, clusterName, ps.Namespace, nodeID, nil, &pauseScheduling); err != nil {
			log.Info("ConfigureNode(PauseForRestart) 失败，继续删除流程", "error", err)
		}

		// 可选：标记为 Deleting
		_ = r.SCClient.StartNodeDelete(ctx, clusterName, ps.Namespace, nodeID, false)

		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDrainComplete,
				metav1.ConditionTrue, utils.ReasonAsExpected,
				"Drain 完成，所有 shard 已迁移")
			p.Status.SCSchedulingPolicy = "PauseForRestart"
		})

		return r.removePageserverFinalizer(ctx, ps)
	}

	// 检查超时
	drainDuration := time.Since(ps.DeletionTimestamp.Time)
	if drainDuration > drainTimeout {
		log.Info("Drain 超时", "nodeID", nodeID, "duration", drainDuration)
		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDrainTimeout,
				metav1.ConditionTrue, utils.ReasonDrainStuck,
				fmt.Sprintf("Drain 超时 (%v)，仍有 %d 个 attached shard", drainDuration, attachedCount))
		})
		// 继续等待而不是强制删除（除非有 force-delete annotation）
	}

	// 继续轮询
	return ctrl.Result{RequeueAfter: drainPollInterval}, nil
}

// syncSCState 从 Storage Controller 查询节点状态并同步到 Pageserver Status。
// 这是一个尽力而为的操作；如果 SC 不可达，仅记录日志。
func (r *PageserverReconciler) syncSCState(ctx context.Context, ps *neonv1alpha1.Pageserver) {
	log := logf.FromContext(ctx)

	if r.SCClient == nil {
		return
	}

	clusterName := ps.Spec.Cluster
	nodeID := ps.Spec.ID

	// 获取节点详情
	node, err := r.SCClient.GetNode(ctx, clusterName, ps.Namespace, nodeID)
	if err != nil {
		// SC 尚未注册或不可达，记录状态未知
		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			p.Status.RegisteredWithSC = false
		})
		log.Info("无法获取 SC 节点状态，可能尚未注册", "error", err)
		return
	}

	// 获取 shard 计数
	var attachedCount, totalCount int32
	if shards, err := r.SCClient.GetNodeShards(ctx, clusterName, ps.Namespace, nodeID); err == nil {
		for _, s := range shards {
			if s.Attached {
				attachedCount++
			}
		}
		totalCount = int32(len(shards))
	}

	// 更新状态
	_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
		p.Status.RegisteredWithSC = true
		p.Status.NodeID = node.ID
		p.Status.SCSchedulingPolicy = node.Scheduling
		p.Status.SCAvailability = node.Availability
		p.Status.SCAttachedShardCount = attachedCount
		p.Status.SCTotalShardCount = totalCount
		utils.SetCondition(p, p.StatusConditions(), utils.ConditionRegisteredWithSC,
			metav1.ConditionTrue, utils.ReasonAsExpected,
			fmt.Sprintf("已注册到 SC (scheduling=%s, availability=%s)", node.Scheduling, node.Availability))
	})
}

// handleNodeFailure 检测并处理本地盘 PVC 粘滞导致的 Pod Pending 问题。
// 返回 (requeue, error)。当 Pod 因 volume node affinity conflict 处于
// Pending 状态超过阈值时，删除 PVC 和 Pod 以触发 StatefulSet 重建。
func (r *PageserverReconciler) handleNodeFailure(ctx context.Context, ps *neonv1alpha1.Pageserver) (bool, error) {
	log := logf.FromContext(ctx)

	// 节点故障恢复未启用
	if ps.Spec.NodeFailure == nil || !ps.Spec.NodeFailure.AutoRecover {
		return false, nil
	}

	threshold := nodeFailurePendingThreshold
	if ps.Spec.NodeFailure.MaxPendingDuration != nil {
		threshold = ps.Spec.NodeFailure.MaxPendingDuration.Duration
	}

	podName := pageserverspec.Name(ps) + "-0"
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: ps.Namespace}, pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // Pod 尚未创建
		}
		return false, fmt.Errorf("获取 pod: %w", err)
	}

	// 只处理 Pending 状态的 Pod
	if pod.Status.Phase != corev1.PodPending {
		return false, nil
	}

	// 检查是否因 volume node affinity conflict 导致 Pending
	hasVolumeConflict := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable {
			// 可能在等待调度或 volume conflict
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason == "" {
					hasVolumeConflict = true
				}
			}
			// 也检查 pod conditions message
			if c.Message != "" {
				hasVolumeConflict = true
			}
		}
	}

	if hasVolumeConflict {
		pendingTime := time.Since(pod.CreationTimestamp.Time)
		if pendingTime < threshold {
			return false, nil // 尚未超过阈值
		}

		log.Info("Pod 因 volume node affinity 长时间 Pending，触发自动恢复",
			"pod", podName, "pending", pendingTime, "threshold", threshold)

		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionNodeRecoveryInProgress,
				metav1.ConditionTrue, "VolumeNodeAffinityConflict",
				fmt.Sprintf("Pod 因 volume node affinity Pending %v，正在删除 PVC 以触发重建", pendingTime))
		})

		// 删除 Pod（非级联删除 PVC）
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "删除 Pending Pod 失败")
			return true, err
		}

		// 删除 PVC
		pvcName := pageserverspec.Name(ps) + "-" + storageVolumeName + "-0"
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: ps.Namespace}, pvc); err == nil {
			if err := r.Delete(ctx, pvc); err != nil {
				log.Error(err, "删除 PVC 失败")
				return true, err
			}
			log.Info("已删除 PVC，StatefulSet 将创建新 PVC 在新节点上", "pvc", pvcName)
		}

		return true, nil
	}

	return false, nil
}

// removePageserverFinalizer 移除 finalizer，允许 K8s 清理资源。
func (r *PageserverReconciler) removePageserverFinalizer(ctx context.Context, ps *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	current := &neonv1alpha1.Pageserver{}
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Name, Namespace: ps.Namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("重新获取 pageserver 以移除 finalizer: %w", err)
	}

	if !controllerutil.ContainsFinalizer(current, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(current, utils.FinalizerName)
	if err := r.Update(ctx, current); err != nil {
		log.Error(err, "移除 finalizer 失败")
		return ctrl.Result{}, fmt.Errorf("移除 finalizer: %w", err)
	}

	log.Info("Finalizer 已移除，Pageserver 将由 APIServer 删除",
		"pageserver", ps.Name)
	return ctrl.Result{}, nil
}

// storageVolumeName matches the volume claim template name in StatefulSet.
const storageVolumeName = "pageserver-storage"

func (r *PageserverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Pageserver{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("pageserver").
		Complete(r)
}
