package compute_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/compute"
	"oltp.molnett.org/neon-operator/test/fixtures"
	testutils "oltp.molnett.org/neon-operator/test/utils"
)

func TestSpecs(t *testing.T) {
	const (
		projectName = "test-project"
		branchName  = "test-branch"
		namespace   = "neon"
		clusterName = "test-cluster"
	)

	project := fixtures.NewProject(projectName, namespace, clusterName)
	branch := fixtures.NewBranch(branchName, namespace, projectName)

	cases := []struct {
		name string
		obj  any
	}{
		{"deployment", compute.Deployment(branch, project, "")},
		{"admin_service", compute.AdminService(branch, project)},
		{"postgres_service", compute.PostgresService(branch, project, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutils.AssertGolden(t, "testdata/"+tc.name+".yaml", tc.obj)
		})
	}
}

func TestServiceExposure(t *testing.T) {
	const (
		projectName = "test-project"
		branchName  = "test-branch"
		namespace   = "neon"
		clusterName = "test-cluster"
	)

	project := fixtures.NewProject(projectName, namespace, clusterName)
	branch := fixtures.NewBranch(branchName, namespace, projectName)

	cases := []struct {
		name string
		obj  any
	}{
		{
			"postgres_service_default",
			compute.PostgresService(branch, project, nil),
		},
		{
			"postgres_service_nodeport",
			compute.PostgresService(branch, project, &neonv1alpha1.ServiceExposure{
				Type: corev1.ServiceTypeNodePort,
			}),
		},
		{
			"postgres_service_loadbalancer",
			compute.PostgresService(branch, project, &neonv1alpha1.ServiceExposure{
				Type:                     corev1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy:    corev1.ServiceExternalTrafficPolicyTypeLocal,
				LoadBalancerSourceRanges: []string{"203.0.113.0/24", "198.51.100.5/32"},
				Annotations:              map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": "nlb"},
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutils.AssertGolden(t, "testdata/"+tc.name+".yaml", tc.obj)
		})
	}
}
