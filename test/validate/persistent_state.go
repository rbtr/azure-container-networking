//nolint:goconst // Command and platform literals are intentionally explicit in validation fixtures.
package validate

import (
	"encoding/json"
	"strings"

	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/pkg/errors"
)

const (
	stateBackendJSON = "json"
	stateBackendBolt = "bolt"
)

var (
	cnsPersistentStateLinuxCmd = []string{
		"bash",
		"-c",
		"curl -sf localhost:10090/debug/persistentstate -d '{}'",
	}
	cnsPersistentStateWindowsCmd = []string{
		"powershell",
		"-c",
		"Invoke-WebRequest -Uri 127.0.0.1:10090/debug/persistentstate " +
			"-Method Post -UseBasicParsing -ErrorAction Stop | Select-Object -Expand Content",
	}
)

type persistentStateDetails struct {
	Backend         string
	DBFilePresent   bool
	DBFileSizeBytes int64
	Authority       string
	SchemaVersion   uint32
	Generation      uint64
	BootID          string
	EndpointCount   int
	AssignmentCount int
	OwnerCount      int
	TombstoneCount  int
}

func configureStateChecks(checks []check, osName, backend string) ([]check, error) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		backend = stateBackendJSON
	}
	if backend != stateBackendJSON && backend != stateBackendBolt {
		return nil, errors.Errorf("unsupported validation state backend %q", backend)
	}

	configured := append([]check(nil), checks...)
	managedStateCheck := false
	for i := range configured {
		if !configured[i].cnsManagedState {
			continue
		}
		managedStateCheck = true
		if backend == stateBackendBolt {
			configured[i].cmd = persistentStateCommand(osName)
			configured[i].stateFileIPs = cnsPersistentStateIPs
			configured[i].persistentState = true
		}
	}

	if backend == stateBackendBolt && !managedStateCheck {
		metadataCheck := check{
			name:            "cns persistent metadata",
			stateFileIPs:    cnsPersistentStateIPs,
			podNamespace:    privilegedNamespace,
			cmd:             persistentStateCommand(osName),
			metadataOnly:    true,
			persistentState: true,
		}
		switch osName {
		case "linux":
			metadataCheck.podLabelSelector = validatorPod
			metadataCheck.containerName = "debug"
		case "windows":
			metadataCheck.podLabelSelector = cnsWinLabelSelector
		default:
			return nil, errors.Errorf("unsupported os %q for persistent state validation", osName)
		}
		configured = append(configured, metadataCheck)
	}
	return configured, nil
}

func persistentStateCommand(osName string) []string {
	if osName == "windows" {
		return append([]string(nil), cnsPersistentStateWindowsCmd...)
	}
	return append([]string(nil), cnsPersistentStateLinuxCmd...)
}

func cnsPersistentStateIPs(result []byte) (map[string]string, error) {
	response, err := decodePersistentState(result)
	if err != nil {
		return nil, err
	}

	podIPs := make(map[string]string)
	for _, endpoint := range response.Snapshot.Endpoints {
		for _, ipInfo := range endpoint.IfnameToIPMap {
			if ipInfo == nil || !ipInfo.NICType.IsInfraOrLegacy() {
				continue
			}
			for _, ipNet := range ipInfo.IPv4 {
				podIPs[ipNet.IP.String()] = endpoint.PodName
			}
			for _, ipNet := range ipInfo.IPv6 {
				podIPs[ipNet.IP.String()] = endpoint.PodName
			}
		}
	}
	return podIPs, nil
}

func inspectPersistentState(result []byte) (persistentStateDetails, error) {
	response, err := decodePersistentState(result)
	if err != nil {
		return persistentStateDetails{}, err
	}
	snapshot := response.Snapshot
	details := persistentStateDetails{
		Backend:         string(response.Storage.Backend),
		DBFilePresent:   response.Storage.FilePresent,
		DBFileSizeBytes: response.Storage.FileSizeBytes,
		Authority:       string(snapshot.Metadata.Authority),
		SchemaVersion:   snapshot.Metadata.SchemaVersion,
		Generation:      snapshot.Metadata.Generation,
		BootID:          snapshot.Metadata.BootID,
		EndpointCount:   len(snapshot.Endpoints),
		AssignmentCount: len(snapshot.Assignments),
		OwnerCount:      len(snapshot.IPOwners),
		TombstoneCount:  len(snapshot.DeleteIntents),
	}

	if response.Storage.Backend == "" {
		return details, errors.New("persistent state storage metadata is missing")
	}
	if response.Storage.Backend != persistentstate.StorageBackendBolt {
		return details, errors.Errorf(
			"unexpected persistent state storage backend %q, expected %q",
			response.Storage.Backend,
			persistentstate.StorageBackendBolt,
		)
	}
	if !response.Storage.FilePresent {
		return details, errors.New("persistent state database file is missing")
	}
	if response.Storage.FileSizeBytes <= 0 {
		return details, errors.Errorf(
			"persistent state database file size must be positive, got %d",
			response.Storage.FileSizeBytes,
		)
	}
	return details, nil
}

func decodePersistentState(result []byte) (persistentstate.DebugResponse, error) {
	var envelope struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return persistentstate.DebugResponse{}, errors.Wrap(err, "failed to unmarshal CNS persistent state response")
	}

	var response persistentstate.DebugResponse
	if envelope.Snapshot == nil {
		if err := json.Unmarshal(result, &response.Snapshot); err != nil { //nolint:musttag // Snapshot includes existing CNS wire types.
			return persistentstate.DebugResponse{}, errors.Wrap(err, "failed to unmarshal legacy CNS persistent state")
		}
	} else if err := json.Unmarshal(result, &response); err != nil { //nolint:musttag // Snapshot includes existing CNS wire types.
		return persistentstate.DebugResponse{}, errors.Wrap(err, "failed to unmarshal CNS persistent state response")
	}

	snapshot := response.Snapshot
	if err := snapshot.Validate(); err != nil {
		return persistentstate.DebugResponse{}, errors.Wrap(err, "invalid CNS persistent state")
	}
	if snapshot.Metadata.Authority != persistentstate.AuthorityBolt {
		return persistentstate.DebugResponse{}, errors.Errorf(
			"unexpected persistent state authority %q, expected %q",
			snapshot.Metadata.Authority,
			persistentstate.AuthorityBolt,
		)
	}
	if snapshot.Metadata.SchemaVersion != persistentstate.SchemaVersion {
		return persistentstate.DebugResponse{}, errors.Errorf(
			"unexpected persistent state schema %d, expected %d",
			snapshot.Metadata.SchemaVersion,
			persistentstate.SchemaVersion,
		)
	}
	if snapshot.Metadata.BootID == "" {
		return persistentstate.DebugResponse{}, errors.New("persistent state boot ID is empty")
	}
	return response, nil
}
