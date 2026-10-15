package connect

import (
	"context"
	"errors"
	"hash/crc64"
	"net"
	"sync"
	"time"
)

// TransferProgressEvent is opt-in diagnostic metadata, never a delivery signal.
// No payload, address, credential, or borrowed buffer is retained. WireHash is
// only a correlation checksum of the exact carrier bytes, not an authenticator.
// Observers must be bounded and nonblocking; leave them nil for measurements
// that qualify production memory or throughput.
type TransferProgressEvent struct {
	Stage          string
	AtUnixNano     int64
	ElapsedNanos   int64
	ClientId       Id
	PeerId         Id
	SequenceId     Id
	MessageId      Id
	SequenceNumber uint64
	WireHash       uint64
	ByteCount      int
	QueueLength    int
	QueueCapacity  int
	TransportType  TransportType
	NoAck          bool
	Selective      bool
	Success        bool
	ErrorKind      string
	Outcome        string
}

func transferProgressAckOutcome(result receiveAckHandoffResult) string {
	labels := [...]string{"accepted", "accepted_after_wait", "queue_full", "queue_wait_timeout", "sequence_missing", "sequence_closed"}
	if int(result) < len(labels) {
		return labels[result]
	}
	return "unknown"
}

var transferProgressChecksumTable = sync.OnceValue(func() *crc64.Table {
	return crc64.MakeTable(crc64.ECMA)
})

// Nil observers do not hash bytes, read a clock, or allocate event storage.
func beginTransferProgress(
	observer func(TransferProgressEvent),
	event TransferProgressEvent,
	wire []byte,
) TransferProgressEvent {
	if observer == nil {
		return event
	}
	event.AtUnixNano = time.Now().UnixNano()
	if wire != nil {
		event.WireHash = crc64.Checksum(wire, transferProgressChecksumTable())
		event.ByteCount = len(wire)
	}
	observeTransferProgress(observer, event)
	return event
}

func endTransferProgress(
	observer func(TransferProgressEvent),
	event TransferProgressEvent,
	stage string,
	success bool,
	err error,
) {
	if observer == nil {
		return
	}
	now := time.Now().UnixNano()
	event.ElapsedNanos = max(0, now-event.AtUnixNano)
	event.AtUnixNano = now
	event.Stage = stage
	event.Success = success
	event.ErrorKind = transferProgressErrorKind(err)
	if !success && event.ErrorKind == "" {
		event.ErrorKind = "refused"
	}
	observeTransferProgress(observer, event)
}

// Error classes deliberately exclude arbitrary error text (which may contain
// addresses). A route's typed timeout differs from cancellation or closure.
func transferProgressErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errTransferRouteWriteTimeout):
		return "route_timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrSendPackNotAdmitted):
		return "admission"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	default:
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			return "io_timeout"
		}
		return "error"
	}
}

func observeTransferProgress(observer func(TransferProgressEvent), event TransferProgressEvent) {
	if observer == nil {
		return
	}
	// Diagnostics cannot change the reliability/ownership path on panic.
	defer func() { _ = recover() }()
	observer(event)
}
