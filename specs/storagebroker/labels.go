package storagebroker

// ClusterLabel 是所有集群范围资源共用的标签键，
// 用于标识资源所属的集群。
const ClusterLabel = "molnett.org/cluster"

// labels 返回 Storage Broker Pod 的完整标签集。
func labels(clusterName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "storage-broker",
		"app.kubernetes.io/component": "storage-broker",
		"app.kubernetes.io/part-of":   "neon",
		ClusterLabel:                  clusterName,
	}
}

// LabelSelector 返回跨实例选择器的最小标签集，
// 用于 PodAntiAffinity 和 PodDisruptionBudget 等场景。
// 它会匹配同一集群的所有 Storage Broker Pod。
func LabelSelector(clusterName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/component": "storage-broker",
		ClusterLabel:                  clusterName,
	}
}
