package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/log"
	"github.com/avast/retry-go/v3"
	"github.com/pkg/errors"
)

// NodeInterrogator is functionality necessary to read information about nodes.
// It is intended to be strictly read-only.
type NodeInterrogator interface {
	SupportedAPIs(context.Context) ([]string, error)
}

// RegisterNode - Tries to register node with DNC when CNS is started in managed DNC mode
func registerNode(httpc *http.Client, httpRestService cns.HTTPService, dncEP, infraVnet, nodeID string, ni NodeInterrogator) error {
	logger.Printf("[Azure CNS] Registering node %s with Infrastructure Network: %s PrivateEndpoint: %s", nodeID, infraVnet, dncEP)

	var (
		numCPU              = runtime.NumCPU()
		url                 = fmt.Sprintf(common.RegisterNodeURLFmt, dncEP, infraVnet, nodeID, dncApiVersion)
		nodeRegisterRequest cns.NodeRegisterRequest
	)

	nodeRegisterRequest.NumCores = numCPU
	supportedApis, retErr := ni.SupportedAPIs(context.TODO())

	if retErr != nil {
		logger.Errorf("[Azure CNS] Failed to retrieve SupportedApis from NMagent of node %s with Infrastructure Network: %s PrivateEndpoint: %s",
			nodeID, infraVnet, dncEP)
		return retErr
	}

	// To avoid any null-pointer deferencing errors.
	if supportedApis == nil {
		supportedApis = []string{}
	}

	nodeRegisterRequest.NmAgentSupportedApis = supportedApis

	// CNS tries to register Node for maximum of an hour.
	err := retry.Do(func() error {
		return sendRegisterNodeRequest(httpc, httpRestService, nodeRegisterRequest, url)
	}, retry.Delay(common.FiveSeconds), retry.Attempts(maxRetryNodeRegister), retry.DelayType(retry.FixedDelay))

	return errors.Wrap(err, fmt.Sprintf("[Azure CNS] Failed to register node %s after maximum reties for an hour with Infrastructure Network: %s PrivateEndpoint: %s",
		nodeID, infraVnet, dncEP))
}

// sendRegisterNodeRequest func helps in registering the node until there is an error.
func sendRegisterNodeRequest(httpc *http.Client, httpRestService cns.HTTPService, nodeRegisterRequest cns.NodeRegisterRequest, registerURL string) error {
	var body bytes.Buffer
	err := json.NewEncoder(&body).Encode(nodeRegisterRequest)
	if err != nil {
		log.Errorf("[Azure CNS] Failed to register node while encoding json failed with non-retriable err %v", err)
		return errors.Wrap(retry.Unrecoverable(err), "failed to sendRegisterNodeRequest")
	}

	response, err := httpc.Post(registerURL, "application/json", &body)
	if err != nil {
		logger.Errorf("[Azure CNS] Failed to register node with retriable err: %+v", err)
		return errors.Wrap(err, "failed to sendRegisterNodeRequest")
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		err = fmt.Errorf("[Azure CNS] Failed to register node, DNC replied with http status code %s", strconv.Itoa(response.StatusCode))
		logger.Errorf(err.Error())
		return errors.Wrap(err, "failed to sendRegisterNodeRequest")
	}

	var req cns.SetOrchestratorTypeRequest
	err = json.NewDecoder(response.Body).Decode(&req)
	if err != nil {
		log.Errorf("[Azure CNS] decoding Node Resgister response json failed with err %v", err)
		return errors.Wrap(err, "failed to sendRegisterNodeRequest")
	}
	httpRestService.SetNodeOrchestrator(&req)

	logger.Printf("[Azure CNS] Node Registered")
	return nil
}
