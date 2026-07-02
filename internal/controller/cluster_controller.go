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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	pageserverspec "oltp.molnett.org/neon-operator/specs/pageserver"
	safekeeperspec "oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/specs/storagebroker"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

// This is not a proper error. It indicated we should return a empty requeue after an object has been changed.
var ErrRequeueAfterChange = errors.New("requeue after change")

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	SCClient *SCClient // 集群删除时用于清理 SC 节点记录
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	cluster, err := r.getCluster(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.ClusterNameKey, cluster.Name)

	// === 删除路径：执行外部资源清理 ===
	if !cluster.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, cluster)
	}

	// === 创建/更新路径：确保 Finalizer 存在 ===
	if !controllerutil.ContainsFinalizer(cluster, utils.FinalizerName) {
		controllerutil.AddFinalizer(cluster, utils.FinalizerName)
		if err := r.Update(ctx, cluster); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Cluster Finalizer 已添加，直接继续调和")
		// 不依赖 Requeue 返回，而是直接 fall-through 继续后续调和逻辑。
		// 这样可以减少一次不必要的队列往返，提高批量创建场景的调和效率。
	}

	result, err := r.reconcile(ctx, cluster)
	if errors.Is(err, ErrRequeueAfterChange) {
		return result, nil
	} else if err != nil {
		log.Error(err, "Reconcile failed")
		return ctrl.Result{}, err
	}

	return result, nil
}

//nolint:unparam
func (r *ClusterReconciler) reconcile(ctx context.Context, cluster *neonv1alpha1.Cluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	createErr := r.createClusterResources(ctx, cluster)
	if createErr != nil {
		log.Error(createErr, "error while creating cluster resources")
	}

	if err := r.updateStatus(ctx, cluster, createErr); err != nil {
		log.Error(err, "failed to update cluster status")
		return ctrl.Result{}, err
	}

	if createErr != nil {
		return ctrl.Result{}, fmt.Errorf("not able to create cluster resources: %w", createErr)
	}
	return ctrl.Result{}, nil
}

func (r *ClusterReconciler) updateStatus(ctx context.Context, cluster *neonv1alpha1.Cluster, reconcileErr error) error {
	scAvailable, scReason, scMessage := r.deploymentState(ctx, cluster, storagecontroller.Name(cluster.Name), "Storage controller")
	sbAvailable, sbReason, sbMessage := r.deploymentState(ctx, cluster, storagebroker.Name(cluster.Name), "Storage broker")

	// Aggregate Safekeeper status
	skReady, skTotal := r.safekeeperState(ctx, cluster)
	quorumRequired := int(cluster.Spec.NumSafekeepers)/2 + 1

	// Aggregate Pageserver status
	psReady, psTotal := r.pageserverState(ctx, cluster)

	return utils.PatchStatus(ctx, r.Client, cluster, func(c *neonv1alpha1.Cluster) {
		c.Status.ObservedGeneration = c.Generation
		conds := &c.Status.Conditions

		if reconcileErr != nil {
			utils.SetCondition(c, conds, utils.ConditionAvailable, metav1.ConditionFalse, utils.ReasonResourceCreateFailed, reconcileErr.Error())
			utils.SetCondition(c, conds, utils.ConditionProgressing, metav1.ConditionTrue, utils.ReasonReconciling, "Retrying after resource creation failure")
			utils.SetCondition(c, conds, utils.ConditionStorageControllerAvailable, scAvailable, scReason, scMessage)
			utils.SetCondition(c, conds, utils.ConditionStorageBrokerAvailable, sbAvailable, sbReason, sbMessage)
			return
		}

		utils.SetCondition(c, conds, utils.ConditionStorageControllerAvailable, scAvailable, scReason, scMessage)
		utils.SetCondition(c, conds, utils.ConditionStorageBrokerAvailable, sbAvailable, sbReason, sbMessage)

		// Safekeeper quorum status
		if skReady >= quorumRequired {
			utils.SetCondition(c, conds, utils.ConditionSafekeepersAvailable, metav1.ConditionTrue, utils.ReasonAsExpected,
				fmt.Sprintf("%d/%d safekeepers ready (quorum=%d)", skReady, skTotal, quorumRequired))
		} else {
			utils.SetCondition(c, conds, utils.ConditionSafekeepersAvailable, metav1.ConditionFalse, utils.ReasonSafekeeperQuorumLost,
				fmt.Sprintf("%d/%d safekeepers ready (quorum=%d required)", skReady, skTotal, quorumRequired))
		}

		// Pageserver status
		if psTotal > 0 && psReady == psTotal {
			utils.SetCondition(c, conds, utils.ConditionPageserversAvailable, metav1.ConditionTrue, utils.ReasonAsExpected,
				fmt.Sprintf("%d/%d pageservers ready", psReady, psTotal))
		} else if psTotal > 0 {
			utils.SetCondition(c, conds, utils.ConditionPageserversAvailable, metav1.ConditionFalse, utils.ReasonReconciling,
				fmt.Sprintf("%d/%d pageservers ready", psReady, psTotal))
		}

		switch {
		case scAvailable == metav1.ConditionTrue && sbAvailable == metav1.ConditionTrue:
			utils.SetCondition(c, conds, utils.ConditionAvailable, metav1.ConditionTrue, utils.ReasonAsExpected, "Cluster components are Available")
			utils.SetCondition(c, conds, utils.ConditionProgressing, metav1.ConditionFalse, utils.ReasonAsExpected, "Cluster is at desired state")
		default:
			utils.SetCondition(c, conds, utils.ConditionAvailable, metav1.ConditionFalse, utils.ReasonChildDeploymentNotAvailable, "One or more cluster components are not Available")
			utils.SetCondition(c, conds, utils.ConditionProgressing, metav1.ConditionTrue, utils.ReasonReconciling, "Waiting for child Deployments to become Available")
		}
	})
}

