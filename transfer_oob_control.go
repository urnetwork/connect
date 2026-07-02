package connect

import (
	"context"
	"errors"

	"encoding/base64"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// control messages for a client out of band with the client sequence
// some control messages require blocking response, but there is a potential deadlock
// when a send blocks to wait for a control receive, or vice versa, since
// all clients messages are multiplexed in the same client sequence
// and the receive/send may be blocked on the send/receive
// for example think of a remote provider setup forwarding traffic as fast as possible
// to an "echo" server with a finite buffer

type OobResultFunction = func(resultFrames []*protocol.Frame, err error)

type OutOfBandControl interface {
	SendControl(frames []*protocol.Frame, callback OobResultFunction)
}

// OutOfBandControlWithCtx is an optional upgrade interface for one-shot
// control with a caller-chosen context — e.g. shutdown cleanup that must
// outlive the closed client lifecycle (closing pending contracts). Normal
// control should use `SendControl`, which stays bound to the lifecycle
// context.
type OutOfBandControlWithCtx interface {
	OutOfBandControl
	SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction)
}

type ApiOutOfBandControl struct {
	api *BringYourApi
}

func NewApiOutOfBandControl(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	byJwt string,
	apiUrl string,
) *ApiOutOfBandControl {
	api := NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)
	return &ApiOutOfBandControl{
		api: api,
	}
}

func NewApiOutOfBandControlWithApi(api *BringYourApi) *ApiOutOfBandControl {
	return &ApiOutOfBandControl{
		api: api,
	}
}

func (self *ApiOutOfBandControl) SendControl(
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	// bound to the api lifecycle context: keep trying as long as the
	// lifecycle is active
	self.sendControl(self.api.ConnectControl, frames, callback)
}

// SendControlWithCtx is a one-shot send on a caller-chosen context, for
// cleanup that must not be bound to the (possibly closed) lifecycle context.
// The request stays bounded by the client strategy's `RequestTimeout`.
func (self *ApiOutOfBandControl) SendControlWithCtx(
	ctx context.Context,
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	connectControl := func(connectControlArgs *ConnectControlArgs, apiCallback ConnectControlCallback) {
		self.api.ConnectControlWithCtx(ctx, connectControlArgs, apiCallback)
	}
	self.sendControl(connectControl, frames, callback)
}

func (self *ApiOutOfBandControl) sendControl(
	connectControl func(*ConnectControlArgs, ConnectControlCallback),
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	safeCallback := func(resultFrames []*protocol.Frame, err error) {
		if callback != nil {
			HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}

	pack := &protocol.Pack{
		Frames: frames,
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	packBytes, err := ProtoMarshal(pack)
	if err != nil {
		safeCallback(nil, err)
		return
	}
	defer MessagePoolReturn(packBytes)

	connectControl(
		&ConnectControlArgs{
			Pack: EncodeBase64(base64.StdEncoding, packBytes),
		},
		NewApiCallback(func(result *ConnectControlResult, err error) {
			if err != nil {
				safeCallback(nil, err)
				return
			}

			packBytes, err := DecodeBase64(base64.StdEncoding, result.Pack)
			if err != nil {
				safeCallback(nil, err)
				return
			}
			defer MessagePoolReturn(packBytes)

			responsePack := &protocol.Pack{}
			err = ProtoUnmarshal(packBytes, responsePack)
			if err != nil {
				safeCallback(nil, err)
				return
			}

			safeCallback(responsePack.Frames, nil)
		}),
	)
}

type NoContractClientOob struct {
}

func NewNoContractClientOob() *NoContractClientOob {
	return &NoContractClientOob{}
}

func (self *NoContractClientOob) SendControl(frames []*protocol.Frame, callback func(resultFrames []*protocol.Frame, err error)) {
	safeCallback := func(resultFrames []*protocol.Frame, err error) {
		if callback != nil {
			HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}

	// SendControl takes ownership of the frames; this oob cannot deliver them but
	// must still release the pooled bytes (mirrors ApiOutOfBandControl.sendControl)
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}

	safeCallback(nil, errors.New("Not supported."))
}
