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

// EndpointDefaults 定义新建 Endpoint 的默认资源配置。
// [未来 Serverless] 可扩展 AutoscalingLimitMinCU/MaxCU 字段。
type EndpointDefaults struct {
	// Resources 默认计算资源 (CPU/Memory)。
	// 每个 Endpoint 可通过 Endpoint.Spec.Resources 覆盖。
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// IPAllowConfig 定义 IP 白名单访问控制规则。
type IPAllowConfig struct {
	// PrimaryBranchOnly 是否仅对主分支生效。
	// +optional
	PrimaryBranchOnly bool `json:"primaryBranchOnly,omitempty"`

	// SourceRanges 允许的 CIDR 范围列表。
	// +optional
	SourceRanges []string `json:"sourceRanges,omitempty"`
}

// ProjectSpec defines the desired state of Project
type ProjectSpec struct {
	// Name of the cluster where the project will be created.
	ClusterName string `json:"cluster"`

	// Will be generated unless specified.
	// Has to be a 32 character alphanumeric string.
	// +optional
	TenantID string `json:"tenantId"`

	// PostgreSQL version to use for the project.
	// +optional
	PGVersion int `json:"pgVersion"`

	// Name 项目显示名称，用于 Control Plane API 响应。
	// 区别于 metadata.name (K8s 内部标识, DNS label 约束)。
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Name string `json:"name"`

	// DefaultEndpointSettings 新建端点的默认资源配置。
	// 当创建 Endpoint 且未指定 resources 时，使用此默认值。
	// +optional
	DefaultEndpointSettings *EndpointDefaults `json:"defaultEndpointSettings,omitempty"`

	// HistoryRetentionSeconds 历史数据保留期（秒）。
	// 用于控制 PITR（Point-In-Time Recovery）的时间窗口。
	// 默认 604800（7 天）。
	// +optional
	// +kubebuilder:default:=604800
	// +kubebuilder:validation:Minimum:=0
	HistoryRetentionSeconds int64 `json:"historyRetentionSeconds,omitempty"`

	// IPAllow IP 白名单配置。
	// nil 表示不对 IP 进行限制。
	// +optional
	IPAllow *IPAllowConfig `json:"ipAllow,omitempty"`
}

// ProjectStatus defines the observed state of Project.
type ProjectStatus struct {
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

// Project is the Schema for the projects API
type Project struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Project
	// +required
	Spec ProjectSpec `json:"spec"`

	// status defines the observed state of Project
	// +optional
	Status ProjectStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
