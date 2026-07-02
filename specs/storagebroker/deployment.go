package storagebroker

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

func Deployment(cluster *v1alpha1.Cluster) *appsv1.Deployment {
	sbName := Name(cluster.Name)
	lbls := labels(cluster.Name)

	probes := cluster.Spec.StorageBrokerProbes
	var startupCfg, livenessCfg, readinessCfg *v1alpha1.ProbeConfig
	if probes != nil {
		startupCfg = probes.StartupProbe
		livenessCfg = probes.LivenessProbe
		readinessCfg = probes.ReadinessProbe
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbName,
			Namespace: cluster.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: LabelSelector(cluster.Name),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: lbls,
				},
				Spec: corev1.PodSpec{
					// SecurityContext 确保容器以非 root 用户运行。
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  ptr.To(int64(1000)),
						RunAsGroup: ptr.To(int64(1000)),
						FSGroup:    ptr.To(int64(1000)),
					},
					// TerminationGracePeriodSeconds 允许正在处理的 gRPC 流
					// 在被强制终止前排空完成。
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
					// PodAntiAffinity：软反亲和（preferred）尽可能将 Broker Pod
					// 分散到不同节点，但当可用节点不足时不会阻塞调度。
					// 选择软策略是因为 Broker 是无状态服务，
					// 未来可能扩展到 2 个以上副本。
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: LabelSelector(cluster.Name),
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "storage-broker",
							Image:           cluster.Spec.NeonImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"storage_broker",
							},
							Args: []string{
								"--listen-addr", fmt.Sprintf("0.0.0.0:%d", Port),
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: Port,
								},
							},
							Resources: storagebrokerResources(cluster),
							// Health probes using /status (HTTP/1 handler).
							// Broker is a stateless pub-sub service; starts fast (<10s).
							StartupProbe:   utils.ProbeWithConfig("/status", Port, corev1.URISchemeHTTP, 5, 5, 5, 6, startupCfg),
							LivenessProbe:  utils.ProbeWithConfig("/status", Port, corev1.URISchemeHTTP, 10, 10, 5, 3, livenessCfg),
							ReadinessProbe: utils.ProbeWithConfig("/status", Port, corev1.URISchemeHTTP, 5, 5, 3, 2, readinessCfg),
						},
					},
				},
			},
		},
	}
}
