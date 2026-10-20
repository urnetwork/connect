package connect

import (
	"errors"
	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
)

type AuthorizationCloseCause int

const (
	AuthorizationCloseUnavailable AuthorizationCloseCause = 4002
	AuthorizationCloseRevoked     AuthorizationCloseCause = 4003
	AuthorizationCloseExpired     AuthorizationCloseCause = 4004
	AuthorizationCloseRejected    AuthorizationCloseCause = 4005
)
const (
	TransportCloseReasonAuthorizationUnavailable uint32 = 2
	TransportCloseReasonSessionRevoked           uint32 = 3
	TransportCloseReasonCredentialExpired        uint32 = 4
	TransportCloseReasonCredentialRejected       uint32 = 5
)

func authorizationCloseCause(err error) AuthorizationCloseCause {
	var ws *websocket.CloseError
	if errors.As(err, &ws) {
		return validAuthorizationClose(ws.Code)
	}
	var q *quic.ApplicationError
	if errors.As(err, &q) && q.Remote {
		return validAuthorizationClose(int(q.ErrorCode))
	}
	return 0
}
func validAuthorizationClose(code int) AuthorizationCloseCause {
	if code >= 4002 && code <= 4005 {
		return AuthorizationCloseCause(code)
	}
	return 0
}
func (self *PlatformTransport) noteAuthorizationClose(cause AuthorizationCloseCause) {
	if cause != 0 && self.settings.AuthorizationClosed != nil {
		self.settings.AuthorizationClosed(cause)
	}
}
