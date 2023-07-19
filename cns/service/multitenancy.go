package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/configuration"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/cns/multitenantcontroller"
	"github.com/Azure/azure-container-networking/cns/multitenantcontroller/multitenantoperator"
	"github.com/Azure/azure-container-networking/cns/restserver"
	ctrl "sigs.k8s.io/controller-runtime"
)

func initializeMultiTenantController(ctx context.Context, httpRestService cns.HTTPService, cnsconfig configuration.CNSConfig) error {
	var multiTenantController multitenantcontroller.RequestController
	kubeConfig, err := ctrl.GetConfig()
	kubeConfig.UserAgent = fmt.Sprintf("azure-cns-%s", version)
	if err != nil {
		return err
	}

	// convert interface type to implementation type
	httpRestServiceImpl, ok := httpRestService.(*restserver.HTTPRestService)
	if !ok {
		logger.Errorf("Failed to convert interface httpRestService to implementation: %v", httpRestService)
		return fmt.Errorf("Failed to convert interface httpRestService to implementation: %v",
			httpRestService)
	}

	// Set orchestrator type
	orchestrator := cns.SetOrchestratorTypeRequest{
		OrchestratorType: cns.Kubernetes,
	}
	httpRestServiceImpl.SetNodeOrchestrator(&orchestrator)

	// Create multiTenantController.
	multiTenantController, err = multitenantoperator.New(httpRestServiceImpl, kubeConfig)
	if err != nil {
		logger.Errorf("Failed to create multiTenantController:%v", err)
		return err
	}

	// Wait for multiTenantController to start.
	go func() {
		for {
			if err := multiTenantController.Start(ctx); err != nil {
				logger.Errorf("Failed to start multiTenantController: %v", err)
			} else {
				logger.Printf("Exiting multiTenantController")
				return
			}

			// Retry after 1sec
			time.Sleep(time.Second)
		}
	}()
	for {
		if multiTenantController.IsStarted() {
			logger.Printf("MultiTenantController is started")
			break
		}

		logger.Printf("Waiting for multiTenantController to start...")
		time.Sleep(time.Millisecond * 500)
	}

	// TODO: do we need this to be running?
	logger.Printf("Starting SyncHostNCVersion")
	go func() {
		// Periodically poll vfp programmed NC version from NMAgent
		tickerChannel := time.Tick(time.Duration(cnsconfig.SyncHostNCVersionIntervalMs) * time.Millisecond)
		for {
			select {
			case <-tickerChannel:
				timedCtx, cancel := context.WithTimeout(ctx, time.Duration(cnsconfig.SyncHostNCVersionIntervalMs)*time.Millisecond)
				httpRestServiceImpl.SyncHostNCVersion(timedCtx, cnsconfig.ChannelMode)
				cancel()
			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}
