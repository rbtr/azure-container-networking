// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state_test

import (
	"testing"

	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageMetadata(t *testing.T) {
	db, path := openTestDB(t)

	metadata, err := db.StorageMetadata()
	require.NoError(t, err)
	assert.Equal(t, state.StorageBackendBolt, metadata.Backend)
	assert.True(t, metadata.FilePresent)
	assert.Positive(t, metadata.FileSizeBytes)
	assert.FileExists(t, path)
}
