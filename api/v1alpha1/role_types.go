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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleSpec defines the desired state of Role.
// Role 映射到 Postgres 角色 (ComputeSpec.cluster.roles)。
type RoleSpec struct {
	// BranchID 所属分支。
	// +required
	BranchID string `json:"branchID"`

	// Name Postgres 角色名。
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// AuthenticationMethod 认证方式。
	// +kubebuilder:validation:Enum=password;oauth;no_login
	// +kubebuilder:default:="password"
	// +optional
	AuthenticationMethod string `json:"authenticationMethod,omitempty"`
}

// RoleStatus defines the observed state of Role.
type RoleStatus struct {
	// ObservedGeneration 记录最近观察到的 generation。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// PasswordSecretRef 密码所在的 K8s Secret 引用。
	// 仅在 AuthenticationMethod="password" 时有值。
	// +optional
	PasswordSecretRef *corev1.SecretReference `json:"passwordSecretRef,omitempty"`

	// EncryptedPassword 密码的 SCRAM-SHA-256 verifier，用于写入 compute_ctl 的 spec。
	// 由 RoleController 从 Secret 中读取明文密码后计算得到。
	// 格式: SCRAM-SHA-256$4096:<base64_salt>$<base64_stored_key>:<base64_server_key>
	// +optional
	EncryptedPassword string `json:"encryptedPassword,omitempty"`

	// Protected 是否为系统保护角色（如创建分支时的 owner 角色）。
	// +optional
	Protected bool `json:"protected,omitempty"`

	// Phase 当前阶段: "active"。
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions 详细状态条件。
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=".spec.branchID"
// +kubebuilder:printcolumn:name="Auth",type=string,JSONPath=".spec.authenticationMethod"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Role is the Schema for the roles API
type Role struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Role
	// +required
	Spec RoleSpec `json:"spec"`

	// status defines the observed state of Role
	// +optional
	Status RoleStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// RoleList contains a list of Role
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Role `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Role{}, &RoleList{})
}
