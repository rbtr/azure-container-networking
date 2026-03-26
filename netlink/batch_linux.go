// Copyright 2017 Microsoft. All rights reserved.
// MIT License

//go:build linux

package netlink

import (
	"fmt"
	"syscall"

	"github.com/Azure/azure-container-networking/log"
	"golang.org/x/sys/unix"
)

// HostSetupBatch accumulates netlink messages for batch execution in a single
// send+recv round-trip. This reduces RTNL lock contention by acquiring the
// kernel lock once instead of once per operation.
type HostSetupBatch struct {
	entries []batchEntry
}

type batchEntry struct {
	msg *message
	// ignoreExists when true treats EEXIST as success.
	// Used for route additions where the route may already exist.
	ignoreExists bool
}

// NewHostSetupBatch creates a new empty batch.
func NewHostSetupBatch() *HostSetupBatch {
	return &HostSetupBatch{}
}

// SetLinkUp adds an operation to bring an interface up by index.
func (b *HostSetupBatch) SetLinkUp(ifIndex int) {
	req := newRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)
	ifInfo := newIfInfoMsg()
	ifInfo.Type = unix.RTM_SETLINK
	ifInfo.Index = int32(ifIndex)
	ifInfo.Flags = unix.IFF_UP
	ifInfo.Change = unix.IFF_UP
	req.addPayload(ifInfo)
	b.entries = append(b.entries, batchEntry{msg: req})
}

// SetLinkMTU adds an operation to set an interface's MTU by index.
func (b *HostSetupBatch) SetLinkMTU(ifIndex, mtu int) {
	req := newRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	ifInfo := newIfInfoMsg()
	ifInfo.Index = int32(ifIndex)
	req.addPayload(ifInfo)
	req.addPayload(newAttributeUint32(unix.IFLA_MTU, uint32(mtu)))
	b.entries = append(b.entries, batchEntry{msg: req})
}

// AddRoute adds a route creation operation. EEXIST errors are treated as
// success to match the behavior of the individual AddIPRoute path.
func (b *HostSetupBatch) AddRoute(route *Route) {
	req := newRequest(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)

	family := route.Family
	if family == 0 {
		family = unix.AF_INET
	}

	msg := newRtMsg(family)
	msg.Tos = uint8(route.Tos)
	msg.Table = uint8(route.Table)

	if route.Protocol != 0 {
		msg.Protocol = uint8(route.Protocol)
	}
	if route.Scope != 0 {
		msg.Scope = uint8(route.Scope)
	}
	if route.Type != 0 {
		msg.Type = uint8(route.Type)
	}
	msg.Flags = uint32(route.Flags)
	req.addPayload(msg)

	if route.Dst != nil {
		prefixLength, _ := route.Dst.Mask.Size()
		msg.Dst_len = uint8(prefixLength)
		req.addPayload(newAttributeIpAddress(unix.RTA_DST, route.Dst.IP))
	}
	if route.Src != nil {
		req.addPayload(newAttributeIpAddress(unix.RTA_PREFSRC, route.Src))
	}
	if route.Gw != nil {
		req.addPayload(newAttributeIpAddress(unix.RTA_GATEWAY, route.Gw))
	}
	if route.Priority != 0 {
		req.addPayload(newAttributeUint32(unix.RTA_PRIORITY, uint32(route.Priority)))
	}
	if route.LinkIndex != 0 {
		req.addPayload(newAttributeUint32(unix.RTA_OIF, uint32(route.LinkIndex)))
	}
	if route.ILinkIndex != 0 {
		req.addPayload(newAttributeUint32(unix.RTA_IIF, uint32(route.ILinkIndex)))
	}

	b.entries = append(b.entries, batchEntry{msg: req, ignoreExists: true})
}

// SetLinkNetNs adds an operation to move an interface to a network namespace.
func (b *HostSetupBatch) SetLinkNetNs(ifIndex int, fd uintptr) {
	req := newRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	ifInfo := newIfInfoMsg()
	ifInfo.Type = unix.RTM_SETLINK
	ifInfo.Index = int32(ifIndex)
	ifInfo.Flags = unix.NLM_F_REQUEST
	ifInfo.Change = DEFAULT_CHANGE
	req.addPayload(ifInfo)
	req.addPayload(newAttributeUint32(IFLA_NET_NS_FD, uint32(fd)))
	b.entries = append(b.entries, batchEntry{msg: req})
}

// Len returns the number of operations in the batch.
func (b *HostSetupBatch) Len() int {
	return len(b.entries)
}

// Execute sends all accumulated messages and waits for all ACKs in a
// single socket lock acquisition. Returns nil on empty batch.
func (b *HostSetupBatch) Execute() error {
	if len(b.entries) == 0 {
		return nil
	}

	s, err := getSocket()
	if err != nil {
		return fmt.Errorf("batch execute: %w", err)
	}

	s.Lock()
	defer s.Unlock()

	// Send all messages.
	for _, e := range b.entries {
		if err := s.send(e.msg); err != nil {
			return fmt.Errorf("batch send failed at seq %d: %w", e.msg.Seq, err)
		}
	}

	// Receive all ACKs.
	for _, e := range b.entries {
		if _, err := s.receiveResponse(e.msg); err != nil {
			if e.ignoreExists && err == syscall.EEXIST { //nolint:errorlint // syscall.Errno is directly comparable
				log.Debugf("[netlink] Batch: ignoring EEXIST for seq %d\n", e.msg.Seq)
				continue
			}
			return fmt.Errorf("batch ack failed at seq %d: %w", e.msg.Seq, err)
		}
	}

	return nil
}
