//go:build unix || windows

package connect

// net_resilient_combined_test.go — the combined TCP-segment + TLS-record
// fragmentation mode (net_resilient.go `segment`). The root-cause test is a
// reassembling-middlebox model that blocks a hello fragmented by a single
// method (record OR segment) but is slipped by the combined pair, matching the
// foci 2025 finding that only the combination beats russia's tspu reassembly.

import (
	"bytes"
	"net"
	"slices"
	"testing"
	"time"
)

// segmentRecordingConn records each Write as one tcp segment and is
// deliberately NOT a *net.TCPConn, so ResilientTlsConn.Write takes the
// userspace fragment path (no raw sockets, no ttl) -- the ios network
// extension and non-root android case the combined mode must work in. It is a
// test-only conn driven from a single goroutine, so it needs no locking.
type segmentRecordingConn struct {
	segments [][]byte
}

func (self *segmentRecordingConn) Write(b []byte) (int, error) {
	self.segments = append(self.segments, slices.Clone(b))
	return len(b), nil
}

func (self *segmentRecordingConn) Read(b []byte) (int, error)       { return 0, nil }
func (self *segmentRecordingConn) Close() error                     { return nil }
func (self *segmentRecordingConn) LocalAddr() net.Addr              { return nil }
func (self *segmentRecordingConn) RemoteAddr() net.Addr             { return nil }
func (self *segmentRecordingConn) SetDeadline(time.Time) error      { return nil }
func (self *segmentRecordingConn) SetReadDeadline(time.Time) error  { return nil }
func (self *segmentRecordingConn) SetWriteDeadline(time.Time) error { return nil }

// writeThroughResilient writes record through a resilient conn in the given
// mode and returns the tcp segments it emitted (one per Write).
func writeThroughResilient(t *testing.T, record []byte, fragment, reorder, segment bool) [][]byte {
	t.Helper()
	conn := &segmentRecordingConn{}
	rconn := newResilientTlsConn(conn, fragment, reorder, segment)
	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}
	return conn.segments
}

// concatSegments joins segment payloads back into the on-wire byte stream.
func concatSegments(segments [][]byte) []byte {
	var stream []byte
	for _, segment := range segments {
		stream = append(stream, segment...)
	}
	return stream
}

// parseTlsRecordStream walks stream as a sequence of whole tls records,
// returning the record count, the concatenated handshake-record payloads, and
// the byte offset at the end of each record. A record that runs past the
// stream, or trailing bytes, fail the test: the combined mode must emit whole,
// well-framed records (only their placement across tcp segments changes).
func parseTlsRecordStream(t *testing.T, stream []byte) (recordCount int, handshakePayload []byte, recordEndOffsets []int) {
	t.Helper()
	offset := 0
	for offset+5 <= len(stream) {
		header := parseTlsHeader(stream[offset : offset+5])
		end := offset + 5 + int(header.contentLength)
		if len(stream) < end {
			t.Fatalf("record at %d runs past the stream (have %d, want %d)", offset, len(stream), end)
		}
		if header.contentType == TlsContentTypeHandshake {
			handshakePayload = append(handshakePayload, stream[offset+5:end]...)
		}
		recordCount += 1
		offset = end
		recordEndOffsets = append(recordEndOffsets, offset)
	}
	if offset != len(stream) {
		t.Fatalf("trailing %d bytes after the last record", len(stream)-offset)
	}
	return
}

// segmentEndOffsets is the byte offset at the end of each tcp segment.
func segmentEndOffsets(segments [][]byte) []int {
	offsets := []int{}
	sum := 0
	for _, segment := range segments {
		sum += len(segment)
		offsets = append(offsets, sum)
	}
	return offsets
}

// chopIntoSegments splits data into fixed-size tcp segments WITHOUT re-framing
// it into more tls records: a single record spread across segments, which is
// tcp segmentation alone.
func chopIntoSegments(data []byte, size int) [][]byte {
	segments := [][]byte{}
	for offset := 0; offset < len(data); offset += size {
		end := min(offset+size, len(data))
		segments = append(segments, slices.Clone(data[offset:end]))
	}
	return segments
}

