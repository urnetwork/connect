// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

// Deterministic backing-array checks cover retirement and retained ownership
// without relying on garbage collection, scheduling, or payload pool behavior.
package sctp

import (
	"io"
	"maps"
	"slices"
	"testing"
)

// Reports references left outside the queue's live slice.
func assertReassemblyRetiredSlots[T any](t *testing.T, name string, slots []*T) {
	t.Helper()
	for i, slot := range slots {
		if slot != nil {
			t.Errorf("%s: retired backing-array slot %d still retains a payload owner", name, i)
		}
	}
}

// Successful reads retire one slot; short buffers retain the complete message.
func TestReassemblyQueueReadReleasesRetiredSlots(t *testing.T) {
	cases := []struct {
		name         string
		interleaving bool
		unordered    bool
	}{
		{name: "ordered data"},
		{name: "unordered data", unordered: true},
		{name: "ordered interleaved", interleaving: true},
		{name: "unordered interleaved", interleaving: true, unordered: true},
	}
	for _, c := range cases {
		queue := newReassemblyQueue(0, 0)
		messages := [][]string{{"A", "B"}, {"tail"}}
		var inputChunks []*chunkPayloadData
		for messageIndex, fragments := range messages {
			for fragmentIndex, fragment := range fragments {
				chunk := &chunkPayloadData{
					iData:                  c.interleaving,
					unordered:              c.unordered,
					streamSequenceNumber:   uint16(messageIndex),
					messageIdentifier:      uint32(messageIndex),
					fragmentSequenceNumber: uint32(fragmentIndex),
					tsn:                    uint32(len(inputChunks) + 1),
					beginningFragment:      fragmentIndex == 0,
					endingFragment:         fragmentIndex == len(fragments)-1,
					payloadType:            PayloadTypeWebRTCBinary,
					userData:               []byte(fragment),
				}
				inputChunks = append(inputChunks, chunk)
				complete, err := queue.pushWithError(chunk)
				if err != nil || complete != chunk.endingFragment {
					t.Fatalf("%s: enqueue complete=%v, error=%v", c.name, complete, err)
				}
			}
		}

		dataSlots := queue.ordered
		interleavedSlots := queue.orderedMID
		if c.unordered {
			dataSlots = queue.unordered
			interleavedSlots = queue.unorderedMID
		}
		originalDataSets := slices.Clone(dataSlots)
		originalInterleavedSets := slices.Clone(interleavedSlots)
		checkQueue := func(retired, remainingBytes int) {
			t.Helper()
			liveDataSets := queue.ordered
			liveInterleavedSets := queue.orderedMID
			if c.unordered {
				liveDataSets = queue.unordered
				liveInterleavedSets = queue.unorderedMID
			}
			if c.interleaving {
				assertReassemblyRetiredSlots(t, c.name, interleavedSlots[:retired])
				if !slices.Equal(interleavedSlots[retired:], originalInterleavedSets[retired:]) ||
					!slices.Equal(liveInterleavedSets, originalInterleavedSets[retired:]) {
					t.Errorf("%s: live interleaved queue changed after %d reads", c.name, retired)
				}
				if !c.unordered && len(queue.orderedMIDMap) != 2-retired {
					t.Errorf("%s: mapped message count=%d, want %d", c.name, len(queue.orderedMIDMap), 2-retired)
				}
			} else {
				assertReassemblyRetiredSlots(t, c.name, dataSlots[:retired])
				if !slices.Equal(dataSlots[retired:], originalDataSets[retired:]) ||
					!slices.Equal(liveDataSets, originalDataSets[retired:]) {
					t.Errorf("%s: live data queue changed after %d reads", c.name, retired)
				}
			}
			if queue.getNumBytes() != remainingBytes {
				t.Errorf("%s: queued bytes=%d, want %d", c.name, queue.getNumBytes(), remainingBytes)
			}
			if !c.unordered && ((c.interleaving && queue.nextMID != uint32(retired)) ||
				(!c.interleaving && queue.nextSSN != uint16(retired))) {
				t.Errorf("%s: next sequence changed incorrectly after %d reads", c.name, retired)
			}
		}

		var buffer [4]byte
		n, ppi, err := queue.read(buffer[:1])
		if n != 2 || ppi != 0 || err != io.ErrShortBuffer {
			t.Fatalf("%s: short read=(%d, %d, %v)", c.name, n, ppi, err)
		}
		checkQueue(0, 6)
		for i, message := range []string{"AB", "tail"} {
			n, ppi, err = queue.read(buffer[:])
			if err != nil || n != len(message) || ppi != PayloadTypeWebRTCBinary || string(buffer[:n]) != message {
				t.Fatalf("%s: read %d=(%d, %d, %v), payload=%q", c.name, i, n, ppi, err, buffer[:n])
			}
			remainingBytes := 0
			if i == 0 {
				remainingBytes = 4
			}
			checkQueue(i+1, remainingBytes)
		}
		n, ppi, err = queue.read(buffer[:])
		if n != 0 || ppi != 0 || err != errTryAgain {
			t.Errorf("%s: empty read=(%d, %d, %v)", c.name, n, ppi, err)
		}
		for i, payload := range []string{"A", "B", "tail"} {
			if string(inputChunks[i].userData) != payload {
				t.Errorf("%s: retired chunk %d was modified", c.name, i)
			}
		}
	}
}

