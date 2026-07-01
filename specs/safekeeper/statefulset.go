package safekeeper

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/storagebroker"
	"oltp.molnett.org/neon-operator/utils"
)

const storageVolumeName = "safekeeper-storage"

func StatefulSet(sk *v1alpha1.Safekeeper, image string) *appsv1.StatefulSet {
	name := Name(sk)
	lbls := labels(sk)

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   storageVolumeName,
			Labels: lbls,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(sk.Spec.StorageConfig.Size),
				},
			},
			StorageClassName: sk.Spec.StorageConfig.StorageClass,
		},
	}

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sk.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(int32(1)),
			ServiceName: HeadlessName(sk),
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels(sk),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: lbls,
				},
				Spec: podSpec(sk, image),
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc},
		},
	}
}

func podSpec(sk *v1alpha1.Safekeeper, image string) corev1.PodSpec {
	// StatefulSet Pod FQDN (STS replica count = 1, ordinal = 0):
	//   {pod-name}.{headless-service}.{namespace}.svc.cluster.local
	// With STS name = {cluster}-safekeeper-{id}, pod name = {cluster}-safekeeper-{id}-0:
	//   → {cluster}-safekeeper-{id}-0.{cluster}-safekeeper-{id}-headless.{ns}.svc.cluster.local
	advertiseHost := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local",
		Name(sk), HeadlessName(sk), sk.Namespace)

	return corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:  ptr.To(int64(1000)),
			RunAsGroup: ptr.To(int64(1000)),
			FSGroup:    ptr.To(int64(1000)),
		},
		// PodAntiAffinity ensures safekeepers are distributed across nodes.
		// LabelSelector intentionally excludes per-instance labels so that
		// ALL safekeeper pods of the same cluster repel each other.
		Affinity: &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: LabelSelector(sk),
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
		TerminationGracePeriodSeconds: ptr.To(int64(30)),
		Containers: []corev1.Container{
			{
				Name:    "safekeeper",
				Image:   image,
				Command: []string{"/usr/local/bin/safekeeper"},
				Args: []string{
					"--id=" + strconv.FormatUint(uint64(sk.Spec.ID), 10),
					"--broker-endpoint=" + storagebroker.URL(sk.Spec.Cluster),
					"--listen-pg=0.0.0.0:5454",
					"--listen-http=0.0.0.0:7676",
					"--advertise-pg=" + advertiseHost + ":5454",
					"--datadir=/data",
				},
				Ports: []corev1.ContainerPort{
					{Name: "pg", ContainerPort: 5454},
					{Name: "http", ContainerPort: 7676},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: storageVolumeName, MountPath: "/data"},
				},
				// Health probes using /v1/status (the only unauthenticated endpoint).
				// Default parameters are aligned with SC heartbeat intervals:
				// - Liveness: ~30s detection window → matches max_offline_interval=30s
				// - Readiness: 5s period → matches heartbeat_interval=5s
				// - Startup:  60s window → Safekeeper starts fast (no cold-start from S3)
				LivenessProbe:  utils.ProbeWithConfig("/v1/status", 7676, corev1.URISchemeHTTP, 10, 10, 5, 3, sk.Spec.LivenessProbe),
				ReadinessProbe: utils.ProbeWithConfig("/v1/status", 7676, corev1.URISchemeHTTP, 5, 5, 3, 2, sk.Spec.ReadinessProbe),
				StartupProbe:   utils.ProbeWithConfig("/v1/status", 7676, corev1.URISchemeHTTP, 5, 5, 5, 12, sk.Spec.StartupProbe),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			},
		},
	}
}
