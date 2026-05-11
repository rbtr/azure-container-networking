package observer

import (
	"strings"
	"time"

	nncv1alpha "github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/cnslogs"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/spans"
	corev1 "k8s.io/api/core/v1"
)

// buildSpans populates nr.Spans from all gathered sources. Missing data is
// recorded with Missing=true so downstream emitters can surface gaps clearly.
func buildSpans(
	nr *spans.NodeRun,
	node *corev1.Node,
	pod *corev1.Pod,
	nodeEvents []corev1.Event,
	podEvents []corev1.Event,
	nnc *nncv1alpha.NodeNetworkConfig,
	anchors cnslogs.Anchors,
	submit time.Time,
	conflistMtime time.Time,
) {
	set := func(id spans.SpanID, start, end time.Time, source string, inferred bool) {
		sp := spans.Span{ID: id, Start: start.UTC(), End: end.UTC(), Source: source, Inferred: inferred}
		if start.IsZero() || end.IsZero() {
			sp.Missing = true
		}
		nr.Spans[id] = sp
	}

	// 1. node-registered
	kubeletStart := firstNodeEvent(nodeEvents, "Starting")
	regd := firstNodeEvent(nodeEvents, "RegisteredNode")
	set(spans.SpanNodeRegistered, kubeletStart, regd, "node-events", false)

	// 1a. vm-provision: submit (az aks scale invocation) → Node
	// creationTimestamp. This is ARM + VMSS + OS boot + CSE + kubelet
	// registration time — completely outside CNS / DNC-RC. Split out
	// because dnc-rc-create-nnc used to roll this in.
	set(spans.SpanVMProvision, submit, nr.T0, "submit-time+node-creationTimestamp", false)

	// 2. dnc-rc-create-nnc: Node creationTimestamp → CreatedNNC event.
	// Pure DNC-RC reaction time (sees Node watch event, creates NNC CRD,
	// posts the event). Note: Kubernetes events have 1s resolution, so
	// this is a floor, not a precise sub-second number.
	createdNNC := firstNodeEvent(nodeEvents, "CreatedNNC")
	set(spans.SpanDNCRCCreateNNC, nr.T0, createdNNC, "node-creationTimestamp+node-event", false)

	// 3. dnc-rc-create-nc (CreatingNC -> UpdatedNC)
	creatingNC := firstNodeEvent(nodeEvents, "CreatingNC")
	updatedNC := firstNodeEvent(nodeEvents, "UpdatedNC")
	set(spans.SpanDNCRCCreateNC, creatingNC, updatedNC, "node-events", false)

	// 4. nnc-status-written
	if nnc != nil {
		var specTime, statusTime time.Time
		for _, mf := range nnc.ManagedFields {
			if mf.Manager == "dnc-rc" {
				t := mf.Time
				if t == nil {
					continue
				}
				if mf.Subresource == "status" {
					if statusTime.IsZero() || t.Time.Before(statusTime) {
						statusTime = t.Time
					}
				} else if specTime.IsZero() || t.Time.Before(specTime) {
					specTime = t.Time
				}
			}
		}
		if specTime.IsZero() {
			specTime = nnc.CreationTimestamp.Time
		}
		set(spans.SpanNNCStatusWritten, specTime, statusTime, "nnc-managedFields", false)
	} else {
		sp := spans.Span{ID: spans.SpanNNCStatusWritten, Source: "nnc-managedFields", Missing: true}
		nr.Spans[spans.SpanNNCStatusWritten] = sp
	}

	// CNS pod spans (5..16)
	if pod == nil {
		for _, id := range []spans.SpanID{
			spans.SpanCNSPodScheduleLat, spans.SpanCNSInitImagePull, spans.SpanCNSInitContainerRun,
			spans.SpanCNSImagePull, spans.SpanCNSContainerStart, spans.SpanCNSProcessBootstrap,
			spans.SpanCNSNNCIngest, spans.SpanCNSListenerReady, spans.SpanCNSConflistWrite,
			spans.SpanCNSPodReady,
		} {
			nr.Spans[id] = spans.Span{ID: id, Source: "pod-missing", Missing: true}
		}
	} else {
		// 5 cns-pod-schedule-latency: T0 → Pod Scheduled event from
		// kube-scheduler. Static pod mirror pods do NOT emit a Scheduled
		// event because kubelet creates them locally without scheduling.
		// Fall back to (pod.creationTimestamp - node.creationTimestamp)
		// for static pods, marked Inferred.
		scheduled := firstPodEvent(podEvents, "Scheduled")
		if scheduled.IsZero() && !pod.CreationTimestamp.IsZero() {
			set(spans.SpanCNSPodScheduleLat, nr.T0, pod.CreationTimestamp.Time, "pod-creationTimestamp(static-pod-fallback)", true)
		} else {
			set(spans.SpanCNSPodScheduleLat, nr.T0, scheduled, "pod-event", false)
		}

		// 6, 8: image pulls. Match Pulling/Pulled events to specific
		// containers by image name (the event message contains the image
		// reference). This handles the common AKS case where the cns
		// main image is preloaded in the node image: kubelet emits a
		// Pulled event but no Pulling event, so positional pairing
		// would silently lose it. Image-already-present is rendered as
		// a zero-duration span ending at the Pulled event time, not as
		// Missing.
		var initImage, mainImage string
		if len(pod.Spec.InitContainers) > 0 {
			initImage = pod.Spec.InitContainers[0].Image
		}
		for _, c := range pod.Spec.Containers {
			if c.Name == "cns-container" {
				mainImage = c.Image
				break
			}
		}
		if mainImage == "" && len(pod.Spec.Containers) > 0 {
			mainImage = pod.Spec.Containers[0].Image
		}
		initPull := pullSpanForImage(podEvents, initImage)
		mainPull := pullSpanForImage(podEvents, mainImage)
		if initPull.end.IsZero() {
			nr.Spans[spans.SpanCNSInitImagePull] = spans.Span{ID: spans.SpanCNSInitImagePull, Source: "pod-events:image=" + initImage, Missing: true}
		} else {
			set(spans.SpanCNSInitImagePull, initPull.start, initPull.end, "pod-events:image="+initImage, false)
		}
		if mainPull.end.IsZero() {
			nr.Spans[spans.SpanCNSImagePull] = spans.Span{ID: spans.SpanCNSImagePull, Source: "pod-events:image=" + mainImage, Missing: true}
		} else {
			set(spans.SpanCNSImagePull, mainPull.start, mainPull.end, "pod-events:image="+mainImage, false)
		}

		// 7 init container run
		var initStart, initEnd time.Time
		for _, ic := range pod.Status.InitContainerStatuses {
			if term := ic.State.Terminated; term != nil {
				initStart = term.StartedAt.Time
				initEnd = term.FinishedAt.Time
				break
			}
		}
		set(spans.SpanCNSInitContainerRun, initStart, initEnd, "pod-status", false)

		// 7b cns-init-to-main-gap: init container finishedAt → main
		// container Pulled event. This is kubelet's pod-sync pipeline
		// between "init done" and "ensure main image present" — under a
		// fresh-node daemonset stampede this can be 10-15 s of pure
		// scheduler/containerd backpressure even though no work is
		// being attributed to anything. Only meaningful when there IS
		// an init container; when initEnd or mainPull.end is missing
		// we mark it Missing.
		if !initEnd.IsZero() && !mainPull.end.IsZero() && mainPull.end.After(initEnd) {
			set(spans.SpanCNSInitToMainGap, initEnd, mainPull.end, "pod-status+pod-events", false)
		} else {
			nr.Spans[spans.SpanCNSInitToMainGap] = spans.Span{
				ID:      spans.SpanCNSInitToMainGap,
				Source:  "pod-status+pod-events",
				Missing: true,
			}
		}

		// 9 cns-container-start (cns-container Pulled event -> cns startedAt).
		// Use mainPull.end (which we just resolved by image-name match)
		// rather than positional pair indexing, so this works whether the
		// image was actually pulled or "already present".
		var cnsStarted time.Time
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "cns-container" && cs.State.Running != nil {
				cnsStarted = cs.State.Running.StartedAt.Time
			}
		}
		set(spans.SpanCNSContainerStart, mainPull.end, cnsStarted, "pod-events+status:image="+mainImage, false)

		// 10,11,12 from log anchors
		// Split the old cns-process-bootstrap into two phases:
		//   cns-exec-gap         containerStartedAt → first CNS log line
		//                        ("Using config"). Captures kernel/containerd
		//                        not having actually exec'd the binary —
		//                        i.e., the daemonset-stampede gap.
		//   cns-process-bootstrap first CNS log → "Reconciling initial CNS
		//                        state". Real CNS-side work: config read,
		//                        AI/zap init, process lock, controller cache
		//                        sync, first NNC delivered.
		set(spans.SpanCNSExecGap, cnsStarted, anchors.UsingConfig, "cns-log:Using config", false)
		set(spans.SpanCNSProcessBootstrap, anchors.UsingConfig, anchors.ReconcilingInitial, "cns-log", false)
		set(spans.SpanCNSNNCIngest, anchors.RetrievedNNC, anchors.ReconcilingIPAM, "cns-log", false)
		set(spans.SpanCNSListenerReady, cnsStarted, anchors.ListenerStarted, "cns-log", false)

		// 12b cns-sync-host-nc-version — derived from the
		// `sync_host_nc_version_latency_seconds` histogram. CNS calls
		// syncHostNCVersion on a 1s ticker; the FIRST call is the slow path
		// because the local NC HostVersion is initialized to "-1" and DNC
		// publishes "0", so CNS must call NMAgent to confirm v0 before it
		// updates HostVersion and triggers MustGenerateCNIConflistOnce.
		// This is in the critical path for Node Ready in BOTH overlay and
		// Swift modes. Subsequent ticker calls short-circuit when no NCs
		// are outdated, so the histogram sum is dominated by the first call
		// (and any later DNC version bumps for Swift dynamic NCs).
		// We render the cumulative wait as a span anchored at the end of
		// nnc-ingest. Missing if metrics weren't scraped.
		syncSum, syncSeen := readMetric(nr.Metrics, `sync_host_nc_version_latency_seconds_sum{ok="true"}`)
		if !syncSeen {
			syncSum, syncSeen = readMetric(nr.Metrics, "sync_host_nc_version_latency_seconds_sum")
		}
		if syncSeen && !anchors.ReconcilingIPAM.IsZero() {
			start := anchors.ReconcilingIPAM
			end := start.Add(time.Duration(syncSum * float64(time.Second)))
			set(spans.SpanCNSSyncHostNCVersion, start, end, "cns-metric:sync_host_nc_version_latency_seconds_sum", true)
		} else {
			nr.Spans[spans.SpanCNSSyncHostNCVersion] = spans.Span{
				ID: spans.SpanCNSSyncHostNCVersion, Source: "cns-metric:sync_host_nc_version_latency_seconds_sum", Missing: true, Inferred: true,
			}
		}

		// 13 conflist write — prefer CNS log timestamp (sub-second) over DaemonSet mtime (seconds).
		conflistEnd := conflistMtime
		source := "node-annotation"
		if !anchors.ConflistGenerated.IsZero() {
			conflistEnd = anchors.ConflistGenerated
			source = "cns-log"
		}
		set(spans.SpanCNSConflistWrite, cnsStarted, conflistEnd, source, false)

		// 14 cns-pod-ready
		podReady := podConditionTime(pod, corev1.PodReady)
		set(spans.SpanCNSPodReady, nr.T0, podReady, "pod-condition", false)
	}

	// 15 node-ready (the OKR target)
	nodeReady := nodeConditionTime(node, corev1.NodeReady)
	set(spans.SpanNodeReady, nr.T0, nodeReady, "node-condition", false)

	// 16 kubelet-cni-pickup (inferred)
	sp := spans.Span{ID: spans.SpanKubeletCNIPickup, Start: conflistMtime.UTC(), End: nodeReady.UTC(), Source: "inferred(mtime->NodeReady)", Inferred: true}
	if conflistMtime.IsZero() || nodeReady.IsZero() {
		sp.Missing = true
	}
	nr.Spans[spans.SpanKubeletCNIPickup] = sp

	// 17 bootstrap metric extraction. Each cns_*_seconds gauge is
	// the Unix timestamp at which CNS hit a known bootstrap
	// boundary; durations are computed against cns_start_time_seconds
	// to produce process-relative spans. These spans are the
	// authoritative source for the corresponding boundaries when
	// CNS is new enough to emit them; the existing log-based spans
	// remain as a fallback for older CNS images.
	applyBootstrapMetrics(nr, set)
}

