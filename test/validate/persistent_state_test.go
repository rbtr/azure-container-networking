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
	snapshot := persistentstate.NewSnapshot()
	snapshot.Metadata = persistentstate.Metadata{
		SchemaVersion: persistentstate.SchemaVersion,
		Authority:     persistentstate.AuthorityBolt,
		Generation:    7,
		BootID:        "boot-id",
	}
	raw, err := json.Marshal(snapshot) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)

	details, err := inspectPersistentState(raw)
	require.NoError(t, err)
	assert.Equal(t, stateBackendBolt, details.Backend)
	assert.Equal(t, string(persistentstate.AuthorityBolt), details.Authority)
	assert.Equal(t, uint32(persistentstate.SchemaVersion), details.SchemaVersion)
	assert.Equal(t, uint64(7), details.Generation)
	assert.Equal(t, "boot-id", details.BootID)
}

func TestInspectPersistentStateRejectsWrongAuthorityAndEmptyBootID(t *testing.T) {
	snapshot := persistentstate.NewSnapshot()
	snapshot.Metadata = persistentstate.Metadata{
		SchemaVersion: persistentstate.SchemaVersion,
		Authority:     persistentstate.AuthorityJSON,
		BootID:        "boot-id",
	}
	raw, err := json.Marshal(snapshot) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)
	_, err = inspectPersistentState(raw)
	require.Error(t, err)

	snapshot.Metadata.Authority = persistentstate.AuthorityBolt
	snapshot.Metadata.BootID = ""
	raw, err = json.Marshal(snapshot) //nolint:musttag // Snapshot includes existing CNS wire types.
	require.NoError(t, err)
	_, err = inspectPersistentState(raw)
	require.Error(t, err)
}
