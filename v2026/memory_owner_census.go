package connect

import (
	"net/http"
	"unsafe"
)

// These on-demand diagnostics inspect existing owners. They add no registry,
// packet hook, timer or retained references. Counts are point-in-time per owner,
// not an atomic process snapshot. Known bytes cover only named structs/slice
// slots, NOT allocator spans, map buckets, socket buffers or a complete heap.

type TransferOwnerCensus struct {
	Clients                  int64 `json:"clients"`
	SendIndexed              int64 `json:"send_indexed"`
	SendWorkers              int64 `json:"send_workers"`
	SendCanceledWorkers      int64 `json:"send_canceled_workers"`
	ReceiveIndexed           int64 `json:"receive_indexed"`
	ReceiveWorkers           int64 `json:"receive_workers"`
	ReceiveCanceledWorkers   int64 `json:"receive_canceled_workers"`
	PacingServices           int64 `json:"pacing_services"`
	EncryptionSessions       int64 `json:"encryption_sessions"`
	ContractDestinations     int64 `json:"contract_destinations"`
	ContractStatsEntries     int64 `json:"contract_stats_entries"`
	ContractStatsSequences   int64 `json:"contract_stats_sequences"`
	SendPackSlots            int64 `json:"send_pack_slots"`
	SendAckSlots             int64 `json:"send_ack_slots"`
	ReceivePackSlots         int64 `json:"receive_pack_slots"`
	QueuedPacks              int64 `json:"queued_packs"`
	KnownSequenceStructBytes int64 `json:"known_sequence_struct_bytes"`
	KnownPacingStructBytes   int64 `json:"known_pacing_struct_bytes"`
	KnownChannelSlotBytes    int64 `json:"known_channel_slot_bytes"`
}

// addMemoryOwners takes only one owning lock at a time. In particular it must
// not run under a multi-client window lock: sequence teardown calls back into
// the window. Channel handles/capacities are immutable for their worker's life.
func (self *Client) addMemoryOwners(out *TransferOwnerCensus) {
	if self == nil {
		return
	}
	out.Clients++
	if buffer := self.sendBuffer; buffer != nil {
		buffer.mutex.Lock()
		out.SendIndexed += int64(len(buffer.sendSequences))
		out.SendWorkers += int64(len(buffer.activeSendSequences))
		out.PacingServices += int64(len(buffer.windowPacingServices))
		for sequence := range buffer.activeSendSequences {
			if sequence == nil {
				continue
			}
			if sequence.ctx != nil && sequence.ctx.Err() != nil {
				out.SendCanceledWorkers++
			}
			out.SendPackSlots += int64(cap(sequence.packs))
			out.SendAckSlots += int64(cap(sequence.acks))
			out.QueuedPacks += int64(len(sequence.packs))
		}
		buffer.mutex.Unlock()
	}
	if buffer := self.receiveBuffer; buffer != nil {
		buffer.mutex.Lock()
		out.ReceiveIndexed += int64(len(buffer.receiveSequences))
		out.ReceiveWorkers += int64(len(buffer.activeReceiveSequences))
		for sequence := range buffer.activeReceiveSequences {
			if sequence == nil {
				continue
			}
			if sequence.ctx != nil && sequence.ctx.Err() != nil {
				out.ReceiveCanceledWorkers++
			}
			out.ReceivePackSlots += int64(cap(sequence.packs))
			out.QueuedPacks += int64(len(sequence.packs))
		}
		buffer.mutex.Unlock()
	}
	if manager := self.encryptionSessionManager; manager != nil {
		manager.stateLock.Lock()
		out.EncryptionSessions += int64(len(manager.sessions))
		manager.stateLock.Unlock()
	}
	if manager := self.contractManager; manager != nil {
		manager.mutex.Lock()
		out.ContractDestinations += int64(len(manager.destinationContracts))
		manager.mutex.Unlock()
		manager.contractStatsLock.Lock()
		out.ContractStatsEntries += int64(len(manager.contractStatsEntries))
		out.ContractStatsSequences += int64(len(manager.contractStatsSequences))
		manager.contractStatsLock.Unlock()
	}
	out.KnownSequenceStructBytes = out.SendWorkers*int64(unsafe.Sizeof(SendSequence{})) +
		out.ReceiveWorkers*int64(unsafe.Sizeof(ReceiveSequence{}))
	out.KnownPacingStructBytes = out.PacingServices * int64(unsafe.Sizeof(windowPacingService{}))
	out.KnownChannelSlotBytes = (out.SendPackSlots+out.ReceivePackSlots)*int64(unsafe.Sizeof(uintptr(0))) +
		out.SendAckSlots*int64(unsafe.Sizeof(receiveAckMessage{}))
}

