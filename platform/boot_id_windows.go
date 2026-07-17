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

type bootEnvironmentQuery func(*bootEnvironmentInformation) uint32

func queryBootEnvironment(info *bootEnvironmentInformation) uint32 {
	var returnLength uint32
	status, _, _ := ntQuerySystemInformation.Call(
		systemBootEnvironmentInformation,
		uintptr(unsafe.Pointer(info)),
		unsafe.Sizeof(*info),
		uintptr(unsafe.Pointer(&returnLength)),
	)
	return uint32(status)
}

func BootID() (string, error) {
	return bootID(queryBootEnvironment)
}

func bootID(query bootEnvironmentQuery) (string, error) {
	var info bootEnvironmentInformation
	status := query(&info)
	if status != 0 {
		return "", fmt.Errorf("querying Windows boot ID: NTSTATUS 0x%x", status)
	}
	return info.BootIdentifier.String(), nil
}
