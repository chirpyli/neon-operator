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

// BranchSpec defines the desired state of Branch
type BranchSpec struct {
	// Will be generated unless specified.
	// Has to be a 32 character alphanumeric string.
	// +optional
	TimelineID string `json:"timelineID"`

	// PGVersion specifies the PostgreSQL version to use for the branch.
	// kubebuilder:validation:Enum=14;15;16;17
	// kubebuilder:default:=17
	PGVersion int `json:"pgVersion"`

	// The ID of the Project this Branch belongs to
	ProjectID string `json:"projectID"`

	// Name 分支的显示名称（如 "main", "feat/xxx"）。
	// 创建时必需，全局唯一在同一 Project 内。
	Name string `json:"name,omitempty"`

	// ParentBranch 父分支的 Name（显示名称，非 ID）。
	// 为空表示创建主分支（初始分支），非空表示从已有分支创建子分支。
	// +optional
	ParentBranch string `json:"parentBranch,omitempty"`

	// ParentLSN 分支起始 LSN 位点。
	// 仅在 ParentBranch 非空时有效，用于指定从父分支的哪个 LSN 创建分支。
	// 格式如 "0/12345678"。
	// +optional
	ParentLSN string `json:"parentLSN,omitempty"`

	// ParentTimestamp 时间点分支 (PITR)。
	// 仅在 ParentBranch 非空时有效，用于指定从父分支的哪个时间点创建分支。
	// 与 ParentLSN 互斥，优先使用 ParentLSN。
	// +optional
	ParentTimestamp string `json:"parentTimestamp,omitempty"`

	// InitSource 初始化源类型。
	// "parent-data": 从父分支复制全量数据（默认）。
	// "schema-only": 仅复制 schema，不复制数据。
	// +optional
	// +kubebuilder:validation:Enum=parent-data;schema-only
	// +kubebuilder:default:="parent-data"
	InitSource string `json:"initSource,omitempty"`

	// Protected 是否为受保护分支。
	// 受保护分支不能直接删除。
	// +optional
	Protected bool `json:"protected,omitempty"`

	// Default 是否为项目的默认分支。
	// 每个项目只有一个默认分支。默认分支不能删除。
	// +optional
	Default bool `json:"default,omitempty"`
}

// BranchStatus defines the observed state of Branch.
type BranchStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=".status.conditions[?(@.type==\"Available\")].status"
// +kubebuilder:printcolumn:name="Progressing",type=string,JSONPath=".status.conditions[?(@.type==\"Progressing\")].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Branch is the Schema for the branches API
type Branch struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Branch
	// +required
	Spec BranchSpec `json:"spec"`

	// status defines the observed state of Branch
	// +optional
	Status BranchStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// BranchList contains a list of Branch
type BranchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Branch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Branch{}, &BranchList{})
}