func (self *Client) MemoryOwnerCensus() TransferOwnerCensus {
	var out TransferOwnerCensus
	self.addMemoryOwners(&out)
	return out
}

type PoolOwnerCensus struct {
	SendItems                int64 `json:"send_items"`
	AckOverflows             int64 `json:"ack_overflows"`
	NoAckOverflows           int64 `json:"no_ack_overflows"`
	KnownRetainedStructBytes int64 `json:"known_retained_struct_bytes"`
}

func GetPoolOwnerCensus() PoolOwnerCensus {
	out := PoolOwnerCensus{
		SendItems: int64(len(sendItemPool)), AckOverflows: int64(len(sendAckSetOverflowPool)),
		NoAckOverflows: int64(len(noAckSendSetOverflowPool)),
	}
	out.KnownRetainedStructBytes = out.SendItems*int64(unsafe.Sizeof(sendItem{})) +
		out.AckOverflows*int64(unsafe.Sizeof(sendAckSetOverflow{})) +
		out.NoAckOverflows*int64(unsafe.Sizeof(noAckSendSetOverflow{}))
	return out
}

type ApiOwnerCensus struct {
	Dialers              int64 `json:"dialers"`
	HttpClients          int64 `json:"http_clients"`
	NativeHttpPools      int64 `json:"native_http_pools"`
	AltPools             int64 `json:"alt_pools"`
	AltActiveConnections int64 `json:"alt_active_connections"`
	AltIdleConnections   int64 `json:"alt_idle_connections"`
	AltClosedRetained    int64 `json:"alt_closed_retained"`
	InternalDnsEntries   int64 `json:"internal_dns_entries"`
	InternalDnsInflight  int64 `json:"internal_dns_inflight"`
}

// Scope is this strategy, not every NetworkSpace in the process. net/http's
// opaque connection graph is deliberately not inferred from its pool count.
func (self *ClientStrategy) MemoryOwnerCensus() ApiOwnerCensus {
	var out ApiOwnerCensus
	if self == nil {
		return out
	}
	self.mutex.Lock()
	out.Dialers = int64(len(self.dialers))
	for dialer := range self.dialers {
		dialer.mutex.Lock()
		if client := dialer.httpClient; client != nil {
			out.HttpClients++
			switch transport := client.Transport.(type) {
			case *http.Transport:
				out.NativeHttpPools++
			case *altQuicBoundedTransport:
				out.AltPools++
				transport.mutex.Lock()
				for _, entry := range transport.connections {
					if entry == nil || entry.conn == nil {
						continue
					}
					if entry.conn.Context().Err() != nil {
						out.AltClosedRetained++
					} else if entry.active {
						out.AltActiveConnections++
					} else {
						out.AltIdleConnections++
					}
				}
				transport.mutex.Unlock()
			}
		}
		dialer.mutex.Unlock()
	}
	self.mutex.Unlock()
	if resolver := self.internalDohResolver; resolver != nil && resolver.cache != nil {
		resolver.cache.stateLock.Lock()
		out.InternalDnsEntries = int64(len(resolver.cache.queryResultExpiration))
		out.InternalDnsInflight = int64(len(resolver.cache.inflight))
		resolver.cache.stateLock.Unlock()
	}
	return out
}

type MultiClientOwnerCensus struct {
	Transfer              TransferOwnerCensus `json:"transfer"`
	ClientEntriesOmitted  int64               `json:"client_entries_omitted"`
	Flows                 int64               `json:"flows"`
	TcpFlows              int64               `json:"tcp_flows"`
	UdpFlows              int64               `json:"udp_flows"`
	CanceledFlows         int64               `json:"canceled_flows"`
	UnreceivedFlows       int64               `json:"unreceived_flows"`
	FlowClientReferences  int64               `json:"flow_client_references"`
	PathEntries           int64               `json:"path_entries"`
	AffinityGroups        int64               `json:"affinity_groups"`
	AffinityPaths         int64               `json:"affinity_paths"`
	DnsHints              int64               `json:"dns_hints"`
	QualificationEntries  int64               `json:"qualification_entries"`
	ServiceFailureEntries int64               `json:"service_failure_entries"`
	IpAssoc               IpAssocOwnerCensus  `json:"ip_assoc"`
}

