// Copyright 2017 Microsoft. All rights reserved.
// MIT License

//go:build linux

package netlink

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestHostSetupBatchEmpty(t *testing.T) {
	b := NewHostSetupBatch()
	require.Equal(t, 0, b.Len())
	require.NoError(t, b.Execute(), "Execute on empty batch should succeed")
}

func TestHostSetupBatchLen(t *testing.T) {
	b := NewHostSetupBatch()
	b.SetLinkUp(1)
	require.Equal(t, 1, b.Len())
	b.SetLinkMTU(1, 1500)
	require.Equal(t, 2, b.Len())
	b.AddRoute(&Route{
		Family:    unix.AF_INET,
		LinkIndex: 1,
	})
	require.Equal(t, 3, b.Len())
	b.SetLinkNetNs(1, 0)
	require.Equal(t, 4, b.Len())
}

// TestBatchSetLinkUpAndMTU creates a veth pair, then uses a batch to bring it
// up and set the MTU in a single round-trip.
func TestBatchSetLinkUpAndMTU(t *testing.T) {
	const (
		vethHost = "batchtest0"
		vethPeer = "batchtest1"
		testMTU  = 1400
	)

	nl := NewNetlink()
	link := VEthLink{
		LinkInfo: LinkInfo{
			Type: LINK_TYPE_VETH,
			Name: vethHost,
		},
		PeerName: vethPeer,
	}
	err := nl.AddLink(&link)
	require.NoError(t, err, "AddLink should create veth pair")
	defer func() { _ = nl.DeleteLink(vethHost) }()

	hostIf, err := net.InterfaceByName(vethHost)
	require.NoError(t, err)
	peerIf, err := net.InterfaceByName(vethPeer)
	require.NoError(t, err)

	b := NewHostSetupBatch()
	b.SetLinkUp(hostIf.Index)
	b.SetLinkMTU(hostIf.Index, testMTU)
	b.SetLinkMTU(peerIf.Index, testMTU)
	require.Equal(t, 3, b.Len())

	err = b.Execute()
	require.NoError(t, err, "batch Execute should succeed")

	// Verify host veth is up.
	hostIf, err = net.InterfaceByName(vethHost)
	require.NoError(t, err)
	require.True(t, hostIf.Flags&net.FlagUp != 0, "host veth should be up")
	require.Equal(t, testMTU, hostIf.MTU, "host veth MTU mismatch")

	// Verify peer MTU.
	peerIf, err = net.InterfaceByName(vethPeer)
	require.NoError(t, err)
	require.Equal(t, testMTU, peerIf.MTU, "peer veth MTU mismatch")
}

// TestBatchAddRoute creates a dummy interface, brings it up, then uses a batch
// to add a route.
func TestBatchAddRoute(t *testing.T) {
	const testIf = "batchrt0"

	dummy, err := addDummyInterface(testIf)
	require.NoError(t, err, "addDummyInterface failed")
	nl := NewNetlink()
	defer func() { _ = nl.DeleteLink(testIf) }()

	err = nl.SetLinkState(testIf, true)
	require.NoError(t, err)

	_, dstNet, _ := net.ParseCIDR("10.99.0.0/24")

	b := NewHostSetupBatch()
	b.AddRoute(&Route{
		Family:    unix.AF_INET,
		Dst:       dstNet,
		LinkIndex: dummy.Index,
		Scope:     RT_SCOPE_LINK,
	})
	require.Equal(t, 1, b.Len())

	err = b.Execute()
	require.NoError(t, err, "batch Execute should add route")

	// Verify route exists.
	routes, err := nl.GetIPRoute(&Route{
		Family:    unix.AF_INET,
		Dst:       dstNet,
		LinkIndex: dummy.Index,
	})
	require.NoError(t, err)
	require.NotEmpty(t, routes, "route should exist after batch add")
}

// TestBatchAddRouteDuplicate verifies that adding a duplicate route in a batch
// is silently ignored (EEXIST treated as success).
func TestBatchAddRouteDuplicate(t *testing.T) {
	const testIf = "batchrtdup"

	dummy, err := addDummyInterface(testIf)
	require.NoError(t, err)
	nl := NewNetlink()
	defer func() { _ = nl.DeleteLink(testIf) }()

	err = nl.SetLinkState(testIf, true)
	require.NoError(t, err)

	_, dstNet, _ := net.ParseCIDR("10.98.0.0/24")
	route := &Route{
		Family:    unix.AF_INET,
		Dst:       dstNet,
		LinkIndex: dummy.Index,
		Scope:     RT_SCOPE_LINK,
	}

	// Add route individually first.
	err = nl.AddIPRoute(route)
	require.NoError(t, err)

	// Now add the same route via batch — should not error.
	b := NewHostSetupBatch()
	b.AddRoute(route)
	err = b.Execute()
	require.NoError(t, err, "duplicate route in batch should not error")
}

// TestSendBatchAndWaitForAcks tests the lower-level socket batch method.
func TestSendBatchAndWaitForAcks(t *testing.T) {
	const (
		vethHost = "batchsock0"
		vethPeer = "batchsock1"
	)

	nl := NewNetlink()
	link := VEthLink{
		LinkInfo: LinkInfo{
			Type: LINK_TYPE_VETH,
			Name: vethHost,
		},
		PeerName: vethPeer,
	}
	err := nl.AddLink(&link)
	require.NoError(t, err)
	defer func() { _ = nl.DeleteLink(vethHost) }()

	hostIf, err := net.InterfaceByName(vethHost)
	require.NoError(t, err)

	// Build two messages: bring up + set MTU.
	msg1 := newRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)
	ifInfo1 := newIfInfoMsg()
	ifInfo1.Type = unix.RTM_SETLINK
	ifInfo1.Index = int32(hostIf.Index)
	ifInfo1.Flags = unix.IFF_UP
	ifInfo1.Change = unix.IFF_UP
	msg1.addPayload(ifInfo1)

	msg2 := newRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	ifInfo2 := newIfInfoMsg()
	ifInfo2.Index = int32(hostIf.Index)
	msg2.addPayload(ifInfo2)
	msg2.addPayload(newAttributeUint32(unix.IFLA_MTU, 1300))

	s, err := getSocket()
	require.NoError(t, err)

	err = s.sendBatchAndWaitForAcks([]*message{msg1, msg2})
	require.NoError(t, err, "sendBatchAndWaitForAcks should succeed")

	hostIf, err = net.InterfaceByName(vethHost)
	require.NoError(t, err)
	require.True(t, hostIf.Flags&net.FlagUp != 0, "interface should be up")
	require.Equal(t, 1300, hostIf.MTU, "MTU should be 1300")
}
