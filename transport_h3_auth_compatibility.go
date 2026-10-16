// Negotiates the historical h3 auth response without relaxing complete echoes.
package connect

import (
	"fmt"

	"github.com/urnetwork/connect/protocol"
)

// Pre-session version 2 peers reconstruct accepted responses from the original
// auth fields, dropping both metadata and lease support. Recognize only that
// omission as unsupported extensions; a partial or contradictory echo still
// fails. The return value selects the carrier and never certifies lease support.
func negotiateH3DatagramAuthResponse(
	request *protocol.Auth,
	response *protocol.Auth,
	enabled bool,
	localQuicSupport bool,
	remoteQuicSupport bool,
) (bool, error) {
	if request == nil {
		return false, fmt.Errorf("missing H3 auth request")
	}
	negotiatedRequest := request
	if response != nil &&
		request.H3DatagramVersion == 2 && response.H3DatagramAcceptedVersion == 2 &&
		request.ClientInfo != "" && request.StreamLeaseVersion == 1 &&
		response.ClientInfo == "" && response.StreamLeaseVersion == 0 {
		// Retain every authenticated legacy field. The strict validator still
		// checks the offered/accepted versions and both QUIC capabilities.
		negotiatedRequest = &protocol.Auth{
			ByJwt:             request.ByJwt,
			AppVersion:        request.AppVersion,
			InstanceId:        request.InstanceId,
			H3DatagramVersion: request.H3DatagramVersion,
		}
	}
	return ValidateH3DatagramAuthResponse(
		negotiatedRequest,
		response,
		enabled,
		localQuicSupport,
		remoteQuicSupport,
	)
}
