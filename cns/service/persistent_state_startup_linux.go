// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import persistentstate "github.com/Azure/azure-container-networking/cns/state"

const defaultEndpointStorePath = "/var/run/azure-cns/"

func platformPersistentStateBootPolicy(resetNetworkContainerReadiness bool) persistentstate.BootPolicy {
	return persistentstate.BootPolicy{
		ClearEndpoints:                 true,
		ResetNetworkContainerReadiness: resetNetworkContainerReadiness,
	}
}