// safekeeperState returns (ready, total) counts for safekeepers belonging to the cluster.
func (r *ClusterReconciler) safekeeperState(ctx context.Context, cluster *neonv1alpha1.Cluster) (int, int) {
	var sks neonv1alpha1.SafekeeperList
	if err := r.List(ctx, &sks,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{safekeeperspec.ClusterLabel: cluster.Name},
	); err != nil {
		return 0, 0
	}

	ready := 0
	for _, sk := range sks.Items {
		if meta.IsStatusConditionTrue(sk.Status.Conditions, utils.ConditionAvailable) {
			ready++
		}
	}
	return ready, len(sks.Items)
}

// pageserverState returns (ready, total) counts for pageservers belonging to the cluster.
func (r *ClusterReconciler) pageserverState(ctx context.Context, cluster *neonv1alpha1.Cluster) (int, int) {
	var pss neonv1alpha1.PageserverList
	if err := r.List(ctx, &pss,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{pageserverspec.ClusterLabel: cluster.Name},
	); err != nil {
		return 0, 0
	}

	ready := 0
	for _, ps := range pss.Items {
		if meta.IsStatusConditionTrue(ps.Status.Conditions, utils.ConditionAvailable) {
			ready++
		}
	}
	return ready, len(pss.Items)
}

func (r *ClusterReconciler) deploymentState(ctx context.Context, cluster *neonv1alpha1.Cluster, name, label string) (metav1.ConditionStatus, string, string) {
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, dep)
	switch {
	case apierrors.IsNotFound(err):
		return metav1.ConditionFalse, utils.ReasonChildResourceMissing, fmt.Sprintf("%s Deployment has not been observed yet", label)
	case err != nil:
		return metav1.ConditionUnknown, utils.ReasonChildResourceMissing, err.Error()
	case utils.IsDeploymentAvailable(dep):
		return metav1.ConditionTrue, utils.ReasonAsExpected, fmt.Sprintf("%s Deployment is Available", label)
	default:
		return metav1.ConditionFalse, utils.ReasonChildDeploymentNotAvailable, fmt.Sprintf("%s Deployment is not yet Available", label)
	}
}

func (r *ClusterReconciler) getCluster(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Cluster, error) {
	log := logf.FromContext(ctx)
	cluster := &neonv1alpha1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Cluster has been deleted")
			return nil, nil
		}

		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return cluster, nil
}

