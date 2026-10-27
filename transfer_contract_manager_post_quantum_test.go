package connect

import (
	"context"
	"crypto/tls"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect/protocol"
)

// A provider advertises post-quantum encryption sessions in each provide, so
// provider discovery can offer it to a client that requires post-quantum
// encryption and leave out the providers that cannot seal a session (support
// inbox 7918, https://github.com/urnetwork/connect/issues/220). These read
// the provide the way the platform does, from the frame's wire bytes.

// The provide field number of post_quantum_encryption.
const testingProvidePostQuantumEncryptionField = protowire.Number(2)

// testingProvidePostQuantumEncryption decodes a provide frame's message bytes
// and returns the post_quantum_encryption value, and whether the field is on
// the wire at all.
func testingProvidePostQuantumEncryption(t *testing.T, frame *protocol.Frame) (advertised bool, present bool) {
	t.Helper()
	if frame.MessageType != protocol.MessageType_TransferProvide {
		t.Fatalf("frame type = %s, want a provide", frame.MessageType)
	}
	b := frame.MessageBytes
	for 0 < len(b) {
		number, wireType, n := protowire.ConsumeTag(b)
		if n < 0 {
			t.Fatal(protowire.ParseError(n))
		}
		b = b[n:]
		if number == testingProvidePostQuantumEncryptionField && wireType == protowire.VarintType {
			value, m := protowire.ConsumeVarint(b)
			if m < 0 {
				t.Fatal(protowire.ParseError(m))
			}
			b = b[m:]
			advertised = value != 0
			present = true
			continue
		}
		m := protowire.ConsumeFieldValue(number, wireType, b)
		if m < 0 {
			t.Fatal(protowire.ParseError(m))
		}
		b = b[m:]
	}
	return
}

// testingProvideClient starts a client with the encryption mode, providing to
// the public with return traffic.
func testingProvideClient(t *testing.T, ctx context.Context, mode EncryptionMode, serverTlsConfig *tls.Config) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = mode
	settings.EncryptionSettings.ServerTlsConfig = serverTlsConfig
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(client.Cancel)
	client.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public: true,
	})
	return client
}

// A provider whose encryption sessions serve every initiator, as every sdk
// provider runs (Opportunistic), advertises post-quantum encryption in its
// provide, and keeps advertising it while paused. Before this change no
// provide carried the capability, so discovery could not tell such a provider
// from one that drops a post-quantum handshake.
func TestProvideAdvertisesPostQuantumEncryptionSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, mode := range []EncryptionMode{EncryptionModeOpportunistic, EncryptionModeRequired} {
		client := testingProvideClient(t, ctx, mode, nil)
		contractManager := client.ContractManager()

		frame, err := contractManager.provideFrame()
		AssertEqual(t, err, nil)
		if advertised, _ := testingProvidePostQuantumEncryption(t, frame); !advertised {
			t.Fatalf("mode %d: the provide does not advertise post-quantum encryption sessions", mode)
		}

		contractManager.SetProvidePaused(true)
		frame, err = contractManager.provideFrame()
		AssertEqual(t, err, nil)
		if advertised, _ := testingProvidePostQuantumEncryption(t, frame); !advertised {
			t.Fatalf("mode %d: the paused provide does not advertise post-quantum encryption sessions", mode)
		}
	}
}

// A client without encryption sessions cannot seal a post-quantum session,
// and its provide stays exactly as before the field: it does not carry the
// field at all, so an older platform reads the same bytes.
func TestProvideWithoutEncryptionSessionsDoesNotAdvertise(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := testingProvideClient(t, ctx, EncryptionModeOff, nil)
	frame, err := client.ContractManager().provideFrame()
	AssertEqual(t, err, nil)
	advertised, present := testingProvidePostQuantumEncryption(t, frame)
	AssertEqual(t, advertised, false)
	AssertEqual(t, present, false)
}
