package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

// endpointLogger 为 endpoint 包提供一个默认 logger，避免硬依赖 slog。
var endpointLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// =============================================================================
// Endpoint 专用的 Compute Spec 构建函数
// =============================================================================

// EndpointDeployment 为 Endpoint 构建 Deployment。
// image 指定使用的 compute 容器镜像。
// 调用方应通过 deriveComputeImage() 自动推导镜像地址：
// 优先使用 Cluster.Spec.ComputeImage，为空时从 Cluster.Spec.NeonImage 提取
// registry 前缀构造本地 registry 路径，兜底为 neondatabase/compute-node-v{PGVersion}。
func EndpointDeployment(endpoint *neonv1alpha1.Endpoint, branch *neonv1alpha1.Branch, project *neonv1alpha1.Project, image string) *appsv1.Deployment {
	if image == "" {
		image = fmt.Sprintf("neondatabase/compute-node-v%d", branch.Spec.PGVersion)
	}
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
					// 优雅终止：给予 PostgreSQL checkpoint + compute_ctl 退出的时间。
					// 与 pageserver 保持一致使用 60s。
					TerminationGracePeriodSeconds: ptr.To(int64(60)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: ptr.To(int64(1000)),
						FSGroup:   ptr.To(int64(1000)),
					},
					Containers: []corev1.Container{
						{
							Name:            "compute-node",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"bash",
								"-c",
								fmt.Sprintf(
									// exec 让 compute_ctl 替换 bash 成为 PID 1，
									// 确保 K8s SIGTERM 直接送达 compute_ctl 而非被 bash 吞掉。
									"echo \"$INITIAL_SPEC_JSON\" > /var/spec.json && "+
										"exec /usr/local/bin/compute_ctl --pgdata /.neon/data/pgdata "+
										"--connstr=postgresql://cloud_admin:@0.0.0.0:55433/postgres "+
										"--compute-id %s -p http://neon-controlplane.neon:8081 "+
										"--pgbin /usr/local/bin/postgres",
									endpoint.Name,
								),
							},
							// preStop 在 SIGTERM 之前执行，主动触发 PostgreSQL 干净关闭：
							//   -m fast   : 回滚活跃事务，正常做 checkpoint
							//   -t 25     : 等待 25s（在 60s 宽限期内留足够缓冲）
							//   || true   : pg down 时不因错误阻塞终止流程
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{
											"/usr/local/bin/pg_ctl",
											"stop",
											"-D", "/.neon/data/pgdata",
											"-m", "fast",
											"-t", "25",
										},
									},
								},
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

// defaultPostgresPassword 是 postgres 角色的默认 SCRAM-SHA-256 密码 verifier。
// 对应明文密码为 "postgres"（仅用于开发/测试环境）。
const defaultPostgresPassword = "SCRAM-SHA-256$4096:159kANhgWW13pz78P02IMQ==$grA4JzZPeRmUeV8VjsCzn8QhgdcUZZXXPyUf3vyZZV4=:XgEolgh2F/C2yTbY16t856WqifqPlQsRnCUU4Q7BLVM="

