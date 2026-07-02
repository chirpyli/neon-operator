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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
// 对于永久移除（缩容），正确的 SC API 是 StartNodeDelete（PUT /node/:id/delete），
// 而非 StartNodeDrain。原因：
//
//   - Drain 是 best-effort：只迁移有 secondary 的 HA tenant shard，非 HA tenant 会被跳过。
//     drain 完成后 SC 将调度策略设为 PauseForRestart，但节点上可能仍有 attached shard。
//   - Delete 会迁移**所有** shard（包括非 HA），然后调用 set_tombstone 永久删除节点记录。
//     StartNodeDelete 接受 Active 或 Pause 状态作为起始态。
//
// 流程：
//
//	Phase 0: 获取节点当前调度策略
//	Phase 1: 根据当前状态转换到可删除状态（Active/Pause），调用 StartNodeDelete
//	Phase 2: 轮询 GetNode 直到 404（节点已被 tombstone 并从 SC 移除）
//	Phase 3: 移除 Finalizer
func (r *PageserverReconciler) finalize(ctx context.Context, ps *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(ps, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Finalizing Pageserver deletion", "pageserver", ps.Name)

	// 更新状态，标记正在进行终止清理
	_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
		p.Status.ObservedGeneration = p.Generation
		utils.SetCondition(p, p.StatusConditions(), utils.ConditionTerminating,
			metav1.ConditionTrue, utils.ReasonTerminating,
			"Pageserver 正在安全下线")
	})

	if r.SCClient == nil {
		log.Info("SCClient 未配置，跳过 SC 清理，直接移除 finalizer")
		return r.removePageserverFinalizer(ctx, ps)
	}

	clusterName := ps.Spec.Cluster
	nodeID := ps.Spec.ID

	// 检查 force-delete annotation
	forceDelete := ps.Annotations[utils.ForceDeleteAnnotation] == "true"

	// Phase 0: 获取节点当前状态
	node, err := r.SCClient.GetNode(ctx, clusterName, ps.Namespace, nodeID)
	if err != nil {
		// 节点不在 SC 中（已 tombstone 或从未注册），直接移除 finalizer
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
			log.Info("节点不在 SC 中，直接移除 finalizer", "nodeID", nodeID)
			return r.removePageserverFinalizer(ctx, ps)
		}

		// 判断 SC 是否永久不可达（Cluster CR 已删除 = SC 已被 cascade 删除）
		clusterCR := &neonv1alpha1.Cluster{}
		crErr := r.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ps.Namespace}, clusterCR)
		if crErr != nil && apierrors.IsNotFound(crErr) {
			log.Info("Cluster 已删除，SC 不可达，跳过 SC 节点清理", "error", err)
			return r.removePageserverFinalizer(ctx, ps)
		}

		// SC 存在但暂时不可达：保留 finalizer，稍后重试
		log.Info("无法连接 SC 获取节点状态，保留 finalizer 稍后重试", "error", err)
		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
				metav1.ConditionFalse, utils.ReasonDrainFailed,
				fmt.Sprintf("SC 不可达，稍后重试: %v", err))
		})
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil
	}

	currentScheduling := node.Scheduling
	log.Info("节点当前 SC 调度策略", "nodeID", nodeID, "scheduling", currentScheduling)

	// 如果节点卡在 Deleting 状态（SC 后台删除任务已失败），
	// 检查 attached shard 数量：若为 0，说明 shard 已迁移完成，
	// 可以安全地用 force=true 重新发起删除（跳过调度，直接 tombstone）。
	if currentScheduling == "Deleting" {
		shards, shardErr := r.SCClient.GetNodeShards(ctx, clusterName, ps.Namespace, nodeID)
		if shardErr == nil {
			attachedCount := 0
			for _, s := range shards {
				if s.Attached {
					attachedCount++
				}
			}
			if attachedCount == 0 {
				log.Info("节点卡在 Deleting 但无 attached shard，用 force=true 重新删除（跳过调度直接 tombstone）",
					"nodeID", nodeID)
				// 先取消当前的 delete 操作（将 scheduling 恢复为 Active），
				// 然后用 force=true 重新发起。
				if err := r.SCClient.CancelNodeDelete(ctx, clusterName, ps.Namespace, nodeID); err != nil {
					log.Info("CancelNodeDelete 失败（可能后台任务已结束），继续尝试 force delete", "error", err)
				}
				// 标记需要在下一轮使用 force=true 重试
				_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
					utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
						metav1.ConditionTrue, utils.ReasonForceDelete,
						"SC 后台删除失败（Impossible constraint），将在下一轮用 force=true 重试")
				})
				return ctrl.Result{RequeueAfter: drainPollInterval}, nil
			}
		}
		// 仍有 attached shard 或查询失败，继续监控
		return r.monitorNodeDeletion(ctx, ps)
	}

	// Phase 1: 根据当前状态启动或继续节点删除
	switch currentScheduling {
	case "Deleting":
		// 删除已在进行中，直接进入监控
		log.Info("节点已在 Deleting 状态，进入监控", "nodeID", nodeID)
		return r.monitorNodeDeletion(ctx, ps)

	case "Active", "Pause":
		// 可直接调用 StartNodeDelete
		// 如果之前 force=false 的删除因 Impossible constraint 失败，
		// 且 shard 已迁移完成（attachedCount=0），自动升级为 force=true。
		effectiveForce := forceDelete
		if !effectiveForce {
			// 检查是否有之前 force delete 失败的标记
			if cond := meta.FindStatusCondition(ps.Status.Conditions, utils.ConditionDraining); cond != nil &&
				cond.Status == metav1.ConditionTrue && cond.Reason == utils.ReasonForceDelete {
				effectiveForce = true
				log.Info("检测到之前 force=false 删除失败，自动升级为 force=true", "nodeID", nodeID)
			}
		}

		log.Info("启动 SC Node Delete", "nodeID", nodeID, "force", effectiveForce)
		if err := r.SCClient.StartNodeDelete(ctx, clusterName, ps.Namespace, nodeID, effectiveForce); err != nil {
			// 检查是否因无其他可调度节点而失败
			if strings.Contains(err.Error(), "No other schedulable nodes") && !forceDelete {
				log.Info("无其他可调度节点且未启用 force-delete，保留 finalizer",
					"nodeID", nodeID)
				_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
					utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
						metav1.ConditionFalse, utils.ReasonNoSchedulableNodes,
						"无其他可调度节点，无法安全删除。添加 force-delete annotation 可强制删除")
				})
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			// SC 返回 "Node node_id not found for update"（500）表示节点在 SC 内存中存在
			// 但数据库记录已被删除（可能由之前的手动 tombstone 清理导致）。
			// 这种状态下节点已无法通过正常 API 管理，直接移除 finalizer。
			// SC 重启后会从数据库重新加载节点列表，内存中的残留节点会自动消失。
			if strings.Contains(err.Error(), "not found for update") {
				log.Info("节点在 SC 数据库中已不存在（内存残留），直接移除 finalizer",
					"nodeID", nodeID, "error", err)
				_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
					utils.SetCondition(p, p.StatusConditions(), utils.ConditionDrainComplete,
						metav1.ConditionTrue, utils.ReasonAsExpected,
						"节点在 SC 数据库中已不存在（内存残留），SC 重启后自动清理")
				})
				return r.removePageserverFinalizer(ctx, ps)
			}
			log.Error(err, "启动 node delete 失败")
			_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
				utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
					metav1.ConditionFalse, utils.ReasonDrainFailed,
					fmt.Sprintf("启动 node delete 失败: %v", err))
			})
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDraining,
				metav1.ConditionTrue, utils.ReasonDrainStarted,
				"Node Delete 已启动，SC 正在迁移 shard 并 tombstone 节点")
		})
		log.Info("Node Delete 已启动，进入监控阶段", "nodeID", nodeID)
		return r.monitorNodeDeletion(ctx, ps)

	case "Draining":
		// 之前可能由旧代码启动了 drain。Drain 完成后 SC 会设为 PauseForRestart。
		// 等待 drain 完成，然后转换为 Pause 再删除。
		log.Info("节点处于 Draining 状态（可能由之前的 drain 操作启动），等待 drain 完成后转换", "nodeID", nodeID)
		return r.waitForDrainThenDelete(ctx, ps)

	case "PauseForRestart":
		// Drain 已完成（SC 自动设置的）。需要先转回 Pause 才能调用 StartNodeDelete。
		log.Info("节点处于 PauseForRestart（drain 已完成），转换为 Pause 后启动删除", "nodeID", nodeID)
		pausePolicy := "Pause"
		if err := r.SCClient.ConfigureNode(ctx, clusterName, ps.Namespace, nodeID, nil, &pausePolicy); err != nil {
			log.Error(err, "ConfigureNode(Pause) 失败，稍后重试")
			return ctrl.Result{RequeueAfter: drainPollInterval}, nil
		}
		// 转换成功后，下一轮 reconcile 会以 Pause 状态进入 StartNodeDelete 路径
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil

	case "Filling":
		// 取消 Filling（SC 会恢复为 Active），下一轮 reconcile 再删除
		log.Info("节点处于 Filling 状态，取消 fill 后重试删除", "nodeID", nodeID)
		if err := r.SCClient.CancelNodeFill(ctx, clusterName, ps.Namespace, nodeID); err != nil {
			log.Info("CancelNodeFill 失败，稍后重试", "error", err)
		}
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil

	default:
		log.Info("节点处于未知调度策略，稍后重试", "nodeID", nodeID, "scheduling", currentScheduling)
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil
	}
}

