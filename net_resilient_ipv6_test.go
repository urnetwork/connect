//go:build unix || windows

package connect

// net_resilient_ipv6_test.go — the resilient TLS layer over IPv6 sockets.
//
// The TTL reorder technique reads and sets the socket's outgoing TTL through
// GetSocketTtl/SetSocketTtl, which use IPPROTO_IP/IP_TTL. An AF_INET6 socket
// refuses that option (EINVAL on darwin and linux; IPV6_UNICAST_HOPS is the
// v6 equivalent and is not implemented), so over v6 GetSocketTtl reads 0 and
// the fragment+reorder and reorder-only paths take their documented
// nativeTtl <= 0 fallback: the record is written whole, at the native hop
// limit, and the connection stays usable. The tests here pin that fallback;
// the TTL-semantics tests in net_resilient_fail_closed_test.go skip their v6
// case through skipIpv6SocketTtl with the same reason.

import (
	"bytes"
	"io"
	"testing"
)

// skipIpv6SocketTtl skips the v6 case of a test whose assertions need the
// socket TTL to be readable and settable, which an AF_INET6 socket refuses.
func skipIpv6SocketTtl(t *testing.T, ipVersion int) {
	t.Helper()
	if ipVersion == 6 {
		t.Skip("GetSocketTtl/SetSocketTtl use IPPROTO_IP/IP_TTL, which an AF_INET6 socket refuses (EINVAL); the resilient reorder technique needs IPPROTO_IPV6/IPV6_UNICAST_HOPS on v6 and currently falls back to a single whole-record write (net_resilient.go, the nativeTtl <= 0 branch)")
	}
}

// Over v6 the reorder modes cannot touch the hop limit, so they must degrade
// to a single whole-record write that the peer receives intact, with the
// layer still enabled and the connection open. Both v4 and v6 pairs run so
// the v4 case documents that the same record arrives fragmented there.
func TestResilientTlsConnIpv6FallsBackToWholeRecordWrite(t *testing.T) {
	for _, mode := range []struct {
		name     string
		fragment bool
		reorder  bool
	}{
		{name: "fragment+reorder", fragment: true, reorder: true},
		{name: "reorder-only", fragment: false, reorder: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			forEachIpVersion(t, func(t *testing.T, ipVersion int) {
				record := buildClientHelloRecord(t)
				client, server := newTcpPairOnFamily(t, ipVersion)
				if ipVersion == 6 {
					if ttl := socketTtl(t, client); ttl != 0 {
						t.Fatalf("v6 socket read TTL %d through IPPROTO_IP/IP_TTL; the fallback premise no longer holds", ttl)
					}
				}

				rconn := NewResilientTlsConn(client, mode.fragment, mode.reorder)
				n, err := rconn.Write(record)
				if err != nil {
					t.Fatalf("write: %v", err)
				}
				if n != len(record) {
					t.Fatalf("write n=%d want %d", n, len(record))
				}
				if !rconn.Enabled() {
					t.Fatal("layer disabled by a successful write")
				}

				if ipVersion == 6 || !mode.fragment {
					// whole record: the exact bytes, in one record
					got := make([]byte, len(record))
					if _, err := io.ReadFull(server, got); err != nil {
						t.Fatalf("read: %v", err)
					}
					if !bytes.Equal(got, record) {
						t.Fatal("peer received different bytes than written")
					}
				} else {
					// v4 fragment+reorder: re-framed into several records whose
					// payloads concatenate to the original handshake
					got := readTlsRecords(t, server, len(record)-5)
					if !bytes.Equal(got, record[5:]) {
						t.Fatal("peer received different payload than written")
					}
				}

				// the connection remains usable after the fallback
				followup := []byte("after")
				if _, err := client.Write(followup); err != nil {
					t.Fatalf("follow-up write on the still-open connection: %v", err)
				}
				got := make([]byte, len(followup))
				if _, err := io.ReadFull(server, got); err != nil {
					t.Fatalf("follow-up read: %v", err)
				}
				if !bytes.Equal(got, followup) {
					t.Fatal("follow-up bytes differ")
				}
			})
		})
	}
}
