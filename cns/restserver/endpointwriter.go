// Copyright Microsoft. All rights reserved.
// MIT License

package restserver

import (
	"context"
	"net"

	"github.com/Azure/azure-container-networking/cns/logger"
	cnsstore "github.com/Azure/azure-container-networking/cns/store"
)

// endpointWriter provides async per-record writes of endpoint state to a
// boltdb EndpointBoltStore. In-memory mutations happen under the caller's
// lock; the I/O is serialised through a buffered channel so the service
// lock is never held during disk writes.
//
// Each operation (put or delete) targets a single container ID, so boltdb
// only touches one key per write instead of rewriting the entire map.
type endpointWriter struct {
	store *cnsstore.EndpointBoltStore
	ops   chan endpointOp
	done  chan struct{}
}

type endpointOpKind int

const (
	opPut endpointOpKind = iota
	opDelete
)

type endpointOp struct {
	kind        endpointOpKind
	containerID string
	record      cnsstore.EndpointRecord // only used for opPut
}

const endpointOpBufSize = 256

func newEndpointWriter(s *cnsstore.EndpointBoltStore) *endpointWriter {
	w := &endpointWriter{
		store: s,
		ops:   make(chan endpointOp, endpointOpBufSize),
		done:  make(chan struct{}),
	}
	go w.loop()
	return w
}

// PutEndpoint enqueues an async put for a single container endpoint.
// The EndpointInfo is deep-copied and converted to an EndpointRecord
// so the caller can continue mutating in-memory state without races.
func (w *endpointWriter) PutEndpoint(containerID string, info *EndpointInfo) {
	w.ops <- endpointOp{
		kind:        opPut,
		containerID: containerID,
		record:      endpointInfoToRecord(info),
	}
}

// DeleteEndpoint enqueues an async delete for a single container endpoint.
func (w *endpointWriter) DeleteEndpoint(containerID string) {
	w.ops <- endpointOp{
		kind:        opDelete,
		containerID: containerID,
	}
}

func (w *endpointWriter) loop() {
	defer close(w.done)
	for op := range w.ops {
		ctx := context.Background()
		var err error
		switch op.kind {
		case opPut:
			err = w.store.PutEndpoint(ctx, op.containerID, op.record)
		case opDelete:
			err = w.store.DeleteEndpoint(ctx, op.containerID)
		}
		if err != nil {
			logger.Errorf("[endpointWriter] async %s for %s failed: %v", opName(op.kind), op.containerID, err)
			asyncEndpointWriteFailures.Inc()
		}
	}
}

// Close drains pending operations and stops the background goroutine.
func (w *endpointWriter) Close() {
	close(w.ops)
	<-w.done
}

func opName(k endpointOpKind) string {
	if k == opDelete {
		return "delete"
	}
	return "put"
}

// endpointInfoToRecord converts the restserver EndpointInfo to the
// cns/store EndpointRecord, deep-copying net.IPNet byte slices.
func endpointInfoToRecord(info *EndpointInfo) cnsstore.EndpointRecord {
	rec := cnsstore.EndpointRecord{
		PodName:       info.PodName,
		PodNamespace:  info.PodNamespace,
		IfnameToIPMap: make(map[string]*cnsstore.IPInfoRecord, len(info.IfnameToIPMap)),
	}
	for ifn, ipInfo := range info.IfnameToIPMap {
		rec.IfnameToIPMap[ifn] = ipInfoToRecord(ipInfo)
	}
	return rec
}

func ipInfoToRecord(info *IPInfo) *cnsstore.IPInfoRecord {
	r := &cnsstore.IPInfoRecord{
		HnsEndpointID:      info.HnsEndpointID,
		HnsNetworkID:       info.HnsNetworkID,
		HostVethName:       info.HostVethName,
		MacAddress:         info.MacAddress,
		NetworkContainerID: info.NetworkContainerID,
		NICType:            info.NICType,
	}
	r.IPv4 = deepCopyIPNets(info.IPv4)
	r.IPv6 = deepCopyIPNets(info.IPv6)
	return r
}

func deepCopyIPNets(nets []net.IPNet) []net.IPNet {
	if len(nets) == 0 {
		return nil
	}
	out := make([]net.IPNet, len(nets))
	for i := range nets {
		out[i] = net.IPNet{
			IP:   append(net.IP(nil), nets[i].IP...),
			Mask: append(net.IPMask(nil), nets[i].Mask...),
		}
	}
	return out
}

// EndpointRecordToInfo converts a cns/store EndpointRecord back to the
// restserver EndpointInfo. Used when restoring state from bolt at startup.
func EndpointRecordToInfo(rec cnsstore.EndpointRecord) *EndpointInfo {
	info := &EndpointInfo{
		PodName:       rec.PodName,
		PodNamespace:  rec.PodNamespace,
		IfnameToIPMap: make(map[string]*IPInfo, len(rec.IfnameToIPMap)),
	}
	for ifn, ipRec := range rec.IfnameToIPMap {
		info.IfnameToIPMap[ifn] = ipInfoRecordToIPInfo(ipRec)
	}
	return info
}

func ipInfoRecordToIPInfo(rec *cnsstore.IPInfoRecord) *IPInfo {
	return &IPInfo{
		IPv4:               rec.IPv4,
		IPv6:               rec.IPv6,
		HnsEndpointID:      rec.HnsEndpointID,
		HnsNetworkID:       rec.HnsNetworkID,
		HostVethName:       rec.HostVethName,
		MacAddress:         rec.MacAddress,
		NetworkContainerID: rec.NetworkContainerID,
		NICType:            rec.NICType,
	}
}
