package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func TestExtenderTCPResponseHeaderParserHasAnInputBound(t *testing.T) {
	for _, headers := range []string{
		"X-Large: " + strings.Repeat("x", 2*1024*1024) + "\r\n",
		strings.Repeat("X-Many: a\r\n", 64*1024),
	} {
		source := strings.NewReader("HTTP/1.1 200 OK\r\n" + headers + "Content-Length: 4\r\n\r\n\x00\x00\x00\x00")
		initial := source.Len()
		reader := &extenderResponseHeaderReader{reader: source, remaining: ExtenderMaxHeaderByteCount}
		if _, err := http.ReadResponse(bufio.NewReader(reader), nil); err == nil {
			t.Fatal("oversized MIME headers were parsed")
		}
		if read := initial - source.Len(); read != ExtenderMaxHeaderByteCount {
			t.Fatalf("parser consumed %d input bytes, cap=%d", read, ExtenderMaxHeaderByteCount)
		}
	}
	// Read-ahead bytes must still become the tunnel's first bytes when the
	// header cap is disabled after a valid response.
	const inner = "first inner bytes"
	source := strings.NewReader("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\n\x00\x00\x00\x00" + inner)
	limited := &extenderResponseHeaderReader{reader: source, remaining: ExtenderMaxHeaderByteCount}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	limited.remaining = -1
	if _, err := ReadExtenderResponseFrame(response.Body); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != inner {
		t.Fatalf("read-ahead handoff = %q, %v", got, err)
	}
}

func TestExtenderOuterResponseHeadersFailBoundedAndReleaseClaim(t *testing.T) {
	old := MemoryBudget()
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(old)
	certPEM, keyPEM, err := selfSign([]string{"127.0.0.1"}, "extender-header-memory", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, carrier := range []string{"tcp-large-line", "tcp-many-fields", "tcp-chunked", "quic-encoded", "quic-decoded"} {
		t.Run(carrier, func(t *testing.T) {
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			ctx, cancel := context.WithTimeout((&PlatformTransport{settings: settings}).dialContext(t.Context()), 5*time.Second)
			defer cancel()
			budget, root := settings.PlatformTransportBudget, DefaultPlatformTransportBudget()
			base := root.Stats().UsedByteCount
			config := &ExtenderConfig{Ip: netip.MustParseAddr("127.0.0.1")}
			tlsConfig := &tls.Config{InsecureSkipVerify: true} // only this malicious loopback fixture
			connectSettings := DefaultConnectSettings()
			var conn net.Conn
			var dialErr error
			if strings.HasPrefix(carrier, "tcp") {
				listener, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				config.Profile.Port = listener.Addr().(*net.TCPAddr).Port
				done := make(chan struct{})
				go func() {
					defer close(done)
					peer, err := listener.Accept()
					if err != nil {
						return
					}
					defer peer.Close()
					peer.SetDeadline(time.Now().Add(5 * time.Second))
					request, err := http.ReadRequest(bufio.NewReader(peer))
					if err != nil {
						return
					}
					io.Copy(io.Discard, request.Body)
					header := "X-Large: " + strings.Repeat("x", 4096) + "\r\nContent-Length: 4\r\n\r\n\x00\x00\x00\x00"
					if carrier == "tcp-many-fields" {
						header = strings.Repeat("X-Many: a\r\n", 256) + "Content-Length: 4\r\n\r\n\x00\x00\x00\x00"
					} else if carrier == "tcp-chunked" {
						header = "Transfer-Encoding: chunked\r\n\r\n4\r\n\x00\x00\x00\x00\r\n0\r\nX-Trailer: " + strings.Repeat("x", 4096) + "\r\n\r\n"
					}
					io.WriteString(peer, "HTTP/1.1 200 OK\r\n"+header)
				}()
				conn, _, dialErr = dialExtenderTcp(ctx, connectSettings, config, tlsConfig, nil, nil)
				listener.Close()
				<-done
			} else {
				listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{http3.NextProtoH3}}, &quic.Config{})
				if err != nil {
					t.Fatal(err)
				}
				value := strings.Repeat("X~Z|Q^", 512)
				if carrier == "quic-decoded" {
					value = strings.Repeat("a", ExtenderMaxHeaderByteCount+64)
				}
				server := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Large", value)
					io.Copy(w, bytes.NewReader([]byte{0, 0, 0, 0}))
				})}
				done := make(chan error, 1)
				go func() { done <- server.ServeListener(listener) }()
				config.Profile.Port = listener.Addr().(*net.UDPAddr).Port
				config.Profile.ConnectMode = ExtenderConnectModeQuic
				conn, _, dialErr = dialExtenderQuic(ctx, connectSettings, config, tlsConfig, nil, nil)
				server.Close()
				listener.Close()
				<-done
			}
			if conn != nil {
				conn.Close()
			}
			if dialErr == nil || (!strings.Contains(strings.ToLower(dialErr.Error()), "header") && !strings.Contains(dialErr.Error(), "content length")) {
				t.Fatal(fmt.Sprintf("oversized outer response was not refused by the bounded parser: %v", dialErr))
			}
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || root.Stats().UsedByteCount != base {
				t.Fatalf("header failure leaked claim: child=%+v root=%+v", stats, root.Stats())
			}
		})
	}
}
