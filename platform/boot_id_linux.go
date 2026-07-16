// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package platform

import (
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
)

const linuxBootIDPath = "/proc/sys/kernel/random/boot_id"

func BootID() (string, error) {
	return readBootID(linuxBootIDPath)
}

func readBootID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading boot ID: %w", err)
	}
	id, err := uuid.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("parsing boot ID: %w", err)
	}
	return id.String(), nil
}
