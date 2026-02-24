package validate

import (
	"context"
	"encoding/json"
	"log"
	"reflect"
	"sort"

	acnk8s "github.com/Azure/azure-container-networking/test/internal/kubernetes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	ciliumBinary         = "cilium"
	reservedIngressLabel = "reserved:ingress"
)

type ipComparisonResult struct {
	ExpectedCount int      `json:"expectedCount"`
	ActualCount   int      `json:"actualCount"`
	MissingIPs    []string `json:"missingIPs,omitempty"`
	UnexpectedIPs []string `json:"unexpectedIPs,omitempty"`
	DuplicateIPs  []string `json:"duplicateIPs,omitempty"`
}

func (r ipComparisonResult) HasMismatch() bool {
	return len(r.MissingIPs) > 0 || len(r.UnexpectedIPs) > 0 || len(r.DuplicateIPs) > 0
}

func compareIPsDetailed(expected map[string]string, actual []string) ipComparisonResult {
	result := ipComparisonResult{
		ExpectedCount: len(expected),
		ActualCount:   len(actual),
	}

	seen := make(map[string]struct{}, len(actual))
	for _, ip := range actual {
		if _, found := seen[ip]; found {
			result.DuplicateIPs = append(result.DuplicateIPs, ip)
			continue
		}
		seen[ip] = struct{}{}

		if _, ok := expected[ip]; !ok {
			result.UnexpectedIPs = append(result.UnexpectedIPs, ip)
		}
	}

	for ip := range expected {
		if _, ok := seen[ip]; !ok {
			result.MissingIPs = append(result.MissingIPs, ip)
		}
	}

	sort.Strings(result.MissingIPs)
	sort.Strings(result.UnexpectedIPs)
	sort.Strings(result.DuplicateIPs)
	return result
}

// func to get the pods ip without the node ip (ie. host network as false)
func getPodIPsWithoutNodeIP(ctx context.Context, clientset *kubernetes.Clientset, node corev1.Node) []string {
	podsIpsWithoutNodeIP := []string{}
	podIPs, err := acnk8s.GetPodsIpsByNode(ctx, clientset, "", "", node.Name)
	if err != nil {
		return podsIpsWithoutNodeIP
	}
	nodeIPs := make([]string, 0)
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			nodeIPs = append(nodeIPs, address.Address)
		}
	}

	for _, podIP := range podIPs {
		if !contain(podIP, nodeIPs) {
			podsIpsWithoutNodeIP = append(podsIpsWithoutNodeIP, podIP)
		}
	}
	return podsIpsWithoutNodeIP
}

// hasL7PolicyEnabled checks if L7 policy is enabled on the given node by looking
// for a cilium pod and an acns-security-agent pod with a cilium-envoy container.
func hasL7PolicyEnabled(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) bool {
	pods, err := acnk8s.GetPodsByNode(ctx, clientset, "kube-system", "k8s-app=acns-security-agent", nodeName)
	if err != nil || len(pods.Items) == 0 {
		return false
	}
	for i := range pods.Items[0].Spec.Containers {
		if pods.Items[0].Spec.Containers[i].Name == "cilium-envoy" {
			return true
		}
	}
	return false
}

// getCiliumInternalEndpointIPs execs into the cilium agent pod on the given node
// and runs `cilium endpoint list -o json` to find IPs of reserved:ingress endpoints.
// These are not real Kubernetes pods but still have IPs allocated from CNS.
// Returns nil when Cilium is not installed or the exec fails.
// Uses the cilium binary directly (no shell) so it works on distroless containers.
func getCiliumInternalEndpointIPs(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, nodeName string) []string {
	pods, err := acnk8s.GetPodsByNode(ctx, clientset, "kube-system", "k8s-app=cilium", nodeName)
	if err != nil || len(pods.Items) == 0 {
		return nil
	}

	cmd := []string{ciliumBinary, "endpoint", "list", "-o", "json"}
	result, _, err := acnk8s.ExecCmdOnPod(ctx, clientset, "kube-system", pods.Items[0].Name, "cilium-agent", cmd, config, false)
	if err != nil {
		return nil
	}

	return parseCiliumIngressIPs(result)
}

// parseCiliumIngressIPs parses the JSON output of `cilium endpoint list -o json`
// and extracts IPs from endpoints with the reserved:ingress label.
func parseCiliumIngressIPs(output []byte) []string {
	var endpoints []CiliumEndpointStatus
	if err := json.Unmarshal(output, &endpoints); err != nil {
		log.Printf("Failed to parse cilium endpoint list JSON: %v", err)
		return nil
	}

	var ips []string
	for _, ep := range endpoints {
		isIngress := false
		for _, label := range ep.Status.Labels.SecurityRelevant {
			if label == reservedIngressLabel {
				isIngress = true
				break
			}
		}
		if !isIngress {
			continue
		}
		for _, addr := range ep.Status.Networking.Addresses {
			if addr.IPv4 != "" {
				ips = append(ips, addr.IPv4)
			}
			if addr.IPv6 != "" {
				ips = append(ips, addr.IPv6)
			}
		}
	}

	if len(ips) > 0 {
		log.Printf("Parsed Cilium internal endpoint IPs: %v", ips)
	} else {
		log.Printf("No Cilium internal endpoint IPs found")
	}
	return ips
}

func contain(obj, target interface{}) bool {
	targetValue := reflect.ValueOf(target)
	switch reflect.TypeOf(target).Kind() { //nolint
	case reflect.Slice, reflect.Array:
		for i := 0; i < targetValue.Len(); i++ {
			if targetValue.Index(i).Interface() == obj {
				return true
			}
		}
	}
	return false
}
