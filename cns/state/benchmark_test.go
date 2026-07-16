// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values make benchmark fixtures easier to read.
package state_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns/state"
)

func BenchmarkSnapshot500IPs(b *testing.B) {
	ctx := context.Background()
	db, err := state.Open(filepath.Join(b.TempDir(), "benchmark.db"), state.Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	record := sampleNCRecord()
	ips := make(map[string]state.IPRecord, 500)
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("ip-%03d", i)
		ips[id] = state.IPRecord{
			ID:        id,
			IPAddress: fmt.Sprintf("10.0.%d.%d", i/250, i%250+1),
			NCID:      record.ID,
			NCVersion: 2,
		}
	}
	if err := db.ApplyNetworkContainer(ctx, record, ips); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Snapshot(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAssignRelease500IPInventory(b *testing.B) {
	ctx := context.Background()
	db, err := state.Open(filepath.Join(b.TempDir(), "benchmark.db"), state.Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	record := sampleNCRecord()
	ips := make(map[string]state.IPRecord, 500)
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("ip-%03d", i)
		ips[id] = state.IPRecord{
			ID:        id,
			IPAddress: fmt.Sprintf("10.0.%d.%d", i/250, i%250+1),
			NCID:      record.ID,
			NCVersion: 2,
		}
	}
	if err := db.ApplyNetworkContainer(ctx, record, ips); err != nil {
		b.Fatal(err)
	}

	assignment := state.AssignmentRecord{
		Pod:   state.PodIdentity{PodKey: "container-1", InfraContainerID: "container-1"},
		IPIDs: []string{"ip-000"},
	}
	endpoint := sampleEndpoint(ips["ip-000"].IPAddress)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := testNow.Add(time.Duration(i) * time.Second)
		if err := db.AssignEndpoint(ctx, assignment, endpoint, now, time.Nanosecond); err != nil {
			b.Fatal(err)
		}
		if err := db.ReleaseEndpoint(ctx, "container-1", "container-1", state.DeleteIntent{CreatedAt: now}, time.Nanosecond); err != nil {
			b.Fatal(err)
		}
	}
}
