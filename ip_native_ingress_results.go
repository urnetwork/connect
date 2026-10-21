// Exact native-burst results support owners which retain per-packet admission
// observations without changing the mobile count-only entry or packet custody.
package connect

import (
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Takes every packet and borrows both slices. Result length must match before
// any ownership transfers, exactly as SendPacketBatchWithResults requires.
// Only remote admission differs; local routing keeps localTimeout.
func (self *RemoteUserNatMultiClient) SendNativePacketBatchWithResults(
	source TransferPath,
	provideMode protocol.ProvideMode,
	packets [][]byte,
	localTimeout time.Duration,
	accepted []bool,
) int {
	if len(accepted) != len(packets) {
		panic("SendNativePacketBatchWithResults: result length must match packet count")
	}
	clear(accepted)
	return self.sendPacketBatchWithPolicy(source, provideMode, packets, localTimeout, accepted, true)
}
