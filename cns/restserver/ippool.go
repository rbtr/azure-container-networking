package restserver

import (
	"net/netip"

	"github.com/Azure/azure-container-networking/cns"
)

// ipPool maintains stacks of available IP IDs keyed by NC×IPFamily.
// Pop and Push are O(1). Remove is O(n) per stack but is only used on
// cold paths (NC reconciliation, not pod startup).
//
// All methods must be called while holding the service write lock.
type ipPool struct {
	stacks map[string][]string
}

func newIPPool() *ipPool {
	return &ipPool{stacks: make(map[string][]string)}
}

// ipPoolKeyFromIP derives the pool key from an IPConfigurationStatus.
// It matches generateAssignedIPKey format for consistency.
func ipPoolKeyFromIP(ip cns.IPConfigurationStatus) string {
	family := cns.IPv4
	if addr, err := netip.ParseAddr(ip.IPAddress); err == nil && addr.Is6() {
		family = cns.IPv6
	}
	return generateAssignedIPKey(ip.NCID, family)
}

// Push adds an IP ID to the available pool for its NC×family key.
func (p *ipPool) Push(key, ipID string) {
	p.stacks[key] = append(p.stacks[key], ipID)
}

// Pop removes and returns an available IP ID from the given key's stack.
func (p *ipPool) Pop(key string) (string, bool) {
	stack := p.stacks[key]
	if len(stack) == 0 {
		return "", false
	}
	id := stack[len(stack)-1]
	p.stacks[key] = stack[:len(stack)-1]
	return id, true
}

// Remove removes a specific IP ID from any stack in the pool.
// Used on cold paths only (NC reconciliation, IP release marking).
func (p *ipPool) Remove(ipID string) {
	for key, stack := range p.stacks {
		for i, id := range stack {
			if id == ipID {
				p.stacks[key] = append(stack[:i], stack[i+1:]...)
				return
			}
		}
	}
}

// PopN removes and returns up to n IP IDs from the given key's stack.
func (p *ipPool) PopN(key string, n int) []string {
	stack := p.stacks[key]
	if n > len(stack) {
		n = len(stack)
	}
	if n == 0 {
		return nil
	}
	ids := make([]string, n)
	copy(ids, stack[len(stack)-n:])
	p.stacks[key] = stack[:len(stack)-n]
	return ids
}

// PushAll adds multiple IP IDs to the pool for the given key.
func (p *ipPool) PushAll(key string, ids []string) {
	p.stacks[key] = append(p.stacks[key], ids...)
}

// Len returns the number of available IPs for a given key.
func (p *ipPool) Len(key string) int {
	return len(p.stacks[key])
}

// Total returns the total number of available IPs across all keys.
func (p *ipPool) Total() int {
	n := 0
	for _, stack := range p.stacks {
		n += len(stack)
	}
	return n
}
