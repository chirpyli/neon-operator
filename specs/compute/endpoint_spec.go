package compute

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

// =============================================================================
// Endpoint 专用的 Compute Spec 构建函数
// =============================================================================

// EndpointDeployment 为 Endpoint 构建 Deployment。
func EndpointDeployment(endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) *appsv1.Deployment {
	deploymentName := endpointDeploymentName(endpoint)

	labels := map[string]string{
		"app":                       deploymentName,
		"molnett.org/cluster":       project.Spec.ClusterName,
		"molnett.org/component":     "compute",
		"molnett.org/branch":        branch.Name,
		"molnett.org/endpoint":      endpoint.Name,
		"molnett.org/endpoint-type": endpoint.Spec.Type,
		"neon.tenant_id":            project.Spec.TenantID,
		"neon.timeline_id":          branch.Spec.TimelineID,
	}

	annotations := map[string]string{
		"neon.compute_id":   endpoint.Name,
		"neon.cluster_name": project.Spec.ClusterName,
		"neon.branch_id":    branch.Name,
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        deploymentName,
			Namespace:   endpoint.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: ptr.To(int64(1000)),
						FSGroup:   ptr.To(int64(1000)),
					},
					Containers: []corev1.Container{
						{
							Name:  "compute-node",
							Image: fmt.Sprintf("neondatabase/compute-node-v%d", branch.Spec.PGVersion),
							Command: []string{
								"bash",
								"-c",
								fmt.Sprintf(
									"echo \"$INITIAL_SPEC_JSON\" > /var/spec.json && "+
										"/usr/local/bin/compute_ctl --pgdata /.neon/data/pgdata "+
										"--connstr=postgresql://cloud_admin:@0.0.0.0:55433/postgres "+
										"--compute-id %s -p http://neon-controlplane.neon:8081 "+
										"--pgbin /usr/local/bin/postgres",
									endpoint.Name,
								),
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 55433,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "spec-volume",
									MountPath: "/var",
								},
								{
									Name:      "pgdata",
									MountPath: "/.neon/data",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "OTEL_SDK_DISABLED",
									Value: "true",
								},
								{
									Name: "INITIAL_SPEC_JSON",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: fmt.Sprintf("endpoint-%s-spec", endpoint.Name),
											},
											Key: "spec.json",
										},
									},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "spec-volume",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "pgdata",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: &[]resource.Quantity{resource.MustParse("500Mi")}[0],
								},
							},
						},
					},
				},
			},
		},
	}
}

// EndpointConfigMap 为 Endpoint 构建 ConfigMap。
// 区别于 Branch 的 ConfigMap：此函数从集群中读取 Role/Database CR 来填充 compute spec。
func EndpointConfigMap(
	endpoint *neonv1alpha1.Endpoint,
	branch *neonv1alpha1.Branch,
	project *neonv1alpha1.Project,
	jwtSecret corev1.Secret,
) (*corev1.ConfigMap, error) {

	jwtManager, err := utils.NewJWTManagerFromSecret(&jwtSecret)
	if err != nil {
		return nil, err
	}

	jwk := jwtManager.ToJWK()

	type clusterConfig struct {
		ClusterID string          `json:"cluster_id"`
		Name      string          `json:"name"`
		Roles     []Role          `json:"roles"`
		Databases []interface{}   `json:"databases"`
		Settings  []SettingsEntry `json:"settings"`
	}

	type computeCtlConfig struct {
		JWKS utils.JWKResponse `json:"jwks"`
	}

	type computeSpec struct {
		FormatVersion    string           `json:"format_version"`
		Cluster          clusterConfig    `json:"cluster"`
		ComputeCtlConfig computeCtlConfig `json:"compute_ctl_config"`
	}

	// 默认 role：postgres 用户
	roles := []Role{
		{
			Name:              "postgres",
			EncryptedPassword: "SCRAM-SHA-256$4096:159kANhgWW13pz78P02IMQ==$grA4JzZPeRmUeV8VjsCzn8QhgdcUZZXXPyUf3vyZZV4=:XgEolgh2F/C2yTbY16t856WqifqPlQsRnCUU4Q7BLVM=",
			Options:           nil,
		},
	}

	// Phase 2+: 从 Role/Database CR 聚合 roles 和 databases
	// 当前阶段：使用默认的 postgres role 和空 databases 列表

	spec := computeSpec{
		FormatVersion: "1.0",
		Cluster: clusterConfig{
			ClusterID: project.Spec.TenantID,
			Name:      project.Name,
			Roles:     roles,
			Databases: []interface{}{},
			Settings: []SettingsEntry{
				{Name: "neon.tenant_id", Value: project.Spec.TenantID, Vartype: "string"},
				{Name: "neon.timeline_id", Value: branch.Spec.TimelineID, Vartype: "string"},
			},
		},
	}
	spec.ComputeCtlConfig.JWKS = utils.JWKResponse{Keys: []*utils.JWK{jwk}}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("endpoint-%s-spec", endpoint.Name),
			Namespace: endpoint.Namespace,
			Labels: map[string]string{
				"molnett.org/cluster":   project.Spec.ClusterName,
				"molnett.org/component": "compute",
				"molnett.org/branch":    branch.Name,
				"molnett.org/endpoint":  endpoint.Name,
			},
		},
		Data: map[string]string{
			"spec.json": string(specJSON),
		},
	}, nil
}

// EndpointAdminService 为 Endpoint 构建 admin Service。
func EndpointAdminService(endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) *corev1.Service {
	serviceName := fmt.Sprintf("endpoint-%s-admin", endpoint.Name)
	labels := map[string]string{
		"molnett.org/cluster":   project.Spec.ClusterName,
		"molnett.org/component": "compute-admin",
		"molnett.org/branch":    branch.Name,
		"molnett.org/endpoint":  endpoint.Name,
		"neon.timeline_id":      branch.Spec.TimelineID,
		"neon.tenant_id":        project.Spec.TenantID,
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: endpoint.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": endpointDeploymentName(endpoint),
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "admin",
					Port:     3080,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
}

// EndpointPostgresService 为 Endpoint 构建 postgres Service。
func EndpointPostgresService(endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project, exposure *neonv1alpha1.ServiceExposure) *corev1.Service {
	serviceName := fmt.Sprintf("endpoint-%s-postgres", endpoint.Name)
	labels := map[string]string{
		"molnett.org/cluster":   project.Spec.ClusterName,
		"molnett.org/component": "compute-postgres",
		"molnett.org/branch":    branch.Name,
		"molnett.org/endpoint":  endpoint.Name,
		"neon.timeline_id":      branch.Spec.TimelineID,
		"neon.tenant_id":        project.Spec.TenantID,
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: endpoint.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": endpointDeploymentName(endpoint),
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "postgres",
					Port:     55433,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	applyServiceExposure(svc, exposure)
	return svc
}

func endpointDeploymentName(endpoint *neonv1alpha1.Endpoint) string {
	return fmt.Sprintf("endpoint-%s", endpoint.Name)
}
