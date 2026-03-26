package network

import (
	"fmt"
	"net"

	"github.com/Azure/azure-container-networking/netio"
	"github.com/Azure/azure-container-networking/netlink"
	"github.com/Azure/azure-container-networking/network/networkutils"
	"github.com/Azure/azure-container-networking/platform"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	virtualGwIPString     = "169.254.1.1/32"
	defaultGwCidr         = "0.0.0.0/0"
	defaultGw             = "0.0.0.0"
	virtualv6GwString     = "fe80::1234:5678:9abc/128"
	defaultv6Cidr         = "::/0"
	ipv4Bits              = 32
	ipv6Bits              = 128
	ipv4FullMask          = 32
	ipv6FullMask          = 128
	defaultHostVethHwAddr = "aa:aa:aa:aa:aa:aa"
)

var errorTransparentEndpointClient = errors.New("TransparentEndpointClient Error")

func newErrorTransparentEndpointClient(err error) error {
	return errors.Wrapf(err, "%s", errorTransparentEndpointClient)
}

type TransparentEndpointClient struct {
	bridgeName        string
	hostPrimaryIfName string
	hostVethName      string
	containerVethName string
	hostPrimaryMac    net.HardwareAddr
	containerMac      net.HardwareAddr
	hostVethMac       net.HardwareAddr
	mode              string
	netlink           netlink.NetlinkInterface
	netioshim         netio.NetIOInterface
	plClient          platform.ExecClient
	netUtilsClient    networkutils.NetworkUtils

	// Batch state for reducing RTNL lock contention. Populated in
	// AddEndpoints and flushed in MoveEndpointsToContainerNS so that
	// post-creation ops (setState, setMTU, addRoutes, moveNetNs) are
	// sent in a single netlink round-trip.
	pendingBatch     *netlink.HostSetupBatch
	hostIfIndex      int
	containerIfIndex int
	batchMTU         int
	batchRoutes      []RouteInfo
}

func NewTransparentEndpointClient(
	extIf *externalInterface,
	hostVethName string,
	containerVethName string,
	mode string,
	nl netlink.NetlinkInterface,
	nioc netio.NetIOInterface,
	plc platform.ExecClient,
) *TransparentEndpointClient {
	client := &TransparentEndpointClient{
		bridgeName:        extIf.BridgeName,
		hostPrimaryIfName: extIf.Name,
		hostVethName:      hostVethName,
		containerVethName: containerVethName,
		hostPrimaryMac:    extIf.MacAddress,
		mode:              mode,
		netlink:           nl,
		netioshim:         nioc,
		plClient:          plc,
		netUtilsClient:    networkutils.NewNetworkUtils(nl, plc),
	}

	return client
}

func (client *TransparentEndpointClient) setArpProxy(ifName string) error {
	cmd := fmt.Sprintf("echo 1 > /proc/sys/net/ipv4/conf/%v/proxy_arp", ifName)
	_, err := client.plClient.ExecuteRawCommand(cmd)
	return err
}