// Incomplete and future messages retain their slots when no read can proceed.
func TestReassemblyQueueBlockedReadPreservesOwnedSlots(t *testing.T) {
	cases := []struct {
		name         string
		interleaving bool
		unordered    bool
		sequence     uint16
		complete     bool
	}{
		{name: "ordered incomplete data"},
		{name: "unordered incomplete data", unordered: true},
		{name: "ordered incomplete interleaved", interleaving: true},
		{name: "unordered incomplete interleaved", interleaving: true, unordered: true},
		{name: "ordered future data", sequence: 1, complete: true},
		{name: "ordered future interleaved", interleaving: true, sequence: 1, complete: true},
	}
	for _, c := range cases {
		queue := newReassemblyQueue(0, 0)
		chunk := &chunkPayloadData{
			iData:                c.interleaving,
			unordered:            c.unordered,
			streamSequenceNumber: c.sequence,
			messageIdentifier:    uint32(c.sequence),
			tsn:                  1,
			beginningFragment:    true,
			endingFragment:       c.complete,
			payloadType:          PayloadTypeWebRTCBinary,
			userData:             []byte("keep"),
		}
		queue.push(chunk)
		orderedSets := slices.Clone(queue.ordered)
		unorderedSets := slices.Clone(queue.unordered)
		unorderedChunks := slices.Clone(queue.unorderedChunks)
		orderedInterleavedSets := slices.Clone(queue.orderedMID)
		unorderedInterleavedSets := slices.Clone(queue.unorderedMID)
		orderedInterleavedIdSets := maps.Clone(queue.orderedMIDMap)
		unorderedInterleavedIdSets := maps.Clone(queue.unorderedMIDMap)
		var buffer [4]byte
		n, ppi, err := queue.read(buffer[:])
		if n != 0 || ppi != 0 || err != errTryAgain {
			t.Fatalf("%s: blocked read=(%d, %d, %v)", c.name, n, ppi, err)
		}
		if !slices.Equal(queue.ordered, orderedSets) || !slices.Equal(queue.unordered, unorderedSets) ||
			!slices.Equal(queue.unorderedChunks, unorderedChunks) ||
			!slices.Equal(queue.orderedMID, orderedInterleavedSets) ||
			!slices.Equal(queue.unorderedMID, unorderedInterleavedSets) ||
			!maps.Equal(queue.orderedMIDMap, orderedInterleavedIdSets) ||
			!maps.Equal(queue.unorderedMIDMap, unorderedInterleavedIdSets) {
			t.Errorf("%s: blocked read changed queue ownership", c.name)
		}
		if queue.getNumBytes() != 4 || queue.nextSSN != 0 || queue.nextMID != 0 || string(chunk.userData) != "keep" {
			t.Errorf("%s: blocked read changed bytes, sequence, or payload", c.name)
		}
	}
}

