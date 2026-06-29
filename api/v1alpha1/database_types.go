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

// DatabaseSpec defines the desired state of Database.
// Database 映射到 Postgres 数据库 (ComputeSpec.cluster.databases)。
type DatabaseSpec struct {
	// BranchID 所属分支。
	// +required
	BranchID string `json:"branchID"`

	// Name 数据库名。
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// OwnerName 拥有者角色名。
	// 未设置时默认为创建该分支的角色。
	// +optional
	OwnerName string `json:"ownerName,omitempty"`
}

// DatabaseStatus defines the observed state of Database.
type DatabaseStatus struct {
	// ObservedGeneration 记录最近观察到的 generation。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=".spec.ownerName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Database is the Schema for the databases API
type Database struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Database
	// +required
	Spec DatabaseSpec `json:"spec"`

	// status defines the observed state of Database
	// +optional
	Status DatabaseStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseList contains a list of Database
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Database `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Database{}, &DatabaseList{})
}
