// Close-report signatures use the actual client key owner and one immutable
// policy domain. Retries retain the already encoded report instead of resigning.
package connect

import (
	"errors"

	"github.com/urnetwork/connect/protocol"
)

// Key selection and signing share the same lock as rotation. Cleanup after
// lifecycle cancellation may sign its one bounded final original, as before.
func (self *ClientKeyManager) signOriginalCloseReport(domainHash [32]byte, clientId Id, report *protocol.CloseContract) ([]byte, error) {
	if self == nil || report == nil || len(report.ContractId) != 16 || len(report.ReportId) != 16 {
		return nil, errors.New("original close report key owner or tuple is unavailable")
	}
	self.stateLock.RLock()
	defer self.stateLock.RUnlock()
	original, err := protocol.SignOriginalCloseReport(protocol.OriginalCloseReport{
		DomainHash: domainHash, ClientId: [16]byte(clientId),
		ContractId: [16]byte(report.ContractId), ReportId: [16]byte(report.ReportId),
		AckedByteCount: report.AckedByteCount, UnackedByteCount: report.UnackedByteCount,
		Checkpoint: report.Checkpoint,
	}, self.privateKey)
	if err != nil {
		return nil, err
	}
	return original.Bytes()
}
