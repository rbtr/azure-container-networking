package restserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-container-networking/cns/state"
	"github.com/Azure/azure-container-networking/cns/types"
	"github.com/stretchr/testify/require"
)

func attachPersistentState(t *testing.T, svc *HTTPRestService) *state.DB {
	t.Helper()
	db, _ := openPersistentStateForTest(t, svc)
	return db
}

func TestPersistentStateAddDeleteTransaction(t *testing.T) {
	svc := getTestService("KubernetesCRD")
	enableManagedEndpointState(svc)
	require.NoError(t, seedAvailableIPs(t, svc, testNCID, map[string]string{testIPID1: testIP1}))
	db := attachPersistentState(t, svc)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	req := newTestIPConfigsRequest(t, testPod1Info)
	resp, err := svc.requestIPConfigHandlerHelper(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, types.Success, resp.Response.ReturnCode)

	snapshot, err := db.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, testPod1Info.Key(), snapshot.IPOwners[testIPID1])
	require.Contains(t, snapshot.Endpoints, testPod1Info.InfraContainerID())

	resp, err = svc.ReleaseIPConfigHandlerHelper(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, types.Success, resp.Response.ReturnCode)

	snapshot, err = db.Snapshot(context.Background())
	require.NoError(t, err)
	require.NotContains(t, snapshot.IPOwners, testIPID1)
	require.NotContains(t, snapshot.Endpoints, testPod1Info.InfraContainerID())
	require.Contains(t, snapshot.DeleteIntents, testPod1Info.InfraContainerID())
}

func TestPersistentStateDeleteBeforeAdd(t *testing.T) {
	svc := getTestService("KubernetesCRD")
	enableManagedEndpointState(svc)
	require.NoError(t, seedAvailableIPs(t, svc, testNCID, map[string]string{testIPID1: testIP1}))
	db := attachPersistentState(t, svc)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	req := newTestIPConfigsRequest(t, testPod1Info)
	_, err := svc.ReleaseIPConfigHandlerHelper(context.Background(), req)
	require.NoError(t, err)

	resp, err := svc.requestIPConfigHandlerHelper(context.Background(), req)
	require.Error(t, err)
	require.Equal(t, types.FailedToAllocateIPConfig, resp.Response.ReturnCode)
	ipState := svc.PodIPConfigState[testIPID1]
	require.Equal(t, types.Available, ipState.GetState())
	require.Empty(t, svc.EndpointState)
}

func TestPersistentStateCommitFailureRollsBackNewAssignment(t *testing.T) {
	svc := getTestService("KubernetesCRD")
	enableManagedEndpointState(svc)
	require.NoError(t, seedAvailableIPs(t, svc, testNCID, map[string]string{testIPID1: testIP1}))
	db, path := openPersistentStateForTest(t, svc)
	require.NoError(t, db.Close())
	readOnlyDB, err := state.Open(path, state.Options{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readOnlyDB.Close()) })
	svc.SetPersistentStateStore(readOnlyDB)

	req := newTestIPConfigsRequest(t, testPod1Info)
	resp, err := svc.requestIPConfigHandlerHelper(context.Background(), req)
	require.Error(t, err)
	require.Equal(t, types.UnexpectedError, resp.Response.ReturnCode)
	ipState := svc.PodIPConfigState[testIPID1]
	require.Equal(t, types.Available, ipState.GetState())
	require.Empty(t, svc.PodIPIDByPodInterfaceKey[testPod1Info.Key()])
	require.Empty(t, svc.EndpointState)
}

func TestPersistentDurableWriteFailureRestoresCommittedState(t *testing.T) {
	svc := getTestService("KubernetesCRD")
	require.NoError(t, seedAvailableIPs(t, svc, testNCID, map[string]string{testIPID1: testIP1}))
	db, path := openPersistentStateForTest(t, svc)
	require.NoError(t, db.Close())

	readOnlyDB, err := state.Open(path, state.Options{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readOnlyDB.Close()) })
	svc.SetPersistentStateStore(readOnlyDB)
	require.NoError(t, svc.restorePersistentState(context.Background()))

	committed := svc.state.ContainerStatus[testNCID]
	mutated := committed
	mutated.HostVersion = "999"
	svc.state.ContainerStatus[testNCID] = mutated

	require.Error(t, svc.saveState(context.Background()))
	require.Equal(t, committed.HostVersion, svc.state.ContainerStatus[testNCID].HostVersion)
}

func TestHandleDebugPersistentState(t *testing.T) {
	svc := getTestService("KubernetesCRD")
	require.NoError(t, seedAvailableIPs(t, svc, testNCID, map[string]string{testIPID1: testIP1}))
	db, path := openPersistentStateForTest(t, svc)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.Background(), "POST", "/debug/persistentstate", http.NoBody)
	svc.HandleDebugPersistentState(recorder, request)
	require.Equal(t, 200, recorder.Code)

	var response state.DebugResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response)) //nolint:musttag // Snapshot includes legacy CNS request types.
	require.Contains(t, response.Snapshot.IPs, testIPID1)
	require.Equal(t, state.StorageBackendBolt, response.Storage.Backend)
	require.True(t, response.Storage.FilePresent)
	require.Positive(t, response.Storage.FileSizeBytes)
	require.NotContains(t, recorder.Body.String(), path)
}

func openPersistentStateForTest(t *testing.T, svc *HTTPRestService) (db *state.DB, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "azure-cns.db")
	db, err := state.Open(path, state.Options{})
	require.NoError(t, err)
	require.NoError(t, db.ReplaceDurableState(context.Background(), svc.persistentSnapshot()))
	svc.SetPersistentStateStore(db)
	svc.EndpointStateStore = nil
	return db, path
}
