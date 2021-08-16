// Code generated by MockGen. DO NOT EDIT.
// Source: cns/cnsclient/apiclient.go

// Package mockclients is a generated GoMock package.
package mockclients

import (
	reflect "reflect"

	cns "github.com/Azure/azure-container-networking/cns"
	v1alpha "github.com/Azure/azure-container-networking/nodenetworkconfig/api/v1alpha"
	gomock "github.com/golang/mock/gomock"
)

// MockAPIClient is a mock of APIClient interface.
type MockAPIClient struct {
	ctrl     *gomock.Controller
	recorder *MockAPIClientMockRecorder
}

// MockAPIClientMockRecorder is the mock recorder for MockAPIClient.
type MockAPIClientMockRecorder struct {
	mock *MockAPIClient
}

// NewMockAPIClient creates a new mock instance.
func NewMockAPIClient(ctrl *gomock.Controller) *MockAPIClient {
	mock := &MockAPIClient{ctrl: ctrl}
	mock.recorder = &MockAPIClientMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockAPIClient) EXPECT() *MockAPIClientMockRecorder {
	return m.recorder
}

// CreateOrUpdateNC mocks base method.
func (m *MockAPIClient) CreateOrUpdateNC(nc cns.CreateNetworkContainerRequest) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "CreateOrUpdateNC", nc)
	ret0, _ := ret[0].(error)
	return ret0
}

// CreateOrUpdateNC indicates an expected call of CreateOrUpdateNC.
func (mr *MockAPIClientMockRecorder) CreateOrUpdateNC(nc interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CreateOrUpdateNC", reflect.TypeOf((*MockAPIClient)(nil).CreateOrUpdateNC), nc)
}

// DeleteNC mocks base method.
func (m *MockAPIClient) DeleteNC(nc cns.DeleteNetworkContainerRequest) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "DeleteNC", nc)
	ret0, _ := ret[0].(error)
	return ret0
}

// DeleteNC indicates an expected call of DeleteNC.
func (mr *MockAPIClientMockRecorder) DeleteNC(nc interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "DeleteNC", reflect.TypeOf((*MockAPIClient)(nil).DeleteNC), nc)
}

// GetNC mocks base method.
func (m *MockAPIClient) GetNC(nc cns.GetNetworkContainerRequest) (cns.GetNetworkContainerResponse, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetNC", nc)
	ret0, _ := ret[0].(cns.GetNetworkContainerResponse)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetNC indicates an expected call of GetNC.
func (mr *MockAPIClientMockRecorder) GetNC(nc interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetNC", reflect.TypeOf((*MockAPIClient)(nil).GetNC), nc)
}

// ReconcileNCState mocks base method.
func (m *MockAPIClient) ReconcileNCState(nc *cns.CreateNetworkContainerRequest, pods map[string]cns.PodInfo, scalar v1alpha.Scaler, spec v1alpha.NodeNetworkConfigSpec) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ReconcileNCState", nc, pods, scalar, spec)
	ret0, _ := ret[0].(error)
	return ret0
}

// ReconcileNCState indicates an expected call of ReconcileNCState.
func (mr *MockAPIClientMockRecorder) ReconcileNCState(nc, pods, scalar, spec interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ReconcileNCState", reflect.TypeOf((*MockAPIClient)(nil).ReconcileNCState), nc, pods, scalar, spec)
}

// UpdateIPAMPoolMonitor mocks base method.
func (m *MockAPIClient) UpdateIPAMPoolMonitor(scalar v1alpha.Scaler, spec v1alpha.NodeNetworkConfigSpec) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "UpdateIPAMPoolMonitor", scalar, spec)
}

// UpdateIPAMPoolMonitor indicates an expected call of UpdateIPAMPoolMonitor.
func (mr *MockAPIClientMockRecorder) UpdateIPAMPoolMonitor(scalar, spec interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "UpdateIPAMPoolMonitor", reflect.TypeOf((*MockAPIClient)(nil).UpdateIPAMPoolMonitor), scalar, spec)
}
