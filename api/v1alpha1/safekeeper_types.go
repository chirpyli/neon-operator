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

type StorageConfig struct {
	// Name of the storage class to use for PVCs.
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`

	// Size of the PVCs.
	// kubebuilder:default:=10Gi
	Size string `json:"size"`
}

// SafekeeperSpec defines the desired state of Safekeeper
type SafekeeperSpec struct {
	// safekeeper 向 storage-controller 注册时使用的 ID。
	// 必须 >= 1，且在集群内唯一。
	// +kubebuilder:validation:Minimum:=1
	ID uint32 `json:"id"`

	// Used to deterministically setup which storage controller and broker to communicate with
	Cluster string `json:"cluster"`

	// PVC configuration
	StorageConfig StorageConfig `json:"storageConfig"`

	// NodeFailure 控制节点故障时的自动恢复策略。
	// +optional
	NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`

	// LivenessProbe overrides the default liveness probe configuration.
	// When nil, the operator uses built-in defaults aligned with SC's
	// max_offline_interval (30s).
	// +optional
	LivenessProbe *ProbeConfig `json:"livenessProbe,omitempty"`

	// ReadinessProbe overrides the default readiness probe configuration.
	// When nil, the operator uses built-in defaults aligned with SC's
	// heartbeat_interval (5s).
	// +optional
	ReadinessProbe *ProbeConfig `json:"readinessProbe,omitempty"`

	// StartupProbe overrides the default startup probe configuration.
	// When nil, the operator uses a 60s window (Safekeeper starts fast,
	// no cold-start from S3 like Pageserver).
	// +optional
	StartupProbe *ProbeConfig `json:"startupProbe,omitempty"`
}

// SafekeeperStatus defines the observed state of Safekeeper.
type SafekeeperStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RegisteredWithSC indicates whether the safekeeper has been
	// successfully registered with the Storage Controller via the upsert API.
	// +optional
	RegisteredWithSC bool `json:"registeredWithSC,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=".status.conditions[?(@.type==\"Available\")].status"
// +kubebuilder:printcolumn:name="Progressing",type=string,JSONPath=".status.conditions[?(@.type==\"Progressing\")].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Safekeeper is the Schema for the safekeepers API
type Safekeeper struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Safekeeper
	// +required
	Spec SafekeeperSpec `json:"spec"`

	// status defines the observed state of Safekeeper
	// +optional
	Status SafekeeperStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SafekeeperList contains a list of Safekeeper
type SafekeeperList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Safekeeper `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Safekeeper{}, &SafekeeperList{})
}
