// Package fixtures provides constructors for neon-operator CRDs and their
// supporting Secrets, with sensible defaults for use in unit and envtest tests.
// Callers are expected to mutate the returned value to vary test scenarios.
package fixtures

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
)

const (
	DefaultPGVersion      = 17
	DefaultNumSafekeepers = uint8(3)
	DefaultStorageSize    = "1Gi"
)

// DefaultNeonImage is the default neon image used when creating Cluster fixtures.
// It can be overridden via the NEON_IMAGE environment variable in e2e tests.
// When set to a single image that also contains the compute binary (like
// ghcr.io/neondatabase/neon:latest), the e2e suite will retag it as
// neondatabase/compute-node-v{PGVersion} so the controller's hardcoded compute
// image reference resolves correctly.
var DefaultNeonImage = "neondatabase/neon:8463"

func NewCluster(name, namespace string) *neonv1alpha1.Cluster {
	return &neonv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: neonv1alpha1.ClusterSpec{
			NumSafekeepers:   DefaultNumSafekeepers,
			DefaultPGVersion: DefaultPGVersion,
			NeonImage:        DefaultNeonImage,
			BucketCredentialsSecret: &corev1.SecretReference{
				Name:      name + "-bucket-creds",
				Namespace: namespace,
			},
			StorageControllerDatabaseSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: name + "-storcon-db"},
				Key:                  "uri",
			},
		},
	}
}

func NewProject(name, namespace, clusterName string) *neonv1alpha1.Project {
	return &neonv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: neonv1alpha1.ProjectSpec{
			Name:        name,
			ClusterName: clusterName,
			PGVersion:   DefaultPGVersion,
		},
	}
}

func NewBranch(name, namespace, projectID string) *neonv1alpha1.Branch {
	return &neonv1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: neonv1alpha1.BranchSpec{
			ProjectID: projectID,
			PGVersion: DefaultPGVersion,
		},
	}
}

func NewPageserver(name, namespace, clusterName string, id uint64) *neonv1alpha1.Pageserver {
	return &neonv1alpha1.Pageserver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: neonv1alpha1.PageserverSpec{
			ID:      id,
			Cluster: clusterName,
			BucketCredentialsSecret: &corev1.SecretReference{
				Name:      clusterName + "-bucket-creds",
				Namespace: namespace,
			},
			StorageConfig: neonv1alpha1.StorageConfig{
				Size: DefaultStorageSize,
			},
		},
	}
}

func NewSafekeeper(name, namespace, clusterName string, id uint32) *neonv1alpha1.Safekeeper {
	return &neonv1alpha1.Safekeeper{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: neonv1alpha1.SafekeeperSpec{
			ID:      id,
			Cluster: clusterName,
			StorageConfig: neonv1alpha1.StorageConfig{
				Size: DefaultStorageSize,
			},
		},
	}
}

func NewBucketCredsSecret(clusterName, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-bucket-creds",
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("test-key"),
			"AWS_SECRET_ACCESS_KEY": []byte("test-secret"),
			"AWS_REGION":            []byte("us-east-1"),
			"BUCKET_NAME":           []byte("test-bucket"),
			"ENDPOINT":              []byte("http://minio.test:9000"),
		},
	}
}

func NewEndpoint(name, namespace, branchID string) *neonv1alpha1.Endpoint {
	return &neonv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: neonv1alpha1.EndpointSpec{
			BranchID: branchID,
			Type:     "read_write",
		},
	}
}

func NewStorcondDBSecret(clusterName, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName + "-storcon-db",
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"uri": []byte("postgres://storcon:storcon@localhost:5432/storage_controller"),
		},
	}
}