// Snapshot a bounded mobile topology without a new global owner registry or
// retaining a dynamic slice. Larger embedders get an explicit omitted-entry count,
// never a silently complete-looking census. Window locks are released before
// inspecting Transfer owners. Duplicate client pointers are counted once.
// Scope is currently indexed window clients: old generations already unlinked
// from both windows need private heap/goroutine evidence, not a false zero-leak
// inference. Adding a global registry here would itself change owner lifetime.
func (self *RemoteUserNatMultiClient) MemoryOwnerCensus() MultiClientOwnerCensus {
	var out MultiClientOwnerCensus
	if self == nil {
		return out
	}
	var clients [64]*Client
	count := 0
	for _, kind := range [...]WindowType{WindowTypeQuality, WindowTypeSpeed} {
		window := self.windows[kind]
		if window == nil {
			continue
		}
		window.stateLock.Lock()
		for _, channel := range window.clients {
			if channel == nil || channel.client == nil {
				continue
			}
			duplicate := false
			for _, client := range clients[:count] {
				if client == channel.client {
					duplicate = true
					break
				}
			}
			if !duplicate {
				if count == len(clients) {
					out.ClientEntriesOmitted++
				} else {
					clients[count] = channel.client
					count++
				}
			}
		}
		window.stateLock.Unlock()
	}
	for _, client := range clients[:count] {
		client.addMemoryOwners(&out.Transfer)
	}
	self.stateLock.Lock()
	out.Flows = int64(len(self.flowUpdates))
	for flow := range self.flowUpdates {
		if flow.ctx != nil && flow.ctx.Err() != nil {
			out.CanceledFlows++
		}
		if !flow.receivedInbound.Load() {
			out.UnreceivedFlows++
		}
		if flow.ipPath != nil {
			switch flow.ipPath.Protocol {
			case IpProtocolTcp:
				out.TcpFlows++
			case IpProtocolUdp:
				out.UdpFlows++
			}
		}
	}
	for _, flows := range self.clientUpdates {
		out.FlowClientReferences += int64(len(flows))
	}
	out.PathEntries = int64(len(self.ip4PathUpdates) + len(self.ip6PathUpdates))
	out.AffinityGroups = int64(len(self.affinityIp4Paths) + len(self.affinityIp6Paths))
	for _, paths := range self.affinityIp4Paths {
		out.AffinityPaths += int64(len(paths))
	}
	for _, paths := range self.affinityIp6Paths {
		out.AffinityPaths += int64(len(paths))
	}
	out.DnsHints = int64(len(self.dnsExitHints) + len(self.dnsAddressExitHints))
	out.QualificationEntries = int64(len(self.qualification))
	out.ServiceFailureEntries = int64(len(self.destinationServiceFailures))
	self.stateLock.Unlock()
	out.IpAssoc = self.ipAssoc.MemoryOwnerCensus()
	return out
}

type IpAssocOwnerCensus struct {
	Blocks                 int64 `json:"blocks"`
	Entities               int64 `json:"entities"`
	Pairs                  int64 `json:"pairs"`
	ActiveEntities         int64 `json:"active_entities"`
	NamedEntities          int64 `json:"named_entities"`
	PublishedEntities      int64 `json:"published_entities"`
	ScratchEntities        int64 `json:"scratch_entities"`
	ScratchRawPairCapacity int64 `json:"scratch_raw_pair_capacity"`
	KnownBlockSliceBytes   int64 `json:"known_block_slice_bytes"`
	KnownScratchSliceBytes int64 `json:"known_scratch_slice_bytes"`
}

