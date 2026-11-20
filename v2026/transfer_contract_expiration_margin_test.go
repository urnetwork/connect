// A send contract retires a margin ahead of its signed deadline, so the
// messages it carries are acknowledged while the receiver still admits it.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// A send contract stops taking new messages a margin ahead of its signed
// deadline (ContractExpirationMargin). The receiver checks the deadline by its
// own clock as each message arrives and refuses one under a contract past it
// for good, so a sequence that carries little, and holds one contract to its
// deadline, stalled until its ACK lifetime closed it (measured: a quiet tunnel
// died exactly one hour after its contract was made). The message that finds
// the contract retiring takes the acknowledged successor; the retired
// contract keeps its pending accounting until its ACKs settle it.
func TestContractExpirationRetiresAheadOfTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, sequence, destination, old := newSendNoContractHarness(t, t.Context())
		expirationRequireSendContract(client, destination)
		margin := sequence.sendBufferSettings.ContractExpirationMargin
		if margin <= 0 {
			t.Fatal("the default send buffer settings carry no expiration margin")
		}
		deadline := time.Now().Add(margin + time.Second).UnixMilli()
		old.expirationTimeUnixMilli = &deadline
		ahead := newContractAheadTestContract(t, client, destination)
		ahead.acknowledgedAhead = true
		sequence.openSendContracts[ahead.contractId] = ahead
		sequence.aheadSendContract = ahead
		sequence.aheadSendContractMetadataGeneration = sequence.contractMetadata().generation

		if !sequence.updateContract(100) || sequence.sendContract != old || !sequence.sendContractCanUpdate(1) {
			t.Fatal("a contract outside its margin stopped taking messages")
		}
		time.Sleep(2 * time.Second)
		if old.expired() {
			t.Fatal("the margin was reached only at the deadline itself")
		}
		if sequence.sendContractCanUpdate(1) {
			t.Fatal("the coalescing check would still size a batch for a retiring contract")
		}
		if !sequence.updateContract(50) || sequence.sendContract != ahead || !sequence.sendContractAcked {
			t.Fatal("a retiring contract did not hand off to its acknowledged successor")
		}
		if old.unackedByteCount != 100 || sequence.openSendContracts[old.contractId] != old {
			t.Fatal("retiring ahead of the deadline changed or closed the pending accounting")
		}
		if ahead.unackedByteCount != 50 {
			t.Fatal("the message inside the margin was not attributed to the successor")
		}
		sequence.ackItem(newContractSendItem(&old.contractId, 0, 100))
		if old.ackedByteCount != 100 || old.unackedByteCount != 0 || sequence.openSendContracts[old.contractId] != nil {
			t.Fatal("the retired contract's acknowledgement did not settle and close it")
		}
	})
}

// A grant shorter than four margins keeps the first three quarters of the life
// it had when the sequence took it: the margin never retires a contract the
// moment it arrives, which would request one per message.
func TestContractExpirationMarginShrinksForShortGrants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, sequence, destination, contract := newSendNoContractHarness(t, t.Context())
		expirationRequireSendContract(client, destination)
		life := sequence.sendBufferSettings.ContractExpirationMargin
		sequence.sendContractSetTime = time.Now()
		deadline := time.Now().Add(life).UnixMilli()
		contract.expirationTimeUnixMilli = &deadline
		time.Sleep(life / 2)
		if sequence.sendContractRetiring(time.Now()) || !sequence.updateContract(10) || sequence.sendContract != contract {
			t.Fatal("a short grant retired before three quarters of its life")
		}
		time.Sleep(life/4 + time.Second)
		if contract.expired() || !sequence.sendContractRetiring(time.Now()) {
			t.Fatal("a short grant was not retired in the last quarter of its life")
		}
	})
}

// An announced successor already within its margin of its own deadline would
// retire on the message after the switch: it is discarded, not taken.
func TestContractExpirationDiscardsAnAheadContractWithinItsMargin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, sequence, destination, _ := newSendNoContractHarness(t, t.Context())
		expirationRequireSendContract(client, destination)
		margin := sequence.sendBufferSettings.ContractExpirationMargin
		ahead := newContractAheadTestContract(t, client, destination)
		ahead.acknowledgedAhead = true
		aheadDeadline := time.Now().Add(margin / 2).UnixMilli()
		ahead.expirationTimeUnixMilli = &aheadDeadline
		sequence.openSendContracts[ahead.contractId] = ahead
		sequence.aheadSendContract = ahead
		sequence.aheadSendContractMetadataGeneration = sequence.contractMetadata().generation
		if sequence.setAheadContract(10) || sequence.aheadSendContract != nil || sequence.sendContract == ahead {
			t.Fatal("a successor within its own margin was taken")
		}
	})
}
