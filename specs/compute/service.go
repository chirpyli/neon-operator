package compute

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
)

type ServiceConfig struct {
	Suffix    string
	Component string
	PortName  string
	Port      int32
}

func createService(branch *neonv1alpha1.Branch, project *neonv1alpha1.Project, config ServiceConfig) *corev1.Service {
	serviceName := fmt.Sprintf("%s-%s", branch.Name, config.Suffix)
	labels := map[string]string{
		"molnett.org/cluster":   project.Spec.ClusterName,
		"molnett.org/component": config.Component,
		"molnett.org/branch":    branch.Name,
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
			Namespace: branch.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": fmt.Sprintf("%s-compute-node", branch.Name),
			},
			Ports: []corev1.ServicePort{
				{
					Name:       config.PortName,
					Port:       config.Port,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(int(config.Port)),
				},
			},
		},
	}
}

func AdminService(branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) *corev1.Service {
	return createService(branch, project, ServiceConfig{
		Suffix:    "admin",
		Component: "compute-admin",
		PortName:  "admin",
		Port:      3080,
	})
}

// PostgresService 创建 PostgreSQL 连接的 Service。
// exposure 从 Cluster.Spec.PostgresExposure 读取并应用。
func PostgresService(branch *neonv1alpha1.Branch, project *neonv1alpha1.Project, exposure *neonv1alpha1.ServiceExposure) *corev1.Service {
	svc := createService(branch, project, ServiceConfig{
		Suffix:    "postgres",
		Component: "compute-postgres",
		PortName:  "postgres",
		Port:      55433,
	})
	applyServiceExposure(svc, exposure)
	return svc
}

// applyServiceExposure 将 ServiceExposure 配置应用到 Service 对象
func applyServiceExposure(svc *corev1.Service, exposure *neonv1alpha1.ServiceExposure) {
	if exposure == nil {
		return
	}
	if exposure.Type != "" {
		svc.Spec.Type = exposure.Type
	}
	if exposure.ExternalTrafficPolicy != "" {
		svc.Spec.ExternalTrafficPolicy = exposure.ExternalTrafficPolicy
	}
	if len(exposure.LoadBalancerSourceRanges) > 0 {
		svc.Spec.LoadBalancerSourceRanges = exposure.LoadBalancerSourceRanges
	}
	if len(exposure.Annotations) > 0 {
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		for k, v := range exposure.Annotations {
			svc.Annotations[k] = v
		}
	}
}
