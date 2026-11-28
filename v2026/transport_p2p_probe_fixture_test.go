// In-memory probe fixtures shared by native and browser builds.
package connect

import (
	"context"
	"net"
	"sync"
	"time"
)

// Creates an endpoint-only transport without a competing send worker. The
// relay harness consumes its exact route in the same ownership pattern.
func newP2pProbeTestSendTransport(
	peerId Id,
	streamId Id,
	route Route,
	settings *P2pTransportSettings,
) *P2pSendTransport {
	transport := &P2pSendTransport{
		transportId:               NewId(),
		peerId:                    peerId,
		streamId:                  streamId,
		send:                      route,
		endToEndReadinessRequired: true,
		settings:                  settings,
	}
	transport.probeSendAdmission.open = true
	return transport
}

// Builds probe state without starting its lifecycle, so barrier tests can
// control the first producer exactly.
func newStoppedP2pStreamProbe(
	ctx context.Context,
	routeManager *RouteManager,
	streamId Id,
	settings *P2pTransportSettings,
) *p2pStreamProbe {
	probeCtx, cancel := context.WithCancel(ctx)
	interval := settings.EndToEndProbeInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := settings.EndToEndProbeTimeout
	if timeout < 2*interval {
		timeout = 2 * interval
	}
	return &p2pStreamProbe{
		ctx:              probeCtx,
		cancel:           cancel,
		routeManager:     routeManager,
		streamId:         streamId,
		interval:         interval,
		timeout:          timeout,
		observer:         settings.EndToEndProbeObserver,
		progressObserver: settings.ProgressObserver,
		routeUpdate:      NewMonitor(),
		responses:        make(chan p2pStreamProbeResponse, 1),
		done:             make(chan struct{}),
	}
}

// A loopback fast carrier lets one send and receive worker prove that probe
// messages are consumed but excluded from every application traffic counter.
type p2pProbeFastLoopConn struct {
	done         chan struct{}
	closeOnce    sync.Once
	fastMessages chan p2pFastPathReceivedMessage
}

// Allocates a bounded in-memory carrier.
func newP2pProbeFastLoopConn() *p2pProbeFastLoopConn {
	return &p2pProbeFastLoopConn{
		done:         make(chan struct{}),
		fastMessages: make(chan p2pFastPathReceivedMessage, 8),
	}
}

// The legacy lane stays blocked until teardown.
func (self *p2pProbeFastLoopConn) Read([]byte) (int, error) {
	<-self.done
	return 0, net.ErrClosed
}

// The forced-fast test must never use the legacy lane.
func (self *p2pProbeFastLoopConn) Write([]byte) (int, error) {
	return 0, net.ErrClosed
}

// Releases the blocking compatibility reader.
func (self *p2pProbeFastLoopConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.done)
	})
	return nil
}

// Returns a stable synthetic address.
func (self *p2pProbeFastLoopConn) LocalAddr() net.Addr {
	return p2pProbeTestNetAddr("probe-fast-local")
}

// Returns a stable synthetic address.
func (self *p2pProbeFastLoopConn) RemoteAddr() net.Addr {
	return p2pProbeTestNetAddr("probe-fast-remote")
}

// Deadlines are irrelevant to the in-memory carrier.
func (self *p2pProbeFastLoopConn) SetDeadline(time.Time) error {
	return nil
}

// Deadlines are irrelevant to the in-memory carrier.
func (self *p2pProbeFastLoopConn) SetReadDeadline(time.Time) error {
	return nil
}

// Deadlines are irrelevant to the in-memory carrier.
func (self *p2pProbeFastLoopConn) SetWriteDeadline(time.Time) error {
	return nil
}

// The synthetic carrier is ready from construction.
func (self *p2pProbeFastLoopConn) FastPathReady() bool {
	return true
}

// The synthetic carrier is ready from construction.
func (self *p2pProbeFastLoopConn) WaitFastPathReady(context.Context, time.Duration) bool {
	return true
}

// Copies the borrowed send buffer into receiver-owned pooled storage.
func (self *p2pProbeFastLoopConn) WriteFastPathMessage(message []byte) (int, error) {
	select {
	case <-self.done:
		return 0, net.ErrClosed
	case self.fastMessages <- p2pFastPathReceivedMessage{
		message:       MessagePoolCopy(message),
		fragmentCount: 1,
	}:
		return 1, nil
	}
}

// Exposes complete reassembled messages to the production receive worker.
func (self *p2pProbeFastLoopConn) FastPathMessages() <-chan p2pFastPathReceivedMessage {
	return self.fastMessages
}

// A stable string implements net.Addr for the in-memory carrier.
type p2pProbeTestNetAddr string

// Reports the synthetic transport family.
func (self p2pProbeTestNetAddr) Network() string {
	return "probe-test"
}

// Reports the stable endpoint label.
func (self p2pProbeTestNetAddr) String() string {
	return string(self)
}
