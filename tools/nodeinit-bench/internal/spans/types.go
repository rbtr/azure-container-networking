// Package spans defines the data model for per-Node initialization spans
// and emits CSV/Mermaid/Plotly/Markdown artifacts from collected runs.
package spans

import "time"

// SpanID is a stable identifier used for column headers, Mermaid rows, etc.
type SpanID string

const (
	SpanNodeRegistered       SpanID = "node-registered"
	SpanVMProvision          SpanID = "vm-provision"
	SpanDNCRCCreateNNC       SpanID = "dnc-rc-create-nnc"
	SpanDNCRCCreateNC        SpanID = "dnc-rc-create-nc"
	SpanNNCStatusWritten     SpanID = "nnc-status-written"
	SpanCNSPodScheduleLat    SpanID = "cns-pod-schedule-latency"
	SpanCNSInitImagePull     SpanID = "cns-init-image-pull"
	SpanCNSInitContainerRun  SpanID = "cns-init-container-run"
	SpanCNSInitToMainGap     SpanID = "cns-init-to-main-gap"
	SpanCNSImagePull         SpanID = "cns-image-pull"
	SpanCNSContainerStart    SpanID = "cns-container-start"
	SpanCNSExecGap           SpanID = "cns-exec-gap"
	SpanCNSProcessBootstrap  SpanID = "cns-process-bootstrap"
	SpanCNSStateRestored     SpanID = "cns-state-restored"
	SpanCNSNNCIngest         SpanID = "cns-nnc-ingest"
	SpanCNSFirstNNCReceived  SpanID = "cns-first-nnc-received"
	SpanCNSInitialIPAMReconciled SpanID = "cns-initial-ipam-reconciled"
	SpanCNSFirstNCProgrammed SpanID = "cns-first-nc-programmed"
	SpanCNSSyncHostNCVersion SpanID = "cns-sync-host-nc-version"
	SpanCNSListenerReady     SpanID = "cns-listener-ready"
	SpanCNSConflistWrite     SpanID = "cns-conflist-write"
	SpanCNSReadyToAssign     SpanID = "cns-ready-to-assign"
	SpanCNSPodReady          SpanID = "cns-pod-ready"
	SpanNodeReady            SpanID = "node-ready"
	SpanKubeletCNIPickup     SpanID = "kubelet-cni-pickup"
)

// OrderedSpans is the canonical top-to-bottom order for Gantt rows.
var OrderedSpans = []SpanID{
	SpanVMProvision,
	SpanNodeRegistered,
	SpanDNCRCCreateNNC,
	SpanDNCRCCreateNC,
	SpanNNCStatusWritten,
	SpanCNSPodScheduleLat,
	SpanCNSInitImagePull,
	SpanCNSInitContainerRun,
	SpanCNSInitToMainGap,
	SpanCNSImagePull,
	SpanCNSContainerStart,
	SpanCNSExecGap,
	SpanCNSProcessBootstrap,
	SpanCNSStateRestored,
	SpanCNSNNCIngest,
	SpanCNSFirstNNCReceived,
	SpanCNSInitialIPAMReconciled,
	SpanCNSFirstNCProgrammed,
	SpanCNSSyncHostNCVersion,
	SpanCNSListenerReady,
	SpanCNSConflistWrite,
	SpanCNSReadyToAssign,
	SpanCNSPodReady,
	SpanNodeReady,
	SpanKubeletCNIPickup,
}

// Span is a single measured interval.
type Span struct {
	ID       SpanID
	Start    time.Time
	End      time.Time
	Source   string // human-readable source description (e.g. "pod-event", "nnc-managedFields", "cns-log")
	Inferred bool   // true for spans we had to approximate
	Missing  bool   // true if Start or End could not be observed
}

// Duration returns End-Start, or zero if Missing.
func (s Span) Duration() time.Duration {
	if s.Missing || s.Start.IsZero() || s.End.IsZero() {
		return 0
	}
	return s.End.Sub(s.Start)
}

// NodeRun is all spans collected for a single Node in a single run.
type NodeRun struct {
	RunID   int
	Node    string
	T0      time.Time // node.metadata.creationTimestamp
	NodeUID string
	Spans   map[SpanID]Span
	PodName string
	// Metrics holds CNS Prometheus series scraped after PodReady, flattened
	// as `<name>` / `<name>_sum` / `<name>_count` / `<name>_bucket{le="..."}`.
	// Empty if scraping is disabled, unsupported, or failed.
	Metrics map[string]float64

	// BootState carries the value of cns_boot_state{state} that was
	// active when this run started. One of "fresh", "reboot",
	// "restart", "unknown", or "" if metrics weren't scraped /
	// CNS doesn't emit the metric yet (older builds).
	BootState string

	// Mode is a small set of dashboard-pivotable labels extracted
	// from cns_mode_info: channel_mode, ipam_v2, swift_v2,
	// manage_endpoint_state, dual_stack. Empty if metrics weren't
	// scraped.
	Mode map[string]string
}
