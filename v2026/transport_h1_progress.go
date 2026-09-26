package connect

import (
	"hash/crc64"
	"time"
)

// Only an explicitly enabled diagnostic owns this bounded metadata scratch.
// The single H1 writer owns pending/batching; concurrent reader events use
// immutable identity fields only. No payload or borrowed buffer is retained.
type h1PhysicalProgress struct {
	observer     func(TransferProgressEvent)
	client       Id
	connection   Id
	kind         string
	pending      [platformWebSocketWriteBatchMaxMessages]TransferProgressEvent
	count        int
	pendingBytes int
	batching     bool
}

func newH1PhysicalProgress(observer func(TransferProgressEvent), client Id, conn H1MessageConn) *h1PhysicalProgress {
	if observer == nil {
		return nil
	}
	kind := "websocket"
	if _, framed := conn.(*FramedMessageConn); framed {
		kind = "h1plus"
	}
	p := &h1PhysicalProgress{observer: observer, client: client, connection: NewId(), kind: kind}
	p.event("h1_connected", nil, true, nil)
	return p
}

func (p *h1PhysicalProgress) identity(stage string) TransferProgressEvent {
	return TransferProgressEvent{Stage: stage, ClientId: p.client, SequenceId: p.connection,
		TransportType: TransportTypeH1, Outcome: p.kind}
}

func (p *h1PhysicalProgress) event(stage string, wire []byte, success bool, err error) TransferProgressEvent {
	if p == nil {
		return TransferProgressEvent{}
	}
	event := p.identity(stage)
	event.Success, event.ErrorKind = success, transferProgressErrorKind(err)
	return beginTransferProgress(p.observer, event, wire)
}

// queue copies only a checksum/length. The later begin/end pair brackets the
// actual framed flush (or the WebSocket ready batch's delegated flush).
func (p *h1PhysicalProgress) queue(wire []byte) {
	if p == nil {
		return
	}
	if p.count == len(p.pending) {
		p.event("h1_trace_overflow", nil, false, nil)
		return
	}
	p.pendingBytes += len(wire) + 14 // conservative maximum WS frame header
	if p.kind == "websocket" && webSocketWriteBatchMaxByteCount < p.pendingBytes {
		// Oversized WebSocket messages may bypass the coalescing buffer before
		// the explicit flush. Mark this unsupported join instead of claiming
		// that its begin timestamp is the physical socket-write start.
		p.event("h1_trace_unsupported", nil, false, nil)
	}
	event := p.identity("h1_write_begin")
	event.WireHash = crc64.Checksum(wire, transferProgressChecksumTable())
	event.ByteCount = len(wire)
	p.pending[p.count] = event
	p.count++
}

func (p *h1PhysicalProgress) beginWrite() {
	if p == nil {
		return
	}
	for index := range p.count {
		p.pending[index].AtUnixNano = time.Now().UnixNano()
		observeTransferProgress(p.observer, p.pending[index])
	}
}

func (p *h1PhysicalProgress) endWrite(err error) {
	if p == nil {
		return
	}
	for index := range p.count {
		endTransferProgress(p.observer, p.pending[index], "h1_write_end", err == nil, err)
		p.pending[index] = TransferProgressEvent{}
	}
	p.count, p.pendingBytes = 0, 0
}

func (p *h1PhysicalProgress) prepareWebSocketWrite(wire []byte) bool {
	if p == nil || p.kind != "websocket" {
		return false
	}
	p.queue(wire)
	if !p.batching {
		p.beginWrite()
		return true
	}
	return false
}

func (p *h1PhysicalProgress) beginWebSocketBatch() {
	if p != nil {
		p.batching = true
	}
}

func (p *h1PhysicalProgress) finishWebSocketBatch() {
	if p == nil {
		return
	}
	// Encoding/cancellation may abort before any delegated write. Such bytes
	// must never get a successful physical-write completion.
	for index := range p.count {
		event := p.pending[index]
		event.Stage, event.AtUnixNano = "h1_write_aborted", time.Now().UnixNano()
		observeTransferProgress(p.observer, event)
		p.pending[index] = TransferProgressEvent{}
	}
	p.count, p.pendingBytes, p.batching = 0, 0, false
}
