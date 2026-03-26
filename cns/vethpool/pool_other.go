// Copyright 2025 Microsoft. All rights reserved.
// MIT License

//go:build !linux

package vethpool

import (
	"context"
	"errors"
	"net"
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

// Pool is a no-op on non-Linux platforms.
type Pool struct{}

// New creates a Pool (no-op on non-Linux).
func New(mtu, poolSize int) *Pool { return &Pool{} }

// Start is a no-op on non-Linux platforms.
func (p *Pool) Start(ctx context.Context) error { return nil }

// Acquire always returns ErrPoolEmpty on non-Linux platforms.
func (p *Pool) Acquire() (VethPair, error) { return VethPair{}, ErrPoolEmpty }

// Size always returns 0 on non-Linux platforms.
func (p *Pool) Size() int { return 0 }

// Close is a no-op on non-Linux platforms.
func (p *Pool) Close() {}
