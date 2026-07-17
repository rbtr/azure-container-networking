// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rollbackTestNCID      = "nc-rollback"
	rollbackTestIPID      = "ip-rollback"
	rollbackTestIPAddress = "10.0.0.4"
	rollbackTestIfName    = "eth0"
)

var errInjectedRollbackFileSystem = errors.New("injected rollback filesystem failure")

type rollbackFailureStage string

const (
	rollbackFailureMkdir      rollbackFailureStage = "mkdir"
	rollbackFailureCreate     rollbackFailureStage = "create"
	rollbackFailureWrite      rollbackFailureStage = "write"
	rollbackFailureSync       rollbackFailureStage = "sync"
	rollbackFailureClose      rollbackFailureStage = "close"
	rollbackFailureReplace    rollbackFailureStage = "replace"
	rollbackFailureDurability rollbackFailureStage = "directory-durability"
)

type faultRollbackFileSystem struct {
	osRollbackFileSystem

	destination string
	stage       rollbackFailureStage
	failed      bool
}

func (f *faultRollbackFileSystem) mkdirAll(path string, perm os.FileMode) error {
	if f.stage == rollbackFailureMkdir && filepath.Clean(path) == filepath.Clean(filepath.Dir(f.destination)) {
		f.failed = true
		return errInjectedRollbackFileSystem
	}
	return f.osRollbackFileSystem.mkdirAll(path, perm)
}

func (f *faultRollbackFileSystem) createTemp(dir, pattern string) (rollbackTemporaryFile, error) {
	destination := filepath.Join(dir, strings.TrimSuffix(pattern, ".tmp-*"))
	if filepath.Clean(destination) != filepath.Clean(f.destination) {
		return f.osRollbackFileSystem.createTemp(dir, pattern)
	}
	if f.stage == rollbackFailureCreate {
		f.failed = true
		return nil, errInjectedRollbackFileSystem
	}

	file, err := f.osRollbackFileSystem.createTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &faultRollbackTemporaryFile{
		rollbackTemporaryFile: file,
		fileSystem:            f,
	}, nil
}

func (f *faultRollbackFileSystem) durableReplace(source, destination string) error {
	if filepath.Clean(destination) != filepath.Clean(f.destination) {
		return f.osRollbackFileSystem.durableReplace(source, destination)
	}

	switch f.stage {
	case rollbackFailureReplace:
		f.failed = true
		return errInjectedRollbackFileSystem
	case rollbackFailureDurability:
		if err := platform.ReplaceFile(source, destination); err != nil {
			return fmt.Errorf("replacing file: %w", err)
		}
		f.failed = true
		return errInjectedRollbackFileSystem
	case rollbackFailureMkdir,
		rollbackFailureCreate,
		rollbackFailureWrite,
		rollbackFailureSync,
		rollbackFailureClose:
		return f.osRollbackFileSystem.durableReplace(source, destination)
	default:
		return f.osRollbackFileSystem.durableReplace(source, destination)
	}
}

type faultRollbackTemporaryFile struct {
	rollbackTemporaryFile
	fileSystem *faultRollbackFileSystem
}

func (f *faultRollbackTemporaryFile) Write(data []byte) (int, error) {
	if f.fileSystem.stage == rollbackFailureWrite {
		f.fileSystem.failed = true
		return 0, errInjectedRollbackFileSystem
	}
	written, err := f.rollbackTemporaryFile.Write(data)
	if err != nil {
		return written, fmt.Errorf("writing temporary file: %w", err)
	}
	return written, nil
}

