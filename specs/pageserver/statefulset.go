package pageserver

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

const storageVolumeName = "pageserver-storage"

const initScript = `echo "id=%d" > /config/identity.toml

echo "{\"host\":\"%s-0.%s.%s.svc.cluster.local\"," \
     "\"http_host\":\"%s-0.%s.%s.svc.cluster.local\"," \
     "\"http_port\":9898,\"port\":6400," \
     "\"availability_zone_id\":\"%s\"}" > /config/metadata.json

cp /configmap/pageserver.toml /config/pageserver.toml
`

// defaultAvailabilityZone 是未在 PageserverSpec 中显式指定时的默认可用区。
const defaultAvailabilityZone = "se-ume"

// DefaultResources are the default CPU/memory requests and limits for pageserver.
var DefaultResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	},
}

func StatefulSet(ps *v1alpha1.Pageserver, image string, safekeeperAuthToken string) *appsv1.StatefulSet {
	name := Name(ps)
	lbls := labels(ps)

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   storageVolumeName,
			Labels: lbls,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(ps.Spec.StorageConfig.Size),
				},
			},
			StorageClassName: ps.Spec.StorageConfig.StorageClass,
		},
	}

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ps.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(int32(1)),
			ServiceName: HeadlessName(ps),
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels(ps),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: lbls,
				},
				Spec: podSpec(ps, image, name, safekeeperAuthToken),
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc},
		},
	}
}

// availabilityZone returns the AZ to advertise for this pageserver.
// If the spec has an explicit AZ, use it; otherwise fall back to the default.
func availabilityZone(ps *v1alpha1.Pageserver) string {
	if ps.Spec.AvailabilityZone != "" {
		return ps.Spec.AvailabilityZone
	}
	return defaultAvailabilityZone
}

func podSpec(ps *v1alpha1.Pageserver, image, serviceName, safekeeperAuthToken string) corev1.PodSpec {
	var envVars []corev1.EnvVar
	envVars = append(envVars,
		corev1.EnvVar{Name: "RUST_LOG", Value: "info,pageserver=info,walredo=warn"},
		corev1.EnvVar{Name: "DEFAULT_PG_VERSION", Value: "16"},
		bucketEnv("AWS_ACCESS_KEY_ID", ps),
		bucketEnv("AWS_SECRET_ACCESS_KEY", ps),
		bucketEnv("AWS_REGION", ps),
		bucketEnv("BUCKET_NAME", ps),
		bucketEnv("AWS_ENDPOINT_URL", ps),
	)
	// NEON_AUTH_TOKEN is used for Pageserver → Safekeeper WAL connections.
	// The token must have "safekeeperdata" scope. If empty (e.g. SK auth not yet
	// enabled), the env var is omitted and the PS will not authenticate to SK.
	if safekeeperAuthToken != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "NEON_AUTH_TOKEN",
			Value: safekeeperAuthToken,
		})
	}

	return corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:  ptr.To(int64(1000)),
			RunAsGroup: ptr.To(int64(1000)),
			FSGroup:    ptr.To(int64(1000)),
		},
		// PodAntiAffinity 确保 pageserver 分布在不同的节点上
		Affinity: &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: LabelSelector(ps),
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
		TerminationGracePeriodSeconds: ptr.To(int64(60)),
		InitContainers: []corev1.Container{
			{
				Name:            "setup-config",
				Image:           "busybox:latest",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/sh", "-c"},
				Args: []string{
					fmt.Sprintf(initScript,
						ps.Spec.ID,
						serviceName, HeadlessName(ps), ps.Namespace,
						serviceName, HeadlessName(ps), ps.Namespace,
						availabilityZone(ps)),
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pageserver-config", MountPath: "/configmap"},
					{Name: "config", MountPath: "/config"},
				},
			},
		},
		Containers: []corev1.Container{
			{
				Name:            "pageserver",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways,
				Command:         []string{"/usr/local/bin/pageserver"},
				Ports: []corev1.ContainerPort{
					{Name: "pg", ContainerPort: 6400},
					{Name: "http", ContainerPort: 9898},
				},
				Env: envVars,
				// Health probes using /v1/status endpoint.
				// Default parameters are aligned with SC heartbeat intervals:
				// - Liveness:  ~30s detection window → matches max_offline_interval=30s
				// - Readiness:  5s period → matches heartbeat_interval=5s
				// - Startup:  300s window → matches max_warming_up_interval=300s
				LivenessProbe:  utils.ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 30, 10, 5, 3, ps.Spec.LivenessProbe),
				ReadinessProbe: utils.ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 10, 5, 3, 2, ps.Spec.ReadinessProbe),
				StartupProbe:   utils.ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 10, 10, 5, 30, ps.Spec.StartupProbe),
				Resources:      pageserverResources(ps),
				VolumeMounts: []corev1.VolumeMount{
					{Name: storageVolumeName, MountPath: "/data/.neon/tenants"},
					{Name: "config", MountPath: "/data/.neon"},
					utils.JWTVolumeMount(),
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "pageserver-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: serviceName},
					},
				},
			},
			{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			utils.JWTVolume(ps.Spec.Cluster),
		},
	}
}

// pageserverResources returns the resource requirements for the pageserver container.
// If the PS spec specifies resources, use those. Otherwise use defaults.
func pageserverResources(ps *v1alpha1.Pageserver) corev1.ResourceRequirements {
	if ps.Spec.Resources != nil {
		return *ps.Spec.Resources
	}
	return DefaultResources
}

func bucketEnv(key string, ps *v1alpha1.Pageserver) corev1.EnvVar {
	return corev1.EnvVar{
		Name: key,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: ps.Spec.BucketCredentialsSecret.Name,
				},
				Key: key,
			},
		},
	}
}
