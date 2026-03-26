// Copyright 2025 Microsoft. All rights reserved.
// MIT License

//go:build !linux

package restserver

import "github.com/Azure/azure-container-networking/cns"

// assignVethPairs is a no-op on non-Linux platforms.
func (service *HTTPRestService) assignVethPairs(_ []cns.PodIpInfo) {}
