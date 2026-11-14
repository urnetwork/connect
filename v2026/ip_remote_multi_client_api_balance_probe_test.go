package connect

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// An in-process control that answers every CreateContract with `answer` and
// records what it was asked, as the hosted proxy's authority does.
type balanceProbeTestControl struct {
	answer func(createContract *protocol.CreateContract) (*protocol.CreateContractResult, error)

	stateLock         sync.Mutex
	tokens            []string
	creates           []*protocol.CreateContract
	closedContractIds []Id
}

func (self *balanceProbeTestControl) ConnectControl(ctx context.Context, token string, args *ConnectControlArgs) (*ConnectControlResult, error) {
	packBytes, err := DecodeBase64(base64.StdEncoding, args.Pack)
	if err != nil {
		return nil, err
	}
	pack := &protocol.Pack{}
	err = ProtoUnmarshal(packBytes, pack)
	MessagePoolReturn(packBytes)
	if err != nil {
		return nil, err
	}
	out := []*protocol.Frame{}
	defer func() {
		for _, frame := range out {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	for _, frame := range pack.Frames {
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		switch v := message.(type) {
		case *protocol.CreateContract:
			self.stateLock.Lock()
			self.tokens = append(self.tokens, token)
			self.creates = append(self.creates, v)
			self.stateLock.Unlock()
			result, err := self.answer(v)
			if err != nil {
				return nil, err
			}
			resultFrame, err := ToFrame(result, DefaultProtocolVersion)
			if err != nil {
				return nil, err
			}
			out = append(out, resultFrame)
		case *protocol.CloseContract:
			contractId, err := IdFromBytes(v.ContractId)
			if err != nil {
				return nil, err
			}
			self.stateLock.Lock()
			self.closedContractIds = append(self.closedContractIds, contractId)
			self.stateLock.Unlock()
		}
	}
	resultBytes, err := ProtoMarshal(&protocol.Pack{Frames: out})
	if err != nil {
		return nil, err
	}
	encoded := EncodeBase64(base64.StdEncoding, resultBytes)
	MessagePoolReturn(resultBytes)
	return &ConnectControlResult{Pack: encoded}, nil
}

// The probe asks for a window contract's standard size to the refused
// destination under the generator's own credential, and reads the answer: a
// refusal for the balance is short, a granted contract is restored and is
// closed unused, a rate limit is limited, and anything else is unknown.
func TestApiGeneratorProbeBalanceReadsThePlatformsAnswer(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	short := protocol.ContractError_InsufficientBalance
	setup := protocol.ContractError_Setup
	grantedContractId := NewId()
	for _, c := range []struct {
		name    string
		answer  func(createContract *protocol.CreateContract) (*protocol.CreateContractResult, error)
		outcome BalanceProbeOutcome
		closed  []Id
	}{
		{
			name: "short",
			answer: func(*protocol.CreateContract) (*protocol.CreateContractResult, error) {
				return &protocol.CreateContractResult{Error: &short}, nil
			},
			outcome: BalanceProbeShort,
		},
		{
			name: "restored",
			answer: func(createContract *protocol.CreateContract) (*protocol.CreateContractResult, error) {
				storedContractBytes, err := ProtoMarshal(&protocol.StoredContract{
					ContractId:        grantedContractId.Bytes(),
					TransferByteCount: createContract.TransferByteCount,
					DestinationId:     createContract.DestinationId,
				})
				if err != nil {
					return nil, err
				}
				return &protocol.CreateContractResult{Contract: &protocol.Contract{StoredContractBytes: storedContractBytes}}, nil
			},
			outcome: BalanceProbeRestored,
			closed:  []Id{grantedContractId},
		},
		{
			name: "rate limited",
			answer: func(*protocol.CreateContract) (*protocol.CreateContractResult, error) {
				return nil, &HttpStatusError{StatusCode: http.StatusTooManyRequests}
			},
			outcome: BalanceProbeLimited,
		},
		{
			name: "setup",
			answer: func(*protocol.CreateContract) (*protocol.CreateContractResult, error) {
				return &protocol.CreateContractResult{Error: &setup}, nil
			},
			outcome: BalanceProbeUnknown,
		},
		{
			name: "failed",
			answer: func(*protocol.CreateContract) (*protocol.CreateContractResult, error) {
				return nil, errors.New("synthetic control unavailable")
			},
			outcome: BalanceProbeUnknown,
		},
	} {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("the probe contacted the public api")
			return nil, errors.New("forbidden")
		}))
		control := &balanceProbeTestControl{answer: c.answer}
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.ClientControl = control
		generator := NewApiMultiClientGenerator(t.Context(), nil, strategy, nil,
			"https://api.probe.example", "synthetic-device-token", "wss://platform.probe.example",
			"synthetic probe", "synthetic", "test", nil, DefaultClientSettings, settings)
		destinationId := NewId()

		outcome := generator.ProbeBalance(t.Context(), destinationId)

		if outcome != c.outcome {
			t.Errorf("%s: outcome %s, want %s", c.name, outcome, c.outcome)
		}
		control.stateLock.Lock()
		if len(control.creates) != 1 {
			t.Errorf("%s: %d contract requests, want one", c.name, len(control.creates))
		} else {
			createContract := control.creates[0]
			if requested, err := IdFromBytes(createContract.DestinationId); err != nil || requested != destinationId {
				t.Errorf("%s: the probe asked for %s, want the refused destination", c.name, requested)
			}
			if ByteCount(createContract.TransferByteCount) != DefaultContractManagerSettings().StandardContractTransferByteCount {
				t.Errorf("%s: the probe asked for %d bytes, want a window contract's standard size", c.name, createContract.TransferByteCount)
			}
			if createContract.Companion {
				t.Errorf("%s: the probe asked for a companion contract", c.name)
			}
			if control.tokens[0] != "synthetic-device-token" {
				t.Errorf("%s: the probe used another credential than the generator's own", c.name)
			}
		}
		if len(control.closedContractIds) != len(c.closed) {
			t.Errorf("%s: %d contracts closed, want %d", c.name, len(control.closedContractIds), len(c.closed))
		}
		for i := range min(len(control.closedContractIds), len(c.closed)) {
			if control.closedContractIds[i] != c.closed[i] {
				t.Errorf("%s: closed %s, want the granted %s", c.name, control.closedContractIds[i], c.closed[i])
			}
		}
		control.stateLock.Unlock()
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Errorf("%s: close the generator: %v", c.name, err)
		}
		api.Close()
	}
}
