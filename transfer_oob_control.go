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

	self.api.ConnectControl(
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

	safeCallback(nil, errors.New("Not supported."))
}
