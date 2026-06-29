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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OperationSpec defines the desired state of Operation.
// Operation 追踪异步操作的执行状态，对应 Neon 生产 API 的 operations 模型。
type OperationSpec struct {
	// Action 操作类型。
	// 如: create_project, delete_project, create_branch, delete_branch,
	//      create_endpoint, delete_endpoint, reset_role_password, create_database, ...
	// +required
	Action string `json:"action"`

	// Status 执行状态。
	// scheduling → running → finished / failed
	// +kubebuilder:validation:Enum=scheduling;running;finished;failed
	// +kubebuilder:default:="scheduling"
	// +required
	Status string `json:"status"`

	// ProjectID 相关项目。
	// +required
	ProjectID string `json:"projectID"`

	// BranchID 相关分支，可选。
	// +optional
	BranchID string `json:"branchID,omitempty"`

	// EndpointID 相关端点，可选。
	// +optional
	EndpointID string `json:"endpointID,omitempty"`

	// Error 错误信息。Status=failed 时填充。
	// +optional
	Error string `json:"error,omitempty"`

	// FailuresCount 失败次数。用于重试控制。
	// +optional
	FailuresCount int32 `json:"failuresCount,omitempty"`
}

// OperationStatus defines the observed state of Operation.
type OperationStatus struct {
	// ObservedGeneration 记录最近观察到的 generation。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CompletedAt 完成时间。
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// TotalDurationMs 总耗时 (毫秒)。
	// +optional
	TotalDurationMs int64 `json:"totalDurationMs,omitempty"`

	// Conditions 详细状态条件。
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".spec.action"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".spec.status"
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=".spec.projectID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Operation is the Schema for the operations API
type Operation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Operation
	// +required
	Spec OperationSpec `json:"spec"`

	// status defines the observed state of Operation
	// +optional
	Status OperationStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// OperationList contains a list of Operation
type OperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Operation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Operation{}, &OperationList{})
}
