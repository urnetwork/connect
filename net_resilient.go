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
	"io"
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
	"sync/atomic"
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
	return newResilientDialTlsContext(connectSettings, fragment, reorder, nil)
}

func newResilientDialTlsContext(
	connectSettings *ConnectSettings,
	fragment bool,
	reorder bool,
	nextProtos []string,
) DialTlsContextFunction {
	baseTlsConfig := newClientTlsConfig(connectSettings.TlsConfig, nextProtos)
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
		tlsConfig := baseTlsConfig.Clone()
		tlsConfig.ServerName = host
		tlsConn := tls.Client(rconn, tlsConfig)

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
		if err := rconn.Off(); err != nil {
			tlsConn.Close()
			return nil, err
		}

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

	enabled atomic.Bool
}

// must be created before the tls connection starts
func NewResilientTlsConn(conn net.Conn, fragment bool, reorder bool) *ResilientTlsConn {
	resilientTlsConn := &ResilientTlsConn{
		conn:     conn,
		fragment: fragment,
		reorder:  reorder,
		buffer:   []byte{},
	}
	resilientTlsConn.enabled.Store(true)
	return resilientTlsConn
}

// Off permanently disables the resilient fragment/reorder layer. It drains
// any partially-buffered record first — an earlier Write already returned
// len(b), nil for those bytes, so stranding them would silently lose data
// the caller believes was sent. A partial or failed drain leaves the wire
// state indeterminate: the connection is failed closed (closed, so the
// caller must not hand it back as established) and the drain error is
// returned — io.ErrShortWrite when the drain came up short with a nil
// error. Returns nil on a successful drain or when the buffer is empty.
// Off is not safe for concurrent use with Write.
func (self *ResilientTlsConn) Off() error {
	if 0 < len(self.buffer) {
		n, err := self.conn.Write(self.buffer)
		if err != nil || n < len(self.buffer) {
			if err == nil {
				err = io.ErrShortWrite
			}
			self.failConnection()
			return err
		}
		self.buffer = nil
	}
	// can't turn back on after off because we don't know where to align the tls header
	self.enabled.Store(false)
	return nil
}

func (self *ResilientTlsConn) Enabled() bool {
	return self.enabled.Load()
}

// failConnection marks the connection unusable after an indeterminate write
// (a partial or failed record send: the peer has part of the bytes and the
// wire state is unknowable). The buffered record is dropped so it is never
// re-sent, the resilient layer is disabled, and the underlying connection is
// closed so later writes fail instead of appending to a corrupt stream.
func (self *ResilientTlsConn) failConnection() {
	self.buffer = nil
	self.enabled.Store(false)
	self.conn.Close()
}