// finalize 处理 Cluster 的删除逻辑。
//
// 删除策略：
//  1. 列出所有引用此 Cluster 的 Project，逐个发起删除
//  2. 等待所有依赖 Project 完全清理（Project 的 finalize 会调用
//     Storage Controller DELETE tenant，级联清除 timeline）
//  3. Branch 的 finalize 会检测到 Project 不存在并优雅降级
//  4. 所有依赖清除后，移除 Cluster Finalizer，允许 K8s 级联删除子资源
//
// 在此期间 Storage Controller 仍可用（Cluster Finalizer 存在时 K8s
// 不会级联删除其 Owned 资源），因此 Project/Branch 可以正常完成外部清理。
func (r *ClusterReconciler) finalize(ctx context.Context, cluster *neonv1alpha1.Cluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cluster, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Finalizing Cluster deletion", "cluster", cluster.Name)

	// Step 1: 查找所有引用此 Cluster 的 Project
	var projects neonv1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(cluster.Namespace)); err != nil {
		log.Error(err, "Cluster 终止清理期间列出 Project 失败")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	var dependentProjects []string
	for i := range projects.Items {
		p := &projects.Items[i]
		if p.Spec.ClusterName == cluster.Name {
			dependentProjects = append(dependentProjects, p.Name)
		}
	}

	// Step 2: 如果仍有依赖 Project，删除它们并等待
	if len(dependentProjects) > 0 {
		log.Info("Cluster 仍有依赖 Project，正在删除",
			"cluster", cluster.Name, "projects", dependentProjects)

		// 对未被标记删除的 Project 发起删除
		for i := range projects.Items {
			p := &projects.Items[i]
			if p.Spec.ClusterName == cluster.Name && p.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
					log.Error(err, "删除依赖 Project 失败", "project", p.Name)
				} else if err == nil {
					log.Info("已发起 Project 删除", "project", p.Name)
				}
			}
		}

		// 更新状态，标记正在等待依赖清理
		_ = utils.PatchStatus(ctx, r.Client, cluster, func(c *neonv1alpha1.Cluster) {
			c.Status.ObservedGeneration = c.Generation
			conds := &c.Status.Conditions
			utils.SetCondition(c, conds, utils.ConditionTerminating,
				metav1.ConditionTrue, utils.ReasonTerminating,
				fmt.Sprintf("等待 %d 个关联 Project 删除完成: %v", len(dependentProjects), dependentProjects))
		})

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Step 3: 所有依赖已清除，清理 SC 节点记录并移除 Cluster Finalizer
	log.Info("所有依赖 Project 已清除，准备清理 SC 节点并移除 Cluster Finalizer",
		"cluster", cluster.Name)

	// Step 3a: 强制下线所有 SC 节点（兜底清理）
	// SC 节点记录（nodes + safekeepers）通过外部 PostgreSQL 持久化，
	// 跨集群生命周期存在。如果不在 Cluster 删除时主动清理：
	//   - Pageserver: 残留 Active 节点 → 重建时 re-attach 409 Conflict
	//   - Safekeeper: 残留 Active 记录 → 重建时可能分配到已不存在的节点
	if r.SCClient != nil {
		r.cleanupPageserverNodes(ctx, cluster)
		r.cleanupSafekeeperNodes(ctx, cluster)
	}

	// Step 3b: 移除 Cluster Finalizer

	// 重新获取以规避冲突
	current := &neonv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("重新获取 cluster 以移除 finalizer: %w", err)
	}

	if !controllerutil.ContainsFinalizer(current, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(current, utils.FinalizerName)
	if err := r.Update(ctx, current); err != nil {
		log.Error(err, "移除 finalizer 失败")
		return ctrl.Result{}, fmt.Errorf("移除 finalizer: %w", err)
	}

	log.Info("Finalizer 已移除，Cluster 将由 APIServer 删除",
		"cluster", cluster.Name)
	return ctrl.Result{}, nil
}

