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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

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

// RoleReconciler reconciles a Role object.
// 职责：
// 1. 对于 authenticationMethod=password 的角色，生成随机密码并存入 K8s Secret
// 2. 更新 Role Status（Phase, PasswordSecretRef, Protected）
// 3. 变更时触发关联 Endpoint 的 ConfigMap 重建（通过 Annotation 标记）
type RoleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=roles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=neon.oltp.molnett.org,resources=roles/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *RoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconcile loop start", "request", req)
	defer func() {
		log.Info("Reconcile loop end", "request", req)
	}()

	role, err := r.getRole(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if role == nil {
		return ctrl.Result{}, nil
	}

	ctx = context.WithValue(ctx, utils.RoleNameKey, role.Name)

	// 删除路径
	if !role.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, role)
	}

	// 创建/更新路径：确保 Finalizer
	if !controllerutil.ContainsFinalizer(role, utils.FinalizerName) {
		controllerutil.AddFinalizer(role, utils.FinalizerName)
		if err := r.Update(ctx, role); err != nil {
			log.Error(err, "添加 Finalizer 失败")
			return ctrl.Result{}, fmt.Errorf("添加 finalizer: %w", err)
		}
		log.Info("Role Finalizer 已添加")
	}

	// 校验 Branch 存在
	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: role.Spec.BranchID, Namespace: role.Namespace}, branch); err != nil {
		log.Error(err, "failed to get branch", "branchID", role.Spec.BranchID)
		return r.updateStatus(ctx, role, fmt.Errorf("branch not found: %w", err))
	}

	// 密码生成
	var genErr error
	if role.Spec.AuthenticationMethod == "password" {
		genErr = r.ensurePasswordSecret(ctx, role, branch)
		if genErr == nil {
			// 从 Secret 读取明文密码并计算 SCRAM-SHA-256 verifier
			genErr = r.ensureEncryptedPassword(ctx, role)
		}
		// [Phase 2.2] 确保 EncryptedPassword 已就绪后才触发 Endpoint reconcile
		// 避免 Endpoint Controller 在 SCRAM verifier 就绪前聚合角色列表
		if genErr == nil {
			if triggerErr := r.triggerEndpointReconcile(ctx, role); triggerErr != nil {
				log.Error(triggerErr, "failed to trigger endpoint reconcile")
			}
		}
	}

	return r.updateStatus(ctx, role, genErr)
}

func (r *RoleReconciler) getRole(ctx context.Context, req ctrl.Request) (*neonv1alpha1.Role, error) {
	log := logf.FromContext(ctx)
	role := &neonv1alpha1.Role{}
	if err := r.Get(ctx, req.NamespacedName, role); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Role has been deleted")
			return nil, nil
		}
		return nil, fmt.Errorf("cannot get the resource: %w", err)
	}
	return role, nil
}

// ensurePasswordSecret 为密码认证的角色生成密码并存入 Secret。
func (r *RoleReconciler) ensurePasswordSecret(ctx context.Context, role *neonv1alpha1.Role, branch *neonv1alpha1.Branch) error {
	log := logf.FromContext(ctx)

	secretName := fmt.Sprintf("role-%s-password", role.Name)

	var existingSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: role.Namespace}, &existingSecret); err == nil {
		// Secret 已存在，更新 Status.PasswordSecretRef（如果为空）
		if role.Status.PasswordSecretRef == nil {
			_ = utils.PatchStatus(ctx, r.Client, role, func(rl *neonv1alpha1.Role) {
				rl.Status.PasswordSecretRef = &corev1.SecretReference{
					Name:      secretName,
					Namespace: role.Namespace,
				}
			})
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get password secret: %w", err)
	}

	// Secret 不存在：生成密码并创建
	password := generateSecurePassword(32)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: role.Namespace,
			Labels: map[string]string{
				"molnett.org/component": "role-password",
				"molnett.org/role":      role.Name,
				"molnett.org/branch":    branch.Name,
			},
		},
		Data: map[string][]byte{
			"password": []byte(password),
			"username": []byte(role.Spec.Name),
		},
	}

	if err := controllerutil.SetControllerReference(role, secret, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("create password secret: %w", err)
	}
	log.Info("Role 密码 Secret 已创建", "role", role.Name, "secret", secretName)

	// 更新 Status.PasswordSecretRef
	_ = utils.PatchStatus(ctx, r.Client, role, func(rl *neonv1alpha1.Role) {
		rl.Status.PasswordSecretRef = &corev1.SecretReference{
			Name:      secretName,
			Namespace: role.Namespace,
		}
	})

	return nil
}