func (f *faultRollbackTemporaryFile) Sync() error {
	if f.fileSystem.stage == rollbackFailureSync {
		f.fileSystem.failed = true
		return errInjectedRollbackFileSystem
	}
	if err := f.rollbackTemporaryFile.Sync(); err != nil {
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	return nil
}

func (f *faultRollbackTemporaryFile) Close() error {
	closeErr := f.rollbackTemporaryFile.Close()
	if f.fileSystem.stage == rollbackFailureClose {
		f.fileSystem.failed = true
		return fmt.Errorf("closing temporary file: %w", errors.Join(closeErr, errInjectedRollbackFileSystem))
	}
	if closeErr != nil {
		return fmt.Errorf("closing temporary file: %w", closeErr)
	}
	return nil
}

func TestExportLegacyFilesystemFailures(t *testing.T) {
	stages := []rollbackFailureStage{
		rollbackFailureMkdir,
		rollbackFailureCreate,
		rollbackFailureWrite,
		rollbackFailureSync,
		rollbackFailureClose,
		rollbackFailureReplace,
		rollbackFailureDurability,
	}
	outputs := []struct {
		name  string
		first bool
	}{
		{name: "CNS first file", first: true},
		{name: "endpoint second file"},
	}

	for _, output := range outputs {
		for _, stage := range stages {
			t.Run(output.name+"/"+string(stage), func(t *testing.T) {
				ctx := context.Background()
				db := openRollbackTestDB(t)
				seedRollbackTestDB(ctx, t, db)

				outputDir := t.TempDir()
				cnsPath := filepath.Join(outputDir, "cns", "azure-cns.json")
				endpointPath := filepath.Join(outputDir, "endpoint", "azure-endpoints.json")
				oldCNS := []byte(`{"old":"cns"}`)
				oldEndpoint := []byte(`{"old":"endpoint"}`)
				writeRollbackDestination(t, cnsPath, oldCNS)
				writeRollbackDestination(t, endpointPath, oldEndpoint)

				destination := endpointPath
				if output.first {
					destination = cnsPath
				}
				fileSystem := &faultRollbackFileSystem{
					destination: destination,
					stage:       stage,
				}
				before := readRollbackTransition(ctx, t, db)

				err := db.exportLegacy(
					ctx,
					cnsPath,
					endpointPath,
					fileSystem,
				)
				require.ErrorIs(t, err, errInjectedRollbackFileSystem)
				assert.True(t, fileSystem.failed)

				failed := readRollbackTransition(ctx, t, db)
				assert.Equal(t, AuthorityBolt, failed.authority)
				assert.False(t, failed.rollbackComplete)
				assert.Equal(t, before.generation, failed.generation)
				assertNoRollbackTemporaryFiles(t, cnsPath, endpointPath)

				if output.first {
					if stage == rollbackFailureDurability {
						assertLegacyEnvelopeKeys(t, cnsPath, legacyCNSStoreKey)
					} else {
						assertRollbackFileBytes(t, cnsPath, oldCNS)
					}
					assertRollbackFileBytes(t, endpointPath, oldEndpoint)
				} else {
					assertLegacyEnvelopeKeys(t, cnsPath, legacyCNSStoreKey)
					if stage == rollbackFailureDurability {
						assertLegacyEnvelopeKeys(
							t,
							endpointPath,
							legacyEndpointStoreKey,
							legacyDeleteIntentStoreKey,
						)
					} else {
						assertRollbackFileBytes(t, endpointPath, oldEndpoint)
					}
				}

				require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
				assertLegacyEnvelopeKeys(t, cnsPath, legacyCNSStoreKey)
				assertLegacyEnvelopeKeys(
					t,
					endpointPath,
					legacyEndpointStoreKey,
					legacyDeleteIntentStoreKey,
				)
				assertNoRollbackTemporaryFiles(t, cnsPath, endpointPath)

				retried := readRollbackTransition(ctx, t, db)
				assert.Equal(t, AuthorityJSON, retried.authority)
				assert.True(t, retried.rollbackComplete)
				assert.Equal(t, before.generation+1, retried.generation)
			})
		}
	}
}

func TestExportLegacyOmittedOutputPaths(t *testing.T) {
	tests := []struct {
		name          string
		writeCNS      bool
		writeEndpoint bool
	}{
		{name: "both outputs", writeCNS: true, writeEndpoint: true},
		{name: "CNS only", writeCNS: true},
		{name: "endpoint only", writeEndpoint: true},
		{name: "both omitted"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRollbackTestDB(t)
			seedRollbackTestDB(ctx, t, db)
			before := readRollbackTransition(ctx, t, db)

			outputDir := t.TempDir()
			expectedCNSPath := filepath.Join(outputDir, "azure-cns.json")
			expectedEndpointPath := filepath.Join(outputDir, "azure-endpoints.json")
			cnsPath := ""
			if test.writeCNS {
				cnsPath = expectedCNSPath
			}
			endpointPath := ""
			if test.writeEndpoint {
				endpointPath = expectedEndpointPath
			}

			require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))

			if test.writeCNS {
				assertLegacyEnvelopeKeys(t, expectedCNSPath, legacyCNSStoreKey)
			} else {
				assertRollbackPathMissing(t, expectedCNSPath)
			}
			if test.writeEndpoint {
				assertLegacyEnvelopeKeys(
					t,
					expectedEndpointPath,
					legacyEndpointStoreKey,
					legacyDeleteIntentStoreKey,
				)
			} else {
				assertRollbackPathMissing(t, expectedEndpointPath)
			}

			after := readRollbackTransition(ctx, t, db)
			assert.Equal(t, AuthorityJSON, after.authority)
			assert.True(t, after.rollbackComplete)
			assert.Equal(t, before.generation+1, after.generation)
		})
	}
}

