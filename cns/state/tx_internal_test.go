// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//nolint:goconst // Repeated values keep state fixtures readable.
package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func TestWriteTxPersistenceFailures(t *testing.T) {
	db := openInternalTestDB(t)
	require.NoError(t, db.db.View(func(rawTx *bolt.Tx) error {
		tx := &WriteTx{ReadTx: ReadTx{tx: rawTx}}
		tests := []struct {
			name string
			run  func() error
		}{
			{name: "PutMetadata", run: func() error { return tx.PutMetadata(Metadata{}) }},
			{name: "DeleteNetworkContainer", run: func() error { return tx.DeleteNetworkContainer("nc-1") }},
			{name: "DeleteIP", run: func() error { return tx.DeleteIP("ip-1") }},
			{name: "PutNetwork", run: func() error { return tx.PutNetwork(NetworkRecord{NetworkName: "network-1"}) }},
			{name: "DeleteNetwork", run: func() error { return tx.DeleteNetwork("network-1") }},
			{name: "PutOrchestratorContext", run: func() error { return tx.PutOrchestratorContext("context", []string{"nc-1"}) }},
			{name: "DeleteOrchestratorContext", run: func() error { return tx.DeleteOrchestratorContext("context") }},
			{name: "PutPnPIDByMAC", run: func() error { return tx.PutPnPIDByMAC("mac", "pnp") }},
			{name: "DeletePnPIDByMAC", run: func() error { return tx.DeletePnPIDByMAC("mac") }},
			{name: "ClearState", run: tx.ClearState},
			{name: "ClearDurableState", run: tx.ClearDurableState},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				require.ErrorIs(t, tt.run(), bolterrors.ErrTxNotWritable)
			})
		}
		return nil
	}))
}

func TestPutJSONEncodingFailure(t *testing.T) {
	db := openInternalTestDB(t)
	err := db.Update(context.Background(), func(tx *WriteTx) error {
		return tx.PutNetwork(NetworkRecord{
			NetworkName: "network-1",
			Options:     map[string]any{"unsupported": make(chan int)},
		})
	})
	require.Error(t, err)
}

func TestSingularGettersAttributeCorruption(t *testing.T) {
	tests := []struct {
		name   string
		bucket []byte
		key    string
		get    func(*ReadTx) error
	}{
		{
			name:   "network container",
			bucket: bucketNetworkContainers,
			key:    "nc-1",
			get: func(tx *ReadTx) error {
				_, err := tx.NetworkContainer("nc-1")
				return err
			},
		},
		{
			name:   "IP",
			bucket: bucketIPs,
			key:    "ip-1",
			get: func(tx *ReadTx) error {
				_, err := tx.IP("ip-1")
				return err
			},
		},
		{
			name:   "network",
			bucket: bucketNetworks,
			key:    "network-1",
			get: func(tx *ReadTx) error {
				_, err := tx.Network("network-1")
				return err
			},
		},
		{
			name:   "assignment",
			bucket: bucketAssignments,
			key:    "pod-1",
			get: func(tx *ReadTx) error {
				_, err := tx.Assignment("pod-1")
				return err
			},
		},
		{
			name:   "endpoint",
			bucket: bucketEndpoints,
			key:    "container-1",
			get: func(tx *ReadTx) error {
				_, err := tx.Endpoint("container-1")
				return err
			},
		},
		{
			name:   "delete intent",
			bucket: bucketDeleteIntents,
			key:    "container-1",
			get: func(tx *ReadTx) error {
				_, err := tx.DeleteIntent("container-1")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openInternalTestDB(t)
			require.NoError(t, db.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(tt.bucket).Put([]byte(tt.key), []byte("{"))
			}))
			require.NoError(t, db.View(context.Background(), func(tx *ReadTx) error {
				err := tt.get(tx)
				require.ErrorContains(t, err, string(tt.bucket))
				require.ErrorContains(t, err, tt.key)
				return nil
			}))
		})
	}
}

func TestIntegerDecodingInvalidLengths(t *testing.T) {
	assert.Zero(t, bytesUint32(nil))
	assert.Zero(t, bytesUint32([]byte{1}))
	assert.Zero(t, bytesUint64(nil))
	assert.Zero(t, bytesUint64([]byte{1}))
}
