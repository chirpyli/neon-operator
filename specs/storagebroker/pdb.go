package storagebroker

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// PodDisruptionBudget 为 Storage Broker Deployment 创建 PDB。
//
// MaxUnavailable=1 表示同时最多允许一个 Broker Pod 被主动中断。
// Broker 是无状态服务，重启时间在亚秒级，因此主动中断影响极小。
// PDB 的主要目的是在 Broker 扩展到多副本后，
// 防止节点排水时同时中断多个 Pod。
//
// LabelSelector 通过 component+cluster 标签有意匹配集群中
// 所有 Broker Pod，确保 PDB 在整个集群范围内生效。
func PodDisruptionBudget(clusterName, namespace string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "policy/v1",
			Kind:       "PodDisruptionBudget",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(clusterName),
			Namespace: namespace,
			Labels:    labels(clusterName),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: LabelSelector(clusterName),
			},
		},
	}
}
