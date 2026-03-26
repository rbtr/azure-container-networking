// Copyright 2025 Microsoft. All rights reserved.
// MIT License

//go:build linux

package vethpool

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/netlink"
	"go.uber.org/zap"
)

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("skipping: requires root / CAP_NET_ADMIN")
	}
}

func newTestPool(t *testing.T, size int) *Pool {
	t.Helper()
	p := NewWithNetlink(1500, size, netlink.NewNetlink(), zap.NewNop())
	t.Cleanup(func() { p.Close() })
	return p
}

func TestPoolStartAndSize(t *testing.T) {
	skipIfNotRoot(t)
	p := newTestPool(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if got := p.Size(); got != 5 {
		t.Fatalf("Size() = %d, want 5", got)
	}
}

func TestPoolAcquire(t *testing.T) {
	skipIfNotRoot(t)
	p := newTestPool(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	vp, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}

	// Verify host-side interface exists.
	hostIf, err := net.InterfaceByName(vp.HostName)
	if err != nil {
		t.Fatalf("host veth %s not found: %v", vp.HostName, err)
	}
	if hostIf.Index != vp.HostIndex {
		t.Errorf("host index = %d, want %d", hostIf.Index, vp.HostIndex)
	}
	if len(vp.HostMAC) == 0 {
		t.Error("HostMAC is empty")
	}
	if len(vp.ContainerMAC) == 0 {
		t.Error("ContainerMAC is empty")
	}
	if vp.MTU != 1500 {
		t.Errorf("MTU = %d, want 1500", vp.MTU)
	}

	// Clean up the acquired veth (pool won't delete it since it was acquired).
	nl := netlink.NewNetlink()
	_ = nl.DeleteLink(vp.HostName)
}

func TestPoolAcquireAll(t *testing.T) {
	skipIfNotRoot(t)
	p := newTestPool(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	nl := netlink.NewNetlink()
	for range 3 {
		vp, err := p.Acquire()
		if err != nil {
			t.Fatalf("Acquire() error: %v", err)
		}
		_ = nl.DeleteLink(vp.HostName)
	}

	_, err := p.Acquire()
	if err != ErrPoolEmpty {
		t.Fatalf("Acquire() error = %v, want ErrPoolEmpty", err)
	}
}

func TestPoolReplenish(t *testing.T) {
	skipIfNotRoot(t)
	p := newTestPool(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Drain the pool.
	nl := netlink.NewNetlink()
	for range 4 {
		vp, err := p.Acquire()
		if err != nil {
			t.Fatalf("Acquire() error: %v", err)
		}
		_ = nl.DeleteLink(vp.HostName)
	}

	// Wait for the replenisher to refill.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.Size() >= 4 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pool did not replenish: Size() = %d, want >= 4", p.Size())
}

func TestPoolClose(t *testing.T) {
	skipIfNotRoot(t)
	p := NewWithNetlink(1500, 3, netlink.NewNetlink(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Grab the names before closing so we can verify deletion.
	var names []string
	// Peek at interfaces to find pool veths.
	ifaces, _ := net.Interfaces()
	for i := range ifaces {
		if strings.HasPrefix(ifaces[i].Name, vethPrefix) && !strings.HasSuffix(ifaces[i].Name, "c") {
			names = append(names, ifaces[i].Name)
		}
	}

	p.Close()

	for _, name := range names {
		if _, err := net.InterfaceByName(name); err == nil {
			t.Errorf("veth %s still exists after Close()", name)
		}
	}
}

func TestPoolCleanupOrphans(t *testing.T) {
	skipIfNotRoot(t)
	nl := netlink.NewNetlink()

	// Manually create orphan veths that look like pool leftovers.
	for i := 9000; i < 9003; i++ {
		hostName := fmt.Sprintf("%s%d", vethPrefix, i)
		contName := fmt.Sprintf("%s%dc", vethPrefix, i)
		veth := &netlink.VEthLink{
			LinkInfo: netlink.LinkInfo{
				Type: netlink.LINK_TYPE_VETH,
				Name: hostName,
			},
			PeerName: contName,
		}
		if err := nl.AddLink(veth); err != nil {
			t.Fatalf("failed to create orphan veth %s: %v", hostName, err)
		}
	}

	// Start a pool; it should clean up orphans.
	p := NewWithNetlink(1500, 2, nl, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Close()

	// Verify orphans are gone.
	for i := 9000; i < 9003; i++ {
		name := fmt.Sprintf("%s%d", vethPrefix, i)
		if _, err := net.InterfaceByName(name); err == nil {
			t.Errorf("orphan veth %s still exists after Start()", name)
		}
	}
}
