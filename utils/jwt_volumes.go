package utils

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// JWTSecretName returns the standard name of the JWT secret for a cluster.
func JWTSecretName(clusterName string) string {
	return fmt.Sprintf("cluster-%s-jwt", clusterName)
}

// JWTVolume creates a Volume that projects the public.pem key from the JWT secret.
// The volume is read-only (mode 0444) and exposes only the public key.
func JWTVolume(clusterName string) corev1.Volume {
	return corev1.Volume{
		Name: "jwt-keys",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: JWTSecretName(clusterName),
				Items: []corev1.KeyToPath{
					{
						Key:  "public.pem",
						Path: "public.pem",
					},
				},
				DefaultMode: ptr.To(int32(0444)),
			},
		},
	}
}

// JWTVolumeMount returns the standard volume mount for the JWT public key.
// Mounts at /certs, read-only.
func JWTVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      "jwt-keys",
		MountPath: "/certs",
		ReadOnly:  true,
	}
}

// JWTPublicKeyPath returns the standard path to the JWT public key inside the container.
func JWTPublicKeyPath() string {
	return "/certs/public.pem"
}
