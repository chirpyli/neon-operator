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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	safekeeperspec "oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/utils"
)

type SafekeeperReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	StorageControllerBaseURL string
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=safekeepers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *SafekeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	safekeeper, err := r.getSafekeeper(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if safekeeper == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.SafekeeperNameKey, safekeeper.Name)

	// === 删除路径：执行外部资源清理 ===
	if !safekeeper.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, safekeeper)
	}

	// === 创建/更新路径：确保 Finalizer 存在 ===
	if !controllerutil.ContainsFinalizer(safekeeper, utils.FinalizerName) {
		controllerutil.AddFinalizer(safekeeper, utils.FinalizerName)
		if err := r.Update(ctx, safekeeper); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		log.Info("Finalizer added to Safekeeper, requeuing")
		return ctrl.Result{Requeue: true}, nil
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
			log.Info("Safekeeper has been deleted")
			return nil, nil
		}

		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return safekeeper, nil
}

//nolint:unparam
func (r *SafekeeperReconciler) reconcile(ctx context.Context, safekeeper *neonv1alpha1.Safekeeper) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	createErr := r.createSafekeeperResources(ctx, safekeeper)
	if createErr != nil {
		log.Error(createErr, "error while creating safekeeper resources")
	}

	stsName := safekeeperspec.Name(safekeeper)
	if err := utils.UpdateSTSBackedStatus(ctx, r.Client, safekeeper, stsName, "Safekeeper", createErr); err != nil {
		log.Error(err, "failed to update safekeeper status")
		return ctrl.Result{}, err
	}

	if createErr != nil {
		return ctrl.Result{}, fmt.Errorf("not able to create safekeeper resources: %w", createErr)
	}
	return ctrl.Result{}, nil
}

// finalize 处理 Safekeeper 的删除逻辑。
//
// 当前实现直接移除 Finalizer，不向 Storage Controller 发送注销请求。
//
// TODO: 从 Storage Controller 注销 safekeeper 的逻辑待后续实现。
// Neon 的 Storage Controller 目前没有 safekeeper 的 DELETE 端点，
// 且 safekeeper 删除涉及数据迁移等问题，不可简单注销。
// 详见 docs/design/safekeeper-deletion.md
func (r *SafekeeperReconciler) finalize(ctx context.Context, sk *neonv1alpha1.Safekeeper) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(sk, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info("Finalizing Safekeeper deletion (Storage Controller deregistration not yet implemented)",
		"safekeeper", sk.Name, "id", sk.Spec.ID)

	controllerutil.RemoveFinalizer(sk, utils.FinalizerName)
	if err := r.Update(ctx, sk); err != nil {
		log.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	log.Info("Finalizer removed, Safekeeper will be deleted by APIServer",
		"safekeeper", sk.Name)
	return ctrl.Result{}, nil
}

func (r *SafekeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Safekeeper{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("safekeeper").
		Complete(r)
}
