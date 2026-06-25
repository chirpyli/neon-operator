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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/storagebroker"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

// This is not a proper error. It indicated we should return a empty requeue after an object has been changed.
var ErrRequeueAfterChange = errors.New("requeue after change")

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters/finalizers,verbs=update
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
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		log.Info("Finalizer added to Cluster, requeuing")
		return ctrl.Result{Requeue: true}, nil
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

	// Step 3: 所有依赖已清除，移除 Cluster Finalizer
	log.Info("所有依赖 Project 已清除，移除 Cluster Finalizer",
		"cluster", cluster.Name)

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

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Cluster{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("cluster").
		Complete(r)
}
