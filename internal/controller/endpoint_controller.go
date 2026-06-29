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
	"oltp.molnett.org/neon-operator/utils"
)

// EndpointReconciler reconciles an Endpoint object
type EndpointReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=endpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=endpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches,verbs=get;list;watch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *EndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	endpoint, err := r.getEndpoint(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if endpoint == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.EndpointNameKey, endpoint.Name)

	// 删除路径
	if !endpoint.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, endpoint)
	}

	// 创建/更新路径：确保 Finalizer
	if !controllerutil.ContainsFinalizer(endpoint, utils.FinalizerName) {
		controllerutil.AddFinalizer(endpoint, utils.FinalizerName)
		if err := r.Update(ctx, endpoint); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Endpoint Finalizer 已添加")
	}

	result, err := r.reconcile(ctx, endpoint)
	if errors.Is(err, ErrRequeueAfterChange) {
		return result, nil
	} else if err != nil {
		log.Error(err, "Reconcile failed")
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *EndpointReconciler) getEndpoint(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Endpoint, error) {
	log := logf.FromContext(ctx)
	endpoint := &neonv1alpha1.Endpoint{}
	if err := r.Get(ctx, req.NamespacedName, endpoint); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Endpoint has been deleted")
			return nil, nil
		}
		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return endpoint, nil
}

func (r *EndpointReconciler) reconcile(ctx context.Context, endpoint *neonv1alpha1.Endpoint) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 获取 Branch 和 Project
	branch, err := r.getBranch(ctx, endpoint.Spec.BranchID, endpoint.Namespace)
	if err != nil {
		log.Error(err, "failed to get branch")
		return r.updateStatus(ctx, endpoint, err, nil)
	}

	project, err := r.getProject(ctx, branch.Spec.ProjectID, endpoint.Namespace)
	if err != nil {
		log.Error(err, "failed to get project")
		return r.updateStatus(ctx, endpoint, nil, err)
	}

	// 如果 Disabled，将 Deployment replicas 设为 0，不删除 Service
	if endpoint.Spec.Disabled {
		if err := r.reconcileDisabled(ctx, endpoint, branch, project); err != nil {
			log.Error(err, "failed to reconcile disabled endpoint")
			return r.updateStatus(ctx, endpoint, err, nil)
		}
		return r.updateStatus(ctx, endpoint, nil, nil)
	}

	// 正常路径：创建/更新 Compute 资源
	createErr := r.createEndpointResources(ctx, endpoint, branch, project)
	if createErr != nil {
		log.Error(createErr, "error while creating endpoint resources")
	}

	if _, statusErr := r.updateStatus(ctx, endpoint, createErr, nil); statusErr != nil {
		log.Error(statusErr, "failed to update endpoint status")
		return ctrl.Result{}, statusErr
	}

	if createErr != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *EndpointReconciler) getBranch(ctx context.Context, branchID, namespace string) (*neonv1alpha1.Branch, error) {
	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: branchID, Namespace: namespace}, branch); err != nil {
		return nil, fmt.Errorf("failed to get branch %s: %w", branchID, err)
	}
	return branch, nil
}

func (r *EndpointReconciler) getProject(ctx context.Context, projectID, namespace string) (*neonv1alpha1.Project, error) {
	project := &neonv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Name: projectID, Namespace: namespace}, project); err != nil {
		return nil, fmt.Errorf("failed to get project %s: %w", projectID, err)
	}
	return project, nil
}

