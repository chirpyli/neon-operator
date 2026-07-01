package utils_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/utils"
)

func TestProbeWithConfig_NilConfig(t *testing.T) {
	probe := utils.ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 30, 10, 5, 3, nil)

	assert.Equal(t, int32(30), probe.InitialDelaySeconds)
	assert.Equal(t, int32(10), probe.PeriodSeconds)
	assert.Equal(t, int32(5), probe.TimeoutSeconds)
	assert.Equal(t, int32(3), probe.FailureThreshold)

	assert.NotNil(t, probe.HTTPGet)
	assert.Equal(t, "/v1/status", probe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt(9898), probe.HTTPGet.Port)
	assert.Equal(t, corev1.URISchemeHTTP, probe.HTTPGet.Scheme)
}

func TestProbeWithConfig_PartialOverride(t *testing.T) {
	cfg := &v1alpha1.ProbeConfig{
		InitialDelaySeconds: ptr.To(int32(60)),
	}
	probe := utils.ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 30, 10, 5, 3, cfg)

	// Overridden value
	assert.Equal(t, int32(60), probe.InitialDelaySeconds)
	// Default values
	assert.Equal(t, int32(10), probe.PeriodSeconds)
	assert.Equal(t, int32(5), probe.TimeoutSeconds)
	assert.Equal(t, int32(3), probe.FailureThreshold)
}

func TestProbeWithConfig_FullOverride(t *testing.T) {
	cfg := &v1alpha1.ProbeConfig{
		InitialDelaySeconds: ptr.To(int32(60)),
		PeriodSeconds:       ptr.To(int32(20)),
		TimeoutSeconds:      ptr.To(int32(10)),
		FailureThreshold:    ptr.To(int32(5)),
	}
	probe := utils.ProbeWithConfig("/live", 8080, corev1.URISchemeHTTP, 5, 10, 5, 3, cfg)

	assert.Equal(t, int32(60), probe.InitialDelaySeconds)
	assert.Equal(t, int32(20), probe.PeriodSeconds)
	assert.Equal(t, int32(10), probe.TimeoutSeconds)
	assert.Equal(t, int32(5), probe.FailureThreshold)
}

func TestTCPProbe_Basic(t *testing.T) {
	probe := utils.TCPProbe(55433, 5, 10, 5, 30, nil)

	assert.NotNil(t, probe.TCPSocket)
	assert.Equal(t, intstr.FromInt(55433), probe.TCPSocket.Port)
	assert.Equal(t, int32(5), probe.InitialDelaySeconds)
	assert.Equal(t, int32(10), probe.PeriodSeconds)
	assert.Equal(t, int32(5), probe.TimeoutSeconds)
	assert.Equal(t, int32(30), probe.FailureThreshold)
}

func TestTCPProbe_WithOverride(t *testing.T) {
	cfg := &v1alpha1.ProbeConfig{
		InitialDelaySeconds: ptr.To(int32(15)),
		FailureThreshold:    ptr.To(int32(60)),
	}
	probe := utils.TCPProbe(55433, 5, 10, 5, 30, cfg)

	assert.Equal(t, int32(15), probe.InitialDelaySeconds)
	assert.Equal(t, int32(10), probe.PeriodSeconds) // default
	assert.Equal(t, int32(60), probe.FailureThreshold)
}
