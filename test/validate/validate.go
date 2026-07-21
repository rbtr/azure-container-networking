package validate

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	acnk8s "github.com/Azure/azure-container-networking/test/internal/kubernetes"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var privilegedDaemonSetPathMap = map[string]string{
	"windows": "../manifests/load/privileged-daemonset-windows.yaml",
	"linux":   "../manifests/load/privileged-daemonset.yaml",
}

var nodeSelectorMap = map[string]string{
	"windows": "kubernetes.io/os=windows",
	"linux":   "kubernetes.io/os=linux",
}

// IPv4 overlay Linux and windows nodes must have this label
var v4OverlayNodeLabels = map[string]string{
	"kubernetes.azure.com/podnetwork-type": "overlay",
}

// dualstack overlay Linux and windows nodes must have these labels
var dualstackOverlayNodeLabels = map[string]string{
	"kubernetes.azure.com/podnetwork-type":   "overlay",
	"kubernetes.azure.com/podv6network-type": "overlay",
}

const (
	privilegedLabelSelector  = "app=privileged-daemonset"
	privilegedNamespace      = "kube-system"
	IPv4ExpectedIPCount      = 1
	DualstackExpectedIPCount = 2
)

type Validator struct {
	clientset   *kubernetes.Clientset
	config      *rest.Config
	checks      []check
	namespace   string
	cni         string
	restartCase bool
	os          string
	summary     ValidationSummary
}

type ValidationSummary struct {
	GeneratedAt string                 `json:"generatedAt"`
	OS          string                 `json:"os"`
	CNI         string                 `json:"cni"`
	Namespace   string                 `json:"namespace"`
	RestartCase bool                   `json:"restartCase"`
	Checks      []ValidationCheckEntry `json:"checks,omitempty"`
}

type ValidationCheckEntry struct {
	CheckName       string   `json:"checkName"`
	NodeName        string   `json:"nodeName"`
	ExpectedCount   int      `json:"expectedCount"`
	ActualCount     int      `json:"actualCount"`
	Attempts        int      `json:"attempts"`
	DurationMS      int64    `json:"durationMS"`
	Converged       bool     `json:"converged"`
	MissingIPs      []string `json:"missingIPs,omitempty"`
	UnexpectedIPs   []string `json:"unexpectedIPs,omitempty"`
	DuplicateIPs    []string `json:"duplicateIPs,omitempty"`
	ValidationPass  bool     `json:"validationPass"`
	StateBackend    string   `json:"stateBackend,omitempty"`
	DBFilePresent   *bool    `json:"dbFilePresent,omitempty"`
	DBFileSizeBytes *int64   `json:"dbFileSizeBytes,omitempty"`
	Authority       string   `json:"authority,omitempty"`
	SchemaVersion   uint32   `json:"schemaVersion,omitempty"`
	Generation      uint64   `json:"generation,omitempty"`
	BootID          string   `json:"bootID,omitempty"`
	EndpointCount   int      `json:"endpointCount,omitempty"`
	AssignmentCount int      `json:"assignmentCount,omitempty"`
	OwnerCount      int      `json:"ownerCount,omitempty"`
	TombstoneCount  int      `json:"tombstoneCount,omitempty"`
}

type check struct {
	name             string
	stateFileIPs     func([]byte) (map[string]string, error)
	podLabelSelector string
	podNamespace     string
	containerName    string
	cmd              []string
	cnsManagedState  bool
	metadataOnly     bool
	persistentState  bool
}

func CreateValidator(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, namespace, cni string, restartCase bool, osName string) (*Validator, error) {
	// deploy privileged pod
	privilegedDaemonSet := acnk8s.MustParseDaemonSet(privilegedDaemonSetPathMap[osName])
	daemonsetClient := clientset.AppsV1().DaemonSets(privilegedNamespace)
	acnk8s.MustCreateDaemonset(ctx, daemonsetClient, privilegedDaemonSet)

	// Ensures that pods have been replaced if test is re-run after failure
	if err := acnk8s.WaitForPodDaemonset(ctx, clientset, privilegedNamespace, privilegedDaemonSet.Name, privilegedLabelSelector); err != nil {
		return nil, errors.Wrap(err, "unable to wait for daemonset")
	}

	var checks []check
	switch osName {
	case "windows":
		checks = windowsChecksMap[cni]
		err := acnk8s.RestartKubeProxyService(ctx, clientset, privilegedNamespace, privilegedLabelSelector, config)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to restart kubeproxy")
		}
	case "linux":
		checks = linuxChecksMap[cni]
	default:
		return nil, errors.Errorf("unsupported os: %s", osName)
	}
	checks, err := configureStateChecks(checks, osName, os.Getenv("VALIDATE_STATE_BACKEND"))
	if err != nil {
		return nil, err
	}

	return &Validator{
		clientset:   clientset,
		config:      config,
		namespace:   namespace,
		cni:         cni,
		restartCase: restartCase,
		checks:      checks,
		os:          osName,
		summary: ValidationSummary{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			OS:          osName,
			CNI:         cni,
			Namespace:   namespace,
			RestartCase: restartCase,
		},
	}, nil
}

