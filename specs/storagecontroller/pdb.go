package storagecontroller

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// PodDisruptionBudget 为 Storage Controller Deployment 创建 PDB。
//
// MaxUnavailable=1 表示同时最多允许一个 SC Pod 被主动中断。
// 由于 SC 以单副本运行并通过数据库 leader 表进行选主，
// 主动中断会触发快速 leader 故障转移而不是服务中断。
// 非 leader Pod 的 /live 端点返回 503，由 Kubernetes 自动重启。
//
// LabelSelector 通过 component+cluster 标签有意匹配集群中
// 所有 SC Pod，确保 PDB 在整个集群范围内生效。
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
