package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestMigrationFaultInjectionTemplateContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".pipelines", "cni", "load-test-templates", "migration-fault-injection-template.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var document struct {
		Parameters map[string]any   `json:"parameters"`
		Steps      []map[string]any `json:"steps"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &document))

	for _, parameter := range []string{
		"clusterName",
		"os",
		"cni",
		"scenario",
		"scaleReplicas",
		"timeoutMinutes",
		"testTimeoutMinutes",
		"taskTimeoutMinutes",
		"runID",
		"artifactName",
		"workloadImage",
	} {
		require.Contains(t, document.Parameters, parameter)
	}
	require.Contains(t, document.Parameters["runID"], "$(System.JobId)")
	require.Contains(t, document.Parameters["artifactName"], "$(System.JobId)")

	var inlineScript string
	var publishAlways bool
	for _, step := range document.Steps {
		if inputs, ok := step["inputs"].(map[string]any); ok {
			if script, ok := inputs["inlineScript"].(string); ok {
				inlineScript = script
			}
		}
		if step["task"] == "PublishPipelineArtifact@1" && step["condition"] == "always()" {
			publishAlways = true
		}
	}
	require.NotEmpty(t, inlineScript)
	require.True(t, publishAlways)

	for _, value := range []string{
		"MIGRATION_FAULT_SCENARIO",
		"MIGRATION_FAULT_OS",
		"MIGRATION_FAULT_CNI",
		"MIGRATION_FAULT_RUN_ID",
		"MIGRATION_FAULT_ARTIFACT_DIR",
		"VALIDATE_STATE_BACKEND=bolt",
		"export KUBECONFIG=",
		"-test-kubeconfig=\"$KUBECONFIG\"",
		"./test/integration/state",
		"tee \"$artifactDir/go-test.log\"",
	} {
		require.Contains(t, inlineScript, value)
	}
	for _, forbidden := range []string{"killall", "pkill", "rollout restart", "VALIDATE_MANAGE_ENDPOINT_STATE"} {
		require.NotContains(t, strings.ToLower(inlineScript), forbidden)
	}
}

func TestOwnershipHandoffTemplateContract(t *testing.T) {
	const (
		enabled      = "true"
		disabled     = "false"
		statefulCNI  = "cniv2"
		statelessCNI = "stateless"
	)

	type transition struct {
		Job                  string `json:"job"`
		DependsOn            string `json:"dependsOn"`
		BaselineTransition   string `json:"baselineTransition"`
		Name                 string `json:"transition"`
		Action               string `json:"action"`
		Backend              string `json:"backend"`
		CNI                  string `json:"cni"`
		ManageEndpointState  string `json:"manageEndpointState"`
		InitializeFromCNI    string `json:"initializeFromCNI"`
		EnableStateMigration string `json:"enableStateMigration"`
		StateRelation        string `json:"stateRelation"`
		BootRelation         string `json:"bootRelation"`
		PodRelation          string `json:"podRelation"`
		FaultInjection       bool   `json:"faultInjection"`
	}
	path := filepath.Join(
		"..",
		"..",
		"..",
		".pipelines",
		"cni",
		"state-migration",
		"handoff.jobs.yaml",
	)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var document struct {
		Parameters []struct {
			Name    string       `json:"name"`
			Default []transition `json:"default"`
		} `json:"parameters"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &document))
	require.Contains(t, string(raw), "cni: ${{ transition.cni }}")

	transitions := []transition{}
	for _, parameter := range document.Parameters {
		if parameter.Name == "transitions" {
			transitions = parameter.Default
			break
		}
	}
	require.Len(t, transitions, 18)

	for index, item := range transitions {
		if index == 0 {
			require.Equal(t, "final_same_boot_restart", item.DependsOn)
			require.Equal(t, "10-final-same-boot-restart", item.BaselineTransition)
		} else {
			require.Equal(t, transitions[index-1].Job, item.DependsOn)
			require.Equal(t, transitions[index-1].Name, item.BaselineTransition)
		}
		require.Equal(t, item.ManageEndpointState, item.EnableStateMigration)
		if item.ManageEndpointState == enabled {
			require.Equal(t, disabled, item.InitializeFromCNI)
		} else {
			require.Equal(t, enabled, item.InitializeFromCNI)
			require.Equal(t, statefulCNI, item.CNI)
		}
	}

	require.Equal(t, transition{
		Job:                  "handoff_ownership_first_import",
		DependsOn:            "handoff_cni_json",
		BaselineTransition:   "11-handoff-cni-json",
		Name:                 "12-handoff-cns-json-import",
		Action:               "configure-state-import",
		Backend:              "json",
		CNI:                  statefulCNI,
		ManageEndpointState:  enabled,
		InitializeFromCNI:    disabled,
		EnableStateMigration: enabled,
		StateRelation:        "exact",
		BootRelation:         "none",
		PodRelation:          "exact",
		FaultInjection:       false,
	}, transitions[1])

	for _, pair := range [][2]int{{1, 2}, {8, 9}, {13, 14}} {
		importTransition := transitions[pair[0]]
		statelessTransition := transitions[pair[1]]
		require.Equal(t, enabled, importTransition.ManageEndpointState)
		require.Equal(t, enabled, importTransition.EnableStateMigration)
		require.Equal(t, "configure-state-import", importTransition.Action)
		require.Equal(t, statefulCNI, importTransition.CNI)
		require.Equal(t, "exact", importTransition.StateRelation)
		require.Equal(t, importTransition.Backend, statelessTransition.Backend)
		require.Equal(t, importTransition.Job, statelessTransition.DependsOn)
		require.Equal(t, statelessCNI, statelessTransition.CNI)
		require.Equal(t, enabled, statelessTransition.ManageEndpointState)
		require.Equal(t, "none", statelessTransition.StateRelation)
	}

	require.Equal(t, "json", transitions[1].Backend)
	require.Equal(t, "bolt", transitions[3].Backend)
	require.Equal(t, "bolt", transitions[7].Backend)
	require.Equal(t, "bolt", transitions[8].Backend)
	require.Equal(t, "json", transitions[12].Backend)
	require.Equal(t, "bolt", transitions[13].Backend)
	require.Equal(t, "24-handoff-direct-cns-bolt-import", transitions[13].Name)

	for _, index := range []int{5, 11} {
		reverse := transitions[index]
		require.Equal(t, "configure-state-and-clear-endpoints", reverse.Action)
		require.Equal(t, statefulCNI, reverse.CNI)
		require.Equal(t, disabled, reverse.ManageEndpointState)
		require.Equal(t, disabled, reverse.EnableStateMigration)
		require.Equal(t, "same", reverse.BootRelation)
		require.Equal(t, "exact", reverse.StateRelation)
		require.Equal(t, "exact", reverse.PodRelation)
	}

	for _, index := range []int{4, 10} {
		stateful := transitions[index]
		require.Equal(t, "configure-state", stateful.Action)
		require.Equal(t, statefulCNI, stateful.CNI)
		require.Equal(t, enabled, stateful.ManageEndpointState)
		require.Equal(t, "exact", stateful.PodRelation)
		require.Equal(t, statelessCNI, transitions[index-1].CNI)
		require.Equal(t, stateful.Job, transitions[index+1].DependsOn)
		require.Equal(t, disabled, transitions[index+1].ManageEndpointState)
	}

	require.True(t, transitions[15].FaultInjection)
	require.Equal(t, statelessCNI, transitions[15].CNI)
	require.Equal(t, "same-boot-restart", transitions[16].Action)
	require.Equal(t, "node-reboot", transitions[17].Action)
	require.Equal(t, statelessCNI, transitions[17].CNI)
	require.Equal(t, enabled, transitions[17].ManageEndpointState)
	require.Equal(t, "exact", transitions[17].PodRelation)

	for index := 1; index < len(transitions); index++ {
		if transitions[index-1].ManageEndpointState == enabled && transitions[index].ManageEndpointState == disabled {
			require.Less(t, index, 15, "reverse must occur before fault-induced pod churn")
			require.Equal(t, "configure-state-and-clear-endpoints", transitions[index].Action)
			require.Equal(t, "exact", transitions[index].PodRelation)
		}
	}
	for _, item := range transitions[15:] {
		require.Equal(t, enabled, item.ManageEndpointState,
			"the sequence must remain CNS-managed after fault, restart, and reboot coverage")
	}

	laneRaw, err := os.ReadFile(filepath.Join(
		"..", "..", "..", ".pipelines", "cni", "state-migration", "lane.stage.yaml",
	))
	require.NoError(t, err)
	require.Contains(t, string(laneRaw), `"K8S_VER=${{ parameters.kubernetesVersion }}"`)
	for _, item := range transitions {
		require.Contains(t, string(laneRaw), "\n            - "+item.Job)
	}
}

