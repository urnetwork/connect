package connect

// A client sends PeerAudit frames only when its settings ask for them
// (`ClientSettings.SendPeerAudits`). No platform has ever read one, and the
// server answers them empty without authenticating them (server CPUOPT3 D3),
// so by default none of the three senders sends: a receive sequence's audit
// (its period rollover, a verified stream change and its exit), ReportAbuse,
// and the receive loop's audit of a malformed inbound frame.

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// One control request that carried audits.
type peerAuditRecordedPack struct {
	frameCount int
	audits     []*protocol.PeerAudit
}

// Records the control requests that carry PeerAudit frames and answers every
// request empty, as the platform does. A request with no audit, such as a
// client key registration, is answered and not recorded. Takes the frames it
// is given, as every out-of-band control does. Safe for concurrent use.
type peerAuditRecordingOob struct {
	stateLock sync.Mutex
	packs     []peerAuditRecordedPack
}

func (self *peerAuditRecordingOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	pack := peerAuditRecordedPack{frameCount: len(frames)}
	for _, frame := range frames {
		if frame.MessageType == protocol.MessageType_TransferPeerAudit {
			if message, err := FromFrame(frame); err == nil {
				if audit, ok := message.(*protocol.PeerAudit); ok {
					pack.audits = append(pack.audits, audit)
				}
			}
		}
		MessagePoolReturn(frame.MessageBytes)
	}
	if 0 < len(pack.audits) {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.packs = append(self.packs, pack)
		}()
	}
	if callback != nil {
		callback(nil, nil)
	}
}

func (self *peerAuditRecordingOob) recordedPacks() []peerAuditRecordedPack {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]peerAuditRecordedPack{}, self.packs...)
}

// A running client whose out-of-band control records audits, closed when the
// test ends. configure, when set, adjusts the default settings.
func newPeerAuditTestClient(t *testing.T, configure func(*ClientSettings)) (*Client, *peerAuditRecordingOob) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	// keep the client key registration out of the recorded control
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	if configure != nil {
		configure(settings)
	}
	oob := &peerAuditRecordingOob{}
	client := NewClient(ctx, NewId(), oob, settings)
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("close the audit client: %v", err)
		}
	})
	return client, oob
}

// Drives one sequence audit through the three ways a receive sequence
// completes it: the period rollover, a verified stream change and the
// sequence's exit. Must run in a synctest bubble.
func runSequencePeerAuditTriggers(client *Client, source TransferPath, nextStreamId Id) {
	maxAuditDuration := client.settings.ReceiveBufferSettings.MaxPeerAuditDuration
	audit := NewSequencePeerAudit(client, source, maxAuditDuration)
	audit.Update(func(a *PeerAudit) { a.received(100) })
	time.Sleep(maxAuditDuration)
	// the rollover completes the first period
	audit.Update(func(a *PeerAudit) { a.received(50) })
	time.Sleep(10 * time.Second)
	audit.Update(func(a *PeerAudit) { a.discard(7) })
	// the stream change completes the second
	audit.SetStreamId(nextStreamId)
	audit.Update(func(a *PeerAudit) { a.badMessage(9) })
	// the exit completes the third
	audit.Complete()
}

// By default a sequence's rollover, stream change and exit send nothing.
func TestSequencePeerAuditSendsNothingByDefault(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, oob := newPeerAuditTestClient(t, nil)
		runSequencePeerAuditTriggers(client, SourceId(NewId()), NewId())
		synctest.Wait()
		if packs := oob.recordedPacks(); len(packs) != 0 {
			t.Fatalf("%d audit packs sent by default, want none", len(packs))
		}
	})
}

// With audits on, the same triggers send exactly three single-frame packs,
// carrying the fields they always carried.
func TestSequencePeerAuditSendsWhenEnabled(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, oob := newPeerAuditTestClient(t, func(settings *ClientSettings) {
			settings.SendPeerAudits = true
		})
		peerId, streamId := NewId(), NewId()
		runSequencePeerAuditTriggers(client, SourceId(peerId), streamId)
		synctest.Wait()
		want := []*protocol.PeerAudit{
			{PeerId: peerId.Bytes(), StreamId: Id{}.Bytes(), Duration: 0, SendCount: 1, SendByteCount: 100},
			{PeerId: peerId.Bytes(), StreamId: Id{}.Bytes(), Duration: 10, SendCount: 1, SendByteCount: 50, DiscardedCount: 1, DiscardedByteCount: 7},
			{PeerId: peerId.Bytes(), StreamId: streamId.Bytes(), Duration: 0, BadMessageCount: 1, BadMessageByteCount: 9},
		}
		packs := oob.recordedPacks()
		if len(packs) != len(want) {
			t.Fatalf("%d audit packs, want %d", len(packs), len(want))
		}
		for i, pack := range packs {
			if pack.frameCount != 1 || len(pack.audits) != 1 {
				t.Fatalf("pack %d has %d frames, %d audits; want one audit alone", i, pack.frameCount, len(pack.audits))
			}
			if !proto.Equal(pack.audits[0], want[i]) {
				t.Fatalf("audit %d = %v, want %v", i, pack.audits[0], want[i])
			}
		}
	})
}

