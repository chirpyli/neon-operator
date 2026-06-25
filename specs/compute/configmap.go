package compute

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

func ConfigMap(
	branch *neonv1alpha1.Branch,
	project *neonv1alpha1.Project,
	jwtSecret corev1.Secret,
) (*corev1.ConfigMap, error) {

	jwtManager, err := utils.NewJWTManagerFromSecret(&jwtSecret)
	if err != nil {
		return nil, err
	}

	jwk := jwtManager.ToJWK()

	// clusterConfig 对应 ComputeSpec 中 ClusterConfig 的核心字段，
	// 注入 tenant_id + timeline_id 作为本地引导身份。
	// 命名说明：cluster_id 是上游 Neon 的历史命名，语义值 = tenant_id。
	type clusterConfig struct {
		ClusterID string          `json:"cluster_id"`
		Name      string          `json:"name"`
		Roles     []interface{}   `json:"roles"`
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

	spec := computeSpec{
		FormatVersion: "1.0",
		Cluster: clusterConfig{
			ClusterID: project.Spec.TenantID,
			Name:      project.Name,
			Roles:     []interface{}{},
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
			Name:      fmt.Sprintf("%s-compute-spec", branch.Name),
			Namespace: branch.Namespace,
			Labels: map[string]string{
				"molnett.org/cluster":   project.Spec.ClusterName,
				"molnett.org/component": "compute",
				"molnett.org/branch":    branch.Name,
			},
		},
		Data: map[string]string{
			"spec.json": string(specJSON),
		},
	}, nil
}
