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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

type projectStatusInputs struct {
	TenantIDErr error
	AttachErr   error
}

// ProjectReconciler reconciles a Project object
type ProjectReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	StorageControllerBaseURL string
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects/finalizers,verbs=update

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	project, err := r.getProject(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}

	if project == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.ProjectNameKey, project.Name)

	// === 删除路径：执行外部资源清理 ===
	if !project.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, project)
	}

	// === 创建/更新路径：确保 Finalizer 存在 ===
	if !controllerutil.ContainsFinalizer(project, utils.FinalizerName) {
		controllerutil.AddFinalizer(project, utils.FinalizerName)
		if err := r.Update(ctx, project); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Project Finalizer 已添加，直接继续调和")
		// 不依赖 Requeue 返回，而是直接 fall-through 继续后续调和逻辑。
		// 这样可以减少一次不必要的队列往返，提高批量创建场景的调和效率。
	}

	result, err := r.reconcile(ctx, project)
	if errors.Is(err, ErrRequeueAfterChange) {
		return result, nil
	} else if err != nil {
		log.Error(err, "Reconcile failed")
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *ProjectReconciler) reconcile(ctx context.Context, project *neonv1alpha1.Project) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if project.Spec.TenantID == "" {
		tenantID := utils.GenerateNeonID()
		if err := r.updateTenantID(ctx, project, tenantID); err != nil {
			log.Error(err, "Failed to update tenant ID")
			if statusErr := r.updateStatus(ctx, project, projectStatusInputs{TenantIDErr: fmt.Errorf("failed to update tenant ID: %w", err)}); statusErr != nil {
				log.Error(statusErr, "failed to update project status")
			}
			return ctrl.Result{}, fmt.Errorf("failed to update tenant ID: %w", err)
		}
		log.Info("Generated and set tenant ID", "tenantID", tenantID)
		return ctrl.Result{RequeueAfter: time.Second}, ErrRequeueAfterChange
	}

	attachErr := r.ensureTenantOnPageserver(ctx, project)
	if attachErr != nil {
		log.Error(attachErr, "Failed to ensure tenant on pageserver")
	}

	if statusErr := r.updateStatus(ctx, project, projectStatusInputs{AttachErr: attachErr}); statusErr != nil {
		log.Error(statusErr, "failed to update project status")
		return ctrl.Result{}, statusErr
	}

	if attachErr != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ProjectReconciler) updateStatus(ctx context.Context, project *neonv1alpha1.Project, in projectStatusInputs) error {
	return utils.PatchStatus(ctx, r.Client, project, func(p *neonv1alpha1.Project) {
		p.Status.ObservedGeneration = p.Generation
		conds := &p.Status.Conditions

		switch {
		case in.TenantIDErr != nil:
			utils.SetCondition(p, conds, utils.ConditionTenantIDAssigned, metav1.ConditionFalse, utils.ReasonTenantIDPending, in.TenantIDErr.Error())
		case p.Spec.TenantID != "":
			utils.SetCondition(p, conds, utils.ConditionTenantIDAssigned, metav1.ConditionTrue, utils.ReasonAsExpected, "Tenant ID is assigned")
		default:
			utils.SetCondition(p, conds, utils.ConditionTenantIDAssigned, metav1.ConditionFalse, utils.ReasonTenantIDPending, "Tenant ID has not been generated yet")
		}

		switch {
		case in.TenantIDErr != nil:
			utils.SetCondition(p, conds, utils.ConditionAttached, metav1.ConditionFalse, utils.ReasonTenantIDPending, "Tenant ID assignment failed")
		case in.AttachErr != nil:
			reason := utils.ReasonAttachFailed
			if isConnectionError(in.AttachErr) {
				reason = utils.ReasonStorageControllerUnreachable
			}
			utils.SetCondition(p, conds, utils.ConditionAttached, metav1.ConditionFalse, reason, in.AttachErr.Error())
		case p.Spec.TenantID == "":
			utils.SetCondition(p, conds, utils.ConditionAttached, metav1.ConditionFalse, utils.ReasonTenantIDPending, "Tenant ID has not been assigned")
		default:
			utils.SetCondition(p, conds, utils.ConditionAttached, metav1.ConditionTrue, utils.ReasonAsExpected, "Tenant attached on storage controller")
		}

		if in.TenantIDErr == nil && in.AttachErr == nil && p.Spec.TenantID != "" {
			utils.SetCondition(p, conds, utils.ConditionAvailable, metav1.ConditionTrue, utils.ReasonAsExpected, "Project is Available")
			utils.SetCondition(p, conds, utils.ConditionProgressing, metav1.ConditionFalse, utils.ReasonAsExpected, "Project is at desired state")
		} else {
			utils.SetCondition(p, conds, utils.ConditionAvailable, metav1.ConditionFalse, utils.ReasonReconciling, "Project is not yet Available")
			utils.SetCondition(p, conds, utils.ConditionProgressing, metav1.ConditionTrue, utils.ReasonReconciling, "Working toward desired state")
		}
	})
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if utilnet.IsConnectionRefused(err) || utilnet.IsConnectionReset(err) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func (r *ProjectReconciler) getProject(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Project, error) {
	log := logf.FromContext(ctx)
	project := &neonv1alpha1.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Project has been deleted")
			return nil, nil
		}

		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return project, nil
}