// reassemblingMiddlebox models a dpi box that reassembles ONE layer at a time
// -- the two single-method reassemblies foci 2025 (foci-2025-0016) reports
// russia's tspu performing. It blocks a hello whose sni it can recover EITHER
// by reassembling the whole tcp stream (detector A, which sees through tcp
// segmentation) OR by reading tls records that each sit whole inside one tcp
// segment (detector B, which sees through record fragmentation that aligns a
// record to its segment). It stitches NEITHER across the other, so a hello
// split across BOTH records and segments -- a record never whole within a
// segment, a segment boundary never on a record boundary -- hides its sni from
// both. This is a model, not the real tspu: its two detectors are exactly the
// two single-method recoveries the finding names, so "blocked unless the
// combination is used" is its defining property.
func reassemblingMiddleboxBlocks(segments [][]byte, serverName string) bool {
	needle := []byte(serverName)

	// detector A: reassemble the whole tcp stream and scan for the sni as a
	// contiguous substring. tcp segmentation is invisible to a full
	// reassembler, so this fires on a single record however it was segmented.
	if bytes.Contains(concatSegments(segments), needle) {
		return true
	}

	// detector B: within each segment independently, read the whole tls
	// records it fully contains and collect their handshake payloads; scan the
	// concatenation for the sni. A record fragmented into its own segment is
	// recovered; a record split across segments (its header not at a segment
	// start, or its length running past the segment) is not.
	var handshakePayload []byte
	for _, segment := range segments {
		rest := segment
		for 5 <= len(rest) {
			header := parseTlsHeader(rest[0:5])
			if !header.valid() {
				break
			}
			end := 5 + int(header.contentLength)
			if len(rest) < end {
				// the record is not whole within this segment
				break
			}
			if header.contentType == TlsContentTypeHandshake {
				handshakePayload = append(handshakePayload, rest[5:end]...)
			}
			rest = rest[end:]
		}
	}
	return bytes.Contains(handshakePayload, needle)
}

// TestWriteRecordMaybeSegmentedCutsIntoTwoSegments is the primitive: with the
// combined mode on, one record write becomes two tcp segments cut at an
// interior byte; with it off, one segment. This is the discriminating anchor
// for the segmentation itself -- it reads the same (one segment) before the
// fix and different (two) after.
func TestWriteRecordMaybeSegmentedCutsIntoTwoSegments(t *testing.T) {
	// a well-framed 8-byte-payload handshake record, 13 bytes total
	record := []byte{TlsContentTypeHandshake, 0x03, 0x03, 0x00, 0x08, 1, 2, 3, 4, 5, 6, 7, 8}

	on := &segmentRecordingConn{}
	rconnOn := newResilientTlsConn(on, true, false, true)
	if err := rconnOn.writeRecordMaybeSegmented(on, record); err != nil {
		t.Fatalf("combined write: %v", err)
	}
	if len(on.segments) != 2 {
		t.Fatalf("combined wrote %d tcp segments, want 2", len(on.segments))
	}
	if len(on.segments[0]) == 0 || len(on.segments[1]) == 0 {
		t.Fatalf("combined wrote an empty segment half: %d and %d", len(on.segments[0]), len(on.segments[1]))
	}
	if !bytes.Equal(concatSegments(on.segments), record) {
		t.Fatal("the two segments do not reassemble the record")
	}

	off := &segmentRecordingConn{}
	rconnOff := newResilientTlsConn(off, true, false, false)
	if err := rconnOff.writeRecordMaybeSegmented(off, record); err != nil {
		t.Fatalf("single-method write: %v", err)
	}
	if len(off.segments) != 1 {
		t.Fatalf("single-method wrote %d tcp segments, want 1", len(off.segments))
	}
}