// waitForDrainThenDelete 等待 drain 完成（scheduling 变为 PauseForRestart），
// 然后将调度策略转为 Pause 并启动 StartNodeDelete。
// 用于处理节点因旧代码或外部操作已处于 Draining 状态的情况。
func (r *PageserverReconciler) waitForDrainThenDelete(ctx context.Context, ps *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	clusterName := ps.Spec.Cluster
	nodeID := ps.Spec.ID

	node, err := r.SCClient.GetNode(ctx, clusterName, ps.Namespace, nodeID)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
			return r.removePageserverFinalizer(ctx, ps)
		}
		log.Info("查询节点状态失败，稍后重试", "error", err)
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil
	}

	switch node.Scheduling {
	case "PauseForRestart":
		// Drain 完成，转为 Pause 后删除
		log.Info("Drain 已完成（PauseForRestart），转换为 Pause 后启动删除", "nodeID", nodeID)
		pausePolicy := "Pause"
		if err := r.SCClient.ConfigureNode(ctx, clusterName, ps.Namespace, nodeID, nil, &pausePolicy); err != nil {
			log.Error(err, "ConfigureNode(Pause) 失败，稍后重试")
			return ctrl.Result{RequeueAfter: drainPollInterval}, nil
		}
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil

	case "Active":
		// Drain 被取消或 SC 重启后重置为 Active，直接启动删除
		log.Info("节点已回到 Active（drain 被取消或 SC 重启），直接启动删除", "nodeID", nodeID)
		return ctrl.Result{Requeue: true}, nil

	case "Draining":
		// Drain 仍在进行，检查超时
		drainDuration := time.Since(ps.DeletionTimestamp.Time)
		if drainDuration > drainTimeout {
			log.Info("Drain 超时，强制取消 drain 并启动删除", "nodeID", nodeID, "duration", drainDuration)
			_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
				utils.SetCondition(p, p.StatusConditions(), utils.ConditionDrainTimeout,
					metav1.ConditionTrue, utils.ReasonDrainStuck,
					fmt.Sprintf("Drain 超时 (%v)，正在取消并强制删除", drainDuration))
			})
			// 取消 drain，SC 会将调度策略恢复为 Active
			if err := r.SCClient.CancelNodeDrain(ctx, clusterName, ps.Namespace, nodeID); err != nil {
				log.Info("CancelNodeDrain 失败，稍后重试", "error", err)
				return ctrl.Result{RequeueAfter: drainPollInterval}, nil
			}
			return ctrl.Result{RequeueAfter: drainPollInterval}, nil
		}

		// 更新 shard 计数到 Status
		if shards, err := r.SCClient.GetNodeShards(ctx, clusterName, ps.Namespace, nodeID); err == nil {
			attachedCount := 0
			for _, s := range shards {
				if s.Attached {
					attachedCount++
				}
			}
			_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
				p.Status.SCAttachedShardCount = int32(attachedCount)
				p.Status.SCTotalShardCount = int32(len(shards))
			})
			log.Info("Drain 进行中", "nodeID", nodeID, "attachedCount", attachedCount, "duration", drainDuration)
		}
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil

	default:
		// 其他状态（如 Deleting），直接进入删除监控
		return r.monitorNodeDeletion(ctx, ps)
	}
}

