package observer

import (
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

	// 2. dnc-rc-create-nnc
	createdNNC := firstNodeEvent(nodeEvents, "CreatedNNC")
	set(spans.SpanDNCRCCreateNNC, submit, createdNNC, "submit-time+node-event", false)

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
		// 5
		scheduled := firstPodEvent(podEvents, "Scheduled")
		set(spans.SpanCNSPodScheduleLat, nr.T0, scheduled, "pod-event", false)

		// 6, 8: pulls. We expect two pairs of Pulling/Pulled (init + main).
		pulls := findPullPairs(podEvents)
		if len(pulls) >= 1 {
			set(spans.SpanCNSInitImagePull, pulls[0].pullingAt, pulls[0].pulledAt, "pod-events", false)
		} else {
			nr.Spans[spans.SpanCNSInitImagePull] = spans.Span{ID: spans.SpanCNSInitImagePull, Source: "pod-events", Missing: true}
		}
		if len(pulls) >= 2 {
			set(spans.SpanCNSImagePull, pulls[1].pullingAt, pulls[1].pulledAt, "pod-events", false)
		} else {
			nr.Spans[spans.SpanCNSImagePull] = spans.Span{ID: spans.SpanCNSImagePull, Source: "pod-events", Missing: true}
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

		// 9 cns-container-start (second Pulled -> cns startedAt)
		var cnsStarted time.Time
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "cns-container" && cs.State.Running != nil {
				cnsStarted = cs.State.Running.StartedAt.Time
			}
		}
		var cnsPulledAt time.Time
		if len(pulls) >= 2 {
			cnsPulledAt = pulls[1].pulledAt
		}
		set(spans.SpanCNSContainerStart, cnsPulledAt, cnsStarted, "pod-events+status", false)

		// 10,11,12 from log anchors
		set(spans.SpanCNSProcessBootstrap, cnsStarted, anchors.ReconcilingInitial, "cns-log", false)
		set(spans.SpanCNSNNCIngest, anchors.RetrievedNNC, anchors.ReconcilingIPAM, "cns-log", false)
		set(spans.SpanCNSListenerReady, cnsStarted, anchors.ListenerStarted, "cns-log", false)

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
}

type pullPair struct {
	pullingAt time.Time
	pulledAt  time.Time
}

func findPullPairs(events []corev1.Event) []pullPair {
	var out []pullPair
	sorted := sortEvents(events)
	var cur pullPair
	for _, e := range sorted {
		switch e.Reason {
		case "Pulling":
			if !cur.pullingAt.IsZero() && cur.pulledAt.IsZero() {
				out = append(out, cur)
			}
			cur = pullPair{pullingAt: eventTime(e)}
		case "Pulled":
			if cur.pulledAt.IsZero() {
				cur.pulledAt = eventTime(e)
				out = append(out, cur)
				cur = pullPair{}
			}
		}
	}
	if !cur.pullingAt.IsZero() && cur.pulledAt.IsZero() {
		out = append(out, cur)
	}
	return out
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

func podConditionTime(p *corev1.Pod, ct corev1.PodConditionType) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == ct && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}
