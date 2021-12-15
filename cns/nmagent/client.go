package nmagent

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/common"
	"github.com/pkg/errors"
)

const (
	// GetNmAgentSupportedApiURLFmt Api endpoint to get supported Apis of NMAgent
	GetNmAgentSupportedApiURLFmt       = "http://%s/machine/plugins/?comp=nmagent&type=GetSupportedApis"
	GetNetworkContainerVersionURLFmt   = "http://%s/machine/plugins/?comp=nmagent&type=NetworkManagement/interfaces/%s/networkContainers/%s/version/authenticationToken/%s/api-version/1"
	GetNcVersionListWithOutTokenURLFmt = "http://%s/machine/plugins/?comp=nmagent&type=NetworkManagement/interfaces/api-version/%s"
)

const (
	contentTypeJSON   = "application/json"
	defaultBaseURL    = "http://168.63.129.16"
	dialerTimeout     = 5 * time.Second
	headerContentType = "Content-Type"
	headerTimeout     = 120 * time.Second
)

// WireServerIP - wire server ip
var (
	WireserverIP                           = "168.63.129.16"
	getNcVersionListWithOutTokenURLVersion = "2"
)

// NetworkContainerResponse - NMAgent response.
type NetworkContainerResponse struct {
	ResponseCode       string `json:"httpStatusCode"`
	NetworkContainerID string `json:"networkContainerId"`
	Version            string `json:"version"`
}

type SupportedAPIsResponseXML struct {
	SupportedApis []string `xml:"type"`
}

type ContainerInfo struct {
	NetworkContainerID string `json:"networkContainerId"`
	Version            string `json:"version"`
}

type NetworkContainerListResponse struct {
	ResponseCode string          `json:"httpStatusCode"`
	Containers   []ContainerInfo `json:"networkContainers"`
}

// Client is client to handle queries to nmagent
type Client struct {
	baseURL *url.URL
	client  *http.Client
}

// NewClient create a new nmagent client.
func NewClient(baseURL string, client *http.Client) (*Client, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		dialer := &net.Dialer{
			Timeout: dialerTimeout,
		}
		client = &http.Client{
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				ResponseHeaderTimeout: headerTimeout,
			},
		}
	}
	// if url == "" {
	// 	url = fmt.Sprintf(GetNcVersionListWithOutTokenURLFmt, WireserverIP, getNcVersionListWithOutTokenURLVersion)
	// }
	return &Client{
		baseURL: u,
		client:  client,
	}, nil
}

var ErrFailedToJoinNetwork = errors.New("failed to join network")

// JoinNetwork joins the given network
func (c *Client) JoinNetwork(networkID, joinNetworkURL string) error {
	logger.Printf("[NMAgentClient] JoinNetwork: %s", networkID)

	// Empty body is required as wireserver cannot handle a post without the body.
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(""); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, joinNetworkURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set(headerContentType, contentTypeJSON)
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	return errors.Wrapf(ErrFailedToJoinNetwork, res.Status)
}

// PublishNetworkContainer publishes given network container
func PublishNetworkContainer(networkContainerID, createNetworkContainerURL string, requestBodyData []byte) (*http.Response, error) {
	logger.Printf("[NMAgentClient] PublishNetworkContainer NC: %s", networkContainerID)

	requestBody := bytes.NewBuffer(requestBodyData)
	response, err := common.GetHttpClient().Post(createNetworkContainerURL, "application/json", requestBody)

	logger.Printf("[NMAgentClient][Response] Publish NC: %s. Response: %+v. Error: %v",
		networkContainerID, response, err)

	return response, err
}

// UnpublishNetworkContainer unpublishes given network container
func UnpublishNetworkContainer(networkContainerID, deleteNetworkContainerURL string) (*http.Response, error) {
	logger.Printf("[NMAgentClient] UnpublishNetworkContainer NC: %s", networkContainerID)

	// Empty body is required as wireserver cannot handle a post without the body.
	var body bytes.Buffer
	json.NewEncoder(&body).Encode("")
	response, err := common.GetHttpClient().Post(deleteNetworkContainerURL, "application/json", &body)

	logger.Printf("[NMAgentClient][Response] Unpublish NC: %s. Response: %+v. Error: %v",
		networkContainerID, response, err)

	return response, err
}

// GetNetworkContainerVersion :- Retrieves NC version from NMAgent
func GetNetworkContainerVersion(networkContainerID, getNetworkContainerVersionURL string) (*http.Response, error) {
	logger.Printf("[NMAgentClient] GetNetworkContainerVersion NC: %s", networkContainerID)

	response, err := common.GetHttpClient().Get(getNetworkContainerVersionURL)

	logger.Printf("[NMAgentClient][Response] GetNetworkContainerVersion NC: %s. Response: %+v. Error: %v",
		networkContainerID, response, err)
	return response, err
}

// GetNmAgentSupportedApis :- Retrieves Supported Apis from NMAgent
func GetNmAgentSupportedApis(httpc *http.Client, supportedAPIsURL *url.URL) ([]string, error) {
	var returnErr error

	// if supportedAPIsURL == nil {
	// 	supportedAPIsURL =
	// }

	// if getNmAgentSupportedApisURL == "" {
	// 	getNmAgentSupportedApisURL = fmt.Sprintf(
	// 		GetNmAgentSupportedApiURLFmt, WireserverIP)
	// }

	req, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, getNmAgentSupportedApisURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build request")
	}

	resp, err := httpc.Do(req)
	if err != nil {
		returnErr = fmt.Errorf(
			"Failed to retrieve Supported Apis from NMAgent with error %v",
			err.Error())
		logger.Errorf("[Azure-CNS] %s", returnErr)
		return nil, returnErr
	}
	if resp == nil {
		returnErr = fmt.Errorf(
			"Response from getNmAgentSupportedApis call is <nil>")
		logger.Errorf("[Azure-CNS] %s", returnErr)
		return nil, returnErr
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read response body")
	}
	if resp.StatusCode != http.StatusOK {
		returnErr = fmt.Errorf(
			"Failed to retrieve Supported Apis from NMAgent with StatusCode: %d",
			resp.StatusCode)
		logger.Errorf("[Azure-CNS] %s", returnErr)
		return nil, returnErr
	}

	var xmlDoc SupportedAPIsResponseXML
	err = xml.NewDecoder(bytes.NewReader(b)).Decode(&xmlDoc)
	if err != nil {
		returnErr = fmt.Errorf(
			"Failed to decode XML response of Supported Apis from NMAgent with error %v",
			err.Error())
		logger.Errorf("[Azure-CNS] %s", returnErr)
		return nil, returnErr
	}

	logger.Printf("[NMAgentClient][Response] GetNmAgentSupportedApis. Response: %+v.", resp)
	return xmlDoc.SupportedApis, nil
}

// GetNCVersionList query nmagent for programmed container versions.
func (c *Client) GetNCVersionList(ctx context.Context) (*NetworkContainerListResponse, error) {
	now := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.connectionURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build nmagent request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to make nmagent request")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read response body")
	}
	logger.Printf("[NMAgentClient][Response] GetNcVersionListWithOutToken response: %s, latency is %d", string(b), time.Since(now).Milliseconds())

	if resp.StatusCode != http.StatusOK {
		return nil, errors.Wrap(err, "failed to GetNCVersionList")
	}

	var response NetworkContainerListResponse
	if err := json.Unmarshal(b, &response); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response")
	}
	return &response, nil
}
