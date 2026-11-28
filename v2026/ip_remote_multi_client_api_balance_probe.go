// The API generator's balance probe (MultiClientGeneratorBalanceProbe, see
// ip_remote_multi_client_balance_hold.go).
//
// A held window asks through its generator, which holds the device's own
// credential, so a probe needs no minted identity, no Provide and no platform
// transport. The CreateContract goes through the same control path the
// window clients use: the hosted proxy's in-process authority, or the api.
// It asks for a window contract's standard size, so the escrow the platform
// checks is the escrow a window client's contract needs. A granted contract
// is closed at once, unused; if that close fails, the platform's contract
// expiry closes it.
package connect

import (
	"context"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Implements MultiClientGeneratorBalanceProbe. Safe for concurrent use.
func (self *ApiMultiClientGenerator) ProbeBalance(ctx context.Context, destinationId Id) BalanceProbeOutcome {
	if self.api == nil {
		return BalanceProbeUnknown
	}
	byJwt := self.api.ByJwt()
	if byJwt == "" {
		return BalanceProbeUnknown
	}
	var transferByteCount ByteCount
	if self.settings != nil {
		transferByteCount = self.settings.BalanceProbeTransferByteCount
	}
	if transferByteCount <= 0 {
		transferByteCount = DefaultContractManagerSettings().StandardContractTransferByteCount
	}
	streamVersion := uint32(DefaultStreamVersion)
	frame, err := ToFrame(&protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: uint64(transferByteCount),
		StreamVersion:     &streamVersion,
	}, DefaultProtocolVersion)
	if err != nil {
		return BalanceProbeUnknown
	}
	clientOob := self.newClientOob(self.ctx, byJwt)
	defer clientOob.Close()

	// the result frames are the control's, valid only in its callback
	outcome := BalanceProbeUnknown
	grantedContracts := []*protocol.Contract{}
	err = sendControlAndWait(ctx, clientOob, []*protocol.Frame{frame}, func(resultFrames []*protocol.Frame) {
		for _, resultFrame := range resultFrames {
			message, err := FromFrame(resultFrame)
			if err != nil {
				continue
			}
			result, ok := message.(*protocol.CreateContractResult)
			if !ok {
				continue
			}
			if contract := result.Contract; contract != nil {
				outcome = BalanceProbeRestored
				grantedContracts = append(grantedContracts, contract)
			} else if result.Error != nil && *result.Error == protocol.ContractError_InsufficientBalance &&
				outcome == BalanceProbeUnknown {
				outcome = BalanceProbeShort
			}
		}
	})
	if err != nil {
		if controlRateLimited(err) {
			return BalanceProbeLimited
		}
		return BalanceProbeUnknown
	}
	for _, contract := range grantedContracts {
		closeBalanceProbeContract(ctx, clientOob, contract)
	}
	return outcome
}

// Closes a probe's granted contract unused, and waits for the answer so the
// owner may close the control after. A failed close leaves the contract to
// the platform's expiry.
func closeBalanceProbeContract(ctx context.Context, clientOob *ApiOutOfBandControl, contract *protocol.Contract) {
	storedContract := &protocol.StoredContract{}
	if err := ProtoUnmarshal(contract.StoredContractBytes, storedContract); err != nil {
		return
	}
	contractId, err := IdFromBytes(storedContract.ContractId)
	if err != nil {
		return
	}
	frame, err := ToFrame(&protocol.CloseContract{
		ContractId: contractId.Bytes(),
		ReportId:   NewId().Bytes(),
	}, DefaultProtocolVersion)
	if err != nil {
		return
	}
	sendControlAndWait(ctx, clientOob, []*protocol.Frame{frame}, func([]*protocol.Frame) {})
}

// One control request, waited for. `handle` reads the result frames inside
// the control's callback, where they are valid. The control owns the request
// once sent; a canceled wait returns the context's error and leaves the
// request to it.
func sendControlAndWait(
	ctx context.Context,
	clientOob OutOfBandControlWithCtx,
	frames []*protocol.Frame,
	handle func(resultFrames []*protocol.Frame),
) error {
	done := make(chan error, 1)
	clientOob.SendControlWithCtx(ctx, frames, func(resultFrames []*protocol.Frame, err error) {
		if err == nil {
			handle(resultFrames)
		}
		done <- err
	})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
