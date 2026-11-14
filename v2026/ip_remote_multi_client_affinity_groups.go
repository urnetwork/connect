// Native bursts retain exact-flow coalescing except at TCP generation
// boundaries. A new SYN cannot carry an old generation's prefix to a new NAT
// identity. Each resulting group keeps the existing all-or-none send contract.
package connect

// Borrows the grouped packet owners and returns only replacement descriptors.
// Index slices remain exact partitions of the caller's original positions.
// Ordinary groups return unchanged without allocating another descriptor.
func splitAffinitySynGroups(groups []*ipPacketGroup, packetIndexes map[*ipPacketGroup][]int) []*ipPacketGroup {
	var split []*ipPacketGroup
	for groupIndex, group := range groups {
		if group.ipPath == nil || group.ipPath.Protocol != IpProtocolTcp {
			if split != nil {
				split = append(split, group)
			}
			continue
		}
		start, synSeen, synNumber := 0, false, uint32(0)
		appendSegment := func(end int) {
			if split == nil {
				split = append(make([]*ipPacketGroup, 0, len(groups)+1), groups[:groupIndex]...)
			}
			path := group.ipPaths[start]
			path.SourceIp, path.DestinationIp = group.ipPath.SourceIp, group.ipPath.DestinationIp
			segment := &ipPacketGroup{
				ipPath: &path, packets: group.packets[start:end:end],
				ipPaths: group.ipPaths[start:end:end], payloads: group.payloads[start:end:end],
			}
			for _, packet := range segment.packets {
				segment.byteCount += ByteCount(len(packet))
			}
			if packetIndexes != nil {
				segmentIndexes := packetIndexes[group]
				packetIndexes[segment] = segmentIndexes[start:end:end]
			}
			split = append(split, segment)
			start = end
		}
		for packetIndex := range group.ipPaths {
			path := &group.ipPaths[packetIndex]
			if path.Syn && !path.Ack {
				if start < packetIndex && (!synSeen || synNumber != path.SequenceNumber) {
					appendSegment(packetIndex)
				}
				synSeen, synNumber = true, path.SequenceNumber
			}
			if path.Rst {
				// A same-ISN SYN after explicit reset is a new connection, not
				// a retransmission in the just-closed generation.
				synSeen = false
			}
		}
		if start != 0 {
			appendSegment(len(group.packets))
			delete(packetIndexes, group)
		} else if split != nil {
			split = append(split, group)
		}
	}
	if split == nil {
		return groups
	}
	return split
}
