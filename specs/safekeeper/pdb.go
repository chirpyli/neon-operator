package safekeeper

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
)

// PodDisruptionBudget creates a PDB for the safekeeper StatefulSet.
// MaxUnavailable=1 ensures at most one safekeeper is disrupted at a time,
// preserving quorum (N/2+1) during voluntary disruptions like node drains.
//
// The LabelSelector intentionally matches ALL safekeepers of the cluster
// (via component+cluster labels), not just one instance, because the PDB
// applies cluster-wide: we want to limit total disruptions across all replicas.
func PodDisruptionBudget(sk *v1alpha1.Safekeeper) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "policy/v1",
			Kind:       "PodDisruptionBudget",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(sk),
			Namespace: sk.Namespace,
			Labels:    labels(sk),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: LabelSelector(sk),
			},
		},
	}
}
