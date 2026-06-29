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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

// OperationReconciler reconciles an Operation object.
// 职责：
// 1. 将 Spec.Status 从 scheduling → running 推进
// 2. 根据关联资源（Project/Branch/Endpoint）的 Conditions 判断操作是否完成
// 3. 操作完成/失败后更新 Spec.Status 为 finished/failed
// 4. 重试控制（FailuresCount）
type OperationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=operations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=operations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches,verbs=get;list;watch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=endpoints,verbs=get;list;watch

func (r *OperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	operation, err := r.getOperation(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if operation == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.OperationNameKey, operation.Name)

	// 终态：不调和
	if operation.Spec.Status == "finished" || operation.Spec.Status == "failed" {
		return ctrl.Result{}, nil
	}

	// scheduling → running
	if operation.Spec.Status == "scheduling" {
		return r.transitionToRunning(ctx, operation)
	}

	// running → 检查关联资源状态
	return r.checkCompletion(ctx, operation)
}

func (r *OperationReconciler) getOperation(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Operation, error) {
	log := logf.FromContext(ctx)
	operation := &neonv1alpha1.Operation{}
	if err := r.Get(ctx, req.NamespacedName, operation); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Operation has been deleted")
			return nil, nil
		}
		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return operation, nil
}

// transitionToRunning 将 Operation 从 scheduling 推进到 running。
func (r *OperationReconciler) transitionToRunning(ctx context.Context, operation *neonv1alpha1.Operation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 更新 Spec.Status → running
	current := &neonv1alpha1.Operation{}
	if err := r.Get(ctx, types.NamespacedName{Name: operation.Name, Namespace: operation.Namespace}, current); err != nil {
		return ctrl.Result{}, err
	}

	updated := current.DeepCopy()
	updated.Spec.Status = "running"
	updated.ManagedFields = nil

	if err := r.Patch(ctx, updated, client.MergeFrom(current), &client.PatchOptions{FieldManager: "neon-operator"}); err != nil {
		return ctrl.Result{}, err
	}

	// 更新 Status
	if err := r.updateOpStatus(ctx, operation, nil); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Operation transitioned to running", "action", operation.Spec.Action)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// checkCompletion 检查关联资源是否完成调和，决定 Operation 是否 finished/failed。
func (r *OperationReconciler) checkCompletion(ctx context.Context, operation *neonv1alpha1.Operation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 根据 Action 确定需要检查哪些资源
	var checkErr error

	switch {
	case isProjectAction(operation.Spec.Action):
		checkErr = r.checkProjectReady(ctx, operation)
	case isBranchAction(operation.Spec.Action):
		checkErr = r.checkBranchReady(ctx, operation)
	case isEndpointAction(operation.Spec.Action):
		checkErr = r.checkEndpointReady(ctx, operation)
	default:
		// 不支持的 action 类型，标记为 finished
		log.Info("Unknown action, marking as finished", "action", operation.Spec.Action)
	}

	if checkErr != nil {
		// 增加失败计数
		current := &neonv1alpha1.Operation{}
		if err := r.Get(ctx, types.NamespacedName{Name: operation.Name, Namespace: operation.Namespace}, current); err != nil {
			return ctrl.Result{}, err
		}

		updated := current.DeepCopy()
		updated.Spec.FailuresCount++
		if updated.Spec.FailuresCount > 10 {
			updated.Spec.Status = "failed"
			updated.Spec.Error = checkErr.Error()
		} else {
			updated.Spec.Error = checkErr.Error()
		}
		updated.ManagedFields = nil

		if err := r.Patch(ctx, updated, client.MergeFrom(current), &client.PatchOptions{FieldManager: "neon-operator"}); err != nil {
			return ctrl.Result{}, err
		}

		// 更新 Status
		if err := r.updateOpStatus(ctx, operation, checkErr); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// 成功：标记为 finished
	current := &neonv1alpha1.Operation{}
	if err := r.Get(ctx, types.NamespacedName{Name: operation.Name, Namespace: operation.Namespace}, current); err != nil {
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	updated := current.DeepCopy()
	updated.Spec.Status = "finished"
	updated.ManagedFields = nil

	if err := r.Patch(ctx, updated, client.MergeFrom(current), &client.PatchOptions{FieldManager: "neon-operator"}); err != nil {
		return ctrl.Result{}, err
	}

	// 更新 Status
	if err := r.updateOpStatusFinish(ctx, operation, &now); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Operation finished", "action", operation.Spec.Action)
	return ctrl.Result{}, nil
}

func (r *OperationReconciler) checkProjectReady(ctx context.Context, operation *neonv1alpha1.Operation) error {
	project := &neonv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Name: operation.Spec.ProjectID, Namespace: operation.Namespace}, project); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("project not found")
		}
		return err
	}

	for _, c := range project.Status.Conditions {
		if c.Type == utils.ConditionAvailable && c.Status == metav1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("project not yet available")
}

