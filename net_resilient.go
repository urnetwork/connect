package connect

import (
	"context"
	"net"
	// "net/http"

	// "os"
	// "strings"
	"fmt"
	"time"
	// "strconv"
	// "slices"

	"crypto/tls"
	// "crypto/ecdsa"
	// "crypto/ed25519"
	// "crypto/elliptic"
	// "crypto/rand"
	// "crypto/rsa"
	// "crypto/x509"
	// "crypto/x509/pkix"
	// "encoding/pem"
	// "encoding/json"
	// "flag"
	// "log"
	// "math/big"

	// "crypto/md5"
	// "encoding/binary"
	// "encoding/hex"
	// "syscall"

	mathrand "math/rand"
	// "golang.org/x/crypto/cryptobyte"
	// "golang.org/x/net/idna"
	// "google.golang.org/protobuf/proto"
	// "src.agwa.name/tlshacks"
	// "github.com/urnetwork/glog"
)

// see https://upb-syssec.github.io/blog/2023/record-fragmentation/

// set this as the `DialTLSContext` or equivalent
// returns a tls connection
func NewResilientDialTlsContext(
	connectSettings *ConnectSettings,
	fragment bool,
	reorder bool,
) DialTlsContextFunction {
	return func(
		ctx context.Context,
		network string,
		addr string,
	) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			panic(fmt.Errorf("Resilient connections only support tcp network."))
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			panic(err)
		}

		// fmt.Printf("Extender client 1\n")

		conn, err := connectSettings.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}

		rconn := NewResilientTlsConn(conn, fragment, reorder)

		// copy and extend
		tlsConfig := *connectSettings.TlsConfig
		tlsConfig.ServerName = host
		tlsConn := tls.Client(rconn, &tlsConfig)

		func() {
			tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer tlsCancel()
			err = tlsConn.HandshakeContext(tlsCtx)
		}()
		if err != nil {
			tlsConn.Close()
			return nil, err
		}
		// once the stream is established, no longer need the resilient features
		rconn.Off()

		return tlsConn, nil
	}
}

// adapts techniques to overcome adversarial networks
// the network uses this to the connect to the platform and extenders
// inspiraton for techniques taken from the Jigsaw project Outline SDK

type ResilientTlsConn struct {
	conn     net.Conn
	fragment bool
	reorder  bool
	buffer   []byte
	enabled  bool
}

// must be created before the tls connection starts
func NewResilientTlsConn(conn net.Conn, fragment bool, reorder bool) *ResilientTlsConn {
	return &ResilientTlsConn{
		conn:     conn,
		fragment: fragment,
		reorder:  reorder,
		buffer:   []byte{},
		enabled:  true,
	}
}

func (self *ResilientTlsConn) Off() {
	// can't turn back on after off because we don't know where to align the tls header
	self.enabled = false
}