// TestResilientCombinedEmitsRecordsAndSegments asserts the combined dial emits
// BOTH multiple tls records AND multiple tcp segments for one hello, that the
// segment boundaries are distinct from the record boundaries (a segment is cut
// inside a record), and that it all reassembles to the original hello. The
// interior-boundary assertion is what discriminates the combined mode from
// plain record fragmentation, whose segments align to record boundaries.
func TestResilientCombinedEmitsRecordsAndSegments(t *testing.T) {
	record := buildClientHelloRecord(t)
	segments := writeThroughResilient(t, record, true, false, true)

	if len(segments) < 2 {
		t.Fatalf("combined emitted %d tcp segments, want several", len(segments))
	}
	stream := concatSegments(segments)
	recordCount, handshakePayload, recordEndOffsets := parseTlsRecordStream(t, stream)
	if recordCount < 2 {
		t.Fatalf("combined emitted %d tls records, want several", recordCount)
	}
	if !bytes.Equal(handshakePayload, record[5:]) {
		t.Fatal("the combined fragments do not reassemble into the hello handshake")
	}

	recordEndSet := map[int]bool{}
	for _, offset := range recordEndOffsets {
		recordEndSet[offset] = true
	}
	segmentOffsets := segmentEndOffsets(segments)
	interiorSegmentBoundary := false
	// the final segment boundary is the stream end, which is also the last
	// record boundary, so it never counts; a boundary before it that is not a
	// record end is a segment cut inside a record
	for _, offset := range segmentOffsets[:len(segmentOffsets)-1] {
		if !recordEndSet[offset] {
			interiorSegmentBoundary = true
			break
		}
	}
	if !interiorSegmentBoundary {
		t.Fatalf("every tcp-segment boundary fell on a tls-record boundary (segments %v, record ends %v); the combined mode must cut segments inside records", segmentOffsets, recordEndOffsets)
	}
}

// TestCombinedModeDefeatsReassemblingMiddlebox is the root-cause test. The
// middlebox model blocks record fragmentation alone (detector B) and tcp
// segmentation alone (detector A); only the combined mode slips past both.
// Fail-before: with the combined segmentation reverted the combined dial emits
// record fragmentation alone, which detector B blocks, so the final assertion
// fails. Pass-after: the combined dial slips through.
func TestCombinedModeDefeatsReassemblingMiddlebox(t *testing.T) {
	record := buildClientHelloRecord(t)
	const serverName = "example.com" // buildClientHelloRecord's sni

	// single method 1: record fragmentation alone (the existing fragment
	// dialer). The model reassembles the per-segment records and recovers the
	// sni, so it blocks.
	fragmentOnly := writeThroughResilient(t, record, true, false, false)
	if !reassemblingMiddleboxBlocks(fragmentOnly, serverName) {
		t.Fatal("record fragmentation alone slipped the model; the model does not reassemble records from segments and so cannot discriminate the combined mode")
	}

	// single method 2: tcp segmentation alone (one record cut across segments).
	// The model reassembles the tcp stream and recovers the sni, so it blocks.
	tcpSegmentationOnly := chopIntoSegments(record, 40)
	if len(tcpSegmentationOnly) < 2 {
		t.Fatalf("the tcp-segmentation model produced %d segments, want several", len(tcpSegmentationOnly))
	}
	if !reassemblingMiddleboxBlocks(tcpSegmentationOnly, serverName) {
		t.Fatal("tcp segmentation alone slipped the model; the model does not reassemble the tcp stream and so cannot discriminate the combined mode")
	}

	// the combined mode: record fragmentation AND tcp segmentation together.
	// Neither detector recovers the sni, so it slips through. This is the
	// assertion that fails with the combined segmentation reverted.
	combined := writeThroughResilient(t, record, true, false, true)
	if reassemblingMiddleboxBlocks(combined, serverName) {
		t.Fatal("the combined mode was blocked; a hello split across both small tls records and small tcp segments must slip a middlebox that reassembles only one layer at a time")
	}
}