// monitorNodeDeletion 轮询 SC 检查节点是否已被 tombstone 并移除。
// SC 的 delete_node 完成后会调用 set_tombstone 并从内存中移除节点，
// 此时 GET /control/v1/node/:id 会返回 404。
//
// 重要：tombstone 后必须调用 DeleteTombstone 物理删除 SC 数据库中的节点记录，
// 否则相同 node_id 的新 pageserver re-attach 时会被 SC 拒绝：
//
//	persistence.rs re_attach():
//	  "Node {id} is marked as deleted, re-attach is not allowed"
//
// 这会导致缩容后再扩容（或删集群重建）时新 pageserver 永远无法注册。
func (r *PageserverReconciler) monitorNodeDeletion(ctx context.Context, ps *neonv1alpha1.Pageserver) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	clusterName := ps.Spec.Cluster
	nodeID := ps.Spec.ID

	node, err := r.SCClient.GetNode(ctx, clusterName, ps.Namespace, nodeID)
	if err != nil {
		// 节点已从 SC 内存中移除（tombstone 完成）。
		// 但 SC 数据库中仍保留 lifecycle='Deleted' 的 tombstone 记录，
		// 必须物理删除，否则相同 ID 的新节点无法 re-attach。
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
			log.Info("节点已从 SC 内存移除（tombstone 完成），正在物理删除 tombstone 记录", "nodeID", nodeID)
			if err := r.SCClient.DeleteTombstone(ctx, clusterName, ps.Namespace, nodeID); err != nil {
				// DeleteTombstone 可能因 tombstone 尚未落库而失败，稍后重试
				log.Info("DeleteTombstone 失败，稍后重试", "nodeID", nodeID, "error", err)
				return ctrl.Result{RequeueAfter: drainPollInterval}, nil
			}
			log.Info("tombstone 记录已物理删除，移除 finalizer", "nodeID", nodeID)
			_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
				utils.SetCondition(p, p.StatusConditions(), utils.ConditionDrainComplete,
					metav1.ConditionTrue, utils.ReasonAsExpected,
					"节点已从 SC 完全移除（tombstone 已物理删除）")
			})
			return r.removePageserverFinalizer(ctx, ps)
		}
		log.Info("查询节点状态失败，稍后重试", "error", err)
		return ctrl.Result{RequeueAfter: drainPollInterval}, nil
	}

	// 节点仍然存在，更新 Status
	_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
		p.Status.SCSchedulingPolicy = node.Scheduling
		p.Status.SCAvailability = node.Availability
	})

	// 更新 shard 计数（用于可观测性）
	if shards, err := r.SCClient.GetNodeShards(ctx, clusterName, ps.Namespace, nodeID); err == nil {
		attachedCount := 0
		for _, s := range shards {
			if s.Attached {
				attachedCount++
			}
		}
		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			p.Status.SCAttachedShardCount = int32(attachedCount)
			p.Status.SCTotalShardCount = int32(len(shards))
		})
		log.Info("节点删除进行中", "nodeID", nodeID, "scheduling", node.Scheduling,
			"attachedShards", attachedCount)
	}

	// 检查超时
	deleteDuration := time.Since(ps.DeletionTimestamp.Time)
	if deleteDuration > drainTimeout {
		log.Info("节点删除超时", "nodeID", nodeID, "duration", deleteDuration,
			"scheduling", node.Scheduling)
		_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionDrainTimeout,
				metav1.ConditionTrue, utils.ReasonDrainStuck,
				fmt.Sprintf("节点删除超时 (%v)，当前调度策略: %s。"+
					"如需强制删除，请添加 %s annotation",
					deleteDuration, node.Scheduling, utils.ForceDeleteAnnotation))
		})
	}

	// 继续轮询
	return ctrl.Result{RequeueAfter: drainPollInterval}, nil
}