func (client *TransparentEndpointClient) AddEndpoints(epInfo *EndpointInfo) error {
	if _, err := client.netioshim.GetNetworkInterfaceByName(client.hostVethName); err == nil {
		logger.Info("Deleting old host veth", zap.String("hostVethName", client.hostVethName))
		if err = client.netlink.DeleteLink(client.hostVethName); err != nil {
			logger.Error("Failed to delete old", zap.String("hostVethName", client.hostVethName), zap.Error(err))
			return newErrorTransparentEndpointClient(err)
		}
	}

	primaryIf, err := client.netioshim.GetNetworkInterfaceByName(client.hostPrimaryIfName)
	if err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	mac, err := net.ParseMAC(defaultHostVethHwAddr)
	if err != nil {
		logger.Error("Failed to parse the mac addrress", zap.String("defaultHostVethHwAddr", defaultHostVethHwAddr))
	}

	// Create the veth pair (round-trip 1). All subsequent host-namespace
	// netlink operations are batched into a single round-trip 2.
	if err = client.netlink.AddLink(&netlink.VEthLink{
		LinkInfo: netlink.LinkInfo{
			Type:       netlink.LINK_TYPE_VETH,
			Name:       client.hostVethName,
			MacAddress: mac,
		},
		PeerName: client.containerVethName,
	}); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	defer func() {
		if err != nil {
			if delErr := client.netlink.DeleteLink(client.hostVethName); delErr != nil {
				logger.Error("Deleting veth failed on addendpoint failure", zap.Error(delErr))
			}
		}
	}()

	// Disable router advertisement (proc write, not a netlink call).
	if err = client.netUtilsClient.DisableRAForInterface(client.hostVethName); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	containerIf, err := client.netioshim.GetNetworkInterfaceByName(client.containerVethName)
	if err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	client.containerMac = containerIf.HardwareAddr

	hostVethIf, err := client.netioshim.GetNetworkInterfaceByName(client.hostVethName)
	if err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	client.hostVethMac = hostVethIf.HardwareAddr

	// Initialize batch for all remaining host-namespace netlink operations.
	// This batches: setState UP, setMTU ×2, addRoutes, and moveNetNs into
	// a single send+recv cycle (round-trip 2).
	client.pendingBatch = netlink.NewHostSetupBatch()
	client.hostIfIndex = hostVethIf.Index
	client.containerIfIndex = containerIf.Index
	client.batchMTU = primaryIf.MTU

	client.pendingBatch.SetLinkUp(hostVethIf.Index)

	logger.Info("Batching mtu on veth interfaces", zap.Int("MTU", primaryIf.MTU),
		zap.String("hostVethName", client.hostVethName))
	client.pendingBatch.SetLinkMTU(hostVethIf.Index, primaryIf.MTU)
	client.pendingBatch.SetLinkMTU(containerIf.Index, primaryIf.MTU)

	return nil
}

func (client *TransparentEndpointClient) AddEndpointRules(epInfo *EndpointInfo) error {
	var routeInfoList []RouteInfo

	// ip route add <podip> dev <hostveth>
	// This route is needed for incoming packets to pod to route via hostveth
	for _, ipAddr := range epInfo.IPAddresses {
		var (
			routeInfo RouteInfo
			ipNet     net.IPNet
		)

		if ipAddr.IP.To4() != nil {
			ipNet = net.IPNet{IP: ipAddr.IP, Mask: net.CIDRMask(ipv4FullMask, ipv4Bits)}
		} else {
			ipNet = net.IPNet{IP: ipAddr.IP, Mask: net.CIDRMask(ipv6FullMask, ipv6Bits)}
		}
		logger.Info("Adding route for the", zap.String("ip", ipNet.String()))
		routeInfo.Dst = ipNet
		routeInfoList = append(routeInfoList, routeInfo)

		// If batching, add routes directly to the pending batch.
		if client.pendingBatch != nil {
			family := netlink.GetIPAddressFamily(ipNet.IP)
			client.pendingBatch.AddRoute(&netlink.Route{
				Family:    family,
				Dst:       &ipNet,
				LinkIndex: client.hostIfIndex,
			})
		}
	}

	// Store routes for potential fallback.
	client.batchRoutes = routeInfoList

	// If not batching, use the original individual-call path.
	if client.pendingBatch == nil {
		if err := addRoutes(client.netlink, client.netioshim, client.hostVethName, routeInfoList); err != nil {
			return newErrorTransparentEndpointClient(err)
		}
	}

	logger.Info("calling setArpProxy for", zap.String("hostVethName", client.hostVethName))
	if err := client.setArpProxy(client.hostVethName); err != nil {
		logger.Error("setArpProxy failed with", zap.Error(err))
		return err
	}

	return nil
}