// Forwarding clears only discarded fragments, including a fully drained slice.
func TestReassemblyQueueForwardReleasesUnorderedSlots(t *testing.T) {
	cases := []struct {
		name       string
		tsns       []uint32
		cumulative uint32
		retired    int
	}{
		{name: "none", tsns: []uint32{10, 11, 12}, cumulative: 9},
		{name: "prefix", tsns: []uint32{10, 11, 12}, cumulative: 10, retired: 1},
		{name: "all", tsns: []uint32{10, 11, 12}, cumulative: 12, retired: 3},
		{name: "wrapped prefix", tsns: []uint32{^uint32(0) - 1, ^uint32(0), 0}, cumulative: ^uint32(0), retired: 2},
	}
	for _, c := range cases {
		queue := newReassemblyQueue(0, 0)
		queue.unorderedChunks = make([]*chunkPayloadData, 0, 4)
		for i, tsn := range c.tsns {
			queue.push(&chunkPayloadData{unordered: true, tsn: tsn, userData: []byte{byte(i)}})
		}
		slots := queue.unorderedChunks
		originalChunks := slices.Clone(slots)
		queue.forwardTSNForUnordered(c.cumulative)
		assertReassemblyRetiredSlots(t, c.name, slots[:c.retired])
		if !slices.Equal(queue.unorderedChunks, originalChunks[c.retired:]) ||
			!slices.Equal(slots[c.retired:], originalChunks[c.retired:]) {
			t.Errorf("%s: forwarding modified a live fragment", c.name)
		}
		if queue.getNumBytes() != len(c.tsns)-c.retired {
			t.Errorf("%s: queued bytes=%d, want %d", c.name, queue.getNumBytes(), len(c.tsns)-c.retired)
		}
		queue.forwardTSNForUnordered(c.tsns[len(c.tsns)-1])
		assertReassemblyRetiredSlots(t, c.name+" drained", slots)
		if len(queue.unorderedChunks) != 0 || queue.getNumBytes() != 0 {
			t.Errorf("%s: fully forwarded queue remains live", c.name)
		}
		for i, chunk := range originalChunks {
			if len(chunk.userData) != 1 || chunk.userData[0] != byte(i) {
				t.Errorf("%s: discarded chunk %d was modified", c.name, i)
			}
		}
	}
}

// Compaction must clear its vacated tail while preserving extracted ownership.
func TestReassemblyQueueExtractReleasesUnorderedTailSlots(t *testing.T) {
	cases := []struct {
		prefix int
		suffix int
	}{
		{prefix: 0, suffix: 0},
		{prefix: 0, suffix: 2},
		{prefix: 1, suffix: 1},
		{prefix: 2, suffix: 0},
		{prefix: 2, suffix: 1},
	}
	for _, c := range cases {
		queue := newReassemblyQueue(0, 0)
		queue.unorderedChunks = make([]*chunkPayloadData, c.prefix+2+c.suffix, c.prefix+4+c.suffix)
		for i := range queue.unorderedChunks {
			queue.unorderedChunks[i] = &chunkPayloadData{
				unordered:         true,
				tsn:               uint32(i + 1),
				beginningFragment: i == c.prefix,
				endingFragment:    i == c.prefix+1,
				payloadType:       PayloadTypeWebRTCBinary,
				userData:          []byte{byte('a' + i)},
			}
		}
		slots := queue.unorderedChunks
		originalChunks := slices.Clone(slots)
		queue.nBytes = uint64(len(slots))
		completeSet := queue.findCompleteUnorderedChunkSet()
		if completeSet == nil || !slices.Equal(completeSet.chunks, originalChunks[c.prefix:c.prefix+2]) {
			t.Fatalf("prefix=%d suffix=%d: extracted chunks changed", c.prefix, c.suffix)
		}
		remainingChunks := append(slices.Clone(originalChunks[:c.prefix]), originalChunks[c.prefix+2:]...)
		if !slices.Equal(queue.unorderedChunks, remainingChunks) {
			t.Errorf("prefix=%d suffix=%d: compaction changed retained fragments", c.prefix, c.suffix)
		}
		assertReassemblyRetiredSlots(t, "compacted unordered fragments", slots[len(remainingChunks):])
		if queue.getNumBytes() != len(originalChunks) {
			t.Errorf("prefix=%d suffix=%d: extraction changed queued bytes", c.prefix, c.suffix)
		}
		queue.unordered = append(queue.unordered, completeSet)
		var buffer [2]byte
		n, ppi, err := queue.read(buffer[:])
		if n != 2 || ppi != PayloadTypeWebRTCBinary || err != nil ||
			buffer != [2]byte{byte('a' + c.prefix), byte('a' + c.prefix + 1)} {
			t.Fatalf("prefix=%d suffix=%d: extracted read=(%d, %d, %v), payload=%q", c.prefix, c.suffix, n, ppi, err, buffer)
		}
		if queue.getNumBytes() != len(remainingChunks) {
			t.Errorf("prefix=%d suffix=%d: read changed retained byte accounting", c.prefix, c.suffix)
		}
	}
}

