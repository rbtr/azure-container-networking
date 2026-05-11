// Package scaler wraps `az aks nodepool scale` for driving Node creation.
package scaler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Scaler scales a single AKS nodepool.
type Scaler struct {
	ResourceGroup string
	Cluster       string
	Nodepool      string
}

// New returns a Scaler configured for the given nodepool.
func New(rg, cluster, nodepool string) *Scaler {
	return &Scaler{ResourceGroup: rg, Cluster: cluster, Nodepool: nodepool}
}

// Count returns the current nodepool node count reported by ARM.
func (s *Scaler) Count(ctx context.Context) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, "az", "aks", "nodepool", "show",
		"-g", s.ResourceGroup, "--cluster-name", s.Cluster, "-n", s.Nodepool,
		"--query", "count", "-o", "json",
	).Output()
	if err != nil {
		return 0, fmt.Errorf("az nodepool show: %w", err)
	}
	var n int
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &n); err != nil {
		return 0, fmt.Errorf("parse count %q: %w", string(out), err)
	}
	return n, nil
}

// ProvisioningState returns the current ARM provisioningState of the nodepool
// (e.g. "Succeeded", "Scaling", "Updating", "Failed").
func (s *Scaler) ProvisioningState(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, "az", "aks", "nodepool", "show",
		"-g", s.ResourceGroup, "--cluster-name", s.Cluster, "-n", s.Nodepool,
		"--query", "provisioningState", "-o", "tsv",
	).Output()
	if err != nil {
		return "", fmt.Errorf("az nodepool show provisioningState: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// WaitForReady polls ProvisioningState until "Succeeded" or context expires.
// Used between iterations so that the next Scale call doesn't race a
// still-in-flight ARM operation.
func (s *Scaler) WaitForReady(ctx context.Context) error {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		state, err := s.ProvisioningState(ctx)
		if err == nil && state == "Succeeded" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Scale issues `az aks nodepool scale` asynchronously and returns the time
// ARM accepted the request.
func (s *Scaler) Scale(ctx context.Context, target int) (time.Time, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	submit := time.Now().UTC()
	cmd := exec.CommandContext(cctx, "az", "aks", "nodepool", "scale",
		"-g", s.ResourceGroup, "--cluster-name", s.Cluster, "-n", s.Nodepool,
		"--node-count", fmt.Sprintf("%d", target),
		"--no-wait",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return submit, fmt.Errorf("az nodepool scale: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return submit, nil
}
