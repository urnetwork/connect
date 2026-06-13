package connect

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
	"time"

	// mathrand "math/rand"

	"github.com/go-playground/assert/v2"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

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
			assert.Equal(t, true, ok)

			storedContract := &protocol.StoredContract{
				ContractId:        contractId.Bytes(),
				TransferByteCount: uint64(contractByteCount),
				SourceId:          clientId.Bytes(),
				DestinationId:     destinationId.Bytes(),
			}
			storedContractBytes, err := ProtoMarshal(storedContract)
			assert.Equal(t, nil, err)
			defer MessagePoolReturn(storedContractBytes)
			storedContractHmac := SignStoredContract(contractManager.settings, provideSecretKey, storedContractBytes)

			verified := contractManager.Verify(storedContractHmac, storedContractBytes, relationship)
			assert.Equal(t, true, verified)

			result := &protocol.CreateContractResult{
				Contract: &protocol.Contract{
					StoredContractBytes: storedContractBytes,
					StoredContractHmac:  storedContractHmac,
					ProvideMode:         relationship,
				},
			}
			frame, err := ToFrame(result, DefaultProtocolVersion)
			assert.Equal(t, nil, err)

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
			assert.Equal(t, nil, err)

			contractId, err := IdFromBytes(storedContract.ContractId)
			assert.Equal(t, nil, err)

			assert.Equal(t, false, contractIds[contractId])
			contractIds[contractId] = true

		case <-time.After(timeout):
			t.FailNow()
		case <-contractTimeout:
			t.FailNow()
		}
	}

	assert.Equal(t, k*n, len(contractIds))

	// no more
	contractKey := ContractKey{
		Destination: DestinationId(destinationId),
	}
	contract := contractManager.TakeContract(ctx, contractKey, 0)
	assert.Equal(t, nil, contract)

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
	assert.Equal(t, len(storedContractBytes)+sha256.Size, len(legacyExpected))
	assert.Equal(t, sha256.Size, len(standardExpected))

	// future cutoff → signer emits legacy
	futureHmac := SignStoredContract(futureSettings, provideSecretKey, storedContractBytes)
	assert.Equal(t, legacyExpected, futureHmac)

	// past cutoff → signer emits standard
	pastHmac := SignStoredContract(pastSettings, provideSecretKey, storedContractBytes)
	assert.Equal(t, standardExpected, pastHmac)

	// VerifyStoredContract accepts both formats regardless of the cutoff time
	assert.Equal(t, true, VerifyStoredContract(pastSettings, provideSecretKey, storedContractBytes, legacyExpected))
	assert.Equal(t, true, VerifyStoredContract(pastSettings, provideSecretKey, storedContractBytes, standardExpected))
	assert.Equal(t, true, VerifyStoredContract(futureSettings, provideSecretKey, storedContractBytes, legacyExpected))
	assert.Equal(t, true, VerifyStoredContract(futureSettings, provideSecretKey, storedContractBytes, standardExpected))

	// tampered contract bytes are rejected for both formats and both settings
	tampered := []byte("tampered stored contract bytes payload")
	assert.Equal(t, false, VerifyStoredContract(pastSettings, provideSecretKey, tampered, legacyExpected))
	assert.Equal(t, false, VerifyStoredContract(pastSettings, provideSecretKey, tampered, standardExpected))
	assert.Equal(t, false, VerifyStoredContract(futureSettings, provideSecretKey, tampered, legacyExpected))
	assert.Equal(t, false, VerifyStoredContract(futureSettings, provideSecretKey, tampered, standardExpected))

	// wrong provide key is rejected for both formats and both settings
	wrongKey := []byte("wrong-provide-secret-key-which-is-long-enough")
	assert.Equal(t, false, VerifyStoredContract(pastSettings, wrongKey, storedContractBytes, legacyExpected))
	assert.Equal(t, false, VerifyStoredContract(pastSettings, wrongKey, storedContractBytes, standardExpected))
	assert.Equal(t, false, VerifyStoredContract(futureSettings, wrongKey, storedContractBytes, legacyExpected))
	assert.Equal(t, false, VerifyStoredContract(futureSettings, wrongKey, storedContractBytes, standardExpected))

	// an HMAC of an unsupported length is rejected
	bogus := []byte("not-a-valid-hmac")
	assert.Equal(t, false, VerifyStoredContract(pastSettings, provideSecretKey, storedContractBytes, bogus))
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
		assert.Equal(t, true, ok)

		storedContract := &protocol.StoredContract{
			ContractId:        contractId.Bytes(),
			TransferByteCount: uint64(gib(1)),
			SourceId:          clientId.Bytes(),
			DestinationId:     destinationId.Bytes(),
		}
		storedContractBytes, err := ProtoMarshal(storedContract)
		assert.Equal(t, nil, err)
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
	assert.Equal(t, nil, err)
	err = contractManager.HandleControlFrame(contractKey, frame)
	assert.Equal(t, nil, err)

	queueCount := func() int {
		contractManager.mutex.Lock()
		defer contractManager.mutex.Unlock()
		return len(contractManager.destinationContracts)
	}
	assert.Equal(t, 1, queueCount())

	// the janitor expires the orphan and removes the emptied queue
	expired := false
	for range 50 {
		if queueCount() == 0 {
			expired = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	assert.Equal(t, true, expired)

	// the expired contract is no longer takeable
	takenContract := contractManager.TakeContract(ctx, contractKey, 0)
	assert.Equal(t, nil, takenContract)

	// Poll guard: a stale queued contract is never handed out
	queue := newContractQueue(nil, false)
	staleContract, staleStoredContract := makeContract()
	queue.Add(staleContract, staleStoredContract)
	polled, expiredContracts := queue.Poll(time.Now().Add(time.Minute))
	assert.Equal(t, nil, polled)
	assert.Equal(t, 1, len(expiredContracts))

	// a fresh contract polled with expiry disabled (zero minEnqueueTime) is handed out
	freshContract, freshStoredContract := makeContract()
	queue.Add(freshContract, freshStoredContract)
	polled, expiredContracts = queue.Poll(time.Time{})
	assert.Equal(t, freshContract, polled)
	assert.Equal(t, 0, len(expiredContracts))
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
	assert.Equal(t, true, ok)
	storedContract := &protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(gib(1)),
		SourceId:          clientId.Bytes(),
		DestinationId:     destinationId.Bytes(),
	}
	storedContractBytes, err := ProtoMarshal(storedContract)
	assert.Equal(t, nil, err)
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
	assert.Equal(t, nil, err)
	err = contractManager.HandleControlFrame(contractKey, frame)
	assert.Equal(t, nil, err)

	queueCount := func() int {
		contractManager.mutex.Lock()
		defer contractManager.mutex.Unlock()
		return len(contractManager.destinationContracts)
	}
	assert.Equal(t, 1, queueCount())

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
	assert.Equal(t, true, flushed)
}
