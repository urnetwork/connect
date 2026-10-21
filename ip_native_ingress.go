// Native TUN admission makes one ready remote offer without adding a packet
// queue. Local protocol owners retain their existing bounded send timeout.
package connect

import (
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An optional native-input capability; takes every packet on entry and borrows
// the outer slice. Refused remote TCP packets remain the source TCP's recovery
// responsibility, before Transfer has accepted or acknowledged any ownership.
type UserNatNativeBatchClient interface {
	UserNatClient
	SendNativePacketBatch(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, localTimeout time.Duration) int
}

// Takes the native burst; only remote admission is ready-only. Nested local
// overrides and the existing fragment/SMTP policy keep their original owners.
func (self *RemoteUserNatMultiClient) SendNativePacketBatch(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, localTimeout time.Duration) int {
	return self.sendPacketBatchWithPolicy(source, provideMode, packets, localTimeout, nil, true)
}

// Takes the native burst through the same DNS, HTTP and exact-flow classifier.
// A custom upstream without the capability retains its compatibility contract.
func (self *UpgradeMux) SendNativePacketBatch(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, localTimeout time.Duration) int {
	upstream := self.upstreamMultiClient.Load()
	if upstream == nil {
		return self.SendPacketBatch(source, provideMode, packets, localTimeout)
	}
	return self.mux.sendPacketBatchWithUpstreams(source, provideMode, packets, localTimeout,
		func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
			return upstream.sendPacketWithPolicy(source, provideMode, packet, timeout, true)
		}, upstream.sendNativePacketGroup, true)
}

// Takes on success; a native offer cannot retain a retry continuation. Policy,
// collapse and encryption readiness are still evaluated on the actual offer.
func (self *RemoteUserNatMultiClient) sendNativePacketGroup(source TransferPath, provideMode protocol.ProvideMode, group *ipPacketGroup, localTimeout time.Duration) (success bool) {
	if group == nil {
		return false
	}
	admission := ipPacketGroupAdmission{nativeIngress: true}
	previousAdmission := group.batchAdmission
	group.batchAdmission = &admission
	defer func() {
		if admission.parsed != nil && admission.parsed.admissionObservations != nil {
			admission.parsed.admissionObservations.complete(success)
		}
		group.batchAdmission = previousAdmission
	}()
	return self.sendPacketGroup(source, provideMode, group, localTimeout)
}

// The group has already passed local-vs-remote policy classification. No new
// delivery authority is created by a failed ready offer or encryption wait.
func (self *RemoteUserNatMultiClient) sendNativeParsedPacketGroup(source TransferPath, provideMode protocol.ProvideMode, group *parsedPacketGroup) bool {
	admission := ipPacketGroupAdmission{nativeIngress: true}
	return self.sendParsedPacketGroupAttempt(source, provideMode, group, 0, true, &admission)
}