func (client *TransparentEndpointClient) DeleteEndpointRules(ep *endpoint) {
	// ip route del <podip> dev <hostveth>
	// Deleting the route set up for routing the incoming packets to pod
	for _, ipAddr := range ep.IPAddresses {
		var (
			routeInfo RouteInfo
			ipNet     net.IPNet
		)

		if ipAddr.IP.To4() != nil {
			ipNet = net.IPNet{IP: ipAddr.IP, Mask: net.CIDRMask(ipv4FullMask, ipv4Bits)}
		} else {
			ipNet = net.IPNet{IP: ipAddr.IP, Mask: net.CIDRMask(ipv6FullMask, ipv6Bits)}
		}

		logger.Info("Deleting route for the", zap.String("ip", ipNet.String()))
		routeInfo.Dst = ipNet
		if err := deleteRoutes(client.netlink, client.netioshim, client.hostVethName, []RouteInfo{routeInfo}); err != nil {
			logger.Error("Failed to delete route on VM for the", zap.String("ip", ipNet.String()), zap.Error(err))
		}
	}
}

func (client *TransparentEndpointClient) MoveEndpointsToContainerNS(epInfo *EndpointInfo, nsID uintptr) error {
	logger.Info("Setting link netns", zap.String("containerVethName", client.containerVethName), zap.String("NetNsPath", epInfo.NetNsPath))

	if client.pendingBatch != nil {
		// Add the netns move as the final operation in the batch.
		client.pendingBatch.SetLinkNetNs(client.containerIfIndex, nsID)

		logger.Info("Executing batched host-namespace netlink operations",
			zap.Int("ops", client.pendingBatch.Len()))

		if err := client.pendingBatch.Execute(); err != nil {
			// Batch failed — fall back to individual netlink calls.
			logger.Error("Batch execute failed, falling back to individual ops", zap.Error(err))
			client.pendingBatch = nil
			return client.executeFallback(nsID)
		}
		client.pendingBatch = nil
		return nil
	}

	// No batch (e.g. tests or non-standard code path): individual call.
	if err := client.netlink.SetLinkNetNs(client.containerVethName, nsID); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	return nil
}

// executeFallback replays all batched operations using individual netlink
// interface calls. This is the recovery path when batch Execute fails.
func (client *TransparentEndpointClient) executeFallback(nsID uintptr) error {
	if err := client.netlink.SetLinkState(client.hostVethName, true); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	if err := client.netlink.SetLinkMTU(client.hostVethName, client.batchMTU); err != nil {
		logger.Error("Fallback: setting mtu failed for hostveth",
			zap.String("hostVethName", client.hostVethName), zap.Error(err))
	}

	if err := client.netlink.SetLinkMTU(client.containerVethName, client.batchMTU); err != nil {
		logger.Error("Fallback: setting mtu failed for containerveth",
			zap.String("containerVethName", client.containerVethName), zap.Error(err))
	}

	if err := addRoutes(client.netlink, client.netioshim, client.hostVethName, client.batchRoutes); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	if err := client.netlink.SetLinkNetNs(client.containerVethName, nsID); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	return nil
}

func (client *TransparentEndpointClient) SetupContainerInterfaces(epInfo *EndpointInfo) error {
	if err := client.netUtilsClient.SetupContainerInterface(client.containerVethName, epInfo.IfName); err != nil {
		return err
	}

	client.containerVethName = epInfo.IfName

	return nil
}

