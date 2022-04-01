// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package common

import (
	"github.com/Azure/azure-container-networking/cns/logger"
	acn "github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/server/tls"
	"github.com/Azure/azure-container-networking/store"
)

// Service implements behavior common to all services.
type Service struct {
	Name    string
	Version string
	Options map[string]interface{}
	ErrChan chan<- error
	Store   store.KeyValueStore
}

// ServiceAPI defines base interface.
type ServiceAPI interface {
	Init(chan<- error, ServiceConfig) error
	Start(chan<- error) error
	Stop()
	GetOption(string) interface{}
	SetOption(string, interface{})
}

// ServiceConfig specifies common configuration.
type ServiceConfig struct {
	ChannelModeManaged bool
	Name               string
	Version            string
	Listener           *acn.Listener
	Store              store.KeyValueStore
	TLSSettings        tls.TlsSettings
}

// NewService creates a new Service object.
func NewService(name, version string, kvstore store.KeyValueStore) (*Service, error) {
	logger.Debugf("[Azure CNS] Going to create a service object with name: %v. version: %v", name, version)

	svc := &Service{
		Name:    name,
		Version: version,
		Options: make(map[string]interface{}),
		Store:   kvstore,
	}

	logger.Debugf("[Azure CNS] Finished creating service object with name: %v. version: %v", name, version)
	return svc, nil
}

// Initialize initializes the service.
func (service *Service) Initialize(config ServiceConfig) error { //nolint:gocritic // ignore hugeParam
	logger.Debugf("[Azure CNS] Going to initialize the service: %+v with config: %+v.", service, config)

	service.Store = config.Store
	service.Version = config.Version

	logger.Debugf("[Azure CNS] nitialized service: %+v with config: %+v.", service, config)

	return nil
}

// Uninitialize cleans up the service.
func (service *Service) Uninitialize() {
}

// GetOption gets the option value for the given key.
func (service *Service) GetOption(key string) interface{} {
	return service.Options[key]
}

// SetOption sets the option value for the given key.
func (service *Service) SetOption(key string, value interface{}) {
	service.Options[key] = value
}