func TestOwnershipHandoffPipelineContract(t *testing.T) {
	path := filepath.Join(
		"..",
		"..",
		"..",
		".pipelines",
		"cni",
		"state-migration",
		"pipeline.yaml",
	)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "- name: kubernetesVersion")
	require.Contains(t, string(raw), `default: "1.34"`)

	var document struct {
		Stages []struct {
			Template   string         `json:"template"`
			Parameters map[string]any `json:"parameters"`
		} `json:"stages"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &document))

	enabledLanes := []string{}
	for _, stage := range document.Stages {
		if stage.Template != "lane.stage.yaml" {
			continue
		}
		require.Equal(t, "false", stage.Parameters["enableStateMigration"])
		if stage.Parameters["enableOwnershipHandoff"] == true {
			enabledLanes = append(enabledLanes, stage.Parameters["name"].(string))
		}
	}
	require.Equal(t, []string{"windows_podsubnet"}, enabledLanes)

	transitionPath := filepath.Join(
		"..",
		"..",
		"..",
		".pipelines",
		"cni",
		"state-migration",
		"transition.steps.yaml",
	)
	transitionRaw, err := os.ReadFile(transitionPath)
	require.NoError(t, err)
	transitionTemplate := string(transitionRaw)
	for _, value := range []string{
		"expectedManageEndpointState",
		"expectedInitializeFromCNI",
		"expectedEnableStateMigration",
		"configure-state",
		"configure-state-import",
		"configure-state-and-clear-endpoints",
		".ManageEndpointState = $manageEndpointState",
		".InitializeFromCNI = $initializeFromCNI",
		".EnableStateMigration = $enableStateMigration",
		".EnableStateMigration == $enableStateMigration",
		"expectedEnableStateMigration: $enableStateMigration",
		`effectiveCNI="${{ parameters.cni }}"`,
		"effectiveCNI: $effectiveCNI",
		"CNI_TYPE=${{ parameters.cni }}",
		"cni: ${{ parameters.cni }}",
		"cniv2)",
		"expectedCNISource=azure-vnet",
		"expectedCNISource=azure-vnet-stateless",
		"azure-vnet-stateless",
		"Preflight imported CNS endpoint state before CNI replacement",
		`[[ "${{ parameters.cni }}" == "cniv2" ]]`,
		"CNS endpoint import on $node did not converge to the exact live pod endpoint set",
		`index("/k/azurecni/bin/azure-vnet.exe")`,
		`select(.name == "cni-installer")`,
		`kubectl patch daemonset "$daemonset"`,
		`C:\k\azurecns\azure-endpoints.json`,
		`"$uri/network/endpoints/$escaped"`,
		`CNS endpoint records remain after stateful-CNI reverse`,
		`bash patch-kubeclusterconfig.sh || true`,
	} {
		require.Contains(t, transitionTemplate, value)
	}
	for _, forbidden := range []string{
		"ownership-first",
		"backend-first",
		"direct-cni",
		"configure-state-and-reboot",
		"kubectl cordon",
		`C:\k\azure-vnet.json`,
		"expectedCNIStateMode",
		"VALIDATE_MANAGE_ENDPOINT_STATE",
	} {
		require.NotContains(t, transitionTemplate, forbidden)
	}
	require.Less(t,
		strings.Index(transitionTemplate, `kubectl patch daemonset "$daemonset"`),
		strings.Index(transitionTemplate, `kubectl rollout status daemonset "$daemonset"`),
	)
}
