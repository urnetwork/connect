package connect

import (
	"context"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func syntheticRequest(t *testing.T, request string) (header string, bodyByteCount int) {
	c := newSyntheticSpeedConn()
	defer c.Close()
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatalf("write: %s", err)
	}
	var response []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			response = append(response, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %s", err)
		}
	}
	i := strings.Index(string(response), "\r\n\r\n")
	if i < 0 {
		t.Fatalf("no header terminator in response")
	}
	return string(response[:i]), len(response) - (i + 4)
}

func TestSyntheticSpeedPing(t *testing.T) {
	header, bodyByteCount := syntheticRequest(t, "GET /ping HTTP/1.1\r\nHost: 198.18.0.1\r\n\r\n")
	if !strings.HasPrefix(header, "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected status: %s", header)
	}
	if bodyByteCount != 1 {
		t.Fatalf("expected 1-byte ping body, got %d", bodyByteCount)
	}
}

func TestSyntheticSpeedDownload(t *testing.T) {
	header, bodyByteCount := syntheticRequest(t, "GET /download/1000000 HTTP/1.1\r\nHost: 198.18.0.1\r\n\r\n")
	if !strings.HasPrefix(header, "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected status: %s", header)
	}
	if !strings.Contains(header, "Content-Length: 1000000") {
		t.Fatalf("missing content length: %s", header)
	}
	if bodyByteCount != 1000000 {
		t.Fatalf("expected 1000000 body bytes, got %d", bodyByteCount)
	}
}

func TestSyntheticSpeedUpload(t *testing.T) {
	body := strings.Repeat("x", 5000)
	header, _ := syntheticRequest(t, "POST /upload HTTP/1.1\r\nHost: 198.18.0.1\r\nContent-Length: 5000\r\n\r\n"+body)
	if !strings.HasPrefix(header, "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected status: %s", header)
	}
}

func TestSyntheticSpeedIpRange(t *testing.T) {
	for ip, expect := range map[string]bool{
		"198.18.0.1":   true,
		"198.19.255.1": true,
		"198.17.0.1":   false,
		"198.20.0.1":   false,
		"1.1.1.1":      false,
	} {
		if got := isSyntheticSpeedIp(net.ParseIP(ip)); got != expect {
			t.Fatalf("%s: expected %t got %t", ip, expect, got)
		}
	}
}

// the synthetic speed server answers the v4 benchmarking range (RFC 2544)
// and the v6 one (RFC 5180), including the mapped form of the v4 range, and
// nothing adjacent to either
func TestIsSyntheticSpeedIp(t *testing.T) {
	for _, literal := range []string{
		"198.18.0.1", "198.19.255.255", "198.18.255.254",
		"2001:2::1", "2001:2:0:ffff::1", "2001:2::", "2001:2:0:ffff:ffff:ffff:ffff:ffff",
		"::ffff:198.18.0.1",
	} {
		if !isSyntheticSpeedIp(net.ParseIP(literal)) {
			t.Errorf("isSyntheticSpeedIp(%s) = false, want true", literal)
		}
	}
	for _, literal := range []string{
		"198.17.255.255", "198.20.0.0", "197.18.0.1", "8.8.8.8",
		"2001:2:1::1", "2001:3::1", "2001:1::1", "2001:db8::1", "2002:2::1", "::1",
	} {
		if isSyntheticSpeedIp(net.ParseIP(literal)) {
			t.Errorf("isSyntheticSpeedIp(%s) = true, want false", literal)
		}
	}
	if isSyntheticSpeedIp(nil) {
		t.Errorf("isSyntheticSpeedIp(nil) = true, want false")
	}
}

// The benchmark ranges are never dialed: the security policy admits tcp to
// them only because the nat serves them in memory (ip_security_public.go), so
// with the synthetic server on the flow is answered in memory and with it off
// the flow is refused with a reset, and the upstream dialer is not called
// either way. The response follows the decision to dial or not, so once it
// arrives a dial would already have been recorded.
func TestSyntheticSpeedRangeIsNeverDialed(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, destination := range []string{"198.18.0.1", "198.19.255.254", "2001:2::1"} {
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				settings := DefaultLocalUserNatSettings()
				settings.Log = NewNoopLogger()
				settings.TcpBufferSettings.EnableSyntheticSpeed = enabled
				dialed := make(chan string, 4)
				settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
					DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
						dialed <- address
						return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
					},
				}
				nat := NewLocalUserNat(ctx, "synthetic speed test", settings)
				defer nat.Close()
				version := 6
				sourceIp := net.ParseIP("fd00::9")
				headerByteCount := Ipv6HeaderSize
				destinationIp := net.ParseIP(destination)
				if ip4 := destinationIp.To4(); ip4 != nil {
					version = 4
					sourceIp = net.ParseIP("10.0.0.9").To4()
					headerByteCount = Ipv4HeaderSizeWithoutExtensions
					destinationIp = ip4
				}
				flags := make(chan byte, 4)
				nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
					if headerByteCount+13 < len(packet) {
						select {
						case flags <- packet[headerByteCount+13]:
						default:
						}
					}
				})
				flow := &IpPath{
					Version:         version,
					Protocol:        IpProtocolTcp,
					SourceIp:        sourceIp,
					SourcePort:      42001,
					DestinationIp:   destinationIp,
					DestinationPort: 80,
				}
				packet := MessagePoolCopy(ipOosTcpPacketSequence(flow, tcpFlagSyn, 0x5eed, nil))
				if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
					MessagePoolReturn(packet)
					t.Fatalf("%s: the syn did not enter the nat", destination)
				}
				want := byte(tcpFlagSyn | tcpFlagAck)
				if !enabled {
					want = tcpFlagRst | tcpFlagAck
				}
				select {
				case got := <-flags:
					if got&(tcpFlagSyn|tcpFlagAck|tcpFlagRst) != want {
						t.Errorf("synthetic=%t %s: answered with flags %08b, want %08b", enabled, destination, got, want)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("synthetic=%t %s: no answer", enabled, destination)
				}
				select {
				case address := <-dialed:
					t.Errorf("synthetic=%t %s: the nat dialed %s", enabled, destination, address)
				default:
				}
			}()
		}
	}
}
