package connect

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
	"time"

	// mathrand "math/rand"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// TestTakeContractStreamStamped pins the oob loop behind platform stream
// steering: the reply sequence requests contracts under a plain (no-stream)
// contract key, and the platform may return a contract whose stored bytes
// carry a stream id ("switch to the stream"). The stamped contract must
// queue under the request's key and carry the stream id through
// `newSequenceContract` into the sequence path — which is what steers the
// writer onto the stream and marks the contract stats.
func TestTakeContractStreamStamped(t *testing.T) {
	ctx := context.Background()
	clientId := NewId()
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.LegacyCreateContract = true
	client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	destinationId := NewId()
	streamId := NewId()

	contractManager.SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	relationship := protocol.ProvideMode_Network
	provideSecretKey, ok := contractManager.GetProvideSecretKey(relationship)
	AssertEqual(t, true, ok)

	storedContract := &protocol.StoredContract{
		ContractId:        NewId().Bytes(),
		TransferByteCount: uint64(gib(1)),
		SourceId:          clientId.Bytes(),
		DestinationId:     destinationId.Bytes(),
		StreamId:          streamId.Bytes(),
	}
	storedContractBytes, err := ProtoMarshal(storedContract)
	AssertEqual(t, nil, err)
	storedContractHmac := SignStoredContract(contractManager.settings, provideSecretKey, storedContractBytes)

	result := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac:  storedContractHmac,
			ProvideMode:         relationship,
		},
	}
	frame, err := ToFrame(result, DefaultProtocolVersion)
	AssertEqual(t, nil, err)

	// the request key has no stream — the requester does not know the
	// platform will steer it
	contractKey := ContractKey{
		Destination: DestinationId(destinationId),
	}
	err = contractManager.HandleControlFrame(contractKey, frame)
	AssertEqual(t, nil, err)

	contract := contractManager.TakeContract(ctx, contractKey, 5*time.Second)
	AssertEqual(t, true, contract != nil)

	sequenceContract, err := newSequenceContract(client.log, "s", contract, 1, 1.0)
	AssertEqual(t, nil, err)
	AssertEqual(t, true, sequenceContract.path.IsStream())
	AssertEqual(t, streamId, sequenceContract.path.StreamId)
	AssertEqual(
		t,
		TransferPath{DestinationId: destinationId, StreamId: streamId},
		sequenceContract.path.DestinationMask(),
	)
}

func TestTakeContract(t *testing.T) {
	// in parallel, add contracts, take contracts, and optionally return contract
	// make sure all created contracts get eventually taken

	k := 4
	n := 64
	// contractReturnP := float32(0.5)
	timeout := 30 * time.Second

	ctx := context.Background()
	clientId := NewId()
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.LegacyCreateContract = true
	client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	destinationId := NewId()

	contractManager.SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
		protocol.ProvideMode_Public:  true,
	})

	contracts := make(chan *protocol.Contract)
	contractTimeout := make(chan struct{}, 1)

	go func() {
		for i := 0; i < k*n; i += 1 {
			contractId := NewId()
			contractByteCount := gib(1)

			relationship := protocol.ProvideMode_Public
			provideSecretKey, ok := contractManager.GetProvideSecretKey(relationship)
			AssertEqual(t, true, ok)

			storedContract := &protocol.StoredContract{
				ContractId:        contractId.Bytes(),
				TransferByteCount: uint64(contractByteCount),
				SourceId:          clientId.Bytes(),
				DestinationId:     destinationId.Bytes(),
			}
			storedContractBytes, err := ProtoMarshal(storedContract)
			AssertEqual(t, nil, err)
			defer MessagePoolReturn(storedContractBytes)
			storedContractHmac := SignStoredContract(contractManager.settings, provideSecretKey, storedContractBytes)

			verified := contractManager.Verify(storedContractHmac, storedContractBytes, relationship)
			AssertEqual(t, true, verified)

			result := &protocol.CreateContractResult{
				Contract: &protocol.Contract{
					StoredContractBytes: storedContractBytes,
					StoredContractHmac:  storedContractHmac,
					ProvideMode:         relationship,
				},
			}
			frame, err := ToFrame(result, DefaultProtocolVersion)
			AssertEqual(t, nil, err)

			contractManager.HandleControlFrame(
				ContractKey{
					Destination: DestinationId(destinationId),
				},
				frame,
			)
		}
	}()

	for j := 0; j < k; j += 1 {
		go func() {
			for i := 0; i < n; {

				contractKey := ContractKey{
					Destination: DestinationId(destinationId),
				}
				if contract := contractManager.TakeContract(ctx, contractKey, timeout); contract != nil {
					// if mathrand.Float32() < contractReturnP {
					// 	// put back
					// 	contractManager.ReturnContract(ctx, destinationId, contract)
					// } else {
					select {
					case contracts <- contract:
					case <-time.After(timeout):
						select {
						case contractTimeout <- struct{}{}:
						default:
						}
						return
					}
					i += 1
					// }
				}

			}

		}()
	}

	contractIds := map[Id]bool{}

	for i := 0; i < k*n; i += 1 {
		select {
		case contract := <-contracts:
			var storedContract protocol.StoredContract
			err := ProtoUnmarshal(contract.StoredContractBytes, &storedContract)
			AssertEqual(t, nil, err)

			contractId, err := IdFromBytes(storedContract.ContractId)
			AssertEqual(t, nil, err)

			AssertEqual(t, false, contractIds[contractId])
			contractIds[contractId] = true

		case <-time.After(timeout):
			t.FailNow()
		case <-contractTimeout:
			t.FailNow()
		}
	}

	AssertEqual(t, k*n, len(contractIds))

	// no more
	contractKey := ContractKey{
		Destination: DestinationId(destinationId),
	}
	contract := contractManager.TakeContract(ctx, contractKey, 0)
	AssertEqual(t, nil, contract)

	// all the contracts are accounted for
}

