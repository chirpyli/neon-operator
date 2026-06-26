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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
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
// 如果 SC 不可达，仅记录日志警告，不阻塞删除——SC 会通过心跳超时
// 自行检测 safekeeper 消失。
func (r *SafekeeperReconciler) finalize(ctx context.Context, sk *neonv1alpha1.Safekeeper) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(sk, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("正在终止 Safekeeper 删除，向 Storage Controller 发送 Decomissioned 请求",
		"safekeeper", sk.Name, "id", sk.Spec.ID)

	// 步骤1：在 SC 中标记 Decomissioned（尽力而为）
	if err := r.SCClient.DecommissionSafekeeper(ctx, sk); err != nil {
		// SC 不可达不是致命错误——它会通过心跳超时自行检测
		log.Info("SC Decommission 失败，继续删除流程",
			"error", err, "id", sk.Spec.ID)
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
		Named("safekeeper").
		Complete(r)
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
