// Real Multi/Transfer/provider admission and TcpSequence with only in-memory
// routes and a net.Pipe origin; public retries recover bounded reorder refusal.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Stable Network wire value; the constructed sequence verifies its enum name.
const tcpRecoveryNetworkProvideMode = 1

// Infers the existing callback mode type without importing a child package.
// Go has no generic function literals; the adapter keeps observation scalar-only.
func tcpRecoveryReceivePacketCallback[Mode ~int32](
	_ func(TransferPath, Mode, *IpPath, []byte),
	observe func([]byte),
) func(TransferPath, Mode, *IpPath, []byte) {
	return func(_ TransferPath, _ Mode, _ *IpPath, packet []byte) { observe(packet) }
}

func TestMultiTcpReorderBoundRecoversThroughPublicRetry(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 2, 256, 1000, 4, false)
}

func TestMultiTcpReorderByteBoundRecoversThroughPublicRetry(t *testing.T) {
	// Each ordinary one-byte IPv4/TCP packet owns 41 bytes: 80 retains C
	// but refuses D and E despite eight available reorder item slots.
	testMultiTcpReorderPublicRetry(t, 8, 80, 1000, 3, false)
}

func TestMultiTcpReorderCountNoRefusalKeepsPersistentCollapse(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 3, 256, 1000, 5, false)
}

func TestMultiTcpReorderBytesNoRefusalKeepsPersistentCollapse(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 8, 128, 1000, 5, false)
}

func TestMultiTcpReorderRecoveryTrimsOverlapAtRealAck(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 2, 256, 1000, 4, true)
}

func TestMultiTcpReorderRecoveryAcrossSequenceWrap(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 2, 256, ^uint32(0)-2, 4, false)
}

func TestMultiTcpReorderRecoveryThroughNativePublicGroup(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 2, 256, 1000, 4, false, "group")
}

func TestMultiTcpReorderRecoverySurvivesNativeUnfundedRetry(t *testing.T) {
	testMultiTcpReorderPublicRetry(t, 2, 256, 1000, 4, false, "unfunded")
}