// syncSCState 从 Storage Controller 查询节点状态并同步到 Pageserver Status。
// 这是一个尽力而为的操作；如果 SC 不可达，设置 RegisteredWithSC = false 并
// 在 Condition 中记录原因，便于用户通过 kubectl describe 诊断。
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
			utils.SetCondition(p, p.StatusConditions(), utils.ConditionRegisteredWithSC,
				metav1.ConditionFalse, utils.ReasonReconciling,
				fmt.Sprintf("等待 PS 注册到 SC (节点 %d): %v", nodeID, err))
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
func (r *PageserverReconciler) handleNodeFailure(ctx context.Context, ps *neonv1alpha1.Pageserver) (bool, error) {
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
			return false, nil
		}
		return false, fmt.Errorf("获取 pod: %w", err)
	}

	// Terminating 检查必须先于 phase 分发：deletionTimestamp 被设置后，
	// Pod 的 status.phase 不会改变（仍为 Running/Pending），若先按 phase
	// 分发，Terminating 的 Pod 会被错误地路由到 handleRunningPodFailure，
	// 导致 handleTerminatingPodFailure 永远无法被执行。
	if pod.DeletionTimestamp != nil {
		return r.handleTerminatingPodFailure(ctx, ps, pod, podName, threshold)
	}

	switch pod.Status.Phase {
	case corev1.PodPending:
		return r.handlePendingPodFailure(ctx, ps, pod, podName, threshold)
	case corev1.PodRunning:
		return r.handleRunningPodFailure(ctx, ps, pod, podName, threshold)
	case corev1.PodFailed:
		return r.handleFailedPodFailure(ctx, ps, pod, podName)
	case corev1.PodUnknown:
		return r.handleUnknownPodFailure(ctx, ps, pod, podName, threshold)
	}

	return false, nil
}

