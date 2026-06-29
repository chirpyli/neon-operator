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

// ProbeConfig allows overriding the default container health probe parameters.
// Only threshold/hysteresis parameters are exposed; the endpoint path, port,
// and scheme are fixed by the operator (they correspond to upstream Neon's
// design where only /v1/status is unauthenticated and suitable for K8s probes).
type ProbeConfig struct {
	// InitialDelaySeconds is the number of seconds after the container has
	// started before the probe is initiated. Overrides the operator default.
	// +optional
	InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`

	// PeriodSeconds is how often (in seconds) to perform the probe.
	// +optional
	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds is the number of seconds after which the probe times out.
	// +optional
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is the number of consecutive failures required to
	// consider the probe failed.
	// +optional
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`
}

// NodeFailureRecoveryConfig 控制节点故障时的自动恢复策略。
type NodeFailureRecoveryConfig struct {
	// AutoRecover 是否在节点宕机时自动删除 PVC 并重建。
	// 默认 false（需要人工确认），可设为 true 启用自动恢复。
	// +optional
	AutoRecover bool `json:"autoRecover,omitempty"`

	// MaxPendingDuration 在判定 PVC 无法恢复前，Pod Pending 的最大等待时间。
	// 默认 5 分钟。
	// +optional
	MaxPendingDuration *metav1.Duration `json:"maxPendingDuration,omitempty"`
}

// PageserverSpec defines the desired state of Pageserver
type PageserverSpec struct {
	// ID which the pageserver uses when registering with storage-controller
	// This ID must be unique within the cluster.
	ID uint64 `json:"id"`

	// Used to deterministically setup which storage controller and broker to communicate with
	Cluster string `json:"cluster"`

	// Reference to a Secret containing credentials for accessing a storage bucket.
	BucketCredentialsSecret *corev1.SecretReference `json:"bucketCredentialsSecret"`

	// PVC configuration
	StorageConfig StorageConfig `json:"storageConfig"`

	// Resources 指定 pageserver 容器的 CPU/内存配置。
	// 若未设置，使用 operator 内置默认值。
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
	// +optional
	NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`

	// LivenessProbe overrides the default liveness probe configuration.
	// When nil, the operator uses built-in defaults that are aligned with
	// the Storage Controller's max_offline_interval (30s).
	// +optional
	LivenessProbe *ProbeConfig `json:"livenessProbe,omitempty"`

	// ReadinessProbe overrides the default readiness probe configuration.
	// When nil, the operator uses built-in defaults that are aligned with
	// the Storage Controller's heartbeat_interval (5s).
	// +optional
	ReadinessProbe *ProbeConfig `json:"readinessProbe,omitempty"`

	// StartupProbe overrides the default startup probe configuration.
	// When nil, the operator uses built-in defaults that are aligned with
	// the Storage Controller's max_warming_up_interval (300s for cold starts).
	// +optional
	StartupProbe *ProbeConfig `json:"startupProbe,omitempty"`
}

// PageserverStatus defines the observed state of Pageserver.
type PageserverStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RegisteredWithSC 指示 pageserver 是否已在 Storage Controller 中成功注册。
	// +optional
	RegisteredWithSC bool `json:"registeredWithSC,omitempty"`

	// SCSchedulingPolicy mirrors SC's NodeSchedulingPolicy
	// "Active", "Filling", "Pause", "PauseForRestart", "Draining", "Deleting"
	// +optional
	SCSchedulingPolicy string `json:"scSchedulingPolicy,omitempty"`

	// SCAvailability mirrors SC's NodeAvailability
	// "Active", "WarmingUp", "Offline"
	// +optional
	SCAvailability string `json:"scAvailability,omitempty"`

	// SCAttachedShardCount 当前节点作为 Attached 的 shard 数量。
	// +optional
	SCAttachedShardCount int32 `json:"scAttachedShardCount,omitempty"`

	// SCTotalShardCount 当前节点上的 shard 总数（Attached + Secondary）。
	// +optional
	SCTotalShardCount int32 `json:"scTotalShardCount,omitempty"`

	// NodeID 是 SC 分配的节点 ID，与 Spec.ID 相同（注册后确认）。
	// +optional
	NodeID uint64 `json:"nodeID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.cluster"
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=".status.conditions[?(@.type==\"Available\")].status"
// +kubebuilder:printcolumn:name="Progressing",type=string,JSONPath=".status.conditions[?(@.type==\"Progressing\")].status"
// +kubebuilder:printcolumn:name="SC",type=string,JSONPath=".status.registeredWithSC"
// +kubebuilder:printcolumn:name="Shards",type=string,JSONPath=".status.scAttachedShardCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Pageserver is the Schema for the pageservers API
type Pageserver struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Pageserver
	// +required
	Spec PageserverSpec `json:"spec"`

	// status defines the observed state of Pageserver
	// +optional
	Status PageserverStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PageserverList contains a list of Pageserver
type PageserverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Pageserver `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Pageserver{}, &PageserverList{})
}