func (r *ProjectReconciler) updateTenantID(ctx context.Context, project *neonv1alpha1.Project, tenantID string) error {
	current := &neonv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Name: project.GetName(), Namespace: project.GetNamespace()}, current); err != nil {
		return err
	}

	updated := current.DeepCopy()
	updated.Spec.TenantID = tenantID
	updated.ManagedFields = nil

	if err := r.Patch(ctx, updated, client.MergeFrom(current), &client.PatchOptions{FieldManager: "neon-operator"}); err != nil {
		return err
	}

	project.Spec.TenantID = tenantID
	return nil
}

func (r *ProjectReconciler) ensureTenantOnPageserver(ctx context.Context, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	base := r.StorageControllerBaseURL
	if base == "" {
		base = storagecontroller.URL(project.Spec.ClusterName)
	}
	storageControllerURL := fmt.Sprintf("%s/v1/tenant/%s/location_config", base, project.Spec.TenantID)

	log.Info("Sending request to storage controller", "url", storageControllerURL)

	requestBody := []byte(`{"mode": "AttachedSingle", "generation": 1, "tenant_conf": {}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, storageControllerURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Info("Failed to connect to storage controller, will retry", "error", err, "url", storageControllerURL)
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error(err, "failed to close response body")
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Info("Storage controller returned error status", "status", resp.Status)
		return fmt.Errorf("storage controller returned status: %s", resp.Status)
	}

	log.Info("Successfully created tenant on storage controller")
	return nil
}

// finalize 处理 Project 的删除逻辑。在移除 Finalizer 之前，
// 确保 Storage Controller 中的 tenant 先被删除。
func (r *ProjectReconciler) finalize(ctx context.Context, project *neonv1alpha1.Project) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(project, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Finalizing Project deletion",
		"project", project.Name, "tenantID", project.Spec.TenantID)

	// 更新状态，标记正在进行终止清理
	_ = utils.PatchStatus(ctx, r.Client, project, func(p *neonv1alpha1.Project) {
		p.Status.ObservedGeneration = p.Generation
		utils.SetCondition(p, &p.Status.Conditions, utils.ConditionTerminating,
			metav1.ConditionTrue, utils.ReasonTerminating,
			"正在从 Storage Controller 删除 tenant")
		utils.SetCondition(p, &p.Status.Conditions, utils.ConditionAvailable,
			metav1.ConditionFalse, utils.ReasonTerminating, "Project 正在被删除")
	})

	// 如果 tenant 从未创建，跳过外部清理
	if project.Spec.TenantID == "" {
		log.Info("TenantID 为空，跳过 tenant 删除",
			"project", project.Name)
		return r.removeFinalizer(ctx, project)
	}

	delErr := r.deleteTenant(ctx, project.Spec.ClusterName, project.Spec.TenantID)
	if delErr != nil {
		log.Error(delErr, "从 Storage Controller 删除 tenant 失败，将重试",
			"project", project.Name, "tenantID", project.Spec.TenantID)

		_ = utils.PatchStatus(ctx, r.Client, project, func(p *neonv1alpha1.Project) {
			utils.SetCondition(p, &p.Status.Conditions, utils.ConditionTerminating,
				metav1.ConditionTrue, utils.ReasonExternalCleanupFailed,
				fmt.Sprintf("删除 tenant 失败: %v", delErr))
		})
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	log.Info("已从 Storage Controller 删除 tenant",
		"project", project.Name, "tenantID", project.Spec.TenantID)

	return r.removeFinalizer(ctx, project)
}

// deleteTenant 向 Storage Controller 发送 DELETE 请求以删除 tenant。
// 将 404 视为成功（幂等性保证）。
func (r *ProjectReconciler) deleteTenant(ctx context.Context, clusterName, tenantID string) error {
	log := logf.FromContext(ctx)

	base := r.StorageControllerBaseURL
	if base == "" {
		base = storagecontroller.URL(clusterName)
	}
	deleteURL := fmt.Sprintf("%s/v1/tenant/%s", base, tenantID)

	log.Info("Deleting tenant from Storage Controller", "url", deleteURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("create delete request: %w", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to storage controller: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error(err, "failed to close response body")
		}
	}()

	// 200: 删除成功
	// 404: 已被删除（幂等）
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		log.Info("Tenant 删除请求成功", "status", resp.StatusCode)
		return nil
	}

	return fmt.Errorf("storage controller 返回状态码 %d", resp.StatusCode)
}

// removeFinalizer 从 Project 中移除 Finalizer，允许 Kubernetes 完成删除。
func (r *ProjectReconciler) removeFinalizer(ctx context.Context, project *neonv1alpha1.Project) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 重新获取以规避冲突
	current := &neonv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Name: project.Name, Namespace: project.Namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("重新获取 project 以移除 finalizer: %w", err)
	}

	if !controllerutil.ContainsFinalizer(current, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(current, utils.FinalizerName)
	if err := r.Update(ctx, current); err != nil {
		log.Error(err, "移除 finalizer 失败")
		return ctrl.Result{}, fmt.Errorf("移除 finalizer: %w", err)
	}

	log.Info("Finalizer 已移除，Project 将由 APIServer 删除",
		"project", project.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Project{}).
		Named("project").
		Complete(r)
}
