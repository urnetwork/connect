// DATA-credit refusal controls need a genuine DATA-bearing SYN. Pure protocol
// count readiness has separate public tests; existing offer deadlines stay exact.
package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Borrows flow metadata and transfers one newly owned native packet through
// the ordinary batch entry. The real payload makes byte-credit policy apply.
func privateNativeDataSyn(t *testing.T, harness *reraceHarness, path *IpPath, timeout time.Duration) bool {
	t.Helper()
	packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, []byte{0x64}))
	if protocolClassPureTcpControl(packet) {
		MessagePoolReturn(packet)
		t.Fatal("DATA-bearing native SYN was misclassified as protocol-only")
	}
	accepted := [1]bool{}
	count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network,
		[][]byte{packet}, timeout, accepted[:])
	harness.settle()
	if count < 0 || count > 1 || (count == 1) != accepted[0] {
		t.Fatal("DATA-bearing native offer lost its original exact disposition")
	}
	return accepted[0]
}
