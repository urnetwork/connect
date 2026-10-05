//go:build ignore

// Reviewed test NAT: its version nibble selects which IP family to drop.
package sdk

import (
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type socketTestNAT struct {
	peer    *connect.Tun
	drop    atomic.Int32
	packets atomic.Int64
}

func (n *socketTestNAT) SendPacket(_ connect.TransferPath, _ protocol.ProvideMode, p []byte, _ time.Duration) bool {
	defer connect.MessagePoolReturn(p)
	n.packets.Add(1)
	path, _ := connect.ParseIpPath(p)
	if int32(p[0]>>4) != n.drop.Load() || (path != nil && path.DestinationPort == 53) {
		_, _ = n.peer.Write(p)
	}
	return true
}
