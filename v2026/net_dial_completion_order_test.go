package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

// The TUN's shared race selects the first successful dial completion, even
// when its DNS answer was published second. Both attempts enter before either
// can return; the loser returns a late success only after cancellation. These
// controls use channels and in-memory pipes, with no sockets, DNS, or TUNs.
func TestDohDialProgressCompletionOrderOverridesAnswerOrder(t *testing.T) {
	addresses := map[int]netip.Addr{
		4: netip.MustParseAddr("192.0.2.86"),
		6: netip.MustParseAddr("2001:db8::86"),
	}
	for _, firstAnswerVersion := range []int{4, 6} {
		for _, firstCompletionVersion := range []int{4, 6} {
			t.Run(fmt.Sprintf("answer_v%d/completion_v%d", firstAnswerVersion, firstCompletionVersion), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				answers := make(chan dohDialQueryResult, 2)
				answers <- dohDialQueryResult{addrs: []netip.Addr{addresses[firstAnswerVersion]}, authoritative: true}
				for _, version := range []int{4, 6} {
					if version != firstAnswerVersion {
						answers <- dohDialQueryResult{addrs: []netip.Addr{addresses[version]}, authoritative: true}
					}
				}
				clients := make(map[netip.Addr]net.Conn)
				peers := make(map[netip.Addr]net.Conn)
				complete := make(map[netip.Addr]chan struct{})
				for _, addr := range addresses {
					client, peer := net.Pipe()
					clients[addr], peers[addr] = client, peer
					complete[addr] = make(chan struct{})
					t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
				}
				started := make(chan netip.Addr, 2)
				result := make(chan dialRaceResult, 1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					conn, err := dialAddrsRaceWithResolution(ctx, nil, 0, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
						started <- addr
						select {
						case <-complete[addr]:
						case <-ctx.Done():
						}
						return clients[addr], nil
					}, &dialAddrResolution{results: answers, pending: 2, host: "completion-order.example"})
					result <- dialRaceResult{conn: conn, err: err}
				}()
				t.Cleanup(func() { cancel(); <-done })
				seen := make(map[netip.Addr]bool)
				for range 2 {
					select {
					case addr := <-started:
						if seen[addr] || clients[addr] == nil {
							t.Fatalf("unexpected dial attempt: %s", addr)
						}
						seen[addr] = true
					case <-ctx.Done():
						t.Fatal("both families did not enter the completion barrier")
					}
				}
				winner := addresses[firstCompletionVersion]
				close(complete[winner])
				select {
				case got := <-result:
					if got.err != nil || got.conn != clients[winner] {
						t.Fatalf("winner=%v err=%v, want first completed family IPv%d", got.conn, got.err, firstCompletionVersion)
					}
				case <-ctx.Done():
					t.Fatal("race did not return its first completed connection")
				}
				deadline, _ := ctx.Deadline()
				for addr, peer := range peers {
					if addr == winner {
						continue
					}
					_ = peer.SetReadDeadline(deadline)
					if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
						t.Fatalf("late successful loser was not closed before return: %v", err)
					}
				}
			})
		}
	}
}
