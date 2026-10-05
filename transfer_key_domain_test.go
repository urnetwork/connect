// The real native and processed Http paths carry one owned enrollment domain.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// A rejected response retries exact bytes; rotation changes only the key and
// cannot borrow a later caller mutation of the original domain setting.
func TestClientKeyDomainActualHttpRetryAndRotationRetainOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	domain := [32]byte{42}
	client, _, attempts, _ := newClientKeyRegistrationDomainHttpFixture(t, 4, domain)
	first := nextClientKeyRegistrationAttempt(t, attempts)
	if !bytes.Equal(first.domain, domain[:]) {
		t.Fatal("processed key enrollment omitted the original close domain")
	}
	first.response <- `{"pack":"","error":{"message":"synthetic publication retry"}}`
	retry := nextClientKeyRegistrationAttempt(t, attempts)
	if !bytes.Equal(first.key, retry.key) || !bytes.Equal(first.domain, retry.domain) || client.ClientKeyManager().Registered() {
		t.Fatal("key retry changed original bytes or claimed failed enrollment")
	}
	client.settings.ContractManagerSettings.CloseReportDomainHash[0]++
	if err := client.ClientKeyManager().SetSeed(bytes.Repeat([]byte{43}, ed25519.SeedSize)); err != nil {
		t.Fatal(err)
	}
	retry.response <- `{"pack":""}`
	rotation := nextClientKeyRegistrationAttempt(t, attempts)
	if !bytes.Equal(rotation.domain, domain[:]) || bytes.Equal(rotation.key, first.key) || client.ClientKeyManager().Registered() {
		t.Fatal("key rotation changed owned domain or accepted prior key completion")
	}
	rotation.response <- `{"pack":""}`
	if err := client.ClientKeyManager().WaitForRegistration(ctx); err != nil {
		t.Fatal(err)
	}
}

// The ordinary native frame publisher has the same domain contract, without
// opting existing providers into a new registration startup gate.
func TestClientKeyDomainActualNativePublicationAndRotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	settings := closeWaitClientSettings()
	domain := [32]byte{44}
	settings.ContractManagerSettings.CloseReportDomainHash = domain
	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	receiver := NewClient(ctx, ControlId, NewNoContractClientOob(), closeWaitClientSettings())
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, client := range []*Client{sender, receiver} {
			if err := client.CloseAndWait(join); err != nil {
				t.Error(err)
			}
		}
	})
	sender.ContractManager().AddNoContractPeer(ControlId)
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	observed := make(chan *protocol.ClientKey, 16)
	receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			value, err := FromFrame(frame)
			if message, ok := value.(*protocol.ClientKey); err == nil && ok {
				select {
				case observed <- proto.Clone(message).(*protocol.ClientKey):
				default:
				}
			}
		}
	})
	forward, reverse := make(chan []byte), make(chan []byte)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{forward})
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{forward})
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{reverse})
	sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{reverse})
	next := func(want []byte) *protocol.ClientKey {
		t.Helper()
		for {
			select {
			case message := <-observed:
				if bytes.Equal(message.PublicKey, want) {
					return message
				}
			case <-ctx.Done():
				t.Fatal("actual native key publisher did not reach its matching receiver", ctx.Err())
				return nil
			}
		}
	}
	first := next(sender.ClientKeyManager().PublicKey())
	if !bytes.Equal(first.HistoryDomainHash, domain[:]) || sender.ClientKeyManager().Registered() {
		t.Fatal("native publication omitted domain or invented processed enrollment")
	}
	settings.ContractManagerSettings.CloseReportDomainHash[0]++
	if err := sender.ClientKeyManager().SetSeed(bytes.Repeat([]byte{45}, ed25519.SeedSize)); err != nil {
		t.Fatal(err)
	}
	rotation := next(sender.ClientKeyManager().PublicKey())
	if !bytes.Equal(rotation.HistoryDomainHash, domain[:]) || bytes.Equal(first.PublicKey, rotation.PublicKey) {
		t.Fatal("native rotation failed to retain original domain with the new key")
	}
}

// Legacy zero and independent clients remain separate through the shared encoder.
func TestClientKeyDomainMessageOwnsOptionalBytes(t *testing.T) {
	key := bytes.Repeat([]byte{46}, ed25519.PublicKeySize)
	legacy := &ClientKeyManager{}
	first := &ClientKeyManager{historyDomainHash: [32]byte{47}}
	second := &ClientKeyManager{historyDomainHash: [32]byte{48}}
	a, b := first.clientKeyMessage(key), second.clientKeyMessage(key)
	key[0]++
	a.HistoryDomainHash[0]++
	if len(legacy.clientKeyMessage(key).HistoryDomainHash) != 0 || a.PublicKey[0] != 46 || b.PublicKey[0] != 46 || b.HistoryDomainHash[0] != 48 || first.clientKeyMessage(key).HistoryDomainHash[0] != 47 {
		t.Fatal("optional enrollment message borrowed caller or other provider domain bytes")
	}
}