// writeRecord writes record whole to w. On any short or failed write the
// connection is failed closed: a partial record on the wire cannot be
// coherently retried — the buffer still holds the full record, so a retry
// would re-send the bytes already on the wire — and the layer is disabled
// and the connection closed so later writes fail instead of appending to
// the corrupt stream. A short write with a nil error is converted to
// io.ErrShortWrite so the caller never mistakes a closed connection for
// success. The buffer is not advanced here; callers advance it past the
// record only after this returns nil.
func (self *ResilientTlsConn) writeRecord(w io.Writer, record []byte) error {
	n, err := w.Write(record)
	if err == nil && n == len(record) {
		return nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	self.failConnection()
	return err
}

func (self *ResilientTlsConn) Write(b []byte) (int, error) {
	if self.Enabled() {
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
						// guard mathrand.Intn against zero/negative bounds (very short ServerName values panic Intn)
						splitRangeMid := (meta.ServerNameValueEnd + meta.ServerNameValueStart) / 2
						splitRange := splitRangeMid - meta.ServerNameValueStart
						stepRange := meta.ServerNameValueEnd - (meta.ServerNameValueStart + splitRange)

						if splitRange <= 0 || stepRange <= 0 {
							// the server name is too short to fragment;
							// fall back to a single write
							record := tlsHeader.reconstruct(handshakeBytes)
							if err := self.writeRecord(self.conn, record); err != nil {
								return 0, err
							}
							self.buffer = self.buffer[5+tlsHeader.contentLength:]
							continue
						}
						split := meta.ServerNameValueStart + mathrand.Intn(splitRange)
						step := 1 + mathrand.Intn(stepRange)
						blockSize := 64

						if tcpConn, ok := self.conn.(*net.TCPConn); ok {

							if self.fragment && self.reorder {
								tcpConn.SetNoDelay(true)

								f, err := tcpConn.File()
								if err != nil {
									return 0, err
								}
								fd := SocketHandle(f.Fd())
								defer f.Close()

								nativeTtl := GetSocketTtl(fd)
								if nativeTtl <= 0 {
									// syscall failed or returned a value we can't safely restore
									// (setting back to 0 would drop all packets at the first hop)
									record := tlsHeader.reconstruct(handshakeBytes)
									if err := self.writeRecord(tcpConn, record); err != nil {
										return 0, err
									}
									self.buffer = self.buffer[5+tlsHeader.contentLength:]
									continue
								}
								// restore the TTL on every exit after this point,
								// including fragment-write failures; defer LIFO
								// runs it before f.Close() closes the dup'd fd
								defer SetSocketTtl(fd, nativeTtl)

								// fmt.Printf("native ttl=%d, server name start=%d, end=%d\n", nativeTtl, meta.ServerNameValueStart, meta.ServerNameValueEnd)

								SetSocketTtl(fd, 0)
								record := tlsHeader.reconstruct(handshakeBytes[0:split])
								if err := self.writeRecord(tcpConn, record); err != nil {
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
									record := tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])
									if err := self.writeRecord(tcpConn, record); err != nil {
										return 0, err
									}
									// fmt.Printf("frag ttl=%d\n", ttl)
								}

								SetSocketTtl(fd, nativeTtl)

								tailRecord := tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])
								if err := self.writeRecord(tcpConn, tailRecord); err != nil {
									return 0, err
								}
								// fmt.Printf("frag ttl=%d\n", nativeTtl)
							} else if self.fragment {

								record := tlsHeader.reconstruct(handshakeBytes[0:split])
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}

								for i := split; i < meta.ServerNameValueEnd; i += step {
									record := tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])
									if err := self.writeRecord(tcpConn, record); err != nil {
										return 0, err
									}
								}

								record = tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}

							} else if self.reorder {

								tlsBytes := tlsHeader.reconstruct(handshakeBytes)

								tcpConn.SetNoDelay(true)

								f, err := tcpConn.File()
								if err != nil {
									return 0, err
								}
								fd := SocketHandle(f.Fd())
								defer f.Close()

								nativeTtl := GetSocketTtl(fd)
								if nativeTtl <= 0 {
									// syscall failed; fall back to a single write
									if err := self.writeRecord(tcpConn, tlsBytes); err != nil {
										return 0, err
									}
									self.buffer = self.buffer[5+tlsHeader.contentLength:]
									continue
								}
								// restore the TTL on every exit after this point,
								// including block-write failures; defer LIFO
								// runs it before f.Close() closes the dup'd fd
								defer SetSocketTtl(fd, nativeTtl)

								for i := 0; i*blockSize < len(tlsBytes); i += 1 {
									var ttl int
									if 0 == i%2 {
										ttl = 0
									} else {
										ttl = nativeTtl
									}
									SetSocketTtl(fd, ttl)
									b := tlsBytes[i*blockSize : min((i+1)*blockSize, len(tlsBytes))]
									if err := self.writeRecord(tcpConn, b); err != nil {
										return 0, err
									}
								}

								SetSocketTtl(fd, nativeTtl)

							} else {
								record := tlsHeader.reconstruct(handshakeBytes)
								if err := self.writeRecord(tcpConn, record); err != nil {
									return 0, err
								}
							}

						} else {

							if self.fragment {
								record := tlsHeader.reconstruct(handshakeBytes[0:split])
								if err := self.writeRecord(self.conn, record); err != nil {
									return 0, err
								}

								for i := split; i < meta.ServerNameValueEnd; i += step {
									record := tlsHeader.reconstruct(handshakeBytes[i:min(i+step, meta.ServerNameValueEnd)])
									if err := self.writeRecord(self.conn, record); err != nil {
										return 0, err
									}
								}

								record = tlsHeader.reconstruct(handshakeBytes[meta.ServerNameValueEnd:])
								if err := self.writeRecord(self.conn, record); err != nil {
									return 0, err
								}
							} else {
								record := tlsHeader.reconstruct(handshakeBytes)
								if err := self.writeRecord(self.conn, record); err != nil {
									return 0, err
								}
							}

						}

					} else {
						// flush the raw record; a short or failed write leaves a
						// partial record on the wire, so writeRecord fails closed
						if err := self.writeRecord(self.conn, self.buffer[0:5+tlsHeader.contentLength]); err != nil {
							return 0, err
						}
					}
				} else {
					// flush the raw record; a short or failed write leaves a
					// partial record on the wire, so writeRecord fails closed
					if err := self.writeRecord(self.conn, self.buffer[0:5+tlsHeader.contentLength]); err != nil {
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
