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

func Deployment(cluster *v1alpha1.Cluster) *appsv1.Deployment {
	storageControllerName := Name(cluster.Name)

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
			Name:      storageControllerName,
			Namespace: cluster.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": storageControllerName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": storageControllerName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "storage-controller",
							Image:           cluster.Spec.NeonImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"storage_controller",
							},
							Args: []string{
								"--dev",
								"-l",
								fmt.Sprintf("0.0.0.0:%d", Port),
								"--control-plane-url",
								"http://neon-controlplane:8081",
								"--initial-split-shards",
								"0",
							},
							Env: []corev1.EnvVar{
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
