// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/configuration"
	persistentstate "github.com/Azure/azure-container-networking/cns/state"
	"github.com/Azure/azure-container-networking/platform"
	"github.com/Azure/azure-container-networking/processlock"
	"github.com/Azure/azure-container-networking/store"
)

type persistentStatePaths struct {
	legacyCNS      string
	legacyEndpoint string
	database       string
	cnsLock        string
	endpointLock   string
}

type persistentStateStartupConfig struct {
	backend             configuration.StateStoreBackend
	mode                configuration.StateStoreMode
	manageEndpointState bool
	bootPolicy          persistentstate.BootPolicy
	paths               persistentStatePaths
}

type persistentStateProviders struct {
	bootID         func() (string, error)
	lastRebootTime func() (time.Time, error)
	openDatabase   func(string, persistentstate.Options) (*persistentstate.DB, error)
}

// persistentStateResult owns database and must be closed by its caller.
type persistentStateResult struct {
	database            *persistentstate.DB
	legacyCNSStore      store.KeyValueStore
	legacyEndpointStore store.KeyValueStore
	rebooted            bool
	cnsStoreLock        processlock.Interface
	endpointStoreLock   processlock.Interface
}

func persistentStateBootPolicy(channelMode string) persistentstate.BootPolicy {
	resetNetworkContainerReadiness := channelMode == cns.CRD ||
		channelMode == cns.MultiTenantCRD ||
		channelMode == cns.AzureHost
	return platformPersistentStateBootPolicy(resetNetworkContainerReadiness)
}

func (r *persistentStateResult) UnlockLegacyCNSStore() error {
	if r.cnsStoreLock == nil {
		return nil
	}

	lock := r.cnsStoreLock
	r.cnsStoreLock = nil
	if err := lock.Unlock(); err != nil {
		return fmt.Errorf("unlocking legacy CNS state store: %w", err)
	}
	return nil
}

func (r *persistentStateResult) Close() error {
	if r.endpointStoreLock != nil {
		_ = r.endpointStoreLock.Unlock()
		r.endpointStoreLock = nil
	}
	if r.database == nil {
		return nil
	}

	database := r.database
	r.database = nil
	if err := database.Close(); err != nil {
		return fmt.Errorf("closing persistent state database: %w", err)
	}
	return nil
}

func initializePersistentState(
	ctx context.Context,
	config persistentStateStartupConfig,
	providers persistentStateProviders,
) (result persistentStateResult, err error) {
	defer func() {
		if err == nil {
			return
		}
		if closeErr := result.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing persistent state after startup failure: %w", closeErr))
		}
	}()

	if err := platform.CreateDirectory(filepath.Dir(config.paths.legacyCNS)); err != nil {
		return result, fmt.Errorf("creating legacy CNS state directory %q: %w", filepath.Dir(config.paths.legacyCNS), err)
	}
	if config.manageEndpointState {
		if err := platform.CreateDirectory(filepath.Dir(config.paths.legacyEndpoint)); err != nil {
			return result, fmt.Errorf(
				"creating legacy endpoint state directory %q: %w",
				filepath.Dir(config.paths.legacyEndpoint),
				err,
			)
		}
	}

	_, databaseStatErr := os.Stat(config.paths.database)
	databaseExisted := databaseStatErr == nil

	if config.mode == configuration.StateStoreModeRollbackToJSON {
		if err := exportPersistentStateRollback(ctx, config.paths, providers.openDatabase); err != nil {
			return result, err
		}
	}

	switch config.backend {
	case configuration.StateStoreBackendBolt:
		return initializeBoltPersistentState(ctx, config, providers, databaseExisted)
	case configuration.StateStoreBackendJSON:
		return initializeJSONPersistentState(config)
	default:
		return result, fmt.Errorf("%w: unsupported backend %q", configuration.ErrInvalidStateStoreConfig, config.backend)
	}
}

// runPersistentStateStartup transfers ownership of initialized state to start.
func runPersistentStateStartup(
	ctx context.Context,
	config persistentStateStartupConfig,
	providers persistentStateProviders,
	start func(persistentStateResult),
) error {
	result, err := initializePersistentState(ctx, config, providers)
	if err != nil {
		return err
	}

	start(result)
	return nil
}