// TestStoredContractHmacCutover exercises the NetworkEventTimeChangeHmac
// cutover by setting an artificial cutoff time in ContractManagerSettings and
// asserting:
//   - SignStoredContract emits the legacy format when the cutoff is in the future
//   - SignStoredContract emits the standard format when the cutoff is in the past
//   - VerifyStoredContract accepts BOTH formats regardless of the cutoff time
//   - Tampered bytes and wrong keys are rejected for both formats
func TestStoredContractHmacCutover(t *testing.T) {
	provideSecretKey := []byte("test-provide-secret-key-which-is-long-enough")
	storedContractBytes := []byte("test stored contract bytes payload")

	pastSettings := DefaultContractManagerSettings()
	pastSettings.NetworkEventTimeChangeHmac = time.Now().Add(-time.Hour)

	futureSettings := DefaultContractManagerSettings()
	futureSettings.NetworkEventTimeChangeHmac = time.Now().Add(time.Hour)

	// canonical encodings of both formats computed independently of the helper
	legacyExpected := func() []byte {
		mac := hmac.New(sha256.New, provideSecretKey)
		return mac.Sum(storedContractBytes)
	}()
	standardExpected := func() []byte {
		mac := hmac.New(sha256.New, provideSecretKey)
		mac.Write(storedContractBytes)
		return mac.Sum(nil)
	}()

	// sanity: the two formats have different lengths and contents
	AssertEqual(t, len(storedContractBytes)+sha256.Size, len(legacyExpected))
	AssertEqual(t, sha256.Size, len(standardExpected))

	// future cutoff → signer emits legacy
	futureHmac := SignStoredContract(futureSettings, provideSecretKey, storedContractBytes)
	AssertEqual(t, legacyExpected, futureHmac)

	// past cutoff → signer emits standard
	pastHmac := SignStoredContract(pastSettings, provideSecretKey, storedContractBytes)
	AssertEqual(t, standardExpected, pastHmac)

	// VerifyStoredContract accepts both formats regardless of the cutoff time
	AssertEqual(t, true, VerifyStoredContract(pastSettings, provideSecretKey, storedContractBytes, legacyExpected))
	AssertEqual(t, true, VerifyStoredContract(pastSettings, provideSecretKey, storedContractBytes, standardExpected))
	AssertEqual(t, true, VerifyStoredContract(futureSettings, provideSecretKey, storedContractBytes, legacyExpected))
	AssertEqual(t, true, VerifyStoredContract(futureSettings, provideSecretKey, storedContractBytes, standardExpected))

	// tampered contract bytes are rejected for both formats and both settings
	tampered := []byte("tampered stored contract bytes payload")
	AssertEqual(t, false, VerifyStoredContract(pastSettings, provideSecretKey, tampered, legacyExpected))
	AssertEqual(t, false, VerifyStoredContract(pastSettings, provideSecretKey, tampered, standardExpected))
	AssertEqual(t, false, VerifyStoredContract(futureSettings, provideSecretKey, tampered, legacyExpected))
	AssertEqual(t, false, VerifyStoredContract(futureSettings, provideSecretKey, tampered, standardExpected))

	// wrong provide key is rejected for both formats and both settings
	wrongKey := []byte("wrong-provide-secret-key-which-is-long-enough")
	AssertEqual(t, false, VerifyStoredContract(pastSettings, wrongKey, storedContractBytes, legacyExpected))
	AssertEqual(t, false, VerifyStoredContract(pastSettings, wrongKey, storedContractBytes, standardExpected))
	AssertEqual(t, false, VerifyStoredContract(futureSettings, wrongKey, storedContractBytes, legacyExpected))
	AssertEqual(t, false, VerifyStoredContract(futureSettings, wrongKey, storedContractBytes, standardExpected))

	// an HMAC of an unsupported length is rejected
	bogus := []byte("not-a-valid-hmac")
	AssertEqual(t, false, VerifyStoredContract(pastSettings, provideSecretKey, storedContractBytes, bogus))
}