// applyBootstrapMetrics extracts bootstrap-phase event timestamps
// from the CNS Prometheus metrics scraped at end-of-run, populates
// nr.BootState and nr.Mode for cross-run filtering, and synthesizes
// or overwrites spans for boundaries that the metrics carry
// authoritatively.
func applyBootstrapMetrics(
	nr *spans.NodeRun,
	set func(id spans.SpanID, start, end time.Time, source string, inferred bool),
) {
	if len(nr.Metrics) == 0 {
		return
	}

	// Boot state is encoded as cns_boot_state{state="X"} = 1 (with
	// other states present at 0). Find the active one.
	for _, state := range []string{"fresh", "reboot", "restart", "unknown"} {
		key := `cns_boot_state{state="` + state + `"}`
		if v, ok := readMetric(nr.Metrics, key); ok && v == 1 {
			nr.BootState = state
			break
		}
	}

	// Mode is encoded similarly via cns_mode_info{...} = 1. There's
	// only ever one mode_info series per process. Walk the map to
	// find the (one) cns_mode_info key set to 1, then extract its
	// label values.
	for key, v := range nr.Metrics {
		if v != 1 || !strings.HasPrefix(key, "cns_mode_info{") {
			continue
		}
		nr.Mode = parseLabelSuffix(strings.TrimPrefix(key, "cns_mode_info"))
		break
	}

	startSeconds, hasStart := readMetric(nr.Metrics, "cns_start_time_seconds")
	if !hasStart {
		return
	}
	startT := time.Unix(0, int64(startSeconds*float64(time.Second))).UTC()

	// Each row: span ID, metric key, source label.
	type metricSpan struct {
		id         spans.SpanID
		metric     string
		sourceTag  string
		overwrite  bool // overwrite any existing span (preferring the metric over a log-derived value)
	}
	rows := []metricSpan{
		{spans.SpanCNSStateRestored, "cns_state_restored_seconds", "cns-metric:cns_state_restored_seconds", false},
		{spans.SpanCNSFirstNNCReceived, "cns_first_nnc_received_seconds", "cns-metric:cns_first_nnc_received_seconds", false},
		{spans.SpanCNSInitialIPAMReconciled, "cns_initial_ipam_reconciled_seconds", "cns-metric:cns_initial_ipam_reconciled_seconds", false},
		{spans.SpanCNSFirstNCProgrammed, "cns_first_nc_programmed_seconds", "cns-metric:cns_first_nc_programmed_seconds", false},
		{spans.SpanCNSReadyToAssign, "cns_ready_to_assign_seconds", "cns-metric:cns_ready_to_assign_seconds", false},
		// Listener-ready and conflist-write also exist as
		// log-derived spans; the metric is sub-second precise and
		// fires exactly once at the correct boundary, so it
		// supersedes the log fallback when both are present.
		{spans.SpanCNSListenerReady, "cns_http_listener_ready_seconds", "cns-metric:cns_http_listener_ready_seconds", true},
		{spans.SpanCNSConflistWrite, "cns_conflist_written_seconds", "cns-metric:cns_conflist_written_seconds", true},
	}

	for _, row := range rows {
		v, ok := readMetric(nr.Metrics, row.metric)
		if !ok || v == 0 {
			// Gauge is 0 when the event hasn't fired yet (or this
			// CNS is too old to emit the metric). Don't overwrite
			// existing log-derived spans with an empty signal.
			continue
		}
		eventT := time.Unix(0, int64(v*float64(time.Second))).UTC()
		// Skip overwriting an already-set span unless explicit.
		if existing, ok := nr.Spans[row.id]; ok && !row.overwrite && !existing.Missing {
			continue
		}
		set(row.id, startT, eventT, row.sourceTag, false)
	}
}

