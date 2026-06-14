package connect

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// ControlSyncOob is like ControlSync, but delivers control messages via the
// client out-of-band control (a synchronous request/response to the platform)
// instead of the in-band client sequence. Because the oob call returns the
// platform's response, its ack means the message was PROCESSED (e.g. a provide
// secret is registered on the platform), not merely delivered — which is the
// reliable "committed" signal a normal control ack does not give. It retries on
// error until success or the context/client is closed, and only the latest
// message per scope is retried.
type ControlSyncOob struct {
	ctx    context.Context
	cancel context.CancelFunc

	client       *Client
	scopeTag     string
	retryTimeout time.Duration

	sendLock  sync.Mutex
	syncCount uint64
}

func NewControlSyncOob(ctx context.Context, client *Client, scopeTag string) *ControlSyncOob {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ControlSyncOob{
		ctx:          cancelCtx,
		cancel:       cancel,
		client:       client,
		scopeTag:     scopeTag,
		retryTimeout: 1 * time.Second,
	}
}

// Send delivers `frame` via the client out-of-band control, retrying on error
// until success or the context/client is closed. `ackCallback` fires (once) when
// the platform has processed the message (the oob call returns). The caller's
// `frame` is consumed here.
func (self *ControlSyncOob) Send(frame *protocol.Frame, ackCallback AckFunction) {
	safeAckCallback := func(err error) {
		if ackCallback != nil {
			HandleError(func() {
				ackCallback(err)
			})
		}
	}

	// own a plain copy of the payload so it survives retries (the oob consumes
	// the frame it is given); the caller's frame is returned to the pool here
	messageType := frame.MessageType
	payload := append([]byte(nil), frame.MessageBytes...)
	MessagePoolReturn(frame.MessageBytes)

	self.sendLock.Lock()
	self.syncCount += 1
	syncIndex := self.syncCount
	self.sendLock.Unlock()

	handleCtx, handleCancel := context.WithCancel(self.ctx)

	var send func()
	send = func() {
		// stop if a newer Send for this scope superseded this one, or done
		self.sendLock.Lock()
		superseded := syncIndex != self.syncCount
		self.sendLock.Unlock()
		if superseded {
			handleCancel()
			return
		}
		select {
		case <-handleCtx.Done():
			handleCancel()
			return
		case <-self.client.Done():
			handleCancel()
			return
		default:
		}

		attemptFrame := &protocol.Frame{
			MessageType:  messageType,
			MessageBytes: MessagePoolCopy(payload),
		}
		self.client.ClientOob().SendControl(
			[]*protocol.Frame{attemptFrame},
			func(resultFrames []*protocol.Frame, err error) {
				if err == nil {
					// the oob returned: the platform has processed the message
					safeAckCallback(nil)
					handleCancel()
					return
				}
				self.client.log.V(2).Infof("[control-oob][%d]retry scope = %s err = %s\n", syncIndex, self.scopeTag, err)
				select {
				case <-handleCtx.Done():
				case <-self.client.Done():
				case <-time.After(self.retryTimeout):
					go HandleError(send, handleCancel)
				}
			},
		)
	}

	go HandleError(send, handleCancel)
}

func (self *ControlSyncOob) Close() {
	self.cancel()
}
