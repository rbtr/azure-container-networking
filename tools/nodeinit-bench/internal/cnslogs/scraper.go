// Package cnslogs fetches CNS stdout logs for a pod and extracts anchor
// timestamps used to build Gantt spans.
package cnslogs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Anchors is the set of timestamped log lines of interest.
type Anchors struct {
	UsingConfig        time.Time
	ReconcilingInitial time.Time
	RetrievedNNC       time.Time
	ReconcilingIPAM    time.Time
	NNCReconcilerStart time.Time
	ListenerStarted    time.Time
	ConflistGenerated  time.Time
}

// Scraper fetches pod logs via client-go.
type Scraper struct {
	kube *kubernetes.Clientset
}

// New builds a log Scraper from a rest.Config-shaped any (kept as any so the
// CLI layer can avoid importing rest itself).
func New(restCfgAny any) *Scraper {
	cfg, ok := restCfgAny.(*rest.Config)
	if !ok {
		return &Scraper{}
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return &Scraper{}
	}
	return &Scraper{kube: kc}
}

// Collect streams pod logs (with timestamps) for the given container and
// extracts anchor timestamps via regex matches against the entire line.
func (s *Scraper) Collect(ctx context.Context, namespace, pod, container string) (Anchors, error) {
	var out Anchors
	if s.kube == nil {
		return out, fmt.Errorf("scraper not initialized")
	}
	req := s.kube.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  container,
		Timestamps: true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return out, err
	}
	defer stream.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(stream); err != nil {
		return out, err
	}

	scan := bufio.NewScanner(&buf)
	scan.Buffer(make([]byte, 1<<20), 1<<20)
	for scan.Scan() {
		line := scan.Text()
		ts, rest, ok := splitKubectlTimestamp(line)
		if !ok {
			continue
		}
		switch {
		case out.UsingConfig.IsZero() && anchorUsingConfig.MatchString(rest):
			out.UsingConfig = ts
		case out.ReconcilingInitial.IsZero() && anchorReconcilingInitial.MatchString(rest):
			out.ReconcilingInitial = ts
		case out.RetrievedNNC.IsZero() && anchorRetrievedNNC.MatchString(rest):
			out.RetrievedNNC = ts
		case out.ReconcilingIPAM.IsZero() && anchorReconcilingIPAM.MatchString(rest):
			out.ReconcilingIPAM = ts
		case out.NNCReconcilerStart.IsZero() && anchorNNCReconcilerStart.MatchString(rest):
			out.NNCReconcilerStart = ts
		case out.ListenerStarted.IsZero() && anchorListenerStarted.MatchString(rest):
			out.ListenerStarted = ts
		case out.ConflistGenerated.IsZero() && anchorConflistGenerated.MatchString(rest):
			out.ConflistGenerated = ts
		}
	}
	return out, scan.Err()
}

var (
	anchorUsingConfig        = regexp.MustCompile(`\[Azure CNS\] Using config`)
	anchorReconcilingInitial = regexp.MustCompile(`Reconciling initial CNS state`)
	anchorRetrievedNNC       = regexp.MustCompile(`Retrieved NNC:`)
	anchorReconcilingIPAM    = regexp.MustCompile(`Reconciling CNS IPAM state`)
	anchorNNCReconcilerStart = regexp.MustCompile(`\[cns-rc\] CNS NNC Reconciler Started`)
	anchorListenerStarted    = regexp.MustCompile(`\[Listener\] Started listening`)
	anchorConflistGenerated  = regexp.MustCompile(`\[Azure CNS\] CNI conflist generated`)
)

// splitKubectlTimestamp parses the leading RFC3339Nano timestamp injected by
// the Kubernetes log API when Timestamps=true.
func splitKubectlTimestamp(line string) (time.Time, string, bool) {
	for i, r := range line {
		if r == ' ' {
			t, err := time.Parse(time.RFC3339Nano, line[:i])
			if err != nil {
				return time.Time{}, "", false
			}
			return t.UTC(), line[i+1:], true
		}
	}
	return time.Time{}, "", false
}