// Holds AB before Transfer admission, lets C/D/E traverse Transfer to the
// bounded reorder buffer, then uses ordinary public retries for refused data.
// Every byte and ACK crosses real routes; all owned workers join at cleanup.
func testMultiTcpReorderPublicRetry(t *testing.T, reorderCount int, reorderBytes int, initialSequence uint32, prefixBytes int, overlap bool, modes ...string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		forward, reverse := make(Route, 8), make(Route, 8)
		local, origin := net.Pipe()
		natBudget := NewTransferMemoryBudget(2 * 1024 * 1024)
		var clients []*Client
		var parent *RemoteUserNatMultiClient
		var nat *LocalUserNat
		var provider *RemoteUserNatProvider
		var gapDone, readerDone chan struct{}
		var starveNext atomic.Bool
		var starvedBytes atomic.Int64
		var senderBudget *TransferMemoryBudget
		gapRelease := make(chan struct{})
		var releaseOnce sync.Once
		releaseGap := func() { releaseOnce.Do(func() { close(gapRelease) }) }
		defer func() {
			cancel()
			releaseGap()
			origin.Close()
			local.Close()
			if gapDone != nil {
				<-gapDone
			}
			if readerDone != nil {
				<-readerDone
			}
			if provider != nil {
				provider.Close()
			}
			if nat != nil {
				if err := nat.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			if parent != nil {
				if err := parent.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			for _, client := range clients {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			if held := starvedBytes.Swap(0); held != 0 {
				senderBudget.Release(ByteCount(held))
			}
			if senderBudget != nil && senderBudget.UsedByteCount() != 0 {
				t.Errorf("joined sender retained %d bytes", senderBudget.UsedByteCount())
			}
			for _, route := range []Route{forward, reverse} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
			if natBudget.UsedByteCount() != 0 {
				t.Errorf("joined NAT/provider retained %d bytes", natBudget.UsedByteCount())
			}
		}()

		completions := make(chan SendPackLifecycleObservation, 16)
		newClient := func(observe bool) *Client {
			settings := closeWaitClientSettings()
			settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(64 * 1024)
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			settings.ReceiveBufferSettings.AckCompressTimeout = 0
			if observe {
				senderBudget = settings.SendBufferSettings.ResendQueueBudget
				settings.SendBufferSettings.beforeGroupRetainForTest = func() {
					if starveNext.CompareAndSwap(true, false) {
						held := senderBudget.Available()
						if !senderBudget.TryReserve(held) {
							t.Error("could not hold exact source retention capacity")
						} else {
							starvedBytes.Store(int64(held))
						}
					}
				}
				settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					if event.Phase == SendPackLifecyclePhaseTerminal &&
						event.MessageType.String() == "IpIpPacketToProvider" {
						select {
						case completions <- event:
						default:
							t.Error("bounded completion observer overflow")
						}
					}
				}
			}
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			clients = append(clients, client)
			return client
		}
		sender, receiver := newClient(true), newClient(false)
		sender.ContractManager().AddNoContractPeer(receiver.ClientId())
		receiver.ContractManager().AddNoContractPeer(sender.ClientId())
		sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(receiver.ClientId())), []Route{forward})
		receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{forward})
		receiver.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(sender.ClientId())), []Route{reverse})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{reverse})

		decisions := make(chan tcpReorderDisposition, 8)
		captured := make(chan *TcpSequence, 1)
		natSettings := DefaultLocalUserNatSettingsWithBufferSize(2)
		natSettings.MemoryBudget, natSettings.Log = natBudget, NewNoopLogger()
		natSettings.TcpBufferSettings.SequenceBufferSize = reorderCount
		natSettings.TcpBufferSettings.Mtu = 80
		natSettings.TcpBufferSettings.MinWindowSize = 64
		natSettings.TcpBufferSettings.InitialWindowSize = 64
		natSettings.TcpBufferSettings.MaxWindowSize = uint32(reorderBytes)
		natSettings.TcpBufferSettings.WriteBatchSize = 1
		natSettings.TcpBufferSettings.AckCompressTimeout = 0
		natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
			sequence.afterReorderDispositionForTest = func(decision tcpReorderDisposition) {
				select {
				case decisions <- decision:
				default:
					t.Error("bounded reorder observer overflow")
				}
			}
			captured <- sequence
		}
		natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return local, nil
			},
		}
		nat = NewLocalUserNat(ctx, "public TCP reorder retry", natSettings)
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		provider = NewRemoteUserNatProvider(receiver, nat, providerSettings)

		// The source receives only scalar control metadata; callback bytes stay borrowed.
		type control struct {
			sequence uint32
			ack      uint32
			syn      bool
		}
		controls := make(chan control, 32)
		multiSettings := DefaultMultiClientSettings()
		multiSettings.Log = NewNoopLogger()
		multiSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		multiSettings.LocalDialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("unexpected local fallback dial")
			},
		}
		parent = NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{},
			tcpRecoveryReceivePacketCallback(ReceivePacketFunction(nil), func(packet []byte) {
				// Multi's advisory callback path has been reversed and drops flags.
				path, err := ParseIpPath(packet)
				if err != nil {
					t.Error(err)
					return
				}
				if path.Protocol != IpProtocolTcp || !path.Ack {
					return
				}
				select {
				case controls <- control{sequence: path.SequenceNumber, ack: path.AckSequenceNumber, syn: path.Syn}:
				default:
					t.Error("bounded source control observer overflow")
				}
			}), tcpRecoveryNetworkProvideMode, multiSettings)
		if !parent.settings.TcpCollapsePrevention || parent.ReliabilitySettings().TcpCollapseMaxHold != 0 {
			t.Fatal("fixture must use the default persistent collapse gate")
		}
		nextSequence := initialSequence + 1
		const sourceWindow = uint16(4096)
		syn := groupRecoveryPacket(initialSequence, 0, sourceWindow, tcpFlagSyn, nil)
		path, err := ParseIpPath(syn)
		if err != nil {
			t.Fatal(err)
		}
		update, _, _ := parent.sendUpdate(path, flowPin{})
		selected := newPacketTransferTestChannel()
		selected.ctx, selected.log, selected.settings, selected.client = ctx, NewNoopLogger(), parent.settings, sender
		selected.healthy = true
		selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(receiver.ClientId())}
		selected.clientReceivePacketCallback = parent.clientReceivePacket
		update.client.Store(selected)
		parent.bindClientFlow(update, selected)
		if parent.sendClientPathForTest != nil || !parent.reuseAffinitySyn(update, selected, path) {
			t.Fatal("selected reorder provider lacks live healthy SYN eligibility")
		}
		sender.AddReceiveCallback(selected.clientReceive)

		gapEntered := make(chan struct{})
		var holdNext atomic.Bool
		selected.beforeGroupCompletionForTest = func() {
			if holdNext.CompareAndSwap(true, false) {
				close(gapEntered)
				select {
				case <-gapRelease:
				case <-ctx.Done():
				}
			}
		}
		source := SourceId(NewId())
		send := func(template []byte) bool {
			packet := MessagePoolCopy(template)
			accepted := parent.SendPacket(source, tcpRecoveryNetworkProvideMode, packet, 0)
			if !accepted {
				MessagePoolReturn(packet)
			}
			return accepted
		}
		waitCompletion := func() {
			t.Helper()
			select {
			case event := <-completions:
				if !event.AckRequired || event.Err != nil {
					t.Fatalf("native Transfer did not complete by peer ACK: %+v", event)
				}
			case <-ctx.Done():
				t.Fatal("native Transfer completion did not arrive", ctx.Err())
			}
		}
		waitControl := func(ack uint32, syn bool) control {
			t.Helper()
			for {
				select {
				case received := <-controls:
					if received.ack == ack && received.syn == syn {
						return received
					}
				case <-ctx.Done():
					t.Fatalf("source did not receive TCP ACK %d (SYN=%t): %v", ack, syn, ctx.Err())
					return control{}
				}
			}
		}
		if !send(syn) {
			t.Fatal("SYN admission failed")
		}
		waitCompletion()
		synAck := waitControl(nextSequence, true)
		var sequence *TcpSequence
		select {
		case sequence = <-captured:
		case <-ctx.Done():
			t.Fatal("provider TCP sequence was not constructed")
		}
		if sequence.provideMode.String() != "Network" {
			t.Fatalf("fixture provide mode = %s, want Network", sequence.provideMode)
		}
		peerAck := synAck.sequence + 1
		if !send(groupRecoveryPacket(nextSequence, peerAck, sourceWindow, tcpFlagAck, nil)) {
			t.Fatal("handshake ACK admission failed")
		}
		waitCompletion()

		// This is a legal concurrent public-send ordering, not reordering behind
		// a Transfer receipt. Its FIFO will correctly deliver C, D, E, then AB.
		holdNext.Store(true)
		gapResult := make(chan bool, 1)
		gapDone = make(chan struct{})
		go func() {
			defer close(gapDone)
			gapResult <- send(groupRecoveryPacket(nextSequence, peerAck, sourceWindow, tcpFlagAck, []byte("AB")))
		}()
		waitCloseWaitBarrier(t, ctx, gapEntered, "first data offer before Transfer admission")
		for index, payload := range []string{"C", "D", "E"} {
			packet := groupRecoveryPacket(nextSequence+2+uint32(index), peerAck, sourceWindow, tcpFlagAck, []byte(payload))
			if !send(packet) {
				t.Fatalf("initial %q admission failed", payload)
			}
			waitCompletion()
			want := tcpReorderDispositionRetained
			if index+2 >= prefixBytes {
				want = tcpReorderDispositionRejected
			}
			select {
			case got := <-decisions:
				if got != want {
					t.Fatalf("%q reorder decision=%d, want %d", payload, got, want)
				}
			case <-ctx.Done():
				t.Fatalf("%q reorder decision absent", payload)
			}
			waitControl(nextSequence, false)
		}

		// Read through the socket writer, keeping prefix and final delivery as
		// explicit events. Closing net.Pipe joins this worker on every fatal path.
		type readResult struct {
			data string
			err  error
		}
		prefix, suffix := make(chan readResult, 1), make(chan readResult, 1)
		readerDone = make(chan struct{})
		go func() {
			defer close(readerDone)
			data := make([]byte, prefixBytes)
			n, err := io.ReadFull(origin, data)
			prefix <- readResult{data: string(data[:n]), err: err}
			if err != nil {
				return
			}
			n, err = io.ReadFull(origin, data[:5-prefixBytes])
			suffix <- readResult{data: string(data[:n]), err: err}
		}()
		releaseGap()
		waitCloseWaitBarrier(t, ctx, gapDone, "first data public send return")
		if !<-gapResult {
			t.Fatal("released first data offer was not admitted")
		}
		waitCompletion()
		select {
		case got := <-prefix:
			if got.err != nil || got.data != "ABCDE"[:prefixBytes] {
				t.Fatalf("upstream prefix=%q err=%v, want %q", got.data, got.err, "ABCDE"[:prefixBytes])
			}
		case <-ctx.Done():
			t.Fatal("gap fill did not drain the retained prefix")
		}
		waitControl(nextSequence+uint32(prefixBytes), false)
		synctest.Wait()

		// Upload cumulative ACK progress is ingress state. The retry's egress
		// ACK still acknowledges the unchanged download side of this TCP flow.
		update.stateLock.Lock()
		ack, window := update.sequenceAdmissionAck, update.sequenceAdmissionWindow
		ingressAck, ingressSeen := update.ingressAckSequence, update.ingressAckSeen
		generation, generationSeen := update.synGenerationNumber, update.synGenerationSeen
		covered, from, to := update.sequenceCovered, update.sequenceCoveredFrom, update.sequenceCoveredTo
		update.stateLock.Unlock()
		if ack != peerAck || window != sourceWindow || !ingressSeen || ingressAck != nextSequence+uint32(prefixBytes) ||
			!generationSeen || generation != initialSequence || !update.receivedInbound.Load() || sequence.ctx.Err() != nil {
			t.Fatalf("retry preconditions changed: egress=%d/%d ingress=%d/%t generation=%d/%t",
				ack, window, ingressAck, ingressSeen, generation, generationSeen)
		}
		for index := prefixBytes; index < 5; index++ {
			start := index
			if overlap {
				start--
			}
			beforeDrops := parent.TcpCollapseDropCount()
			retry := groupRecoveryPacket(nextSequence+uint32(start), peerAck, sourceWindow, tcpFlagAck, []byte("ABCDE"[start:index+1]))
			if len(modes) != 0 && modes[0] == "unfunded" {
				starveNext.Store(true)
				if !send(retry) {
					t.Fatal("unfunded retry did not first enter the native raw queue")
				}
				select {
				case event := <-completions:
					if !event.AckRequired || !sendGroupCapacityFailure(event.Err) {
						t.Fatalf("retry did not reach exact native retained-memory refusal: %+v", event)
					}
				case <-ctx.Done():
					t.Fatal("native unfunded retry did not complete")
				}
				if held := starvedBytes.Swap(0); held == 0 {
					t.Fatal("retention barrier did not reserve its bounded capacity")
				} else {
					senderBudget.Release(ByteCount(held))
				}
				// Capacity is published by the source Run loop after terminal
				// disposition; join that wake before a zero-timeout public offer.
				synctest.Wait()
			}
			accepted := false
			if len(modes) != 0 && modes[0] == "group" {
				prior := groupRecoveryPacket(nextSequence+uint32(index)-1, peerAck, sourceWindow, tcpFlagAck, []byte("ABCDE"[index-1:index]))
				accepted = parent.SendPacketBatch(source, tcpRecoveryNetworkProvideMode,
					[][]byte{MessagePoolCopy(prior), MessagePoolCopy(retry)}, 0) == 2
			} else {
				accepted = send(retry)
			}
			if !accepted {
				t.Fatalf("public TCP retry lost after real Transfer ACK and reorder refusal: collapse drops %d -> %d, coverage=%t [%d,%d), upload ACK=%d",
					beforeDrops, parent.TcpCollapseDropCount(), covered, from, to, ingressAck)
			}
			waitCompletion()
			waitControl(nextSequence+uint32(index)+1, false)
		}
		select {
		case got := <-suffix:
			if got.err != nil || got.data != "ABCDE"[prefixBytes:] {
				t.Fatalf("upstream suffix=%q err=%v, want %q", got.data, got.err, "ABCDE"[prefixBytes:])
			}
		case <-ctx.Done():
			t.Fatal("public TCP retry did not deliver the missing final byte")
		}
		waitCloseWaitBarrier(t, ctx, readerDone, "complete ABCDE delivery")
		synctest.Wait()
		// Delivered bytes remain persistently collapsed in both no-refusal
		// controls and the repaired path, with no elapsed hold or new SYN.
		beforeDrops := parent.TcpCollapseDropCount()
		if send(groupRecoveryPacket(nextSequence+4, peerAck, sourceWindow, tcpFlagAck, []byte("E"))) ||
			parent.TcpCollapseDropCount() != beforeDrops+1 {
			t.Fatal("already cumulatively acknowledged data escaped persistent collapse")
		}
	})
}