// By default an abuse report sends nothing. With audits on, the same report
// is one abuse audit of the source alone.
func TestReportAbuseSendsNothingByDefault(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		client, oob := newPeerAuditTestClient(t, nil)
		client.ReportAbuse(SourceId(NewId()))
		synctest.Wait()
		if packs := oob.recordedPacks(); len(packs) != 0 {
			t.Fatalf("%d abuse reports sent by default, want none", len(packs))
		}

		enabled, enabledOob := newPeerAuditTestClient(t, func(settings *ClientSettings) {
			settings.SendPeerAudits = true
		})
		peerId := NewId()
		enabled.ReportAbuse(SourceId(peerId))
		synctest.Wait()
		packs := enabledOob.recordedPacks()
		if len(packs) != 1 || packs[0].frameCount != 1 || len(packs[0].audits) != 1 {
			t.Fatalf("enabled abuse reports = %+v, want one audit alone", packs)
		}
		audit := packs[0].audits[0]
		if reportedId, err := IdFromBytes(audit.PeerId); err != nil || !audit.Abuse || reportedId != peerId {
			t.Fatalf("enabled abuse report = %v, want an abuse audit of the source", audit)
		}
	})
}

// The receive loop audits each malformed inbound transfer frame: one with
// neither an ack nor a pack, and one whose frame is neither. By default it
// sends nothing for them; with audits on it sends one each.
func TestClientRunMalformedFrameSendsNoAuditByDefault(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, c := range []struct {
		sendPeerAudits bool
		wantPacks      int
	}{
		{sendPeerAudits: false, wantPacks: 0},
		{sendPeerAudits: true, wantPacks: 2},
	} {
		synctest.Test(t, func(t *testing.T) {
			client, oob := newPeerAuditTestClient(t, func(settings *ClientSettings) {
				settings.SendPeerAudits = c.sendPeerAudits
			})
			inbound := make(Route, 4)
			client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{inbound})
			t.Cleanup(func() {
				for len(inbound) != 0 {
					MessagePoolReturn(<-inbound)
				}
			})
			path := TransferPath{SourceId: NewId(), DestinationId: client.ClientId()}.ToProtobuf()
			for _, transferFrame := range []*protocol.TransferFrame{
				{TransferPath: path},
				{TransferPath: path, Frame: &protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage}},
			} {
				transferFrameBytes, err := ProtoMarshal(transferFrame)
				if err != nil {
					t.Fatal(err)
				}
				inbound <- transferFrameBytes
			}
			synctest.Wait()
			if packs := oob.recordedPacks(); len(packs) != c.wantPacks {
				t.Fatalf("SendPeerAudits=%t: %d audit packs for two malformed frames, want %d", c.sendPeerAudits, len(packs), c.wantPacks)
			}
		})
	}
}

// One audit update on the receive path, with audits off (the default) and
// on.
func BenchmarkSequencePeerAuditUpdate(b *testing.B) {
	for _, c := range []struct {
		name           string
		sendPeerAudits bool
	}{
		{name: "off", sendPeerAudits: false},
		{name: "on", sendPeerAudits: true},
	} {
		b.Run(c.name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settings := DefaultClientSettings()
			settings.Log = NewNoopLogger()
			settings.SendPeerAudits = c.sendPeerAudits
			client := NewClient(ctx, NewId(), &peerAuditRecordingOob{}, settings)
			defer client.Cancel()
			audit := NewSequencePeerAudit(client, SourceId(NewId()), settings.ReceiveBufferSettings.MaxPeerAuditDuration)
			messageByteCount := ByteCount(1024)
			b.ReportAllocs()
			for b.Loop() {
				audit.Update(func(a *PeerAudit) { a.received(messageByteCount) })
			}
		})
	}
}