// handlePendingPodFailure 处理 Pending 状态的 Pod 故障（volume node affinity conflict）
func (r *PageserverReconciler) handlePendingPodFailure(ctx context.Context, ps *neonv1alpha1.Pageserver, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
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

		return r.triggerRecovery(ctx, ps, pod, podName, "VolumeNodeAffinityConflict",
			fmt.Sprintf("Pod 因 volume node affinity Pending %v，正在删除 PVC 以触发重建", pendingTime))
	}

	return false, nil
}

// handleRunningPodFailure 处理 Running 状态但节点故障的 Pod
// 当 Pod Ready=False 且所在节点 NotReady 超过阈值时触发恢复
func (r *PageserverReconciler) handleRunningPodFailure(ctx context.Context, ps *neonv1alpha1.Pageserver, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
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

	return r.triggerRecovery(ctx, ps, pod, podName, "NodeNotReady",
		fmt.Sprintf("节点 %s NotReady %v，Pod Ready=False，正在删除 PVC 以触发重建", pod.Spec.NodeName, nodeNotReadyDuration))
}

// handleTerminatingPodFailure 处理 Terminating 状态的 Pod（节点不可达导致删除卡住）
func (r *PageserverReconciler) handleTerminatingPodFailure(ctx context.Context, ps *neonv1alpha1.Pageserver, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
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
					return r.triggerRecovery(ctx, ps, pod, podName, "NodeNotReady",
						fmt.Sprintf("节点 %s NotReady，Pod Terminating %v", pod.Spec.NodeName, terminatingDuration))
				}
			}
		}
	}

	log.Info("Pod 卡在 Terminating 状态超过阈值，触发自动恢复",
		"pod", podName, "duration", terminatingDuration)

	return r.triggerRecovery(ctx, ps, pod, podName, "PodTerminatingStuck",
		fmt.Sprintf("Pod Terminating %v，正在删除 PVC 以触发重建", terminatingDuration))
}

