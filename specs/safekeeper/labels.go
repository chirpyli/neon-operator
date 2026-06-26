package safekeeper

import "oltp.molnett.org/neon-operator/api/v1alpha1"

const ClusterLabel = "molnett.org/cluster"

func labels(sk *v1alpha1.Safekeeper) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "safekeeper",
		"app.kubernetes.io/component": "safekeeper",
		"app.kubernetes.io/part-of":   "neon",
		ClusterLabel:                  sk.Spec.Cluster,
		"molnett.org/safekeeper":      sk.Name,
	}
}

// selectorLabels returns labels used for StatefulSet pod selection.
// Each safekeeper has its own StatefulSet, so per-instance label is correct here.
func selectorLabels(sk *v1alpha1.Safekeeper) map[string]string {
	return map[string]string{
		"molnett.org/safekeeper": sk.Name,
	}
}

// LabelSelector returns the minimal label set for cross-safekeeper selectors
// such as PodAntiAffinity and PodDisruptionBudget. It intentionally excludes
// "molnett.org/safekeeper" (per-instance) so that selectors match ALL
// safekeepers of the same cluster, not just one instance.
func LabelSelector(sk *v1alpha1.Safekeeper) map[string]string {
	return map[string]string{
		"app.kubernetes.io/component": "safekeeper",
		ClusterLabel:                  sk.Spec.Cluster,
	}
}
