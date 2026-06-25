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
	pageserverspec "oltp.molnett.org/neon-operator/specs/pageserver"
	"oltp.molnett.org/neon-operator/utils"
)

type PageserverReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=pageservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

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

	// === 删除路径：执行外部资源清理 ===
	if !pageserver.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, pageserver)
	}

	// === 创建/更新路径：确保 Finalizer 存在 ===
	if !controllerutil.ContainsFinalizer(pageserver, utils.FinalizerName) {
		controllerutil.AddFinalizer(pageserver, utils.FinalizerName)
		if err := r.Update(ctx, pageserver); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		log.Info("Finalizer added to Pageserver, requeuing")
		return ctrl.Result{Requeue: true}, nil
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

// finalize 处理 Pageserver 的删除逻辑。
// 当前无需调用外部 API；此方法作为预留钩子，
// 用于后续扩展（例如从 Storage Controller 注销 pageserver）。
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
			"Pageserver 正在被删除")
	})

	// 重新获取以规避冲突
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

func (r *PageserverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Pageserver{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named("pageserver").
		Complete(r)
}
