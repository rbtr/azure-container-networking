// Copyright 2025 Microsoft. All rights reserved.
// MIT License

//go:build linux

package vethpool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-container-networking/netlink"
	"go.uber.org/zap"
)

const (
	vethPrefix   = "azvp"
	replenishInt = 100 * time.Millisecond
	procIPv6Conf = "/proc/sys/net/ipv6/conf/%s/accept_ra"
)

// ErrPoolEmpty is returned by Acquire when no pre-created veth pairs are available.
var ErrPoolEmpty = errors.New("veth pool empty")

// VethPair holds info about a pre-created veth pair.
type VethPair struct {
	HostName       string
	ContainerName  string
	HostMAC        net.HardwareAddr
	ContainerMAC   net.HardwareAddr
	HostIndex      int
	ContainerIndex int
	MTU            int
}

// Pool maintains a set of pre-created veth pairs ready for immediate use.
type Pool struct {
	mtu      int
	poolSize int
	counter  atomic.Uint64
	ch       chan VethPair
	nl       netlink.NetlinkInterface
	log      *zap.Logger
	cancel   context.CancelFunc
	done     chan struct{}
}

// New creates a Pool. mtu is the MTU to set on created veths.
// poolSize is the target number of ready veths to maintain.
func New(mtu, poolSize int) *Pool {
	return NewWithNetlink(mtu, poolSize, netlink.NewNetlink(), zap.NewNop())
}

// NewWithNetlink creates a Pool with an explicit netlink implementation and logger.
func NewWithNetlink(mtu, poolSize int, nl netlink.NetlinkInterface, log *zap.Logger) *Pool {
	return &Pool{
		mtu:      mtu,
		poolSize: poolSize,
		ch:       make(chan VethPair, poolSize),
		nl:       nl,
		log:      log,
		done:     make(chan struct{}),
	}
}

// Start begins the background veth creation goroutine.
// It cleans up orphaned pool veths, pre-creates poolSize veths, then watches for depletion.
func (p *Pool) Start(ctx context.Context) error {
	p.cleanupOrphans()
	p.fill()

	ctx, p.cancel = context.WithCancel(ctx)
	go p.replenishLoop(ctx)
	return nil
}

// Acquire removes a pre-created veth pair from the pool and returns it.
// Returns ErrPoolEmpty if no veths are available.
func (p *Pool) Acquire() (VethPair, error) {
	select {
	case vp := <-p.ch:
		return vp, nil
	default:
		return VethPair{}, ErrPoolEmpty
	}
}

// Size returns the current number of available veths in the pool.
func (p *Pool) Size() int {
	return len(p.ch)
}

// Close destroys all remaining veths in the pool and stops the background goroutine.
func (p *Pool) Close() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	// Drain and delete remaining veths.
	for {
		select {
		case vp := <-p.ch:
			p.deleteVeth(vp.HostName)
		default:
			return
		}
	}
}

func (p *Pool) replenishLoop(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(replenishInt)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if len(p.ch) < p.poolSize/2 {
				p.fill()
			}
		}
	}
}

// fill creates veths until the pool channel is full.
func (p *Pool) fill() {
	for len(p.ch) < p.poolSize {
		vp, err := p.createVethPair()
		if err != nil {
			p.log.Warn("vethpool: failed to create veth pair", zap.Error(err))
			continue
		}
		select {
		case p.ch <- vp:
		default:
			// Channel full; delete the surplus veth we just created.
			p.deleteVeth(vp.HostName)
			return
		}
	}
}

func (p *Pool) createVethPair() (VethPair, error) {
	n := p.counter.Add(1) - 1
	hostName := fmt.Sprintf("%s%d", vethPrefix, n)
	contName := fmt.Sprintf("%s%dc", vethPrefix, n)

	veth := &netlink.VEthLink{
		LinkInfo: netlink.LinkInfo{
			Type: netlink.LINK_TYPE_VETH,
			Name: hostName,
		},
		PeerName: contName,
	}

	if err := p.nl.AddLink(veth); err != nil {
		return VethPair{}, fmt.Errorf("addlink %s: %w", hostName, err)
	}

	// Look up interfaces to get indices.
	hostIf, err := net.InterfaceByName(hostName)
	if err != nil {
		p.deleteVeth(hostName)
		return VethPair{}, fmt.Errorf("lookup %s: %w", hostName, err)
	}
	contIf, err := net.InterfaceByName(contName)
	if err != nil {
		p.deleteVeth(hostName)
		return VethPair{}, fmt.Errorf("lookup %s: %w", contName, err)
	}

	// Batch: bring up host side, set MTU on both ends.
	batch := netlink.NewHostSetupBatch()
	batch.SetLinkUp(hostIf.Index)
	batch.SetLinkMTU(hostIf.Index, p.mtu)
	batch.SetLinkMTU(contIf.Index, p.mtu)
	if err := batch.Execute(); err != nil {
		p.deleteVeth(hostName)
		return VethPair{}, fmt.Errorf("batch execute for %s: %w", hostName, err)
	}

	// Disable IPv6 router advertisements on host veth.
	raPath := fmt.Sprintf(procIPv6Conf, hostName)
	if err := os.WriteFile(raPath, []byte("0"), 0o644); err != nil {
		p.log.Warn("vethpool: failed to disable RA", zap.String("path", raPath), zap.Error(err))
	}

	// Re-read interfaces to get updated MACs (they may change after link-up).
	hostIf, err = net.InterfaceByName(hostName)
	if err != nil {
		p.deleteVeth(hostName)
		return VethPair{}, fmt.Errorf("re-lookup %s: %w", hostName, err)
	}
	contIf, err = net.InterfaceByName(contName)
	if err != nil {
		p.deleteVeth(hostName)
		return VethPair{}, fmt.Errorf("re-lookup %s: %w", contName, err)
	}

	return VethPair{
		HostName:       hostName,
		ContainerName:  contName,
		HostMAC:        hostIf.HardwareAddr,
		ContainerMAC:   contIf.HardwareAddr,
		HostIndex:      hostIf.Index,
		ContainerIndex: contIf.Index,
		MTU:            p.mtu,
	}, nil
}

func (p *Pool) deleteVeth(hostName string) {
	if err := p.nl.DeleteLink(hostName); err != nil {
		p.log.Warn("vethpool: failed to delete veth", zap.String("name", hostName), zap.Error(err))
	}
}

// cleanupOrphans removes any interfaces with the pool naming prefix left from a previous run.
func (p *Pool) cleanupOrphans() {
	ifaces, err := net.Interfaces()
	if err != nil {
		p.log.Warn("vethpool: failed to list interfaces for orphan cleanup", zap.Error(err))
		return
	}
	for i := range ifaces {
		if strings.HasPrefix(ifaces[i].Name, vethPrefix) {
			// Only delete the host-side; deleting it removes the peer too.
			if !strings.HasSuffix(ifaces[i].Name, "c") {
				p.deleteVeth(ifaces[i].Name)
			}
		}
	}
}
