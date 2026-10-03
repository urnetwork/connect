package extender

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The datagram relay (ExtenderHeader.Datagram).
//
// An extender carrier is one reliable byte stream and the extender cannot see
// inside the inner tls, so it cannot reframe what it relays. A client pinned
// to an h3 carrier therefore had no extender path at all. With this, the
// client frames its datagrams on the carrier, and here they become real udp
// packets to the destination and back.
//
// It is deliberately a different shape from the stream relay. A stream relay
// is two io.Copy calls over one connection; a datagram relay owns a udp socket
// whose lifetime is the carrier's, has to preserve boundaries in both
// directions, and must not let a peer that goes quiet hold the socket forever.

// extenderDatagramIdleTimeout releases a udp socket whose carrier has gone
// quiet in both directions.
//
// A stream relay ends when the stream ends. A udp socket has no such signal:
// nothing arrives and nothing says why. Without a bound, a client that
// disappears without closing its carrier would hold a socket and a goroutine
// pair indefinitely, which is a slow resource leak an extender must not have.
const extenderDatagramIdleTimeout = 60 * time.Second

// extenderDatagramReadBufferSize bounds one read from the destination. It is
// larger than the client-side frame bound so an oversized reply is seen and
// dropped as too large rather than silently truncated into a valid frame.
const extenderDatagramReadBufferSize = 4096

// relayDatagram carries framed datagrams between the client carrier and a udp
// socket pointed at the destination.
func (self *ExtenderServer) relayDatagram(
	ctx context.Context,
	cancel context.CancelFunc,
	clientConn net.Conn,
	header *protocol.ExtenderHeader,
) {
	destination := net.JoinHostPort(
		header.DestinationHost,
		fmt.Sprintf("%d", header.DestinationPort),
	)
	// The family of the client's outer socket, exactly as the stream relay
	// resolves it (A7): a destination with no address in that family fails
	// here rather than crossing families.
	network := datagramForwardNetwork(remoteAddressString(clientConn.RemoteAddr()))

	forwardConn, err := self.dialDatagramForward(ctx, network, destination)
	if err != nil {
		return
	}
	defer forwardConn.Close()

	// Either direction ending ends both: a udp socket with no reader is a
	// leak, and a carrier with no writer is a client waiting forever.
	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	// idle is reset by traffic in either direction, so a busy relay is never
	// closed under load and a silent one is always released.
	idle := time.NewTimer(extenderDatagramIdleTimeout)
	defer idle.Stop()
	var idleMutex sync.Mutex
	touch := func() {
		idleMutex.Lock()
		defer idleMutex.Unlock()
		idle.Reset(extenderDatagramIdleTimeout)
	}

	var relayWorkers sync.WaitGroup
	relayWorkers.Add(2)

	// client carrier -> destination
	go func() {
		defer relayWorkers.Done()
		defer relayCancel()
		buffer := make([]byte, extenderDatagramReadBufferSize)
		var reader connect.ExtenderDatagramReader
		for {
			n, err := reader.Read(clientConn, buffer)
			if err != nil {
				self.reportError("datagram relay read client", err)
				return
			}
			touch()
			if _, err := forwardConn.Write(buffer[:n]); err != nil {
				self.reportError("datagram relay write destination", err)
				return
			}
		}
	}()

	// destination -> client carrier
	go func() {
		defer relayWorkers.Done()
		defer relayCancel()
		buffer := make([]byte, extenderDatagramReadBufferSize)
		var writer connect.ExtenderDatagramWriter
		for {
			n, err := forwardConn.Read(buffer)
			if err != nil {
				self.reportError("datagram relay read destination", err)
				return
			}
			touch()
			if err := writer.Write(clientConn, buffer[:n]); err != nil {
				// An oversized reply is dropped rather than ending the relay:
				// one bad packet from the destination must not take down a
				// working connection, and quic will simply not see it.
				if connect.IsExtenderDatagramTooLarge(err) {
					self.reportError("datagram relay oversized reply", err)
					continue
				}
				self.reportError("datagram relay write client", err)
				return
			}
		}
	}()

	// Unblock both readers on cancellation or idle. Closing the sockets is the
	// only way out of a blocking Read.
	go func() {
		select {
		case <-relayCtx.Done():
		case <-idle.C:
			self.reportError("datagram relay idle", fmt.Errorf(
				"no datagram in either direction for %s", extenderDatagramIdleTimeout,
			))
		}
		clientConn.Close()
		forwardConn.Close()
	}()

	relayWorkers.Wait()
	cancel()
}

// dialDatagramForward opens the udp socket to the destination, through the
// same egress seam the stream relay and the reverse proxy use so a test that
// injects one observes all three.
func (self *ExtenderServer) dialDatagramForward(
	ctx context.Context,
	network string,
	destination string,
) (net.Conn, error) {
	forwardConn, err := self.dialPacketContext()(ctx, network, destination)
	if err != nil {
		if forwardConn != nil {
			forwardConn.Close()
		}
		self.reportError("datagram forward dial", err)
		return nil, err
	}
	if forwardConn == nil {
		err = fmt.Errorf("datagram forward dial returned nil connection")
		self.reportError("datagram forward dial", err)
		return nil, err
	}
	return forwardConn, nil
}

// The udp egress of this extender: the configured seam, or the forward dialer.
func (self *ExtenderServer) dialPacketContext() connect.DialContextFunction {
	if self.settings.DialPacketContext != nil {
		return self.settings.DialPacketContext
	}
	return self.forwardDialer.DialContext
}

// datagramForwardNetwork is forwardNetwork for udp: the family of the client's
// outer socket, and the unnarrowed network for an address with no family such
// as an in-memory pipe.
func datagramForwardNetwork(clientAddress string) string {
	switch forwardNetwork(clientAddress) {
	case "tcp4":
		return "udp4"
	case "tcp6":
		return "udp6"
	default:
		return "udp"
	}
}