// cleanupPageserverNodes 在 Cluster 删除前强制下线所有 Pageserver 节点。
//
// SC 使用外部 PostgreSQL 数据库持久化节点记录（Nodes 表），
// 节点采用 Tombstone 机制（lifecycle='Deleted'）而非物理删除。
// 如果不在 Cluster 删除时清理节点记录：
//   - 节点以 lifecycle='Active' 残留在 SC 数据库
//   - 重建同名 Cluster 时，SC 加载旧节点记录
//   - Pageserver re-attach 时地址不匹配 → 409 Conflict → 永远 Not Ready
//
// 本方法调用 PUT /control/v1/node/{id}/delete?force=true，
// 跳过优雅 drain，直接标记节点为 Deleting 并执行 tombstone。
// 由于 Project 已全部清理（tenant/timeline 已删除），
// 此时节点上不应有任何 attached shard，force 删除是安全的。
func (r *ClusterReconciler) cleanupPageserverNodes(ctx context.Context, cluster *neonv1alpha1.Cluster) {
	log := logf.FromContext(ctx)

	var pss neonv1alpha1.PageserverList
	if err := r.List(ctx, &pss,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{pageserverspec.ClusterLabel: cluster.Name},
	); err != nil {
		log.Error(err, "Cluster 终止清理：列出 Pageserver 失败，跳过 SC 节点清理")
		return
	}

	for _, ps := range pss.Items {
		// 跳过已在删除中的 Pageserver
		if !ps.DeletionTimestamp.IsZero() {
			continue
		}

		log.Info("Cluster 终止清理：强制下线 Pageserver 节点",
			"nodeID", ps.Spec.ID, "pageserver", ps.Name)

		// force=true：跳过优雅 drain，直接标记 tombstone
		if err := r.SCClient.StartNodeDelete(ctx, cluster.Name, cluster.Namespace, ps.Spec.ID, true); err != nil {
			log.Error(err, "Cluster 终止清理：StartNodeDelete 失败（非阻塞）",
				"nodeID", ps.Spec.ID, "pageserver", ps.Name)
		} else {
			log.Info("Cluster 终止清理：节点已提交 tombstone",
				"nodeID", ps.Spec.ID, "pageserver", ps.Name)
		}
	}
}

// cleanupSafekeeperNodes 在 Cluster 删除前强制 Decommission 所有 Safekeeper 节点。
//
// 与 Pageserver 的 nodes 表不同，SC 的 safekeepers 表没有 Tombstone 机制——
// scheduling_policy 状态机（Active/Activating/Pause/Decomissioned）是唯一的生命周期管理手段。
//
// 如果不在 Cluster 删除时清理：
//   - safekeeper 记录以 scheduling_policy='Active' 残留在 SC 数据库
//   - SC 的 safekeepers_for_new_timeline 调度算法中，
//     Active 但 unreachable 的节点仍可能被分配新 timeline（排在候选列表末尾）
//   - 畸形 AZ 分布场景下可能向已不存在的节点写入数据
//
// 本方法调用 POST /control/v1/safekeeper/:id/scheduling_policy，
// 设置 SchedulingPolicy=Decomissioned。
// SC 会停止该节点的 reconciler、跳过心跳、排除调度。
func (r *ClusterReconciler) cleanupSafekeeperNodes(ctx context.Context, cluster *neonv1alpha1.Cluster) {
	log := logf.FromContext(ctx)

	var sks neonv1alpha1.SafekeeperList
	if err := r.List(ctx, &sks,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{safekeeperspec.ClusterLabel: cluster.Name},
	); err != nil {
		log.Error(err, "Cluster 终止清理：列出 Safekeeper 失败，跳过 SC 节点 Decommission")
		return
	}

	for _, sk := range sks.Items {
		log.Info("Cluster 终止清理：Decommission Safekeeper",
			"id", sk.Spec.ID, "safekeeper", sk.Name)

		if err := r.SCClient.DecommissionSafekeeper(ctx, &sk); err != nil {
			log.Error(err, "Cluster 终止清理：DecommissionSafekeeper 失败（非阻塞）",
				"id", sk.Spec.ID, "safekeeper", sk.Name)
		} else {
			log.Info("Cluster 终止清理：Safekeeper 已 Decommission",
				"id", sk.Spec.ID, "safekeeper", sk.Name)
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Cluster{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("cluster").
		Complete(r)
}
