// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const systemBootEnvironmentInformation = 90

var ntQuerySystemInformation = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySystemInformation")

type bootEnvironmentInformation struct {
	BootIdentifier windows.GUID
	FirmwareType   uint32
	BootFlags      uint64
}

func BootID() (string, error) {
	var (
		info         bootEnvironmentInformation
		returnLength uint32
	)
	status, _, _ := ntQuerySystemInformation.Call(
		systemBootEnvironmentInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		uintptr(unsafe.Pointer(&returnLength)),
	)
	if status != 0 {
		return "", fmt.Errorf("querying Windows boot ID: NTSTATUS 0x%x", status)
	}
	return info.BootIdentifier.String(), nil
}
