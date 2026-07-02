package storagecontroller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

const defaultSCName = "storage-controller"

func Deployment(cluster *v1alpha1.Cluster, publicKeyPEM, pageserverToken, controlPlaneToken, safekeeperToken string) *appsv1.Deployment {
	scName := Name(cluster.Name)
	lbls := labels(cluster.Name)

	probes := cluster.Spec.StorageControllerProbes
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
			Name:      scName,
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
					// TerminationGracePeriodSeconds 给 leader 足够时间优雅退出，
					// 允许正常移交后再被强制终止。
					TerminationGracePeriodSeconds: ptr.To(int64(60)),
					// PodAntiAffinity 确保 SC Pod（leader + 滚动更新候选）
					// 分散在不同节点上。硬反亲和可防止单节点故障
					// 同时影响 leader 和替补 Pod。
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: LabelSelector(cluster.Name),
									},
									TopologyKey: "kubernetes.io/hostname",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						utils.JWTVolume(cluster.Name),
					},
					Containers: []corev1.Container{
						{
							Name:            defaultSCName,
							Image:           cluster.Spec.NeonImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"storage_controller",
							},
							Args: []string{
								"-l",
								fmt.Sprintf("0.0.0.0:%d", Port),
								"--control-plane-url",
								"http://neon-controlplane:8081",
								"--initial-split-shards",
								"0",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "PUBLIC_KEY",
									Value: publicKeyPEM,
								},
								{
									Name:  "PAGESERVER_JWT_TOKEN",
									Value: pageserverToken,
								},
								{
									Name:  "CONTROL_PLANE_JWT_TOKEN",
									Value: controlPlaneToken,
								},
								{
									Name:  "SAFEKEEPER_JWT_TOKEN",
									Value: safekeeperToken,
								},
								{
									Name: "DATABASE_URL",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: cluster.Spec.StorageControllerDatabaseSecret.Name,
											},
											Key: cluster.Spec.StorageControllerDatabaseSecret.Key,
										},
									},
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: Port,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								utils.JWTVolumeMount(),
							},
							Resources: storagecontrollerResources(cluster),
							// Health probes using upstream SC endpoints.
							// /live  checks startup_complete AND is_leader (non-leaders return 503).
							// /ready checks startup_complete only (candidates can receive traffic).
							StartupProbe:   utils.ProbeWithConfig("/status", Port, corev1.URISchemeHTTP, 5, 10, 5, 6, startupCfg),
							LivenessProbe:  utils.ProbeWithConfig("/live", Port, corev1.URISchemeHTTP, 10, 10, 5, 3, livenessCfg),
							ReadinessProbe: utils.ProbeWithConfig("/ready", Port, corev1.URISchemeHTTP, 5, 5, 3, 2, readinessCfg),
						},
					},
				},
			},
		},
	}
}
