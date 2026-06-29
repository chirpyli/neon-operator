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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

// DatabaseReconciler reconciles a Database object.
// 职责：
// 1. 验证 Owner Role 是否存在（如果指定了 ownerName）
// 2. 更新 Database Status (Phase)
// 3. 变更时触发关联 Endpoint 的 ConfigMap 更新
type DatabaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=databases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=databases/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=roles,verbs=get;list;watch

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	database, err := r.getDatabase(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if database == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.DatabaseNameKey, database.Name)

	// 删除路径
	if !database.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, database)
	}

	// 创建/更新路径：确保 Finalizer
	if !controllerutil.ContainsFinalizer(database, utils.FinalizerName) {
		controllerutil.AddFinalizer(database, utils.FinalizerName)
		if err := r.Update(ctx, database); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Database Finalizer 已添加")
	}

	// 校验 Branch 存在
	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: database.Spec.BranchID, Namespace: database.Namespace}, branch); err != nil {
		log.Error(err, "failed to get branch", "branchID", database.Spec.BranchID)
		return r.updateStatus(ctx, database, fmt.Errorf("branch not found: %w", err))
	}

	// 如果指定了 ownerName，验证 Role 是否存在
	var validationErr error
	if database.Spec.OwnerName != "" {
		if err := r.validateOwnerRole(ctx, database); err != nil {
			validationErr = err
		}
	}

	// 触发 Endpoint ConfigMap 更新
	if validationErr == nil {
		_ = r.triggerEndpointReconcile(ctx, database)
	}

	return r.updateStatus(ctx, database, validationErr)
}

func (r *DatabaseReconciler) getDatabase(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Database, error) {
	log := logf.FromContext(ctx)
	database := &neonv1alpha1.Database{}
	if err := r.Get(ctx, req.NamespacedName, database); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Database has been deleted")
			return nil, nil
		}
		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return database, nil
}

// validateOwnerRole 验证 ownerName 引用的 Role 存在且在同一分支。
func (r *DatabaseReconciler) validateOwnerRole(ctx context.Context, database *neonv1alpha1.Database) error {
	// Owner Role 的 K8s 名称通常为 "{branchID}-{roleName}"
	roleName := fmt.Sprintf("%s-%s", database.Spec.BranchID, database.Spec.OwnerName)
	role := &neonv1alpha1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Name: roleName, Namespace: database.Namespace}, role); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("owner role %q not found", database.Spec.OwnerName)
		}
		return fmt.Errorf("failed to get owner role: %w", err)
	}
	if role.Spec.BranchID != database.Spec.BranchID {
		return fmt.Errorf("owner role %q belongs to branch %q, not %q",
			database.Spec.OwnerName, role.Spec.BranchID, database.Spec.BranchID)
	}
	return nil
}

// triggerEndpointReconcile 触发 Endpoint Controller 重建 ConfigMap。
func (r *DatabaseReconciler) triggerEndpointReconcile(ctx context.Context, database *neonv1alpha1.Database) error {
	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: database.Spec.BranchID, Namespace: database.Namespace}, branch); err != nil {
		return fmt.Errorf("get branch for trigger: %w", err)
	}

	if branch.Annotations == nil {
		branch.Annotations = make(map[string]string)
	}
	branch.Annotations["neon.oltp.molnett.org/last-database-change"] = time.Now().UTC().Format(time.RFC3339)
	return r.Update(ctx, branch)
}

func (r *DatabaseReconciler) updateStatus(ctx context.Context, database *neonv1alpha1.Database, validationErr error) (ctrl.Result, error) {
	err := utils.PatchStatus(ctx, r.Client, database, func(d *neonv1alpha1.Database) {
		d.Status.ObservedGeneration = d.Generation
		conds := &d.Status.Conditions

		if validationErr != nil {
			utils.SetCondition(d, conds, utils.ConditionAvailable, metav1.ConditionFalse,
				"ValidationFailed", validationErr.Error())
			d.Status.Phase = ""
			return
		}

		d.Status.Phase = "active"
		utils.SetCondition(d, conds, utils.ConditionAvailable, metav1.ConditionTrue,
			utils.ReasonAsExpected, "Database is Active")
		utils.SetCondition(d, conds, utils.ConditionProgressing, metav1.ConditionFalse,
			utils.ReasonAsExpected, "Database is at desired state")
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DatabaseReconciler) finalize(ctx context.Context, database *neonv1alpha1.Database) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(database, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	// [Phase 2.7] 删除前触发 Endpoint ConfigMap 更新，从 spec 中移除该数据库
	if err := r.triggerEndpointReconcile(ctx, database); err != nil {
		log.Error(err, "failed to trigger endpoint reconcile during database deletion")
		// 不返回错误，允许继续删除（最终一致性）
	}

	current := &neonv1alpha1.Database{}
	if err := r.Get(ctx, types.NamespacedName{Name: database.Name, Namespace: database.Namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(current, utils.FinalizerName)
	if err := r.Update(ctx, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	log.Info("Database Finalizer 已移除", "database", database.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Database{}).
		Named("database").
		Complete(r)
}