// TestResilientCombinedCarriesChromeClientHello checks the combined mode on the
// real, larger Chrome hello (~1.7 KiB, not Go's): it reassembles to the Chrome
// hello, parses as Chrome (grease, shuffled extensions, the key share), and the
// combined output slips the model that record fragmentation alone does not.
func TestResilientCombinedCarriesChromeClientHello(t *testing.T) {
	record := captureTestChromeClientHelloRecord(t)
	t.Logf("Chrome hello record: %d bytes", len(record))

	segments := writeThroughResilient(t, record, true, false, true)
	if len(segments) < 2 {
		t.Fatalf("combined emitted %d tcp segments for the Chrome hello, want several", len(segments))
	}
	recordCount, handshakePayload, _ := parseTlsRecordStream(t, concatSegments(segments))
	if recordCount < 2 {
		t.Fatalf("combined emitted %d tls records for the Chrome hello, want several", recordCount)
	}
	if !bytes.Equal(handshakePayload, record[5:]) {
		t.Fatal("the combined fragments do not reassemble into the Chrome hello")
	}

	// it is the Chrome hello, not Go's: the captured hello offers the websocket
	// path's protocols, so assert against those
	hello := parseTestClientHello(t, handshakePayload)
	assertTestChromeClientHello(t, hello, clientWebSocketNextProtos)

	// record fragmentation alone of the Chrome hello is blocked; the combined
	// mode is not
	if !reassemblingMiddleboxBlocks(writeThroughResilient(t, record, true, false, false), testTlsHelloServerName) {
		t.Fatal("record fragmentation of the Chrome hello was not blocked by the model")
	}
	if reassemblingMiddleboxBlocks(segments, testTlsHelloServerName) {
		t.Fatal("the combined Chrome hello was blocked by the model")
	}
}

// TestResilientCombinedComposesWithTtlReorder checks that turning the combined
// tcp segmentation on does not disturb the reorder ttl choreography: on a real
// socket the fragment+reorder+segment path still lowers the first fragment's
// ttl, alternates, and restores the native ttl, and the peer still receives
// the whole hello. Mirrors TestResilientTlsConnFragmentReorderAppliesLowTtlAndRestores
// with segmentation added, so a regression that let segmentation break the
// reorder sequence is caught here. (The segmentation's own write boundaries are
// covered by the injected-conn tests above; a real socket's byte stream hides
// them.)
func TestResilientCombinedComposesWithTtlReorder(t *testing.T) {
	record := buildClientHelloRecord(t)
	client, server := newTcpPair(t)
	setSocketTtl(t, client, 42)
	nativeTtl := 42
	if nativeTtl == resilientLowTtl {
		t.Fatalf("test setup: native ttl %d equals resilientLowTtl, the sequence assertions would be vacuous", nativeTtl)
	}

	seam := &ttlSeam{passthrough: true}
	rconn := newResilientTlsConn(client, true, true, true) // fragment+reorder+segment
	rconn.setTtl = seam.set

	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}
	if rconn.ttlErr != nil {
		t.Fatalf("ttlErr = %v, want nil with segmentation composed onto reorder", rconn.ttlErr)
	}

	if len(seam.applied) < 2 {
		t.Fatalf("applied ttl sequence = %v, want at least a low ttl and a restore", seam.applied)
	}
	if seam.applied[0] != resilientLowTtl {
		t.Fatalf("applied ttl sequence = %v, want it to begin with resilientLowTtl=%d; segmentation must not disturb the reorder choreography", seam.applied, resilientLowTtl)
	}
	if last := seam.applied[len(seam.applied)-1]; last != nativeTtl {
		t.Fatalf("applied ttl sequence = %v, want it to end with the native ttl %d", seam.applied, nativeTtl)
	}
	if got := socketTtl(t, client); got != nativeTtl {
		t.Fatalf("socket ttl after combined+reorder write = %d, want %d (native restored)", got, nativeTtl)
	}

	// the fragment path re-frames the payload into standalone records; segment
	// boundaries vanish into the tcp byte stream, so the peer reassembles the
	// original handshake bytes
	got := readTlsRecords(t, server, len(record)-5)
	if !bytes.Equal(got, record[5:]) {
		t.Fatal("peer received different payload than the hello")
	}
}