func (v *Validator) Validate(ctx context.Context) error {
	defer v.writeSummaryIfRequested()

	log.Printf("Validating State File")
	err := v.ValidateStateFile(ctx)
	if err != nil {
		return errors.Wrapf(err, "failed to validate state file")
	}

	if v.os == "linux" {
		// We are restarting the systmemd network and checking that the connectivity works after the restart. For more details: https://github.com/cilium/cilium/issues/18706
		log.Printf("Validating the restart network scenario")
		err = v.validateRestartNetwork(ctx)
		if err != nil {
			return errors.Wrapf(err, "failed to validate restart network scenario")
		}
	}
	return nil
}

func (v *Validator) ValidateStateFile(ctx context.Context) error {
	for _, check := range v.checks {
		err := v.validateIPs(ctx, check)
		if err != nil {
			return err
		}
	}
	return nil
}

func (v *Validator) validateIPs(ctx context.Context, stateCheck check) error {
	checkType := stateCheck.name
	namespace := stateCheck.podNamespace
	labelSelector := stateCheck.podLabelSelector
	containerName := stateCheck.containerName
	cmd := stateCheck.cmd
	log.Printf("Validating %s state file for %s on %s", checkType, v.cni, v.os)
	nodes, err := acnk8s.GetNodeListByLabelSelector(ctx, v.clientset, nodeSelectorMap[v.os])
	if err != nil {
		return errors.Wrapf(err, "failed to get node list")
	}

	maxAttempts := envInt("VALIDATE_CONVERGENCE_ATTEMPTS", 1)
	retryInterval := time.Duration(envInt("VALIDATE_CONVERGENCE_INTERVAL_SECONDS", 0)) * time.Second

	for index := range nodes.Items {
		nodeName := nodes.Items[index].Name
		started := time.Now()
		var comparison ipComparisonResult
		var persistentDetails persistentStateDetails
		attempts, converged, validationErr := runValidationAttempts(ctx, maxAttempts, retryInterval, func() (bool, error) {
			comparison = ipComparisonResult{}
			persistentDetails = persistentStateDetails{}

			pod, err := acnk8s.GetPodsByNode(ctx, v.clientset, namespace, labelSelector, nodeName)
			if err != nil {
				return false, errors.Wrap(err, "failed to get privileged pod")
			}
			if len(pod.Items) == 0 {
				return false, errors.Errorf("there are no privileged pods on node - %v", nodeName)
			}
			podName := pod.Items[0].Name

			log.Printf("Executing command %s on pod %s, container %s", cmd, podName, containerName)
			result, _, err := acnk8s.ExecCmdOnPod(ctx, v.clientset, namespace, podName, containerName, cmd, v.config, true)
			if err != nil {
				return false, errors.Wrapf(err, "failed to exec into privileged pod - %s", podName)
			}

			filePodIps, err := stateCheck.stateFileIPs(result)
			if err != nil {
				return false, errors.Wrapf(err, "failed to get pod ips from state file on node %v", nodeName)
			}
			if stateCheck.persistentState {
				details, inspectErr := inspectPersistentState(result)
				persistentDetails = details
				if inspectErr != nil {
					return false, errors.Wrapf(
						inspectErr,
						"failed to validate persistent state on node %v",
						nodeName,
					)
				}
			}
			if stateCheck.metadataOnly {
				return true, nil
			}
			podIps := getPodIPsWithoutNodeIP(ctx, v.clientset, nodes.Items[index])
			// include IPs from Cilium internal endpoints (reserved:ingress) that are not real K8s pods.
			// These only exist when L7 policy is enabled, indicated by the acns-security-agent pod with cilium-envoy container.
			if hasL7PolicyEnabled(ctx, v.clientset, nodeName) {
				podIps = append(podIps, getCiliumInternalEndpointIPs(ctx, v.clientset, v.config, nodeName)...)
			}
			comparison = compareIPsDetailed(filePodIps, podIps)
			return !comparison.HasMismatch(), nil
		})

		var dbFilePresent *bool
		var dbFileSizeBytes *int64
		if stateCheck.persistentState {
			present := persistentDetails.DBFilePresent
			sizeBytes := persistentDetails.DBFileSizeBytes
			dbFilePresent = &present
			dbFileSizeBytes = &sizeBytes
		}
		v.summary.Checks = append(v.summary.Checks, ValidationCheckEntry{
			CheckName:       checkType,
			NodeName:        nodeName,
			ExpectedCount:   comparison.ExpectedCount,
			ActualCount:     comparison.ActualCount,
			Attempts:        attempts,
			DurationMS:      time.Since(started).Milliseconds(),
			Converged:       converged,
			MissingIPs:      comparison.MissingIPs,
			UnexpectedIPs:   comparison.UnexpectedIPs,
			DuplicateIPs:    comparison.DuplicateIPs,
			ValidationPass:  validationErr == nil && converged && !comparison.HasMismatch(),
			StateBackend:    persistentDetails.Backend,
			DBFilePresent:   dbFilePresent,
			DBFileSizeBytes: dbFileSizeBytes,
			Authority:       persistentDetails.Authority,
			SchemaVersion:   persistentDetails.SchemaVersion,
			Generation:      persistentDetails.Generation,
			BootID:          persistentDetails.BootID,
			EndpointCount:   persistentDetails.EndpointCount,
			AssignmentCount: persistentDetails.AssignmentCount,
			OwnerCount:      persistentDetails.OwnerCount,
			TombstoneCount:  persistentDetails.TombstoneCount,
		})

		if validationErr != nil {
			return validationErr
		}
		if !converged || comparison.HasMismatch() {
			return errors.Errorf(
				"State file validation failed for %s on node %s after %d/%d attempts: expected=%d actual=%d missing=%v unexpected=%v duplicate=%v",
				checkType,
				nodeName,
				attempts,
				maxAttempts,
				comparison.ExpectedCount,
				comparison.ActualCount,
				comparison.MissingIPs,
				comparison.UnexpectedIPs,
				comparison.DuplicateIPs,
			)
		}
	}
	log.Printf("State file validation for %s passed", checkType)
	return nil
}

