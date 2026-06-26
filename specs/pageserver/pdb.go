package pageserver

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
)

// PodDisruptionBudget creates a PDB for the pageserver StatefulSet.
// MaxUnavailable=1 ensures at most one pageserver is disrupted at a time,
// preventing multiple pageservers from being simultaneously evicted during
// voluntary disruptions like node drains.
//
// The LabelSelector intentionally matches ALL pageservers of the cluster
// (via component+cluster labels), not just one instance, because the PDB
// applies cluster-wide: we want to limit total disruptions across all replicas.
func PodDisruptionBudget(ps *v1alpha1.Pageserver) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "policy/v1",
			Kind:       "PodDisruptionBudget",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(ps),
			Namespace: ps.Namespace,
			Labels:    labels(ps),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: LabelSelector(ps),
			},
		},
	}
}
