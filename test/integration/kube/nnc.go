package kube

import (
	"math"
	"testing"

	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/stretchr/testify/assert"
)

func ValidateNNC(t *testing.T, nnc v1alpha.NodeNetworkConfig, pods int64, subnetScarcity bool) {
	assert.GreaterOrEqual(t, nnc.Spec.RequestedIPCount, pods)
	expectedReq := calculateExpectedRequest(pods, nnc.Status.Scaler.BatchSize, float64(nnc.Status.Scaler.RequestThresholdPercent)/100) //nolint:gomnd // percentage
	if subnetScarcity {
		expectedReq = calculateExpectedRequest(pods, 1, float64(nnc.Status.Scaler.RequestThresholdPercent)/100) //nolint:gomnd // percentage
	}
	assert.Equal(t, expectedReq, nnc.Spec.RequestedIPCount)
}

func calculateExpectedRequest(used, batch int64, minfree float64) int64 {
	return int64(float64(batch) * math.Ceil(minfree+(float64(used)/float64(batch))))
}