func (r *EndpointReconciler) updateStatus(ctx context.Context, endpoint *neonv1alpha1.Endpoint, createErr, parentErr error) (ctrl.Result, error) {
	deploymentName := endpointDeploymentName(endpoint)
	dep := &appsv1.Deployment{}
	depErr := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: endpoint.Namespace}, dep)

	patchErr := utils.PatchStatus(ctx, r.Client, endpoint, func(e *neonv1alpha1.Endpoint) {
		e.Status.ObservedGeneration = e.Generation
		conds := &e.Status.Conditions

		// 父资源可用性
		if parentErr != nil {
			utils.SetCondition(e, conds, utils.ConditionAvailable, metav1.ConditionFalse,
				utils.ReasonParentResourceMissing, parentErr.Error())
			e.Status.Phase = "creating_compute"
			return
		}

		// 禁用状态
		if e.Spec.Disabled {
			utils.SetCondition(e, conds, utils.ConditionAvailable, metav1.ConditionFalse,
				"Disabled", "Endpoint is disabled")
			e.Status.Phase = "stopped"
			return
		}

		// 资源创建状态
		switch {
		case createErr != nil:
			utils.SetCondition(e, conds, utils.ConditionResourcesReady, metav1.ConditionFalse,
				utils.ReasonResourceCreateFailed, createErr.Error())
			e.Status.Phase = "creating_compute"
		case apierrors.IsNotFound(depErr):
			utils.SetCondition(e, conds, utils.ConditionResourcesReady, metav1.ConditionFalse,
				utils.ReasonChildResourceMissing, "Compute Deployment has not been observed yet")
			e.Status.Phase = "creating_compute"
		case depErr != nil:
			utils.SetCondition(e, conds, utils.ConditionResourcesReady, metav1.ConditionUnknown,
				utils.ReasonChildResourceMissing, depErr.Error())
			e.Status.Phase = "starting"
		case utils.IsDeploymentAvailable(dep):
			utils.SetCondition(e, conds, utils.ConditionResourcesReady, metav1.ConditionTrue,
				utils.ReasonAsExpected, "Compute Deployment is Available")
			e.Status.Phase = "active"
		default:
			utils.SetCondition(e, conds, utils.ConditionResourcesReady, metav1.ConditionFalse,
				utils.ReasonChildDeploymentNotAvailable, "Compute Deployment is not Available yet")
			e.Status.Phase = "starting"
		}

		// 总体可用性
		if e.Status.Phase == "active" {
			utils.SetCondition(e, conds, utils.ConditionAvailable, metav1.ConditionTrue,
				utils.ReasonAsExpected, "Endpoint is Available")
			utils.SetCondition(e, conds, utils.ConditionProgressing, metav1.ConditionFalse,
				utils.ReasonAsExpected, "Endpoint is at desired state")
		} else {
			utils.SetCondition(e, conds, utils.ConditionProgressing, metav1.ConditionTrue,
				utils.ReasonReconciling, "Working toward desired state")
		}

		// 从 Service 提取 Host/Port
		pgSvcName := fmt.Sprintf("endpoint-%s-postgres", e.Name)
		pgSvc := &corev1.Service{}
		if svcErr := r.Get(ctx, types.NamespacedName{Name: pgSvcName, Namespace: e.Namespace}, pgSvc); svcErr == nil {
			e.Status.Port = 55433
			// 优先从 LoadBalancer ingress 获取 host
			if len(pgSvc.Status.LoadBalancer.Ingress) > 0 {
				ing := pgSvc.Status.LoadBalancer.Ingress[0]
				if ing.Hostname != "" {
					e.Status.Host = ing.Hostname
				} else if ing.IP != "" {
					e.Status.Host = ing.IP
				}
			} else if pgSvc.Spec.Type == corev1.ServiceTypeClusterIP {
				// 集群内部：使用服务名
				e.Status.Host = fmt.Sprintf("%s.%s.svc.cluster.local", pgSvcName, e.Namespace)
			}
		}
	})
	if patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{}, nil
}

func (r *EndpointReconciler) finalize(ctx context.Context, endpoint *neonv1alpha1.Endpoint) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(endpoint, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Finalizing Endpoint deletion", "endpoint", endpoint.Name)

	_ = utils.PatchStatus(ctx, r.Client, endpoint, func(e *neonv1alpha1.Endpoint) {
		e.Status.ObservedGeneration = e.Generation
		e.Status.Phase = "stopping"
		utils.SetCondition(e, &e.Status.Conditions, utils.ConditionTerminating,
			metav1.ConditionTrue, utils.ReasonTerminating, "正在清理 Endpoint 资源")
	})

	// 重新获取以规避冲突
	current := &neonv1alpha1.Endpoint{}
	if err := r.Get(ctx, types.NamespacedName{Name: endpoint.Name, Namespace: endpoint.Namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("re-fetch endpoint: %w", err)
	}

	if !controllerutil.ContainsFinalizer(current, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(current, utils.FinalizerName)
	if err := r.Update(ctx, current); err != nil {
		log.Error(err, "移除 finalizer 失败")
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	log.Info("Finalizer 已移除", "endpoint", endpoint.Name)
	return ctrl.Result{}, nil
}

// reconcileDisabled 禁用端点：Deployment replicas=0，保留 Service
func (r *EndpointReconciler) reconcileDisabled(ctx context.Context, endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	deploymentName := endpointDeploymentName(endpoint)
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: endpoint.Namespace}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("No Deployment to disable", "endpoint", endpoint.Name)
			return nil
		}
		return err
	}

	// 再检查一遍以防状态变更
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
		return nil
	}

	patch := client.MergeFrom(dep.DeepCopy())
	dep.Spec.Replicas = ptrToInt32(0)
	return r.Patch(ctx, dep, patch)
}

// =============================================================================
// SetupWithManager
// =============================================================================

func (r *EndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Endpoint{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named("endpoint").
		Complete(r)
}