func (self *ResilientTlsConn) Write(b []byte) (int, error) {
	if self.enabled {
		self.buffer = append(self.buffer, b...)
		for 5 <= len(self.buffer) {
			tlsHeader := parseTlsHeader(self.buffer[0:5])
			if 5+int(tlsHeader.contentLength) <= len(self.buffer) {
				if tlsHeader.contentType == TlsContentTypeHandshake {
					// handshake
					handshakeBytes := self.buffer[5 : 5+tlsHeader.contentLength]
					clientHello, meta := UnmarshalClientHello(handshakeBytes)
					if clientHello != nil && clientHello.Info.ServerName != nil {
						// send the server name one character at a time
						// for each fragment, alternate the ttl of the connection to force retransmits and out-of-order arrival

						// initialSplitLen := mathrand.Intn((meta.ServerNameValueEnd+meta.ServerNameValueStart)/2-meta.ServerNameValueStart)
						split := meta.ServerNameValueStart + mathrand.Intn((meta.ServerNameValueEnd+meta.ServerNameValueStart)/2-meta.ServerNameValueStart)
						step := 1 + mathrand.Intn(meta.ServerNameValueEnd-split)
						blockSize := 64

						if tcpConn, ok := self.conn.(*net.TCPConn); ok {

							if self.fragment && self.reorder {
								tcpConn.SetNoDelay(true)

								f, _ := tcpConn.File()
								fd := SocketHandle(f.Fd())

								nativeTtl := GetSocketTtl(fd)

								// fmt.Printf("native ttl=%d, server name start=%d, end=%d\n", nativeTtl, meta.ServerNameValueStart, meta.ServerNameValueEnd)

								SetSocketTtl(fd, 0)
								if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes[0:split])); err != nil {
									return 0, err
								}
								// fmt.Printf("frag ttl=0\n")

								for i := split; i < meta.ServerNameValueEnd; i += step {
									var ttl int
									if 0 == mathrand.Intn(2) {
										ttl = 0
									} else {
										ttl = nativeTtl
									}
									SetSocketTtl(fd, ttl)
									if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])); err != nil {
										return 0, err
									}
									// fmt.Printf("frag ttl=%d\n", ttl)
								}

								SetSocketTtl(fd, nativeTtl)

								if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])); err != nil {
									return 0, err
								}
								// fmt.Printf("frag ttl=%d\n", nativeTtl)
							} else if self.fragment {

								if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes[0:split])); err != nil {
									return 0, err
								}

								for i := split; i < meta.ServerNameValueEnd; i += step {
									if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])); err != nil {
										return 0, err
									}
								}

								if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])); err != nil {
									return 0, err
								}

							} else if self.reorder {

								tlsBytes := tlsHeader.reconstruct(handshakeBytes)

								tcpConn.SetNoDelay(true)

								f, _ := tcpConn.File()
								fd := SocketHandle(f.Fd())

								nativeTtl := GetSocketTtl(fd)

								for i := 0; i*blockSize < len(tlsBytes); i += 1 {
									var ttl int
									if 0 == i%2 {
										ttl = 0
									} else {
										ttl = nativeTtl
									}
									SetSocketTtl(fd, ttl)
									b := tlsBytes[i*blockSize : min((i+1)*blockSize, len(tlsBytes))]
									if _, err := tcpConn.Write(b); err != nil {
										return 0, err
									}
								}

								SetSocketTtl(fd, nativeTtl)

							} else {
								if _, err := tcpConn.Write(tlsHeader.reconstruct(handshakeBytes)); err != nil {
									return 0, err
								}
							}

						} else {

							if self.fragment {
								if _, err := self.conn.Write(tlsHeader.reconstruct(handshakeBytes[0:split])); err != nil {
									return 0, err
								}

								for i := split; i < meta.ServerNameValueEnd; i += step {
									if _, err := self.conn.Write(tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])); err != nil {
										return 0, err
									}
								}

								if _, err := self.conn.Write(tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])); err != nil {
									return 0, err
								}
							} else {
								if _, err := self.conn.Write(tlsHeader.reconstruct(handshakeBytes)); err != nil {
									return 0, err
								}
							}

						}

					} else {
						// flush the raw record
						_, err := self.conn.Write(self.buffer[0 : 5+tlsHeader.contentLength])
						if err != nil {
							return 0, err
						}
					}
				} else {
					// flush the raw record
					_, err := self.conn.Write(self.buffer[0 : 5+tlsHeader.contentLength])
					if err != nil {
						return 0, err
					}
				}

				self.buffer = self.buffer[5+tlsHeader.contentLength:]
			} else {
				break
			}
		}
		return len(b), nil
	} else {
		return self.conn.Write(b)
	}
}

func (self *ResilientTlsConn) Read(b []byte) (int, error) {
	return self.conn.Read(b)
}

func (self *ResilientTlsConn) Close() error {
	return self.conn.Close()
}

func (self *ResilientTlsConn) LocalAddr() net.Addr {
	return self.conn.LocalAddr()
}

func (self *ResilientTlsConn) RemoteAddr() net.Addr {
	return self.conn.RemoteAddr()
}

func (self *ResilientTlsConn) SetDeadline(t time.Time) error {
	return self.conn.SetDeadline(t)
}

func (self *ResilientTlsConn) SetReadDeadline(t time.Time) error {
	return self.conn.SetReadDeadline(t)
}

func (self *ResilientTlsConn) SetWriteDeadline(t time.Time) error {
	return self.conn.SetWriteDeadline(t)
}