// ensureEncryptedPassword 从密码 Secret 读取明文密码，计算 SCRAM-SHA-256 verifier，
// 并更新 Role.Status.EncryptedPassword。如果 EncryptedPassword 已存在且密码未变化则跳过。
func (r *RoleReconciler) ensureEncryptedPassword(ctx context.Context, role *neonv1alpha1.Role) error {
	log := logf.FromContext(ctx)

	if role.Status.PasswordSecretRef == nil {
		return fmt.Errorf("password secret ref not set for role %q", role.Name)
	}

	secretName := types.NamespacedName{
		Name:      role.Status.PasswordSecretRef.Name,
		Namespace: role.Status.PasswordSecretRef.Namespace,
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretName, secret); err != nil {
		return fmt.Errorf("get password secret: %w", err)
	}

	plainPassword, ok := secret.Data["password"]
	if !ok || len(plainPassword) == 0 {
		return fmt.Errorf("password key not found in secret %q", secretName.Name)
	}

	// 如果 EncryptedPassword 已存在且非空，跳过重算。
	// API 层在密码变更时会将 EncryptedPassword 置空，触发 Controller 重新计算 SCRAM。
	if role.Status.EncryptedPassword != "" {
		log.Info("EncryptedPassword already set, skipping SCRAM computation", "role", role.Name)
		return nil
	}

	// 计算 SCRAM-SHA-256 verifier
	encryptedPassword, err := utils.SCRAMSHA256(plainPassword)
	if err != nil {
		return fmt.Errorf("compute SCRAM-SHA-256: %w", err)
	}

	// 更新 Status
	if err := utils.PatchStatus(ctx, r.Client, role, func(rl *neonv1alpha1.Role) {
		rl.Status.EncryptedPassword = encryptedPassword
	}); err != nil {
		return fmt.Errorf("update EncryptedPassword status: %w", err)
	}

	log.Info("SCRAM-SHA-256 verifier computed and stored in status", "role", role.Name)
	return nil
}

// triggerEndpointReconcile 通过更新 Branch 的 annotation 来触发 Endpoint Controller 的 reconcile，
// 从而实现 ConfigMap 中 roles 列表的更新。
// [Phase 2+] 也可通过 OwnerReferences 或 event channel 实现更精确的触发。
func (r *RoleReconciler) triggerEndpointReconcile(ctx context.Context, role *neonv1alpha1.Role) error {
	branch := &neonv1alpha1.Branch{}
	if err := r.Get(ctx, types.NamespacedName{Name: role.Spec.BranchID, Namespace: role.Namespace}, branch); err != nil {
		return fmt.Errorf("get branch for trigger: %w", err)
	}

	if branch.Annotations == nil {
		branch.Annotations = make(map[string]string)
	}
	branch.Annotations["neon.oltp.molnett.org/last-role-change"] = time.Now().UTC().Format(time.RFC3339)
	return r.Update(ctx, branch)
}

func (r *RoleReconciler) updateStatus(ctx context.Context, role *neonv1alpha1.Role, genErr error) (ctrl.Result, error) {
	err := utils.PatchStatus(ctx, r.Client, role, func(rl *neonv1alpha1.Role) {
		rl.Status.ObservedGeneration = rl.Generation
		conds := &rl.Status.Conditions

		if genErr != nil {
			utils.SetCondition(rl, conds, utils.ConditionAvailable, metav1.ConditionFalse,
				utils.ReasonResourceCreateFailed, genErr.Error())
			rl.Status.Phase = ""
			return
		}

		rl.Status.Phase = "active"
		utils.SetCondition(rl, conds, utils.ConditionAvailable, metav1.ConditionTrue,
			utils.ReasonAsExpected, "Role is Active")
		utils.SetCondition(rl, conds, utils.ConditionProgressing, metav1.ConditionFalse,
			utils.ReasonAsExpected, "Role is at desired state")
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RoleReconciler) finalize(ctx context.Context, role *neonv1alpha1.Role) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(role, utils.FinalizerName) {
		return ctrl.Result{}, nil
	}

	// [Phase 2.7] 删除前触发 Endpoint ConfigMap 更新，从 spec 中移除该角色
	if err := r.triggerEndpointReconcile(ctx, role); err != nil {
		log.Error(err, "failed to trigger endpoint reconcile during role deletion")
		// 不返回错误，允许继续删除（最终一致性）
	}

	// 删除关联的密码 Secret
	if role.Status.PasswordSecretRef != nil {
		secret := &corev1.Secret{}
		secretName := types.NamespacedName{
			Name:      role.Status.PasswordSecretRef.Name,
			Namespace: role.Status.PasswordSecretRef.Namespace,
		}
		if err := r.Get(ctx, secretName, secret); err == nil {
			if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "failed to delete password secret")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// 重新获取并移除 Finalizer
	current := &neonv1alpha1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Name: role.Name, Namespace: role.Namespace}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(current, utils.FinalizerName)
	if err := r.Update(ctx, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	log.Info("Role Finalizer 已移除", "role", role.Name)
	return ctrl.Result{}, nil
}

// generateSecurePassword 生成加密安全的随机密码。
func generateSecurePassword(length int) string {
	bytes := make([]byte, length)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)[:length]
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neonv1alpha1.Role{}).
		Owns(&corev1.Secret{}).
		Named("role").
		Complete(r)
}
