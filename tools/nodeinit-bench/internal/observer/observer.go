// Package observer watches a cluster during a nodeinit-bench run and collects
// per-Node span timestamps from the Kubernetes API and from CNS pod logs.
package observer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	nncv1alpha "github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/cnslogs"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/cnsmetrics"
	"github.com/Azure/azure-container-networking/tools/nodeinit-bench/internal/spans"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

const (
	cnsNamespace      = "kube-system"
	cnsLabelSelector  = "k8s-app=azure-cns"
	conflistMtimeAnno = "nodeinit-bench/cni-conflist-mtime"
)

// Observer holds clients used for the duration of a run.
type Observer struct {
	kube   *kubernetes.Clientset
	rest   *rest.Config
	scheme *runtime.Scheme
}

// New initializes an Observer from a kubeconfig-derived rest.Config.
func New(restCfgAny any) (*Observer, error) {
	cfg, ok := restCfgAny.(*rest.Config)
	if !ok {
		return nil, fmt.Errorf("observer.New: expected *rest.Config, got %T", restCfgAny)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = nncv1alpha.AddToScheme(sch)
	return &Observer{kube: kc, rest: cfg, scheme: sch}, nil
}

// RunOne invokes trigger to create nodes, then polls the cluster until the
// target nodepool count is reached and all new Nodes are Ready (or timeout),
// and returns one NodeRun per observed new Node.
func RunOne(
	ctx context.Context,
	obs *Observer,
	scraper *cnslogs.Scraper,
	trigger func(context.Context) error,
	baseline, target int,
	scrapeMetrics bool,
) ([]spans.NodeRun, error) {
	// Record baseline node set before triggering.
	baselineSet, err := obs.listNodeNames(ctx)
	if err != nil {
		return nil, err
	}

	submitTime := time.Now().UTC()
	if err := trigger(ctx); err != nil {
		return nil, err
	}
	fmt.Printf("trigger submitted at %s (baseline=%d target=%d)\n",
		submitTime.Format(time.RFC3339), baseline, target)

	// Poll for new nodes to become Ready.
	newNodes, err := obs.waitForNewNodes(ctx, baselineSet, target-baseline)
	if err != nil {
		return nil, err
	}

	runs := make([]spans.NodeRun, 0, len(newNodes))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, n := range newNodes {
		wg.Add(1)
		go func(nodeName string) {
			defer wg.Done()
			nr, cerr := obs.collectNode(ctx, nodeName, submitTime, scraper, scrapeMetrics)
			if cerr != nil {
				fmt.Printf("collect %s: %v\n", nodeName, cerr)
				return
			}
			mu.Lock()
			runs = append(runs, nr)
			mu.Unlock()
		}(n)
	}
	wg.Wait()

	sort.Slice(runs, func(i, j int) bool { return runs[i].Node < runs[j].Node })
	return runs, nil
}

func (o *Observer) listNodeNames(ctx context.Context) (map[string]struct{}, error) {
	nl, err := o.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make(map[string]struct{}, len(nl.Items))
	for _, n := range nl.Items {
		out[n.Name] = struct{}{}
	}
	return out, nil
}

func (o *Observer) waitForNewNodes(ctx context.Context, baseline map[string]struct{}, need int) ([]string, error) {
	seen := make(map[string]bool)
	var ready []string
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(20 * time.Minute)
	}
	for {
		select {
		case <-ctx.Done():
			return ready, ctx.Err()
		case <-tick.C:
		}
		nl, err := o.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Printf("list nodes: %v\n", err)
			continue
		}
		for _, n := range nl.Items {
			if _, was := baseline[n.Name]; was {
				continue
			}
			if seen[n.Name] {
				continue
			}
			if isNodeReady(&n) {
				seen[n.Name] = true
				ready = append(ready, n.Name)
				fmt.Printf("observed new Ready node: %s\n", n.Name)
			}
		}
		if len(ready) >= need {
			return ready, nil
		}
		if time.Now().After(deadline) {
			return ready, fmt.Errorf("timeout waiting for %d new Ready nodes (got %d)", need, len(ready))
		}
	}
}

func isNodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// collectNode gathers all artifacts and builds a NodeRun.
func (o *Observer) collectNode(
	ctx context.Context,
	nodeName string,
	submit time.Time,
	scraper *cnslogs.Scraper,
	scrapeMetrics bool,
) (spans.NodeRun, error) {
	nr := spans.NodeRun{
		Node:  nodeName,
		Spans: make(map[spans.SpanID]spans.Span, len(spans.OrderedSpans)),
	}

	node, err := o.kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nr, fmt.Errorf("get node: %w", err)
	}
	nr.T0 = node.CreationTimestamp.Time.UTC()
	nr.NodeUID = string(node.UID)

	pod, err := o.findCNSPod(ctx, nodeName)
	if err != nil {
		fmt.Printf("find cns pod on %s: %v\n", nodeName, err)
	} else {
		nr.PodName = pod.Name
		// Wait up to 2 minutes for PodReady so we don't miss the transition.
		if p, werr := o.waitForPodReady(ctx, pod.Namespace, pod.Name, 2*time.Minute); werr == nil {
			pod = p
		} else {
			fmt.Printf("wait pod ready %s: %v\n", pod.Name, werr)
		}
	}

	nodeEvents := o.listEventsForNodeAndNNC(ctx, nodeName)
	var podEvents []corev1.Event
	if pod != nil {
		podEvents, _ = o.listEventsForObject(ctx, pod.Name, "Pod")
	}

	nnc, nncErr := o.getNNC(ctx, nodeName)
	if nncErr != nil {
		fmt.Printf("get nnc %s: %v\n", nodeName, nncErr)
	}

	var logAnchors cnslogs.Anchors
	if pod != nil {
		logAnchors, err = scraper.Collect(ctx, cnsNamespace, pod.Name, "cns-container")
		if err != nil {
			fmt.Printf("cns logs %s: %v\n", pod.Name, err)
		}
	}

	if scrapeMetrics && pod != nil {
		m, merr := cnsmetrics.Scrape(ctx, o.rest, pod.Namespace, pod.Name)
		if merr != nil {
			fmt.Printf("cns metrics %s: %v (skipping)\n", pod.Name, merr)
		} else if len(m) > 0 {
			nr.Metrics = m
		}
	}

	conflistMtime := parseConflistMtime(node)

	buildSpans(&nr, node, pod, nodeEvents, podEvents, nnc, logAnchors, submit, conflistMtime)
	return nr, nil
}

func (o *Observer) waitForPodReady(ctx context.Context, ns, name string, within time.Duration) (*corev1.Pod, error) {
	deadline := time.Now().Add(within)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		p, err := o.kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return p, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return p, fmt.Errorf("timeout waiting for PodReady=True")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func (o *Observer) findCNSPod(ctx context.Context, nodeName string) (*corev1.Pod, error) {
	pods, err := o.kube.CoreV1().Pods(cnsNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: cnsLabelSelector,
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no CNS pod on node %s", nodeName)
	}
	return &pods.Items[0], nil
}

func (o *Observer) listEventsForObject(ctx context.Context, name, kind string) ([]corev1.Event, error) {
	ns := metav1.NamespaceAll
	sel := strings.Join([]string{
		"involvedObject.name=" + name,
		"involvedObject.kind=" + kind,
	}, ",")
	el, err := o.kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	return el.Items, nil
}

// listEventsForNodeAndNNC returns the union of events on the Node and on its
// NodeNetworkConfig (which shares the Node's name). DNC-RC posts CreatedNNC
// on the Node but CreatingNC/UpdatedNC on the NNC, so we need both.
func (o *Observer) listEventsForNodeAndNNC(ctx context.Context, nodeName string) []corev1.Event {
	var out []corev1.Event
	if ev, err := o.listEventsForObject(ctx, nodeName, "Node"); err == nil {
		out = append(out, ev...)
	}
	if ev, err := o.listEventsForObject(ctx, nodeName, "NodeNetworkConfig"); err == nil {
		out = append(out, ev...)
	}
	return out
}

func (o *Observer) getNNC(ctx context.Context, nodeName string) (*nncv1alpha.NodeNetworkConfig, error) {
	// Use a dynamic-free approach: raw REST via RESTClient on the group.
	gvr := "/apis/acn.azure.com/v1alpha/namespaces/kube-system/nodenetworkconfigs/" + nodeName
	raw, err := o.kube.RESTClient().Get().AbsPath(gvr).DoRaw(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("nnc not found for %s", nodeName)
		}
		return nil, err
	}
	nnc := &nncv1alpha.NodeNetworkConfig{}
	if err := nncFromJSON(raw, nnc); err != nil {
		return nil, err
	}
	return nnc, nil
}

func parseConflistMtime(n *corev1.Node) time.Time {
	if v, ok := n.Annotations[conflistMtimeAnno]; ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