func (client *TransparentEndpointClient) ConfigureContainerInterfacesAndRoutes(epInfo *EndpointInfo) error {
	if err := client.netUtilsClient.AssignIPToInterface(client.containerVethName, epInfo.IPAddresses); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	// ip route del 10.240.0.0/12 dev eth0 (removing kernel subnet route added by above call)
	for _, ipAddr := range epInfo.IPAddresses {
		_, ipnet, _ := net.ParseCIDR(ipAddr.String())
		routeInfo := RouteInfo{
			Dst:      *ipnet,
			Scope:    netlink.RT_SCOPE_LINK,
			Protocol: netlink.RTPROT_KERNEL,
		}
		if err := deleteRoutes(client.netlink, client.netioshim, client.containerVethName, []RouteInfo{routeInfo}); err != nil {
			return newErrorTransparentEndpointClient(err)
		}
	}

	// add route for virtualgwip
	// ip route add 169.254.1.1/32 dev eth0
	virtualGwIP, virtualGwNet, _ := net.ParseCIDR(virtualGwIPString)
	routeInfo := RouteInfo{
		Dst:   *virtualGwNet,
		Scope: netlink.RT_SCOPE_LINK,
	}
	if err := addRoutes(client.netlink, client.netioshim, client.containerVethName, []RouteInfo{routeInfo}); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	if !epInfo.SkipDefaultRoutes {
		// ip route add default via 169.254.1.1 dev eth0
		_, defaultIPNet, _ := net.ParseCIDR(defaultGwCidr)
		dstIP := net.IPNet{IP: net.ParseIP(defaultGw), Mask: defaultIPNet.Mask}
		routeInfo = RouteInfo{
			Dst: dstIP,
			Gw:  virtualGwIP,
		}
		if err := addRoutes(client.netlink, client.netioshim, client.containerVethName, []RouteInfo{routeInfo}); err != nil {
			return err
		}
	} else if err := addRoutes(client.netlink, client.netioshim, client.containerVethName, epInfo.Routes); err != nil {
		return newErrorTransparentEndpointClient(err)
	}

	// arp -s 169.254.1.1 e3:45:f4:ac:34:12 - add static arp entry for virtualgwip to hostveth interface mac
	logger.Info("Adding static arp for IP address and MAC in Container namespace",
		zap.String("address", virtualGwNet.String()), zap.Any("hostVethMac", client.hostVethMac))
	linkInfo := netlink.LinkInfo{
		Name:       client.containerVethName,
		IPAddr:     virtualGwNet.IP,
		MacAddress: client.hostVethMac,
	}

	if err := client.netlink.SetOrRemoveLinkAddress(linkInfo, netlink.ADD, netlink.NUD_PROBE); err != nil {
		return fmt.Errorf("Adding arp in container failed: %w", err)
	}

	// IPv6Mode can be ipv6NAT or dual stack overlay
	// set epInfo ipv6Mode to 'dualStackOverlay' to set ipv6Routes and ipv6NeighborEntries for Linux pod in dualStackOverlay ipam mode
	if epInfo.IPV6Mode != "" {
		if err := client.setupIPV6Routes(); err != nil {
			return err
		}
	}

	if epInfo.IPV6Mode != "" {
		return client.setIPV6NeighEntry()
	}

	return nil
}

func (client *TransparentEndpointClient) setupIPV6Routes() error {
	// add route for virtualgwip
	// ip -6 route add fe80::1234:5678:9abc/128 dev eth0
	virtualGwIP, virtualGwNet, _ := net.ParseCIDR(virtualv6GwString)
	gwRoute := RouteInfo{
		Dst:   *virtualGwNet,
		Scope: netlink.RT_SCOPE_LINK,
	}

	// ip -6 route add default via fe80::1234:5678:9abc dev eth0
	_, defaultIPNet, _ := net.ParseCIDR(defaultv6Cidr)
	logger.Info("Setting up ipv6 routes in container", zap.Any("defaultIPNet", defaultIPNet))
	defaultRoute := RouteInfo{
		Dst: *defaultIPNet,
		Gw:  virtualGwIP,
	}

	return addRoutes(client.netlink, client.netioshim, client.containerVethName, []RouteInfo{gwRoute, defaultRoute})
}

func (client *TransparentEndpointClient) setIPV6NeighEntry() error {
	logger.Info("Add v6 neigh entry for default gw ip")
	hostGwIP, _, _ := net.ParseCIDR(virtualv6GwString)
	linkInfo := netlink.LinkInfo{
		Name:       client.containerVethName,
		IPAddr:     hostGwIP,
		MacAddress: client.hostVethMac,
	}

	if err := client.netlink.SetOrRemoveLinkAddress(linkInfo, netlink.ADD, netlink.NUD_PERMANENT); err != nil {
		logger.Error("Failed setting neigh entry in container", zap.Error(err))
		return fmt.Errorf("Failed setting neigh entry in container: %w", err)
	}

	return nil
}

func (client *TransparentEndpointClient) DeleteEndpoints(_ *endpoint) error {
	return nil
}
