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

// ServiceExposure 控制 Service 对外暴露的方式。
// 可用于 PostgreSQL Service 和未来的 Proxy Service。
type ServiceExposure struct {
	// Type 决定 Service 类型。
	// ClusterIP：仅集群内访问（默认）
	// NodePort：  通过节点 IP + 端口对外暴露
	// LoadBalancer：云环境自动创建外部 LB
	// +kubebuilder:default:="ClusterIP"
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// ExternalTrafficPolicy 控制外部流量的路由策略。
	// Cluster：流量均匀分发到所有 Pod（默认，可能丢失源 IP）
	// Local：  保留客户端真实 IP，但要求 Pod 在接收流量的节点上运行
	// +kubebuilder:validation:Enum=Cluster;Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicyType `json:"externalTrafficPolicy,omitempty"`

	// LoadBalancerSourceRanges 限制可访问的源 IP CIDR 列表。
	// 仅在 Type=LoadBalancer 时生效。用于安全白名单。
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

	// Annotations 透传到 Service 的 annotations。
	// 用于云厂商 LB 配置，如 AWS NLB、GCP LB 等。
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PageserverConfig 定义自动创建的 Pageserver 的默认配置。
type PageserverConfig struct {
	// StorageSize 指定 PS 的 PVC 大小。
	// +kubebuilder:default:="100Gi"
	StorageSize string `json:"storageSize,omitempty"`

	// Resources 指定 PS 的 CPU/内存配置。若未设置，使用 operator 内置默认值。
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// InitialSchedulingPolicy 新节点注册后的初始 SC 调度策略。
	// "Active"（默认）：立即参与全量调度。
	// "Filling"：仅接收新 shard placement（用于新节点预热）。
	// +kubebuilder:default:="Active"
	// +kubebuilder:validation:Enum=Active;Filling
	// +optional
	InitialSchedulingPolicy string `json:"initialSchedulingPolicy,omitempty"`

	// NodeFailure 控制节点故障时的自动恢复策略。
	// 用于为自动创建的 Pageserver 统一设置故障恢复行为。
	// +optional
	NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`
}

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	// Decides how many safekeepers to run in the cluster.
	// +kubebuilder:default:=3
	// +kubebuilder:validation:Minimum:=3
	NumSafekeepers uint8 `json:"numSafekeepers"`

	// NumPageservers 指定期望的 pageserver 数量。
	// pageserver 是 cell 内的存储节点，数量由容量需求决定。
	// 默认为 1（开发测试），生产环境建议 ≥ 2。
	// +kubebuilder:default:=1
	// +kubebuilder:validation:Minimum:=1
	NumPageservers int32 `json:"numPageservers,omitempty"`

	// DefaultPageserverConfig 指定自动创建的 pageserver 的默认配置。
	// +optional
	DefaultPageserverConfig *PageserverConfig `json:"defaultPageserverConfig,omitempty"`

	// Default PostgreSQL version to use if no version is specified in projects.
	// kubebuilder:validation:Enum=14;15;16;17
	DefaultPGVersion int `json:"defaultPGVersion"`

	// The default Neon image to use for all neon-specific resources.
	// +kubebuilder:default:="neondatabase/neon:8463"
	NeonImage string `json:"neonImage"`

	// ComputeImage specifies the compute-node container image.
	// When empty, defaults to neondatabase/compute-node-v{PGVersion}.
	// +optional
	ComputeImage string `json:"computeImage,omitempty"`

	// Reference to a Secret containing credentials for accessing a storage bucket.
	BucketCredentialsSecret *corev1.SecretReference `json:"bucketCredentialsSecret"`

	// Reference to a Secret containing credentials for accessing a storage bucket.
	// Must have a field named "uri"
	StorageControllerDatabaseSecret *corev1.SecretKeySelector `json:"storageControllerDatabaseSecret"`

	// DefaultSafekeeperStorage defines the default storage config for
	// auto-created safekeepers. If not set, defaults to Size=10Gi.
	// +optional
	DefaultSafekeeperStorage *StorageConfig `json:"defaultSafekeeperStorage,omitempty"`

	// PostgresExposure 控制计算节点 PostgreSQL Service 对外暴露策略。
	// 不设置时默认为 ClusterIP（仅集群内访问）。
	// +optional
	PostgresExposure *ServiceExposure `json:"postgresExposure,omitempty"`

	// StorageControllerProbes overrides the default health probe
	// configuration for the Storage Controller Deployment.
	// +optional
	StorageControllerProbes *ClusterComponentProbes `json:"storageControllerProbes,omitempty"`

	// StorageBrokerProbes overrides the default health probe
	// configuration for the Storage Broker Deployment.
	// +optional
	StorageBrokerProbes *ClusterComponentProbes `json:"storageBrokerProbes,omitempty"`
}

// ClusterComponentProbes groups probe configurations for a cluster-scoped
// component (Storage Controller, Storage Broker).
type ClusterComponentProbes struct {
	// LivenessProbe overrides the default liveness probe configuration.
	// +optional
	LivenessProbe *ProbeConfig `json:"livenessProbe,omitempty"`

	// ReadinessProbe overrides the default readiness probe configuration.
	// +optional
	ReadinessProbe *ProbeConfig `json:"readinessProbe,omitempty"`

	// StartupProbe overrides the default startup probe configuration.
	// +optional
	StartupProbe *ProbeConfig `json:"startupProbe,omitempty"`
}

// ClusterStatus defines the observed state of Cluster.
type ClusterStatus struct {
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

// Cluster is the Schema for the clusters API
type Cluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	metav1.ObjectMeta `json:"metadata"`

	// spec defines the desired state of Cluster
	// +required
	Spec ClusterSpec `json:"spec"`

	// status defines the observed state of Cluster
	// ReadOnly
	// +optional
	Status ClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}