// EndpointConfigMap 为 Endpoint 构建 ConfigMap。
// 从集群中读取 Role/Database CR 来填充 compute spec 中的 roles 和 databases 列表。
//
// k8sClient 和 ctx 用于查询同一分支下的 Role 和 Database CR。
// 如果 k8sClient 为 nil，则退化为 Phase 1 行为（仅 postgres 角色，空 databases）。
func EndpointConfigMap(
	ctx context.Context,
	k8sClient client.Client,
	endpoint *neonv1alpha1.Endpoint,
	branch *neonv1alpha1.Branch,
	project *neonv1alpha1.Project,
	jwtSecret corev1.Secret,
) (*corev1.ConfigMap, error) {

	jwtManager, err := utils.NewJWTManagerFromSecret(&jwtSecret)
	if err != nil {
		return nil, err
	}

	// 生成 safekeeper WAL 端口 JWT 认证 token。
	// Safekeeper 通过 --pg-auth-public-key-path 要求所有 WAL 连接（端口 5454）
	// 提供有效的 JWT token，walproposer 通过 neon.safekeepers_auth_token GUC 使用此 token。
	safekeeperAuthToken, err := utils.GenerateSafekeeperToken(jwtManager, project.Spec.ClusterName)
	if err != nil {
		return nil, fmt.Errorf("failed to generate safekeeper auth token: %w", err)
	}

	jwk := jwtManager.ToJWK()

	type clusterConfig struct {
		ClusterID string          `json:"cluster_id"`
		Name      string          `json:"name"`
		Roles     []Role          `json:"roles"`
		Databases []Database      `json:"databases"`
		Settings  []SettingsEntry `json:"settings"`
	}

	type computeCtlConfig struct {
		JWKS utils.JWKResponse `json:"jwks"`
	}

	type computeSpec struct {
		FormatVersion    string           `json:"format_version"`
		Cluster          clusterConfig    `json:"cluster"`
		ComputeCtlConfig computeCtlConfig `json:"compute_ctl_config"`
		// StorageAuthToken 是 walproposer 连接 safekeeper 和 pageserver 时使用的认证 token。
		// compute_ctl 从 spec 中读取此字段并设置为 NEON_AUTH_TOKEN 环境变量，
		// walproposer 通过 libpagestore.c 硬编码读取该环境变量完成 JWT 认证。
		StorageAuthToken string `json:"storage_auth_token,omitempty"`
	}

	// 聚合 roles：默认 postgres + 从 Role CR 聚合的用户角色
	roles := aggregateRoles(ctx, k8sClient, branch.Name, project)

	// 聚合 databases：从 Database CR 聚合
	databases := aggregateDatabases(ctx, k8sClient, branch.Name)

	spec := computeSpec{
		FormatVersion:    "1.0",
		StorageAuthToken: safekeeperAuthToken,
		Cluster: clusterConfig{
			ClusterID: project.Spec.TenantID,
			Name:      project.Name,
			Roles:     roles,
			Databases: databases,
			Settings: []SettingsEntry{
				{Name: "neon.tenant_id", Value: project.Spec.TenantID, Vartype: "string"},
				{Name: "neon.timeline_id", Value: branch.Spec.TimelineID, Vartype: "string"},
				{
					Name:    "neon.safekeepers_auth_token",
					Value:   safekeeperAuthToken,
					Vartype: "string",
				},
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

// aggregateRoles 从 K8s Role CR 聚合成 compute_ctl spec 所需的 roles 列表。
// 始终包含默认的 postgres 角色，然后追加同分支下非保护、非 postgres、非 no_login 的用户角色。
func aggregateRoles(ctx context.Context, k8sClient client.Client, branchID string, project *neonv1alpha1.Project) []Role {
	// 默认 postgres 角色（始终存在）
	roles := []Role{
		{
			Name:              "postgres",
			EncryptedPassword: defaultPostgresPassword,
			Options:           nil,
		},
	}

	// 如果没有 k8sClient，退化为 Phase 1 行为
	if k8sClient == nil {
		return roles
	}

	var roleList neonv1alpha1.RoleList
	if err := k8sClient.List(ctx, &roleList,
		client.MatchingFields{"spec.branchID": branchID},
		client.InNamespace(project.Namespace),
	); err != nil {
		endpointLogger.Warn("aggregateRoles: failed to list Role CRs, falling back to default",
			"branchID", branchID, "error", err)
		return roles
	}

	for _, roleCR := range roleList.Items {
		// 跳过系统保护角色
		if roleCR.Status.Protected {
			continue
		}
		// 跳过 postgres（已默认添加）
		if roleCR.Spec.Name == "postgres" {
			continue
		}
		// 跳过 no_login 角色（不需要密码认证）
		if roleCR.Spec.AuthenticationMethod == "no_login" {
			continue
		}
		// 需要有 EncryptedPassword 才能写入 spec
		if roleCR.Status.EncryptedPassword == "" {
			endpointLogger.Warn("aggregateRoles: skipping role without encrypted password, SCRAM hash not yet computed",
				"role", roleCR.Spec.Name)
			continue
		}

		roles = append(roles, Role{
			Name:              roleCR.Spec.Name,
			EncryptedPassword: roleCR.Status.EncryptedPassword,
			Options:           buildRoleOptions(roleCR.Spec.AuthenticationMethod),
		})
	}

	return roles
}

// buildRoleOptions 根据认证方式构建 role 的 options。
// oauth 方式不需要 LOGIN 属性（compute_ctl 会处理 JWKS 验证）。
func buildRoleOptions(authMethod string) any {
	if authMethod == "oauth" {
		return nil
	}
	return nil
}

// aggregateDatabases 从 K8s Database CR 聚合成 compute_ctl spec 所需的 databases 列表。
func aggregateDatabases(ctx context.Context, k8sClient client.Client, branchID string) []Database {
	// 如果没有 k8sClient，退化为空列表
	if k8sClient == nil {
		return nil
	}

	var dbList neonv1alpha1.DatabaseList
	if err := k8sClient.List(ctx, &dbList,
		client.MatchingFields{"spec.branchID": branchID},
	); err != nil {
		endpointLogger.Warn("aggregateDatabases: failed to list Database CRs",
			"branchID", branchID, "error", err)
		return nil
	}

	databases := make([]Database, 0, len(dbList.Items))
	for _, dbCR := range dbList.Items {
		owner := dbCR.Spec.OwnerName
		if owner == "" {
			owner = "postgres" // 默认 owner
		}
		databases = append(databases, Database{
			Name:    dbCR.Spec.Name,
			Owner:   owner,
			Options: nil,
		})
	}

	return databases
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
