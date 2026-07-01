package utils

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
)

// ProbeWithConfig builds an HTTP GET *corev1.Probe using the given defaults,
// then applies any overrides from cfg. The path, port, and scheme are fixed by
// the caller because they correspond to upstream Neon's API design; only the
// threshold/hysteresis parameters are overridable.
func ProbeWithConfig(
	path string, port int, scheme corev1.URIScheme,
	initialDelay, period, timeout, failure int32,
	cfg *v1alpha1.ProbeConfig,
) *corev1.Probe {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromInt(port),
				Scheme: scheme,
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      timeout,
		FailureThreshold:    failure,
	}
	applyProbeConfig(probe, cfg)
	return probe
}

// TCPProbe builds a tcpSocket *corev1.Probe (for components without HTTP
// endpoints, e.g. Compute Node before JWT is deployed). The threshold
// parameters are overridable via cfg.
func TCPProbe(
	port int,
	initialDelay, period, timeout, failure int32,
	cfg *v1alpha1.ProbeConfig,
) *corev1.Probe {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt(port),
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      timeout,
		FailureThreshold:    failure,
	}
	applyProbeConfig(probe, cfg)
	return probe
}

// applyProbeConfig applies non-nil overrides from cfg to probe.
func applyProbeConfig(probe *corev1.Probe, cfg *v1alpha1.ProbeConfig) {
	if cfg == nil {
		return
	}
	if cfg.InitialDelaySeconds != nil {
		probe.InitialDelaySeconds = *cfg.InitialDelaySeconds
	}
	if cfg.PeriodSeconds != nil {
		probe.PeriodSeconds = *cfg.PeriodSeconds
	}
	if cfg.TimeoutSeconds != nil {
		probe.TimeoutSeconds = *cfg.TimeoutSeconds
	}
	if cfg.FailureThreshold != nil {
		probe.FailureThreshold = *cfg.FailureThreshold
	}
}