func (r *OperationReconciler) checkBranchReady(ctx context.Context, operation *neonv1alpha1.Operation) error {
	branchID := operation.Spec.BranchID
	if branchID == "" {
		branchID = operation.Spec.ProjectID // fallback: 以 projectID 查找 branch
	}

	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: branchID, Namespace: operation.Namespace}, branch); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("branch not found")
		}
		return err
	}

	for _, c := range branch.Status.Conditions {
		if c.Type == utils.ConditionAvailable && c.Status == metav1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("branch not yet available")
}

func (r *OperationReconciler) checkEndpointReady(ctx context.Context, operation *neonv1alpha1.Operation) error {
	endpointID := operation.Spec.EndpointID
	if endpointID == "" {
		return fmt.Errorf("endpointID not specified")
	}

	endpoint := &neonv1alpha1.Endpoint{}
	if err := r.Get(ctx, types.NamespacedName{Name: endpointID, Namespace: operation.Namespace}, endpoint); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("endpoint not found")
		}
		return err
	}

	for _, c := range endpoint.Status.Conditions {
		if c.Type == utils.ConditionAvailable && c.Status == metav1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("endpoint not yet available")
}

func (r *OperationReconciler) updateOpStatus(ctx context.Context, operation *neonv1alpha1.Operation, err error) error {
	return utils.PatchStatus(ctx, r.Client, operation, func(o *neonv1alpha1.Operation) {
		o.Status.ObservedGeneration = o.Generation
		conds := &o.Status.Conditions

		if err != nil {
			utils.SetCondition(o, conds, utils.ConditionProgressing, metav1.ConditionTrue,
				utils.ReasonReconciling, err.Error())
		} else {
			utils.SetCondition(o, conds, utils.ConditionProgressing, metav1.ConditionTrue,
				utils.ReasonReconciling, "Waiting for resources to be ready")
		}
	})
}

func (r *OperationReconciler) updateOpStatusFinish(ctx context.Context, operation *neonv1alpha1.Operation, completedAt *metav1.Time) error {
	return utils.PatchStatus(ctx, r.Client, operation, func(o *neonv1alpha1.Operation) {
		o.Status.ObservedGeneration = o.Generation
		o.Status.CompletedAt = completedAt
		if completedAt != nil {
			o.Status.TotalDurationMs = completedAt.Time.Sub(o.CreationTimestamp.Time).Milliseconds()
		}
		conds := &o.Status.Conditions
		utils.SetCondition(o, conds, utils.ConditionAvailable, metav1.ConditionTrue,
			utils.ReasonAsExpected, "Operation completed successfully")
		utils.SetCondition(o, conds, utils.ConditionProgressing, metav1.ConditionFalse,
			utils.ReasonAsExpected, "Operation is finished")
	})
}

// Action 分类辅助函数
func isProjectAction(action string) bool {
	switch action {
	case "create_project", "delete_project", "update_project":
		return true
	}
	return false
}

func isBranchAction(action string) bool {
	switch action {
	case "create_branch", "delete_branch", "copy_branch":
		return true
	}
	return false
}

func isEndpointAction(action string) bool {
	switch action {
	case "create_endpoint", "delete_endpoint", "update_endpoint":
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *OperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Operation{}).
		Named("operation").
		Complete(r)
}
