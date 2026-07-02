package storagecontroller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
)

// DefaultResources 是 Storage Controller 容器的默认 CPU/内存请求与限制。
//
// SC 运行单线程 tokio 异步运行时，包含 99 个用于数据库操作的阻塞线程。
// 每个 shard 约管理 8KB 内存状态，在中等硬件上可扩展到数百万 shard。
//
//   - CPU 请求 250m：正常运行约使用 100-200m CPU
//   - CPU 限制 1：单线程模型无法利用多核
//   - 内存请求 256Mi：典型部署约使用 50MB
//   - 内存限制 512Mi：为阻塞线程栈 + jemalloc 预留余量
var DefaultResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	},
}

// storagecontrollerResources 返回 Storage Controller 容器的资源配置。
// 如果 Cluster spec 提供了自定义覆盖值，则使用自定义配置；
// 否则使用 operator 内置默认值。
func storagecontrollerResources(cluster *v1alpha1.Cluster) corev1.ResourceRequirements {
	if cluster.Spec.StorageControllerResources != nil {
		return *cluster.Spec.StorageControllerResources
	}
	return DefaultResources
}