// handleFailedPodFailure 处理 Failed 状态的 Pod
func (r *PageserverReconciler) handleFailedPodFailure(ctx context.Context, ps *neonv1alpha1.Pageserver, pod *corev1.Pod, podName string) (bool, error) {
	log := logf.FromContext(ctx)

	log.Info("Pod 处于 Failed 状态，触发自动恢复", "pod", podName)

	return r.triggerRecovery(ctx, ps, pod, podName, "PodFailed",
		fmt.Sprintf("Pod %s Failed，正在删除 PVC 以触发重建", podName))
}

// handleUnknownPodFailure 处理 Unknown 状态的 Pod（通常表示节点不可达）
func (r *PageserverReconciler) handleUnknownPodFailure(ctx context.Context, ps *neonv1alpha1.Pageserver, pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
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

	return r.triggerRecovery(ctx, ps, pod, podName, "NodeNotReady",
		fmt.Sprintf("节点 %s NotReady %v，Pod Unknown，正在删除 PVC 以触发重建", pod.Spec.NodeName, nodeNotReadyDuration))
}

// triggerRecovery 执行故障恢复：删除 Pod 和 PVC，触发 StatefulSet 重建
func (r *PageserverReconciler) triggerRecovery(ctx context.Context, ps *neonv1alpha1.Pageserver, pod *corev1.Pod, podName, reason, message string) (bool, error) {
	log := logf.FromContext(ctx)

	_ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
		utils.SetCondition(p, p.StatusConditions(), utils.ConditionNodeRecoveryInProgress,
			metav1.ConditionTrue, reason, message)
	})

	if r.SCClient != nil {
		clusterName := ps.Spec.Cluster
		nodeID := ps.Spec.ID

		node, err := r.SCClient.GetNode(ctx, clusterName, ps.Namespace, nodeID)
		if err == nil && node.Scheduling != "Pause" && node.Scheduling != "Deleting" {
			pausePolicy := "Pause"
			if err := r.SCClient.ConfigureNode(ctx, clusterName, ps.Namespace, nodeID, nil, &pausePolicy); err != nil {
				log.Info("ConfigureNode(Pause) 失败，继续执行恢复", "error", err)
			} else {
				log.Info("已将节点在 SC 中标记为 Pause", "nodeID", nodeID)
			}
		}
	}

	deleteOptions := &client.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	}
	if err := r.Delete(ctx, pod, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "删除 Pod 失败")
		return true, err
	}

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

// pageserverLabelKey 是打在 Pod/StatefulSet 上用于反向映射到 Pageserver CR 的 label。
const pageserverLabelKey = "molnett.org/pageserver"

func (r *PageserverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Pageserver{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// 监听属于 pageserver 的 Pod 状态变化（phase、conditions、deletionTimestamp 等），
		// 以便在节点故障导致 Pod 进入 Terminating/Ready=False 时及时触发故障恢复。
		// Pod 的直接 Owner 是 StatefulSet 而非 Pageserver CR，无法用 Owns() 自动关联，
		// 因此通过 pod label "molnett.org/pageserver" 反向映射到对应的 Pageserver CR。
		// mapPodToPageserver 内部会过滤掉不含该 label 的 Pod，避免无关事件触发调和。
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.mapPodToPageserver)).
		Named("pageserver").
		Complete(r)
}

// mapPodToPageserver 将 pageserver Pod 的事件映射为对应 Pageserver CR 的调和请求。
// 通过 pod label "molnett.org/pageserver" 获取 CR 名称。
func (r *PageserverReconciler) mapPodToPageserver(_ context.Context, obj client.Object) []reconcile.Request {
	psName := obj.GetLabels()[pageserverLabelKey]
	if psName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      psName,
		},
	}}
}
