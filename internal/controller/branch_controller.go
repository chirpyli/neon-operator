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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

// BranchReconciler reconciles a Branch object
type BranchReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	StorageControllerBaseURL string
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

func (r *BranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	branch, err := r.getBranch(ctx, req)
	if err != nil || branch == nil {
		return ctrl.Result{}, err
	}

	ctx = context.WithValue(ctx, utils.BranchNameKey, branch.Name)

	result, err := r.reconcile(ctx, branch)
	if errors.Is(err, ErrRequeueAfterChange) {
		return result, nil
	} else if err != nil {
		log.Error(err, "Reconcile failed")
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *BranchReconciler) getBranch(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Branch, error) {
	log := logf.FromContext(ctx)
	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, req.NamespacedName, branch); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Branch has been deleted")
			return nil, nil
		}

		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return branch, nil
}

func (r *BranchReconciler) reconcile(ctx context.Context, branch *neonv1alpha1.Branch) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if branch.Spec.TimelineID == "" {
		if err := r.updateTimelineID(ctx, branch); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update timelineID: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	project, err := r.getProject(ctx, branch.Spec.ProjectID, branch.Namespace)
	if err != nil {
		log.Error(err, "failed to get project", "projectID", branch.Spec.ProjectID)
		return ctrl.Result{}, err
	}

	timelineErr := r.ensureTimeline(ctx, branch, project)
	if timelineErr != nil {
		log.Error(timelineErr, "failed to ensure timeline")
	}

	var createErr error
	if timelineErr == nil {
		createErr = r.createBranchResources(ctx, branch, project)
		if createErr != nil {
			log.Error(createErr, "error while creating branch resources")
		}
	}

	if statusErr := r.updateStatus(ctx, branch, timelineErr, createErr); statusErr != nil {
		log.Error(statusErr, "failed to update branch status")
		return ctrl.Result{}, statusErr
	}

	if timelineErr != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if createErr != nil {
		return ctrl.Result{}, fmt.Errorf("not able to create branch resources: %w", createErr)
	}
	return ctrl.Result{}, nil
}

func (r *BranchReconciler) updateStatus(ctx context.Context, branch *neonv1alpha1.Branch, timelineErr, createErr error) error {
	deploymentName := fmt.Sprintf("%s-compute-node", branch.Name)
	dep := &appsv1.Deployment{}
	depErr := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: branch.Namespace}, dep)

	return utils.PatchStatus(ctx, r.Client, branch, func(b *neonv1alpha1.Branch) {
		b.Status.ObservedGeneration = b.Generation
		conds := &b.Status.Conditions

		if b.Spec.TimelineID != "" {
			utils.SetCondition(b, conds, utils.ConditionTimelineIDAssigned, metav1.ConditionTrue, utils.ReasonAsExpected, "Timeline ID is assigned")
		} else {
			utils.SetCondition(b, conds, utils.ConditionTimelineIDAssigned, metav1.ConditionFalse, utils.ReasonTimelineIDPending, "Timeline ID has not been generated yet")
		}

		if timelineErr != nil {
			utils.SetCondition(b, conds, utils.ConditionTimelineCreated, metav1.ConditionFalse, utils.ReasonTimelineCreationFailed, timelineErr.Error())
		} else {
			utils.SetCondition(b, conds, utils.ConditionTimelineCreated, metav1.ConditionTrue, utils.ReasonAsExpected, "Timeline created on storage controller")
		}

		computeReady := metav1.ConditionFalse
		computeReason := utils.ReasonChildDeploymentNotAvailable
		computeMessage := "Compute Deployment is not Available yet"
		switch {
		case createErr != nil:
			computeReason = utils.ReasonResourceCreateFailed
			computeMessage = createErr.Error()
		case apierrors.IsNotFound(depErr):
			computeReason = utils.ReasonChildResourceMissing
			computeMessage = "Compute Deployment has not been observed yet"
		case depErr != nil:
			computeReady = metav1.ConditionUnknown
			computeReason = utils.ReasonChildResourceMissing
			computeMessage = depErr.Error()
		case utils.IsDeploymentAvailable(dep):
			computeReady = metav1.ConditionTrue
			computeReason = utils.ReasonAsExpected
			computeMessage = "Compute Deployment is Available"
		}
		utils.SetCondition(b, conds, utils.ConditionComputeReady, computeReady, computeReason, computeMessage)

		if timelineErr == nil && computeReady == metav1.ConditionTrue {
			utils.SetCondition(b, conds, utils.ConditionAvailable, metav1.ConditionTrue, utils.ReasonAsExpected, "Branch is Available")
			utils.SetCondition(b, conds, utils.ConditionProgressing, metav1.ConditionFalse, utils.ReasonAsExpected, "Branch is at desired state")
		} else {
			utils.SetCondition(b, conds, utils.ConditionAvailable, metav1.ConditionFalse, computeReason, computeMessage)
			utils.SetCondition(b, conds, utils.ConditionProgressing, metav1.ConditionTrue, utils.ReasonReconciling, "Working toward desired state")
		}
	})
}