// All forwarding variants keep complete messages and future incomplete owners.
func TestReassemblyQueueForwardPreservesCompleteAndFutureChunks(t *testing.T) {
	cases := []struct {
		name         string
		interleaving bool
		unordered    bool
	}{
		{name: "ordered data"},
		{name: "unordered data", unordered: true},
		{name: "ordered interleaved", interleaving: true},
		{name: "unordered interleaved", interleaving: true, unordered: true},
	}
	for _, c := range cases {
		queue := newReassemblyQueue(0, 0)
		for i, payload := range []string{"drop", "ready", "future"} {
			queue.push(&chunkPayloadData{
				iData:                c.interleaving,
				unordered:            c.unordered,
				streamSequenceNumber: uint16(i),
				messageIdentifier:    uint32(i),
				tsn:                  uint32((i + 1) * 10),
				beginningFragment:    true,
				endingFragment:       i == 1,
				payloadType:          PayloadTypeWebRTCBinary,
				userData:             []byte(payload),
			})
		}
		switch {
		case c.interleaving && c.unordered:
			queue.forwardTSNForUnorderedMID(1)
		case c.interleaving:
			queue.forwardTSNForOrderedMID(1)
		case c.unordered:
			queue.forwardTSNForUnordered(20)
		default:
			queue.forwardTSNForOrdered(1)
		}
		if queue.getNumBytes() != 11 {
			t.Errorf("%s: forwarding retained %d bytes, want 11", c.name, queue.getNumBytes())
		}
		var buffer [16]byte
		n, ppi, err := queue.read(buffer[:])
		if err != nil || n != 5 || ppi != PayloadTypeWebRTCBinary || string(buffer[:n]) != "ready" {
			t.Fatalf("%s: retained complete read=(%d, %d, %v), payload=%q", c.name, n, ppi, err, buffer[:n])
		}
		if queue.getNumBytes() != 6 {
			t.Errorf("%s: complete read retained %d bytes, want 6", c.name, queue.getNumBytes())
		}
		complete, err := queue.pushWithError(&chunkPayloadData{
			iData:                  c.interleaving,
			unordered:              c.unordered,
			streamSequenceNumber:   2,
			messageIdentifier:      2,
			fragmentSequenceNumber: 1,
			tsn:                    31,
			endingFragment:         true,
			payloadType:            PayloadTypeWebRTCBinary,
			userData:               []byte("done"),
		})
		if err != nil || !complete {
			t.Fatalf("%s: future completion=(%v, %v)", c.name, complete, err)
		}
		n, ppi, err = queue.read(buffer[:])
		if err != nil || n != 10 || ppi != PayloadTypeWebRTCBinary || string(buffer[:n]) != "futuredone" {
			t.Fatalf("%s: retained future read=(%d, %d, %v), payload=%q", c.name, n, ppi, err, buffer[:n])
		}
		if queue.getNumBytes() != 0 {
			t.Errorf("%s: drained queue retained %d counted bytes", c.name, queue.getNumBytes())
		}
	}
}