// TestContractQueueExpire verifies that queued contracts no sequence takes are
// expired: the janitor closes them and removes the emptied queue from
// `destinationContracts` (orphan retention), and `Poll` never hands out a
// contract older than the expire window.
func TestContractQueueExpire(t *testing.T) {
	ctx := context.Background()
	clientId := NewId()
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.LegacyCreateContract = true
	settings.ContractManagerSettings.ContractQueueExpireTimeout = 500 * time.Millisecond
	client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	destinationId := NewId()

	contractManager.SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
		protocol.ProvideMode_Public:  true,
	})

	makeContract := func() (*protocol.Contract, *protocol.StoredContract) {
		contractId := NewId()
		relationship := protocol.ProvideMode_Public
		provideSecretKey, ok := contractManager.GetProvideSecretKey(relationship)
		AssertEqual(t, true, ok)

		storedContract := &protocol.StoredContract{
			ContractId:        contractId.Bytes(),
			TransferByteCount: uint64(gib(1)),
			SourceId:          clientId.Bytes(),
			DestinationId:     destinationId.Bytes(),
		}
		storedContractBytes, err := ProtoMarshal(storedContract)
		AssertEqual(t, nil, err)
		storedContractHmac := SignStoredContract(contractManager.settings, provideSecretKey, storedContractBytes)
		contract := &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac:  storedContractHmac,
			ProvideMode:         relationship,
		}
		return contract, storedContract
	}

	contractKey := ContractKey{
		Destination: DestinationId(destinationId),
	}

	// queue an orphan via the control frame path (no sequence ever takes it)
	contract, _ := makeContract()
	result := &protocol.CreateContractResult{
		Contract: contract,
	}
	frame, err := ToFrame(result, DefaultProtocolVersion)
	AssertEqual(t, nil, err)
	err = contractManager.HandleControlFrame(contractKey, frame)
	AssertEqual(t, nil, err)

	queueCount := func() int {
		contractManager.mutex.Lock()
		defer contractManager.mutex.Unlock()
		return len(contractManager.destinationContracts)
	}
	AssertEqual(t, 1, queueCount())

	// the janitor expires the orphan and removes the emptied queue
	expired := false
	for range 50 {
		if queueCount() == 0 {
			expired = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	AssertEqual(t, true, expired)

	// the expired contract is no longer takeable
	takenContract := contractManager.TakeContract(ctx, contractKey, 0)
	AssertEqual(t, nil, takenContract)

	// Poll guard: a stale queued contract is never handed out
	queue := newContractQueue(nil, false)
	staleContract, staleStoredContract := makeContract()
	queue.Add(staleContract, staleStoredContract)
	polled, expiredContracts := queue.Poll(time.Now().Add(time.Minute))
	AssertEqual(t, nil, polled)
	AssertEqual(t, 1, len(expiredContracts))

	// a fresh contract polled with expiry disabled (zero minEnqueueTime) is handed out
	freshContract, freshStoredContract := makeContract()
	queue.Add(freshContract, freshStoredContract)
	polled, expiredContracts = queue.Poll(time.Time{})
	AssertEqual(t, freshContract, polled)
	AssertEqual(t, 0, len(expiredContracts))
}

// TestContractQueueShutdownFlush verifies that when the contract manager
// closes (client context canceled), still-queued pending contracts are
// flushed and closed rather than abandoned. The expire timeout is set long so
// only the shutdown path can drain the queue.
func TestContractQueueShutdownFlush(t *testing.T) {
	ctx := context.Background()
	clientId := NewId()
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.LegacyCreateContract = true
	settings.ContractManagerSettings.ContractQueueExpireTimeout = 1 * time.Hour
	client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	destinationId := NewId()

	contractManager.SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
		protocol.ProvideMode_Public:  true,
	})

	contractId := NewId()
	relationship := protocol.ProvideMode_Public
	provideSecretKey, ok := contractManager.GetProvideSecretKey(relationship)
	AssertEqual(t, true, ok)
	storedContract := &protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(gib(1)),
		SourceId:          clientId.Bytes(),
		DestinationId:     destinationId.Bytes(),
	}
	storedContractBytes, err := ProtoMarshal(storedContract)
	AssertEqual(t, nil, err)
	storedContractHmac := SignStoredContract(contractManager.settings, provideSecretKey, storedContractBytes)
	contract := &protocol.Contract{
		StoredContractBytes: storedContractBytes,
		StoredContractHmac:  storedContractHmac,
		ProvideMode:         relationship,
	}

	contractKey := ContractKey{
		Destination: DestinationId(destinationId),
	}
	result := &protocol.CreateContractResult{
		Contract: contract,
	}
	frame, err := ToFrame(result, DefaultProtocolVersion)
	AssertEqual(t, nil, err)
	err = contractManager.HandleControlFrame(contractKey, frame)
	AssertEqual(t, nil, err)

	queueCount := func() int {
		contractManager.mutex.Lock()
		defer contractManager.mutex.Unlock()
		return len(contractManager.destinationContracts)
	}
	AssertEqual(t, 1, queueCount())

	// closing the client triggers the shutdown flush of pending contracts
	client.Cancel()

	flushed := false
	for range 50 {
		if queueCount() == 0 {
			flushed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	AssertEqual(t, true, flushed)
}
