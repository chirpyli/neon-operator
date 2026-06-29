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

// EndpointSpec defines the desired state of Endpoint.
// Endpoint 表示一个计算节点（Compute Node），提供 PostgreSQL 连接入口。
// 一个 Branch 可以有多个 Endpoint（如 read_write + read_only）。
type EndpointSpec struct {
	// BranchID 引用所属的 Branch。
	// +required
	BranchID string `json:"branchID"`

	// Type 端点类型: read_write 或 read_only。
	// 每个 Branch 最多一个 read_write 端点。
	// +kubebuilder:validation:Enum=read_write;read_only
	// +required
	Type string `json:"type"`

	// Resources 计算资源请求 (固定资源, 无弹性扩缩)。
	// 未设置时继承 Project.Spec.DefaultEndpointSettings.Resources。
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Disabled 是否禁用连接。
	// 禁用后 Deployment replicas 设为 0，Service 保留。
	// +optional
	Disabled bool `json:"disabled,omitempty"`

	// Exposure 覆盖 Cluster 级别的 PostgresExposure 策略。
	// 不设置时使用 Cluster.Spec.PostgresExposure。
	// +optional
	Exposure *ServiceExposure `json:"exposure,omitempty"`

	// [未来 Serverless 预留]
	// SuspendTimeoutSeconds 无活动后暂停的超时秒数, -1=永不暂停。
	// 当前阶段未实现，计算节点常驻运行。Phase 4+ 启用。
	// +optional
	// SuspendTimeoutSeconds *int32 `json:"suspendTimeoutSeconds,omitempty"`
}

// EndpointStatus defines the observed state of Endpoint.
type EndpointStatus struct {
	// ObservedGeneration 记录最近观察到的 generation。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase 当前生命周期阶段。
	// "creating_compute" → "starting" → "active" → "stopping" → "stopped"
	// [未来 Serverless] active → idle → suspended
	// +optional
	Phase string `json:"phase,omitempty"`

	// Host 对外连接地址 (LoadBalancer hostname 或 Node IP)。
	// +optional
	Host string `json:"host,omitempty"`

	// Port PostgreSQL 连接端口。
	// +optional
	Port int32 `json:"port,omitempty"`

	// Conditions 详细状态条件。
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=".spec.branchID"
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=".status.host"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Endpoint is the Schema for the endpoints API
type Endpoint struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Endpoint
	// +required
	Spec EndpointSpec `json:"spec"`

	// status defines the observed state of Endpoint
	// +optional
	Status EndpointStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// EndpointList contains a list of Endpoint
type EndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Endpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Endpoint{}, &EndpointList{})
}
