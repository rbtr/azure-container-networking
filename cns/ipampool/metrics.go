package ipampool

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	subnetLabel                = "subnet"
	subnetCIDRLabel            = "subnet_cidr"
	podnetARMIDLabel           = "podnet_arm_id"
	customerMetricLabel        = "customer_metric"
	customerMetricLabelValue   = "customer metric"
	subnetExhaustionStateLabel = "subnet_exhaustion_state"
	SubnetIPExhausted          = 1
	SubnetIPNotExhausted       = 0
)

var (
	IpamAllocatedIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_pod_allocated_ips",
			Help:        "Count of IPs CNS has allocated to Pods.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamAvailableIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_available_ips",
			Help:        "Available IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamBatchSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_batch_size",
			Help:        "IPAM IP pool batch size.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamCurrentAvailableIPcount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_current_available_ips",
			Help:        "Current available IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamExpectedAvailableIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_expect_available_ips",
			Help:        "Expected future available IP count assuming the Requested IP count is honored.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamMaxIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_max_ips",
			Help:        "Maximum IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamPendingProgramIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_pending_programming_ips",
			Help:        "Pending programming IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamPendingReleaseIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_pending_release_ips",
			Help:        "Pending release IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamPrimaryIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_primary_ips",
			Help:        "NC Primary IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamRequestedIPConfigCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_requested_ips",
			Help:        "Requested IP count.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamTotalIPCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_total_ips",
			Help:        "Count of total IP pool size allocated to CNS by DNC.",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamSubnetExhaustionState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cx_ipam_subnet_exhaustion_state",
			Help:        "IPAM view of subnet exhaustion state",
			ConstLabels: prometheus.Labels{customerMetricLabel: customerMetricLabelValue},
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel},
	)
	IpamSubnetExhaustionCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cx_ipam_subnet_exhaustion_state_count_total",
			Help: "Count of the number of times the ipam pool monitor sees subnet exhaustion",
		},
		[]string{subnetLabel, subnetCIDRLabel, podnetARMIDLabel, subnetExhaustionStateLabel},
	)
)

func init() {
	metrics.Registry.MustRegister(
		IpamAllocatedIPCount,
		IpamAvailableIPCount,
		IpamBatchSize,
		IpamCurrentAvailableIPcount,
		IpamExpectedAvailableIPCount,
		IpamMaxIPCount,
		IpamPendingProgramIPCount,
		IpamPendingReleaseIPCount,
		IpamPrimaryIPCount,
		IpamRequestedIPConfigCount,
		IpamTotalIPCount,
		IpamSubnetExhaustionState,
		IpamSubnetExhaustionCount,
	)
}

func observeIPPoolState(state ipPoolState, meta metaState) {
	labels := []string{meta.subnet, meta.subnetCIDR, meta.subnetARMID}
	IpamAllocatedIPCount.WithLabelValues(labels...).Set(float64(state.allocatedToPods))
	IpamAvailableIPCount.WithLabelValues(labels...).Set(float64(state.available))
	IpamBatchSize.WithLabelValues(labels...).Set(float64(meta.batch))
	IpamCurrentAvailableIPcount.WithLabelValues(labels...).Set(float64(state.currentAvailableIPs))
	IpamExpectedAvailableIPCount.WithLabelValues(labels...).Set(float64(state.expectedAvailableIPs))
	IpamMaxIPCount.WithLabelValues(labels...).Set(float64(meta.max))
	IpamPendingProgramIPCount.WithLabelValues(labels...).Set(float64(state.pendingProgramming))
	IpamPendingReleaseIPCount.WithLabelValues(labels...).Set(float64(state.pendingRelease))
	IpamPrimaryIPCount.WithLabelValues(labels...).Set(float64(len(meta.primaryIPAddresses)))
	IpamRequestedIPConfigCount.WithLabelValues(labels...).Set(float64(state.requestedIPs))
	IpamTotalIPCount.WithLabelValues(labels...).Set(float64(state.totalIPs))
	if meta.exhausted {
		IpamSubnetExhaustionState.WithLabelValues(labels...).Set(float64(SubnetIPExhausted))
	} else {
		IpamSubnetExhaustionState.WithLabelValues(labels...).Set(float64(SubnetIPNotExhausted))
	}
}
