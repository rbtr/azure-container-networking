package v2

import "github.com/Azure/azure-container-networking/cns/ipampool"

var (
	subnetLabel                = "subnet"
	subnetCIDRLabel            = "subnet_cidr"
	podnetARMIDLabel           = "podnet_arm_id"
	customerMetricLabel        = "customer_metric"
	subnetExhaustionStateLabel = "subnet_exhaustion_state"
	ipamAllocatedIPCount       = ipampool.IpamAllocatedIPCount
	ipamAvailableIPCount       = ipampool.IpamAvailableIPCount
	ipamBatchSize              = ipampool.IpamAvailableIPCount
	ipamMaxIPCount             = ipampool.IpamMaxIPCount
	ipamPendingProgramIPCount  = ipampool.IpamPendingProgramIPCount
	ipamPendingReleaseIPCount  = ipampool.IpamPendingReleaseIPCount
	ipamPrimaryIPCount         = ipampool.IpamPrimaryIPCount
	ipamRequestedIPConfigCount = ipampool.IpamRequestedIPConfigCount
	ipamTotalIPCount           = ipampool.IpamTotalIPCount
	ipamSubnetExhaustionState  = ipampool.IpamSubnetExhaustionState
	ipamSubnetExhaustionCount  = ipampool.IpamSubnetExhaustionCount
	subnetIPExhausted          = ipampool.SubnetIPExhausted
	subnetIPNotExhausted       = ipampool.SubnetIPNotExhausted
)

func observeIPPoolState(pool pool, scaler scaler, subnet subnet) {
	labels := []string{subnet.Name, subnet.CIDR, subnet.ARMID}
	ipamAllocatedIPCount.WithLabelValues(labels...).Set(float64(pool.assigned))
	ipamAvailableIPCount.WithLabelValues(labels...).Set(float64(pool.available))
	ipamBatchSize.WithLabelValues(labels...).Set(float64(scaler.batch))
	ipamMaxIPCount.WithLabelValues(labels...).Set(float64(scaler.max))
	ipamPendingProgramIPCount.WithLabelValues(labels...).Set(float64(pool.pendingProgramming))
	ipamPendingReleaseIPCount.WithLabelValues(labels...).Set(float64(pool.pendingRelease))
	ipamPrimaryIPCount.WithLabelValues(labels...).Set(float64(pool.primaryIPs))
	ipamRequestedIPConfigCount.WithLabelValues(labels...).Set(float64(pool.requested))
	ipamTotalIPCount.WithLabelValues(labels...).Set(float64(pool.totalIPs))
	if scaler.exhausted {
		ipamSubnetExhaustionState.WithLabelValues(labels...).Set(float64(subnetIPExhausted))
	} else {
		ipamSubnetExhaustionState.WithLabelValues(labels...).Set(float64(subnetIPNotExhausted))
	}
}