// parseLabelSuffix turns `{a="x",b="y"}` into {"a":"x","b":"y"}.
// Tolerates an empty suffix.
func parseLabelSuffix(s string) map[string]string {
	out := map[string]string{}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		return out
	}
	for _, pair := range strings.Split(s, ",") {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.Trim(strings.TrimSpace(pair[eq+1:]), `"`)
		out[k] = v
	}
	return out
}

type pullPair struct {
	pullingAt time.Time
	pulledAt  time.Time
}

// pullSpan describes one image's pull lifecycle as observed via pod events.
// If the image was already present on the node, only end is populated and
// start == end (zero duration).
type pullSpan struct {
	start time.Time
	end   time.Time
}

// pullSpanForImage matches Pulling/Pulled events to a specific image by
// looking for the image string inside the event message. This handles
// both the "actually pulled" case (Pulling at start, Pulled at end) and
// the "already present on node" case (only Pulled, no Pulling), which
// kubelet emits with a different message format. For an already-present
// image we report start == end at the Pulled event time so the span
// renders as a zero-duration marker rather than as Missing.
func pullSpanForImage(events []corev1.Event, image string) pullSpan {
	if image == "" {
		return pullSpan{}
	}
	var ps pullSpan
	for _, e := range sortEvents(events) {
		if e.Reason != "Pulling" && e.Reason != "Pulled" {
			continue
		}
		if !strings.Contains(e.Message, image) {
			continue
		}
		t := eventTime(e)
		switch e.Reason {
		case "Pulling":
			if ps.start.IsZero() {
				ps.start = t
			}
		case "Pulled":
			if ps.end.IsZero() {
				ps.end = t
			}
		}
	}
	if !ps.end.IsZero() && ps.start.IsZero() {
		// "Container image already present on machine" — zero-duration.
		ps.start = ps.end
	}
	return ps
}