func (self *IpAssoc) MemoryOwnerCensus() IpAssocOwnerCensus {
	var out IpAssocOwnerCensus
	if self == nil {
		return out
	}
	self.stateLock.Lock()
	out.Blocks = int64(len(self.blocks))
	out.ActiveEntities = int64(len(self.lastActive))
	out.NamedEntities = int64(len(self.baseNames))
	for _, block := range self.blocks {
		out.Entities += int64(len(block.indexes))
		out.Pairs += int64(len(block.coCounts))
		out.KnownBlockSliceBytes += sliceCapacityBytes(block.addrs) + sliceCapacityBytes(block.counts)
	}
	self.stateLock.Unlock()
	if clusters := self.clusters.Load(); clusters != nil {
		out.PublishedEntities = int64(len(clusters.members))
	}
	// Never acquire scratchLock with stateLock held (the clustering worker
	// takes the reverse order). This read does not discard or resize anything.
	self.scratchLock.Lock()
	s := &self.scratch
	out.ScratchEntities = int64(len(s.indexes))
	out.ScratchRawPairCapacity = int64(cap(s.pairs))
	out.KnownScratchSliceBytes = sliceCapacityBytes(s.addrs) + sliceCapacityBytes(s.counts) +
		sliceCapacityBytes(s.baseNames) + sliceCapacityBytes(s.pairs) + sliceCapacityBytes(s.remap) +
		sliceCapacityBytes(s.parent) + sliceCapacityBytes(s.nodeIds) + sliceCapacityBytes(s.nodeCounts) +
		sliceCapacityBytes(s.nodeMemberInit) + sliceCapacityBytes(s.nodeMembers) + sliceCapacityBytes(s.memberFill) +
		sliceCapacityBytes(s.compParent) + sliceCapacityBytes(s.compSizes) + sliceCapacityBytes(s.compIds) +
		sliceCapacityBytes(s.compInit) + sliceCapacityBytes(s.compNodes) + sliceCapacityBytes(s.degrees) +
		sliceCapacityBytes(s.adjInit) + sliceCapacityBytes(s.adjNodes) + sliceCapacityBytes(s.adjPs) +
		sliceCapacityBytes(s.splitInCluster) + sliceCapacityBytes(s.splitSums)
	self.scratchLock.Unlock()
	return out
}

func sliceCapacityBytes[T any](values []T) int64 {
	var value T
	return int64(cap(values)) * int64(unsafe.Sizeof(value))
}

type TransportClaimCensus struct {
	H1Count       int64 `json:"h1_count"`
	H1Bytes       int64 `json:"h1_reserved_bytes"`
	H3Count       int64 `json:"h3_count"`
	H3Bytes       int64 `json:"h3_reserved_bytes"`
	ExtenderCount int64 `json:"extender_count"`
	ExtenderBytes int64 `json:"extender_reserved_bytes"`
	PendingCount  int64 `json:"pending_count"`
}

// Reservations are admission claims, never measurements of physical memory.
func (self *PlatformTransportBudget) MemoryOwnerCensus() TransportClaimCensus {
	var out TransportClaimCensus
	if self == nil {
		return out
	}
	self.root.mutex.Lock()
	defer self.root.mutex.Unlock()
	for claim := range self.reservations {
		if claim.pending {
			out.PendingCount++
		}
		if !claim.acquired || claim.closed {
			continue
		}
		var count, bytes *int64
		switch claim.class {
		case platformTransportBudgetH1:
			count, bytes = &out.H1Count, &out.H1Bytes
		case platformTransportBudgetH3Auto, platformTransportBudgetH3Explicit:
			count, bytes = &out.H3Count, &out.H3Bytes
		case platformTransportBudgetExtender:
			count, bytes = &out.ExtenderCount, &out.ExtenderBytes
		default:
			continue
		}
		*bytes += int64(claim.byteCount)
		if claim.usesSlot {
			*count += 1
		}
	}
	return out
}

type DnsOwnerCensus struct {
	CacheEntries   int64 `json:"cache_entries"`
	CacheInflight  int64 `json:"cache_inflight"`
	MuxInflight    int64 `json:"mux_inflight"`
	ReverseEntries int64 `json:"reverse_entries"`
	ReverseNames   int64 `json:"reverse_names"`
	DnsTcpFlows    int64 `json:"dns_tcp_flows"`
}

func (self *UpgradeMux) MemoryOwnerCensus() DnsOwnerCensus {
	var out DnsOwnerCensus
	if self == nil {
		return out
	}
	var caches [2]*DohCache
	if self.mux != nil && self.mux.Tun() != nil {
		caches[0] = self.mux.Tun().DohCache()
	}
	caches[1] = self.fallbackDohCache.Load()
	for i, cache := range caches {
		if cache == nil || (i == 1 && cache == caches[0]) {
			continue
		}
		cache.stateLock.Lock()
		out.CacheEntries += int64(len(cache.queryResultExpiration))
		out.CacheInflight += int64(len(cache.inflight))
		cache.stateLock.Unlock()
	}
	self.inflightLock.Lock()
	out.MuxInflight = int64(len(self.inflight))
	self.inflightLock.Unlock()
	self.dnsTcpLock.Lock()
	out.DnsTcpFlows = int64(len(self.dnsTcpFlows))
	self.dnsTcpLock.Unlock()
	if reverse := self.reverse; reverse != nil {
		reverse.lock.Lock()
		out.ReverseEntries = int64(len(reverse.entries))
		for _, entry := range reverse.entries {
			out.ReverseNames += int64(len(entry.serverNames))
		}
		reverse.lock.Unlock()
	}
	return out
}