func runValidationAttempts(
	ctx context.Context,
	maxAttempts int,
	interval time.Duration,
	validate func() (bool, error),
) (int, bool, error) {
	if maxAttempts < 1 {
		return 0, false, errors.New("validation attempts must be positive")
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		converged, err := validate()
		if converged {
			if err != nil {
				return attempt, false, err
			}
			return attempt, true, nil
		}
		lastErr = err

		retry, retryErr := waitForValidationRetry(ctx, attempt, maxAttempts, interval)
		if retryErr != nil {
			return attempt, false, errors.Wrap(retryErr, "waiting to retry state validation")
		}
		if !retry {
			return attempt, false, lastErr
		}
	}
}

func waitForValidationRetry(ctx context.Context, attempt, maxAttempts int, interval time.Duration) (bool, error) {
	if attempt >= maxAttempts {
		return false, nil
	}
	if interval <= 0 {
		return true, nil
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("invalid %s=%q, using fallback %d", name, raw, fallback)
		return fallback
	}
	if v < 0 {
		log.Printf("invalid %s=%q (<0), using fallback %d", name, raw, fallback)
		return fallback
	}

	return v
}

func (v *Validator) writeSummaryIfRequested() {
	summaryPath := os.Getenv("VALIDATE_SUMMARY_PATH")
	if summaryPath == "" {
		return
	}

	v.summary.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(v.summary, "", "  ")
	if err != nil {
		log.Printf("failed to marshal validation summary: %v", err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(summaryPath), 0o755); err != nil {
		log.Printf("failed to create summary directory: %v", err)
		return
	}

	if err := os.WriteFile(summaryPath, raw, 0o644); err != nil {
		log.Printf("failed to write validation summary %s: %v", summaryPath, err)
		return
	}

	log.Printf("validation summary written to %s", summaryPath)
}

func validateNodeProperties(nodes *corev1.NodeList, labels map[string]string, expectedIPCount int) error {
	log.Print("Validating Node properties")

	for index := range nodes.Items {
		nodeName := nodes.Items[index].ObjectMeta.Name
		// check nodes status;
		// nodes status should be ready after cluster is created
		nodeConditions := nodes.Items[index].Status.Conditions
		if nodeConditions[len(nodeConditions)-1].Type != corev1.NodeReady {
			return errors.Errorf("node %s status is not ready", nodeName)
		}

		// get node labels
		nodeLabels := nodes.Items[index].ObjectMeta.GetLabels()
		for key := range nodeLabels {
			if label, ok := labels[key]; ok {
				log.Printf("label %s is correctly shown on the node %+v", key, nodeName)
				if label != overlayClusterLabelName {
					return errors.Errorf("node %s overlay label name is wrong; expected label:%s but actual label:%s", nodeName, overlayClusterLabelName, label)
				}
			}
		}

		// check if node has correct number of internal IPs
		internalIPCount := 0
		for _, address := range nodes.Items[index].Status.Addresses {
			if address.Type == "InternalIP" {
				internalIPCount++
			}
		}
		if internalIPCount != expectedIPCount {
			return errors.Errorf("number of node internal IPs: %d does not match expected number of IPs %d", internalIPCount, expectedIPCount)
		}
	}
	return nil
}

func (v *Validator) ValidateV4OverlayControlPlane(ctx context.Context) error {
	nodes, err := acnk8s.GetNodeListByLabelSelector(ctx, v.clientset, nodeSelectorMap[v.os])
	if err != nil {
		return errors.Wrap(err, "failed to get node list")
	}

	if err := validateNodeProperties(nodes, v4OverlayNodeLabels, IPv4ExpectedIPCount); err != nil {
		return errors.Wrap(err, "failed to validate IPv4 overlay node properties")
	}

	if v.os == "windows" {
		if err := validateHNSNetworkState(ctx, nodes, v.clientset, v.config); err != nil {
			return errors.Wrap(err, "failed to validate IPv4 overlay HNS network state")
		}
	}

	return nil
}

func (v *Validator) ValidateDualStackControlPlane(ctx context.Context) error {
	nodes, err := acnk8s.GetNodeListByLabelSelector(ctx, v.clientset, nodeSelectorMap[v.os])
	if err != nil {
		return errors.Wrap(err, "failed to get node list")
	}

	if err := validateNodeProperties(nodes, dualstackOverlayNodeLabels, DualstackExpectedIPCount); err != nil {
		return errors.Wrap(err, "failed to validate dualstack overlay node properties")
	}

	if v.os == "windows" {
		if err := validateHNSNetworkState(ctx, nodes, v.clientset, v.config); err != nil {
			return errors.Wrap(err, "failed to validate dualstack overlay HNS network state")
		}
	}

	return nil
}

func (v *Validator) Cleanup(ctx context.Context) {
	// deploy privileged pod
	privilegedDaemonSet := acnk8s.MustParseDaemonSet(privilegedDaemonSetPathMap[v.os])
	daemonsetClient := v.clientset.AppsV1().DaemonSets(privilegedNamespace)
	acnk8s.MustDeleteDaemonset(ctx, daemonsetClient, privilegedDaemonSet)
}

func cnsCacheStateFileIps(result []byte) (map[string]string, error) {
	var cnsLocalCache CNSLocalCache

	err := json.Unmarshal(result, &cnsLocalCache)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal cns local cache")
	}

	cnsPodIps := make(map[string]string)
	for index := range cnsLocalCache.IPConfigurationStatus {
		cnsPodIps[cnsLocalCache.IPConfigurationStatus[index].IPAddress] = cnsLocalCache.IPConfigurationStatus[index].PodInfo.Name()
	}
	return cnsPodIps, nil
}

func cnsManagedStateFileIps(result []byte) (map[string]string, error) {
	var cnsResult CnsManagedState
	err := json.Unmarshal(result, &cnsResult) //nolint:musttag // Legacy endpoint state uses existing untagged CNS types.
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal cns endpoint list")
	}

	cnsPodIps := make(map[string]string)
	for _, v := range cnsResult.Endpoints {
		for ifName, ip := range v.IfnameToIPMap {
			if ifName == "eth0" {
				for _, ipNet := range ip.IPv4 {
					cnsPodIps[ipNet.IP.String()] = v.PodName
				}
			}
		}
	}
	return cnsPodIps, nil
}