func sortEvents(events []corev1.Event) []corev1.Event {
	cp := make([]corev1.Event, len(events))
	copy(cp, events)
	// Simple insertion sort by effective event time; N is small.
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && eventTime(cp[j]).Before(eventTime(cp[j-1])); j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	return cp
}

func eventTime(e corev1.Event) time.Time {
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	return e.LastTimestamp.Time
}

func firstNodeEvent(events []corev1.Event, reason string) time.Time {
	return firstEventWithReason(events, reason)
}

func firstPodEvent(events []corev1.Event, reason string) time.Time {
	return firstEventWithReason(events, reason)
}

func firstEventWithReason(events []corev1.Event, reason string) time.Time {
	var best time.Time
	for _, e := range events {
		if e.Reason != reason {
			continue
		}
		t := eventTime(e)
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	return best
}

func nodeConditionTime(n *corev1.Node, ct corev1.NodeConditionType) time.Time {
	for _, c := range n.Status.Conditions {
		if c.Type == ct && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// readMetric returns (value, ok) for an exact metric series name as flattened
// by cnsmetrics.Scrape. Returns false if metrics map is nil or the key is
// absent.
func readMetric(m map[string]float64, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	return v, ok
}

func podConditionTime(p *corev1.Pod, ct corev1.PodConditionType) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == ct && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}
