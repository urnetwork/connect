//go:build darwin || ios || linux || android

package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// This opt-in RED selector records the required provider return contract while
// its prepared-read/ACK owner is being designed. Ordinary client UDP outbound
// policy is intentionally different. A pass of either subtest alone cannot
// qualify a candidate without paused-read, fairness, lifetime and memory tests.
func TestProviderUdpReturnContractResearchGate(t *testing.T) {
	if os.Getenv("URNETWORK_PROVIDER_UDP_RETURN_REQUIRE_GREEN") != "1" {
		t.Skip("opt-in RED provider-return contract: URNETWORK_PROVIDER_UDP_RETURN_REQUIRE_GREEN=1")
	}
	// The pool's process-lifetime diagnostics worker must belong to the real
	// clock, not to a nested virtual-clock bubble that joins all its workers.
	assertMessagePoolOwnership(t)
	t.Run("provider-ack-independent-of-route", func(t *testing.T) {
		for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp, IpProtocolIcmp} {
			for _, forceStream := range []bool{false, true} {
				for _, defaultAck := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream-%t/default-ack-%t", ipProtocol, forceStream, defaultAck), func(t *testing.T) {
						options := providerReturnIpTransferOptions(
							TransferOptions{Ack: defaultAck}, protocol.ProvideMode_Public,
							TransferKey{ForceStream: forceStream}, ipProtocol,
						)
						if !options.Ack {
							t.Error("accepted provider IP return is not ACK-backed")
						}
						if options.ForceStream != forceStream {
							t.Error("return reliability changed the routing key")
						}
					})
				}
			}
		}
		if ipPacketTransferAckRequired(&IpPath{Protocol: IpProtocolUdp}, true, false, true) {
			t.Error("provider return policy changed normal client UDP outbound")
		}
	})
	t.Run("accepted-return-retains-recovery-through-ack", func(t *testing.T) {
		provider := &RemoteUserNatProvider{}
		item := &providerReturnItem{ipProtocol: IpProtocolUdp, recoveryMode: receiveRecoveryModeNonblocking}
		recovery := provider.returnSendRecoveryOption(item)
		if !recovery.upstreamRecoverable || !recovery.retainAfterAckTimeout {
			t.Error("consumed UDP return has neither an asynchronous recovery owner nor retained ACK delivery")
		}
	})
	t.Run("full-dispatch-shard-does-not-park-shared-reader", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			ctx, cancel := context.WithCancel(context.Background())
			settings := DefaultUdpBufferSettingsWithBufferSize(1)
			settings.ReceiveShardCount = 2
			settings.WriteBatchSize = 1
			dispatcher := newUdpReceiveDispatcher(ctx, settings)
			callbackGate := make(chan struct{})
			gateReleased := false
			blocked := newDispatchTestSequence(ctx, dispatcher, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
				<-callbackGate
			})
			other := newDispatchTestSequence(ctx, dispatcher, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {})
			defer func() {
				if !gateReleased {
					close(callbackGate)
				}
				cancel()
				dispatcher.waitForLifecycle()
				blocked.Close()
				other.Close()
			}()
			enqueue := func(sequence *UdpSequence) bool {
				packet := MessagePoolGet(32)
				admitted := dispatcher.enqueue(sequence, packet)
				if !admitted {
					MessagePoolReturn(packet)
				}
				return admitted
			}
			if !enqueue(blocked) {
				t.Fatal("initial callback was not admitted")
			}
			synctest.Wait()
			if !enqueue(blocked) || len(dispatcher.shards[blocked.receiveShard].items) != 1 {
				t.Fatal("failed to establish full dispatch shard")
			}
			// The readiness worker serially handles ready descriptors. Its next
			// descriptor must remain serviceable even if this flow's shard is
			// full; a future implementation must pause before consuming instead
			// of refusing an already-read packet at this lower boundary.
			readerAdvanced := make(chan struct{})
			go func() {
				enqueue(blocked)
				enqueue(other)
				close(readerAdvanced)
			}()
			synctest.Wait()
			select {
			case <-readerAdvanced:
			default:
				t.Error("full dispatch shard parked shared reader before a different ready shard")
			}
			close(callbackGate)
			gateReleased = true
			// Join the producer before dispatcher shutdown can run its final
			// ownership drain.
			<-readerAdvanced
		})
	})
	t.Run("no-socket-consumption-without-parent-credit", func(t *testing.T) {
		assertMessagePoolOwnership(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		parent := NewTransferMemoryBudget(128 * 1024)
		settings := DefaultUdpBufferSettingsWithBufferSize(1)
		settings.MemoryBudget = parent
		settings.WriteBatchSize = 1
		settings.ReceiveShardCount = 1
		settings.Log = NewNoopLogger()
		sequence := NewUdpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
			t.Error("return was published without parent credit")
		}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 1).To4(), 42000,
			net.IPv4(127, 0, 0, 1).To4(), 44000, settings)
		if sequence == nil {
			t.Fatal("flow did not fit the original parent allowance")
		}
		sequence.sharedSocketLifecycle = true
		dispatcher := newUdpReceiveDispatcher(ctx, settings)
		sequence.receiveDispatcher = dispatcher
		defer func() {
			cancel()
			dispatcher.waitForLifecycle()
			sequence.Close()
			if parent.UsedByteCount() != 0 {
				t.Errorf("flow ownership leaked %d bytes", parent.UsedByteCount())
			}
		}()
		occupied := parent.Available()
		if !parent.TryReserve(occupied) {
			t.Fatal("failed to establish the exact exhausted-parent barrier")
		}
		defer parent.Release(occupied)
		socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		defer socket.Close()
		sender, err := net.DialUDP("udp4", nil, socket.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer sender.Close()
		payload := []byte("retain-unread-until-admitted")
		if _, err := sender.Write(payload); err != nil {
			t.Fatal(err)
		}
		raw, err := socket.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		// RawConn.Read provides the kernel readability barrier. The peek itself
		// consumes nothing and the tested poller drain remains nonblocking.
		buffer := make([]byte, settings.ReadBufferByteCount)
		if err := raw.Read(func(fd uintptr) bool {
			n, _, readErr := syscall.Recvfrom(int(fd), buffer, syscall.MSG_PEEK)
			if errors.Is(readErr, syscall.EAGAIN) || errors.Is(readErr, syscall.EWOULDBLOCK) {
				return false
			}
			if readErr != nil || n != len(payload) {
				t.Errorf("initial readable datagram=%d err=%v", n, readErr)
			}
			_ = drainReadyUdpSocket(int(fd), sequence, buffer)
			n, _, readErr = syscall.Recvfrom(int(fd), buffer, syscall.MSG_PEEK)
			if readErr != nil || n != len(payload) {
				t.Errorf("socket consumed unadmitted return: unread=%d err=%v parent=%d/%d", n, readErr, parent.UsedByteCount(), parent.TotalByteCount())
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
	})
}