func TestExportLegacyCreatesMissingParentDirectories(t *testing.T) {
	ctx := context.Background()
	db := openRollbackTestDB(t)
	seedRollbackTestDB(ctx, t, db)

	outputDir := t.TempDir()
	cnsPath := filepath.Join(outputDir, "missing", "cns", "azure-cns.json")
	endpointPath := filepath.Join(outputDir, "missing", "endpoint", "azure-endpoints.json")

	require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))
	assertLegacyEnvelopeKeys(t, cnsPath, legacyCNSStoreKey)
	assertLegacyEnvelopeKeys(
		t,
		endpointPath,
		legacyEndpointStoreKey,
		legacyDeleteIntentStoreKey,
	)
}

func TestExportLegacyRollbackStateCombinations(t *testing.T) {
	tests := []struct {
		name      string
		authority Authority
		marker    bool
		rewrite   bool
	}{
		{
			name:      "Bolt without marker retries",
			authority: AuthorityBolt,
			rewrite:   true,
		},
		{
			name:      "Bolt with marker retries",
			authority: AuthorityBolt,
			marker:    true,
			rewrite:   true,
		},
		{
			name:      "JSON without marker retries",
			authority: AuthorityJSON,
			rewrite:   true,
		},
		{
			name:      "JSON with marker is complete",
			authority: AuthorityJSON,
			marker:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRollbackTestDB(t)
			seedRollbackTestDB(ctx, t, db)
			setRollbackTransition(
				ctx,
				t,
				db,
				test.authority,
				test.marker,
			)

			outputDir := t.TempDir()
			cnsPath := filepath.Join(outputDir, "azure-cns.json")
			endpointPath := filepath.Join(outputDir, "azure-endpoints.json")
			oldCNS := []byte(`{"old":"cns"}`)
			oldEndpoint := []byte(`{"old":"endpoint"}`)
			writeRollbackDestination(t, cnsPath, oldCNS)
			writeRollbackDestination(t, endpointPath, oldEndpoint)
			before := readRollbackTransition(ctx, t, db)

			require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))

			if test.rewrite {
				assertLegacyEnvelopeKeys(t, cnsPath, legacyCNSStoreKey)
				assertLegacyEnvelopeKeys(
					t,
					endpointPath,
					legacyEndpointStoreKey,
					legacyDeleteIntentStoreKey,
				)
			} else {
				assertRollbackFileBytes(t, cnsPath, oldCNS)
				assertRollbackFileBytes(t, endpointPath, oldEndpoint)
			}

			after := readRollbackTransition(ctx, t, db)
			assert.Equal(t, AuthorityJSON, after.authority)
			assert.True(t, after.rollbackComplete)
			if test.rewrite {
				assert.Equal(t, before.generation+1, after.generation)
			} else {
				assert.Equal(t, before.generation, after.generation)
			}
		})
	}
}

func TestRollbackJSONMutationIsReimportedIntoBolt(t *testing.T) {
	ctx := context.Background()
	db := openRollbackTestDB(t)
	seedRollbackTestDB(ctx, t, db)

	outputDir := t.TempDir()
	cnsPath := filepath.Join(outputDir, "azure-cns.json")
	endpointPath := filepath.Join(outputDir, "azure-endpoints.json")
	require.NoError(t, db.ExportLegacy(ctx, cnsPath, endpointPath))

	const (
		jsonNCID      = "nc-json"
		jsonIPID      = "ip-json"
		jsonIPAddress = "10.0.0.9"
		jsonContainer = "container-json"
	)
	request := cns.CreateNetworkContainerRequest{
		NetworkContainerid: jsonNCID,
		AuthorizationToken: "json-secret",
		SecondaryIPConfigs: map[string]cns.SecondaryIPConfig{
			jsonIPID: {
				IPAddress: jsonIPAddress,
				NCVersion: 9,
			},
		},
	}
	writeRollbackJSON(t, cnsPath, map[string]any{
		legacyCNSStoreKey: legacyCNSState{
			OrchestratorType: "KubernetesCRD",
			ContainerStatus: map[string]legacyContainerStatus{
				jsonNCID: {
					ID:                            jsonNCID,
					VMVersion:                     "9",
					HostVersion:                   "8",
					CreateNetworkContainerRequest: request,
					VfpUpdateComplete:             true,
				},
			},
		},
	})
	writeRollbackJSON(t, endpointPath, map[string]any{
		legacyEndpointStoreKey: map[string]*legacyEndpointInfo{
			jsonContainer: {
				PodName:      "pod-json",
				PodNamespace: "default",
				IfnameToIPMap: map[string]*legacyIPInfo{
					rollbackTestIfName: {
						IPv4: []net.IPNet{{
							IP:   net.ParseIP(jsonIPAddress),
							Mask: net.CIDRMask(24, 32),
						}},
					},
				},
			},
		},
	})

	require.NoError(t, db.ImportLegacy(ctx, ImportOptions{
		CNSJSONPath:         cnsPath,
		EndpointJSONPath:    endpointPath,
		ManageEndpointState: true,
		BootID:              "boot-json",
		Now:                 rollbackTestNow(),
	}))

	snapshot, err := db.Snapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snapshot.Validate())
	assert.NotContains(t, snapshot.NetworkContainers, rollbackTestNCID)
	assert.NotContains(t, snapshot.IPs, rollbackTestIPID)
	assert.Contains(t, snapshot.NetworkContainers, jsonNCID)
	assert.Contains(t, snapshot.IPs, jsonIPID)
	if owner := snapshot.IPOwners[jsonIPID]; owner != jsonContainer {
		t.Errorf("IP owner = %q, want %q", owner, jsonContainer)
	}
	assert.Equal(t, []string{jsonIPID}, snapshot.Assignments[jsonContainer].IPIDs)
	assert.Empty(t, snapshot.NetworkContainers[jsonNCID].Request.AuthorizationToken)
	assert.Equal(t, "boot-json", snapshot.Metadata.BootID)

	transition := readRollbackTransition(ctx, t, db)
	assert.Equal(t, AuthorityBolt, transition.authority)
	assert.True(t, transition.migrationComplete)
	assert.False(t, transition.rollbackComplete)
}

