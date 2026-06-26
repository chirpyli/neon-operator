package pageserver

import "oltp.molnett.org/neon-operator/api/v1alpha1"

const ClusterLabel = "molnett.org/cluster"

func labels(ps *v1alpha1.Pageserver) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "pageserver",
		"app.kubernetes.io/component": "pageserver",
		"app.kubernetes.io/part-of":   "neon",
		ClusterLabel:                  ps.Spec.Cluster,
		"molnett.org/component":       "pageserver",
		"molnett.org/pageserver":      ps.Name,
	}
}

// selectorLabels returns labels used for StatefulSet pod selection.
// Each pageserver has its own StatefulSet, so per-instance label is correct here.
func selectorLabels(ps *v1alpha1.Pageserver) map[string]string {
	return map[string]string{
		"molnett.org/pageserver": ps.Name,
	}
}

// LabelSelector returns the minimal label set for cross-pageserver selectors
// such as PodAntiAffinity and PodDisruptionBudget. It intentionally excludes
// "molnett.org/pageserver" (per-instance) so that selectors match ALL
// pageservers of the same cluster, not just one instance.
func LabelSelector(ps *v1alpha1.Pageserver) map[string]string {
	return map[string]string{
		"app.kubernetes.io/component": "pageserver",
		ClusterLabel:                  ps.Spec.Cluster,
	}
}