func exportPersistentStateRollback(
	ctx context.Context,
	paths persistentStatePaths,
	openDatabase func(string, persistentstate.Options) (*persistentstate.DB, error),
) (err error) {
	if _, statErr := os.Stat(paths.database); statErr != nil {
		return fmt.Errorf("finding Bolt state for rollback at %q: %w", paths.database, statErr)
	}

	database, openErr := openDatabase(paths.database, persistentstate.Options{})
	if openErr != nil {
		return fmt.Errorf("opening Bolt state for rollback: %w", openErr)
	}
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			wrappedCloseErr := fmt.Errorf("closing Bolt state after rollback export: %w", closeErr)
			if err == nil {
				err = wrappedCloseErr
				return
			}
			err = errors.Join(err, wrappedCloseErr)
		}
	}()

	if err := database.ExportLegacy(ctx, paths.legacyCNS, paths.legacyEndpoint); err != nil {
		return fmt.Errorf("exporting Bolt state for JSON rollback: %w", err)
	}
	return nil
}

func initializeBoltPersistentState(
	ctx context.Context,
	config persistentStateStartupConfig,
	providers persistentStateProviders,
	databaseExisted bool,
) (persistentStateResult, error) {
	result := persistentStateResult{}
	if err := platform.CreateDirectory(filepath.Dir(config.paths.database)); err != nil {
		return result, fmt.Errorf("creating Bolt state directory %q: %w", filepath.Dir(config.paths.database), err)
	}

	database, err := providers.openDatabase(config.paths.database, persistentstate.Options{})
	if err != nil {
		return result, fmt.Errorf("opening Bolt state store %q: %w", config.paths.database, err)
	}
	result.database = database

	bootID, err := providers.bootID()
	if err != nil {
		return result, fmt.Errorf("determining boot ID: %w", err)
	}
	if importErr := database.ImportLegacy(ctx, persistentstate.ImportOptions{
		CNSJSONPath:         config.paths.legacyCNS,
		EndpointJSONPath:    config.paths.legacyEndpoint,
		ManageEndpointState: config.manageEndpointState,
		BootID:              bootID,
	}); importErr != nil {
		return result, fmt.Errorf("importing legacy CNS state: %w", importErr)
	}
	if ownershipErr := database.SetManagedEndpointState(ctx, config.manageEndpointState); ownershipErr != nil {
		return result, fmt.Errorf("applying endpoint state ownership mode: %w", ownershipErr)
	}

	result.rebooted, err = database.ApplyBoot(ctx, bootID, config.bootPolicy)
	if err != nil {
		return result, fmt.Errorf("applying CNS boot state policy: %w", err)
	}
	if databaseExisted || result.rebooted {
		return result, nil
	}

	legacyInfo, statErr := os.Stat(config.paths.legacyCNS)
	if statErr != nil {
		return result, nil //nolint:nilerr // Legacy reboot detection is a best-effort fallback.
	}
	rebootTime, rebootErr := providers.lastRebootTime()
	if rebootErr == nil && rebootTime.After(legacyInfo.ModTime()) {
		result.rebooted = true
	}
	return result, nil
}

func initializeJSONPersistentState(config persistentStateStartupConfig) (persistentStateResult, error) {
	result := persistentStateResult{}
	cnsStoreLock, err := processlock.NewFileLock(config.paths.cnsLock)
	if err != nil {
		return result, fmt.Errorf("initializing CNS state file lock: %w", err)
	}
	result.cnsStoreLock = cnsStoreLock
	result.legacyCNSStore, err = store.NewJsonFileStore(config.paths.legacyCNS, cnsStoreLock, nil)
	if err != nil {
		return result, fmt.Errorf("creating legacy CNS state store %q: %w", config.paths.legacyCNS, err)
	}
	if !config.manageEndpointState {
		return result, nil
	}

	result.endpointStoreLock, err = processlock.NewFileLock(config.paths.endpointLock)
	if err != nil {
		return result, fmt.Errorf("initializing endpoint state file lock: %w", err)
	}
	result.legacyEndpointStore, err = store.NewJsonFileStore(
		config.paths.legacyEndpoint,
		result.endpointStoreLock,
		nil,
	)
	if err != nil {
		return result, fmt.Errorf("creating legacy endpoint state store %q: %w", config.paths.legacyEndpoint, err)
	}
	return result, nil
}