type rollbackTransition struct {
	authority         Authority
	generation        uint64
	migrationComplete bool
	rollbackComplete  bool
}

func openRollbackTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "azure-cns.db")
	db, err := Open(path, Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return db
}

func seedRollbackTestDB(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	record := NewNetworkContainerRecord(
		rollbackTestNCID,
		"2",
		"1",
		true,
		cns.CreateNetworkContainerRequest{
			NetworkContainerid: rollbackTestNCID,
			AuthorizationToken: "must-not-persist",
		},
	)
	require.NoError(t, db.ApplyNetworkContainer(ctx, record, map[string]IPRecord{
		rollbackTestIPID: {
			ID:        rollbackTestIPID,
			IPAddress: rollbackTestIPAddress,
			NCID:      rollbackTestNCID,
			NCVersion: 2,
		},
	}))

	assignment := AssignmentRecord{
		Pod: PodIdentity{
			PodKey:           "container-rollback",
			InfraContainerID: "container-rollback",
		},
		IPIDs: []string{rollbackTestIPID},
	}
	endpoint := EndpointRecord{
		PodName:      "pod-rollback",
		PodNamespace: "default",
		IfnameToIPMap: map[string]*IPInfoRecord{
			rollbackTestIfName: {
				IPv4: []net.IPNet{{
					IP:   net.ParseIP(rollbackTestIPAddress),
					Mask: net.CIDRMask(24, 32),
				}},
				NICType: cns.InfraNIC,
			},
		},
	}
	require.NoError(t, db.AssignEndpoint(
		ctx,
		assignment,
		endpoint,
		rollbackTestNow(),
		time.Hour,
	))
}

func readRollbackTransition(ctx context.Context, t *testing.T, db *DB) rollbackTransition {
	t.Helper()
	var result rollbackTransition
	require.NoError(t, db.View(ctx, func(tx *ReadTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return err
		}
		result = rollbackTransition{
			authority:         meta.Authority,
			generation:        meta.Generation,
			migrationComplete: tx.MigrationComplete(),
			rollbackComplete:  tx.RollbackComplete(),
		}
		return nil
	}))
	return result
}

func setRollbackTransition(
	ctx context.Context,
	t *testing.T,
	db *DB,
	authority Authority,
	marker bool,
) {
	t.Helper()
	require.NoError(t, db.Update(ctx, func(tx *WriteTx) error {
		meta, err := tx.Metadata()
		if err != nil {
			return err
		}
		meta.Authority = authority
		if err := tx.PutMetadata(meta); err != nil {
			return err
		}
		if marker {
			return tx.SetRollbackComplete()
		}
		return tx.ClearRollbackComplete()
	}))
}

func writeRollbackDestination(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func writeRollbackJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "\t")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func assertLegacyEnvelopeKeys(t *testing.T, path string, keys ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var envelope LegacyEnvelope
	require.NoError(t, json.Unmarshal(data, &envelope))
	for _, key := range keys {
		assert.Contains(t, envelope, key)
	}
}

func assertRollbackFileBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, actual)
}

func assertRollbackPathMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func assertNoRollbackTemporaryFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), filepath.Base(path)+".tmp-*"))
		require.NoError(t, err)
		assert.Empty(t, matches)
	}
}

func rollbackTestNow() time.Time {
	return time.Date(2026, time.July, 17, 15, 0, 0, 0, time.UTC)
}
