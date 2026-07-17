//nolint:goconst // Repeated values keep validation cases self-contained.
package validate

import (
	"encoding/json"
	"testing"

	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigureStateChecks(t *testing.T) {
	tests := []struct {
		name             string
		osName           string
		backend          string
		checks           []check
		wantLen          int
		wantPersistent   bool
		wantMetadataOnly bool
		wantErr          bool
	}{
		{
			name:    "JSON keeps managed state check",
			osName:  "linux",
			backend: stateBackendJSON,
			checks: []check{{
				name:            "cns",
				cnsManagedState: true,
			}},
			wantLen: 1,
		},
		{
			name:    "Bolt converts managed state check",
			osName:  "linux",
			backend: stateBackendBolt,
			checks: []check{{
				name:            "cns",
				cnsManagedState: true,
			}},
			wantLen:        1,
			wantPersistent: true,
		},
		{
			name:             "Bolt adds metadata check for CNI managed state",
			osName:           "windows",
			backend:          stateBackendBolt,
			checks:           []check{{name: "hns"}},
			wantLen:          2,
			wantPersistent:   true,
			wantMetadataOnly: true,
		},
		{
			name:    "Unsupported backend fails",
			osName:  "linux",
			backend: "other",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := configureStateChecks(tt.checks, tt.osName, tt.backend)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, tt.wantLen)
			if tt.wantPersistent {
				assert.True(t, got[len(got)-1].persistentState)
			}
			assert.Equal(t, tt.wantMetadataOnly, got[len(got)-1].metadataOnly)
		})
	}
}

func TestInspectPersistentState(t *testing.T) {
	response := validPersistentStateResponse()
	raw, err := json.Marshal(response) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)

	details, err := inspectPersistentState(raw)
	require.NoError(t, err)
	assert.Equal(t, stateBackendBolt, details.Backend)
	assert.True(t, details.DBFilePresent)
	assert.Equal(t, int64(4096), details.DBFileSizeBytes)
	assert.Equal(t, string(persistentstate.AuthorityBolt), details.Authority)
	assert.Equal(t, uint32(persistentstate.SchemaVersion), details.SchemaVersion)
	assert.Equal(t, uint64(7), details.Generation)
	assert.Equal(t, "boot-id", details.BootID)
}

func TestInspectPersistentStateRejectsWrongAuthorityAndEmptyBootID(t *testing.T) {
	response := validPersistentStateResponse()
	response.Snapshot.Metadata.Authority = persistentstate.AuthorityJSON
	raw, err := json.Marshal(response) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)
	_, err = inspectPersistentState(raw)
	require.Error(t, err)

	response.Snapshot.Metadata.Authority = persistentstate.AuthorityBolt
	response.Snapshot.Metadata.BootID = ""
	raw, err = json.Marshal(response) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)
	_, err = inspectPersistentState(raw)
	require.Error(t, err)
}

func TestInspectPersistentStateRejectsInvalidStorage(t *testing.T) {
	tests := []struct {
		name        string
		storage     persistentstate.StorageMetadata
		wantErr     string
		wantPresent bool
		wantSize    int64
	}{
		{
			name: "missing database file",
			storage: persistentstate.StorageMetadata{
				Backend: persistentstate.StorageBackendBolt,
			},
			wantErr: "database file is missing",
		},
		{
			name: "zero-sized database file",
			storage: persistentstate.StorageMetadata{
				Backend:     persistentstate.StorageBackendBolt,
				FilePresent: true,
			},
			wantErr:     "file size must be positive",
			wantPresent: true,
		},
		{
			name: "unexpected backend",
			storage: persistentstate.StorageMetadata{
				Backend:       persistentstate.StorageBackend("other"),
				FilePresent:   true,
				FileSizeBytes: 4096,
			},
			wantErr:     "unexpected persistent state storage backend",
			wantPresent: true,
			wantSize:    4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := validPersistentStateResponse()
			response.Storage = tt.storage
			raw, err := json.Marshal(response) //nolint:musttag // Snapshot includes existing CNS wire types.
			require.NoError(t, err)

			details, err := inspectPersistentState(raw)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, string(tt.storage.Backend), details.Backend)
			assert.Equal(t, tt.wantPresent, details.DBFilePresent)
			assert.Equal(t, tt.wantSize, details.DBFileSizeBytes)
		})
	}
}

func TestDecodePersistentStateAcceptsLegacySnapshot(t *testing.T) {
	response := validPersistentStateResponse()
	raw, err := json.Marshal(response.Snapshot) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)

	decoded, err := decodePersistentState(raw)
	require.NoError(t, err)
	assert.Equal(t, response.Snapshot.Metadata, decoded.Snapshot.Metadata)
	assert.Equal(t, persistentstate.StorageMetadata{}, decoded.Storage)

	_, err = cnsPersistentStateIPs(raw)
	require.NoError(t, err)
	_, err = inspectPersistentState(raw)
	require.ErrorContains(t, err, "storage metadata is missing")
}

func TestValidationCheckEntryRecordsMissingStorage(t *testing.T) {
	present := false
	sizeBytes := int64(0)
	raw, err := json.Marshal(ValidationCheckEntry{
		DBFilePresent:   &present,
		DBFileSizeBytes: &sizeBytes,
	})
	require.NoError(t, err)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.JSONEq(t, "false", string(fields["dbFilePresent"]))
	assert.JSONEq(t, "0", string(fields["dbFileSizeBytes"]))
}

func validPersistentStateResponse() persistentstate.DebugResponse {
	snapshot := persistentstate.NewSnapshot()
	snapshot.Metadata = persistentstate.Metadata{
		SchemaVersion: persistentstate.SchemaVersion,
		Authority:     persistentstate.AuthorityBolt,
		Generation:    7,
		BootID:        "boot-id",
	}
	return persistentstate.DebugResponse{
		Snapshot: snapshot,
		Storage: persistentstate.StorageMetadata{
			Backend:       persistentstate.StorageBackendBolt,
			FilePresent:   true,
			FileSizeBytes: 4096,
		},
	}
}
