package storagebroker

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
)

// DefaultResources are the default CPU/memory requests and limits for
// the storage broker container.
//
// Broker is a stateless in-memory pub-sub service. It runs a lightweight
// gRPC server with default message channels of 32 (per-timeline) and
// 16384 (all-keys). Resource requirements are minimal.
//
//   - CPU 100m request: almost entirely network I/O bound
//   - CPU 500m limit: allows short bursts during high-volume pub-sub
//   - Memory 128Mi request: sufficient for typical message buffers
//   - Memory 256Mi limit: headroom for message storms
var DefaultResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
}

// storagebrokerResources 返回 Storage Broker 容器的资源配置。
// 如果 Cluster spec 提供了自定义覆盖值，则使用自定义配置；
// 否则使用 operator 内置默认值。
func storagebrokerResources(cluster *v1alpha1.Cluster) corev1.ResourceRequirements {
	if cluster.Spec.StorageBrokerResources != nil {
		return *cluster.Spec.StorageBrokerResources
	}
	return DefaultResources
}
