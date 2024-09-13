package pathsource_test

import (
	"net/netip"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/pathsource"
)

func TestPathToSourceAddress(t *testing.T) {
	pth := &connect.TransferPath{
		SourceId:      connect.Id{},
		DestinationId: connect.Id{},
		StreamId:      connect.Id{},
	}

	sourceAddress := []byte{192, 168, 1, 1}
	sourcePort := uint16(1234)

	ap := pathsource.PathToSourceAddress(pth, sourceAddress, sourcePort)
	assert.Equal(t, ap, netip.AddrPortFrom(netip.AddrFrom4([4]byte{181, 92, 103, 96}), 7285))

}