func (r *BranchReconciler) updateTimelineID(ctx context.Context, branch *neonv1alpha1.Branch) error {
	log := logf.FromContext(ctx)

	current := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: branch.GetName(), Namespace: branch.GetNamespace()}, current); err != nil {
		return err
	}

	timelineID := utils.GenerateNeonID()
	updated := current.DeepCopy()
	updated.Spec.TimelineID = timelineID
	updated.ManagedFields = nil

	if err := r.Patch(ctx, updated, client.MergeFrom(current), &client.PatchOptions{FieldManager: "neon-operator"}); err != nil {
		return err
	}

	branch.Spec.TimelineID = timelineID
	log.Info("Generated and set timelineID", "timelineID", timelineID)
	return nil
}

func (r *BranchReconciler) getProject(ctx context.Context, projectID string, namespace string) (*neonv1alpha1.Project, error) {
	project := &neonv1alpha1.Project{}
	namespacedName := types.NamespacedName{
		Name:      projectID,
		Namespace: namespace,
	}

	if err := r.Get(ctx, namespacedName, project); err != nil {
		return nil, fmt.Errorf("failed to get project %s: %w", projectID, err)
	}

	return project, nil
}

func (r *BranchReconciler) getCluster(ctx context.Context, clusterName, namespace string) (*neonv1alpha1.Cluster, error) {
	cluster := &neonv1alpha1.Cluster{}
	namespacedName := types.NamespacedName{
		Name:      clusterName,
		Namespace: namespace,
	}

	if err := r.Get(ctx, namespacedName, cluster); err != nil {
		return nil, fmt.Errorf("failed to get cluster %s: %w", clusterName, err)
	}

	return cluster, nil
}

func (r *BranchReconciler) ensureTimeline(ctx context.Context, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) error {
	log := logf.FromContext(ctx)

	base := r.StorageControllerBaseURL
	if base == "" {
		base = storagecontroller.URL(project.Spec.ClusterName)
	}
	storageControllerURL := fmt.Sprintf("%s/v1/tenant/%s/timeline", base, project.Spec.TenantID)

	log.Info("Sending request to storage controller", "url", storageControllerURL)

	requestBody := map[string]interface{}{
		"new_timeline_id": branch.Spec.TimelineID,
		"pg_version":      branch.Spec.PGVersion,
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request body: %w", err)
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := httpClient.Post(storageControllerURL, "application/json", bytes.NewBuffer(bodyBytes))
	if err != nil {
		log.Info("Failed to connect to storage controller, will retry", "url", storageControllerURL, "error", err)
		return fmt.Errorf("failed to connect to storage controller: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error(err, "failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		log.Info("Failed to create timeline on storage controller", "status", resp.StatusCode)
		return fmt.Errorf("failed to create timeline on storage controller, status: %d", resp.StatusCode)
	}

	log.Info("Successfully created timeline on storage controller")
	return nil
}

// Resource creation functions moved to branch_create.go

// SetupWithManager sets up the controller with the Manager.
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// exposurePredicate 仅在 Cluster 的 PostgresExposure 变更时触发 Branch reconcile。
	// 避免 neonImage、labels 等无关变更产生不必要的调和。
	exposurePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			// 跳过 controller 启动时的初始全量同步（Generation != 1 表示非首次创建），
			// 避免启动阶段对全量 Branch 做无效调和。
			return e.Object.GetGeneration() == 1
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldC := e.ObjectOld.(*neonv1alpha1.Cluster)
			newC := e.ObjectNew.(*neonv1alpha1.Cluster)
			return !equality.Semantic.DeepEqual(oldC.Spec.PostgresExposure, newC.Spec.PostgresExposure)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Cluster 删除时仍触发，关联 Branch 需要感知并做相应处理
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Branch{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&neonv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToBranchRequests),
			builder.WithPredicates(exposurePredicate),
		).
		Named("branch").
		Complete(r)
}

// clusterToBranchRequests 将 Cluster 变更映射为 Branch reconcile 请求。
// 当 Cluster 的 postgresExposure 等配置变更时，触发关联的 Branch 重新 reconcile，
// 以便 Branch controller 读取最新配置并更新 PostgresService。
//
// 映射链路: Cluster → Project(Spec.ClusterName) → Branch(Spec.ProjectID)
func (r *BranchReconciler) clusterToBranchRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*neonv1alpha1.Cluster)
	if !ok {
		return nil
	}

	log := logf.FromContext(ctx)

	// 1. 列出所有引用该 Cluster 的 Project
	var projects neonv1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(cluster.Namespace)); err != nil {
		log.Error(err, "failed to list Projects for Cluster watch", "cluster", cluster.Name)
		return nil
	}

	// 2. 收集匹配 Cluster 的 Project ID
	var projectIDs []string
	for _, p := range projects.Items {
		if p.Spec.ClusterName == cluster.Name {
			projectIDs = append(projectIDs, p.Name)
		}
	}
	if len(projectIDs) == 0 {
		return nil
	}

	// 3. 列出所有属于这些 Project 的 Branch
	var branches neonv1alpha1.BranchList
	if err := r.List(ctx, &branches, client.InNamespace(cluster.Namespace)); err != nil {
		log.Error(err, "failed to list Branches for Cluster watch", "cluster", cluster.Name)
		return nil
	}

	var requests []reconcile.Request
	for _, b := range branches.Items {
		for _, pid := range projectIDs {
			if b.Spec.ProjectID == pid {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      b.Name,
						Namespace: b.Namespace,
					},
				})
				break
			}
		}
	}

	log.Info("Cluster change triggered Branch reconcile",
		"cluster", cluster.Name, "branchCount", len(requests))
	return requests
}
