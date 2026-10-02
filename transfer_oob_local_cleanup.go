package connect

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/protocol"
)

// The original request remains admitted until this single cleanup attempt and
// its callback finish, including when CloseAndWait already closed admission.
// There is no HTTP fallback, new lifecycle owner, or retry. Only the requester
// reports zero use; provider accounting and eventual settlement remain normal.
func (self *ApiOutOfBandControl) closeUndeliveredLocalContracts(ctx context.Context, byJwt string, request *ConnectControlArgs, result *ConnectControlResult) error {
	if result == nil || result.Pack == "" {
		return nil
	}
	frames, err := undeliveredLocalContractCloses(byJwt, request, result)
	if err != nil {
		return err
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	if len(frames) == 0 {
		return nil
	}
	pack, err := ProtoMarshal(&protocol.Pack{Frames: frames})
	if err != nil {
		return err
	}
	encoded := EncodeBase64(base64.StdEncoding, pack)
	MessagePoolReturn(pack)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), self.api.clientStrategy.settings.RequestTimeout)
	defer cancel()
	closed, err := self.localControl.ConnectControl(cleanupCtx, byJwt, &ConnectControlArgs{Pack: encoded})
	if err != nil || cleanupCtx.Err() != nil {
		return errors.Join(err, cleanupCtx.Err())
	}
	if closed == nil || closed.Error != nil {
		return errors.New("local undelivered contract close was not acknowledged")
	}
	return nil
}

// Extract only successful contracts returned for destinations in this exact
// create request and belonging to its original JWT's client. Unverified JWT
// parsing supplies an additional equality fence, never authorization: the same
// local controller reauthenticates that exact token for the close. No unknown
// or missing result is guessed, and no contract is closed twice in this pack.
func undeliveredLocalContractCloses(byJwt string, request *ConnectControlArgs, result *ConnectControlResult) (frames []*protocol.Frame, returnErr error) {
	decode := func(encoded string) (*protocol.Pack, error) {
		bytes, err := DecodeBase64(base64.StdEncoding, encoded)
		if err != nil {
			return nil, err
		}
		defer MessagePoolReturn(bytes)
		pack := &protocol.Pack{}
		if err := ProtoUnmarshal(bytes, pack); err != nil {
			return nil, err
		}
		return pack, nil
	}
	original, err := decode(request.Pack)
	if err != nil {
		return nil, err
	}
	destinations := map[Id]int{}
	for _, frame := range original.Frames {
		message, err := FromFrame(frame)
		if err != nil {
			return nil, err
		}
		if create, ok := message.(*protocol.CreateContract); ok {
			destination, err := IdFromBytes(create.DestinationId)
			if err != nil {
				return nil, err
			}
			destinations[destination]++
		}
	}
	if len(destinations) == 0 {
		return nil, nil
	}
	claims := gojwt.MapClaims{}
	_, _, err = gojwt.NewParser().ParseUnverified(byJwt, claims)
	clientText, validClient := claims["client_id"].(string)
	ownerClient, clientErr := ParseId(clientText)
	if err != nil || !validClient || clientErr != nil || ownerClient == (Id{}) {
		return nil, errors.New("local undelivered contract owner is unavailable")
	}
	returned, err := decode(result.Pack)
	if err != nil {
		return nil, err
	}
	contracts := map[Id]bool{}
	for _, frame := range returned.Frames {
		message, err := FromFrame(frame)
		if err != nil {
			return nil, err
		}
		created, ok := message.(*protocol.CreateContractResult)
		if !ok || created.Error != nil || created.Contract == nil {
			continue
		}
		stored := &protocol.StoredContract{}
		if err := ProtoUnmarshal(created.Contract.StoredContractBytes, stored); err != nil {
			return nil, err
		}
		contract, idErr := IdFromBytes(stored.ContractId)
		source, sourceErr := IdFromBytes(stored.SourceId)
		destination, destinationErr := IdFromBytes(stored.DestinationId)
		if idErr != nil || sourceErr != nil || destinationErr != nil || contract == (Id{}) || source != ownerClient {
			return nil, errors.New("local undelivered contract identity does not match its owner")
		}
		if contracts[contract] {
			continue
		}
		if destinations[destination] == 0 {
			return nil, errors.New("local undelivered contract destination is outside its request")
		}
		destinations[destination]--
		contracts[contract] = true
	}
	defer func() {
		if returnErr != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			frames = nil
		}
	}()
	for contract := range contracts {
		frame, err := ToFrame(&protocol.CloseContract{ContractId: contract.Bytes()}, DefaultProtocolVersion)
		if err != nil {
			return frames, fmt.Errorf("local undelivered contract close encoding failed: %w", err)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}
