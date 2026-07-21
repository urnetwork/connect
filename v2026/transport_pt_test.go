package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	mathrand "math/rand"
	"net"
	"os"
	"time"

	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/tls"

	// "crypto/elliptic"
	// "crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	// "crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"

	"math/big"

	quic "github.com/quic-go/quic-go"

	"testing"
)

func TestPtDnsEncodeDecode(t *testing.T) {
	ptEncodeDecodeTest(t, PacketTranslationModeDns, PacketTranslationModeDecode53, 6555)
}

func TestPtDnsPumpEncodeDecode(t *testing.T) {
	ptEncodeDecodeTest(t, PacketTranslationModeDnsPump, PacketTranslationModeDecode53RequireDnsPump, 6555)
}

func ptEncodeDecodeTest(t *testing.T, clientPtMode PacketTranslationMode, serverPtMode PacketTranslationMode, basePort int) {
	if testing.Short() {
		return
	}

	iterations := 16
	attempts := 4
	if os.Getenv("CONNECT_PT_STRESS") != "" {
		iterations = 64
		attempts = 8
	}

	select {
	case <-time.After(1 * time.Second):
	}

	ctx := context.Background()

	consecutive := func(n int) []byte {
		out := make([]byte, 4*n)
		for i := range n {
			binary.BigEndian.PutUint32(out[4*i:4*i+4], uint32(i))
		}
		return out
	}

	for i := range iterations {
		headerPrefix := make([]byte, 8)
		mathrand.Read(headerPrefix)

		// FIXME quic does not seem to recover well with packet loss
		packetLossN := i + 100

		fmt.Printf("[%d]dns test (loss=%.1f%%)\n", i, 100.0/float32(packetLossN))
		success := false
		for range attempts {
			success = func() bool {
				attemptCtx, attemptCancel := context.WithTimeout(ctx, 20*time.Second)
				defer attemptCancel()

				handleCtx, handleCancel := context.WithCancel(attemptCtx)
				defer handleCancel()

				n := 1024 * (8 + mathrand.Intn(8))
				data := consecutive(n)

				tld := []byte("foo.com.")

				serverAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: basePort + i}

				ioTimeout := 5 * time.Second
				ioDeadline := func() time.Time {
					deadline := time.Now().Add(ioTimeout)
					if ctxDeadline, ok := handleCtx.Deadline(); ok && ctxDeadline.Before(deadline) {
						return ctxDeadline
					}
					return deadline
				}

				quicConfig := &quic.Config{
					HandshakeIdleTimeout:    ioTimeout,
					MaxIdleTimeout:          ioTimeout,
					KeepAlivePeriod:         5 * time.Second,
					Allow0RTT:               true,
					DisablePathMTUDiscovery: true,
				}

				serverCtx, serverCancel := context.WithCancel(handleCtx)
				errCh := make(chan error, 4)
				reportErr := func(err error) bool {
					if err == nil {
						return false
					}
					select {
					case errCh <- err:
					default:
					}
					handleCancel()
					return true
				}
				// func() {
				serverTlsConfig := &tls.Config{
					GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
						certPemBytes, keyPemBytes, err := selfSign(
							[]string{clientHello.ServerName},
							clientHello.ServerName,
							180*24*time.Hour,
							180*24*time.Hour,
						)
						if err != nil {
							return nil, err
						}
						// X509KeyPair
						cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)
						return &tls.Config{
							Certificates: []tls.Certificate{cert},
						}, err
					},
				}

				serverConn, err := net.ListenUDP("udp", serverAddr)
				AssertEqual(t, err, nil)
				defer serverConn.Close()

				serverLossConn := newPacketLossPacketConn(packetLossN, serverConn)

				serverPtSettings := DefaultPacketTranslationSettings()
				serverPtSettings.DnsTlds = [][]byte{tld}
				// settings.DnsAddr = serverAddr

				serverPtConn, err := NewPacketTranslationWithPrefix(handleCtx, serverPtMode, serverLossConn, serverPtSettings, headerPrefix)
				AssertEqual(t, err, nil)
				defer serverPtConn.Close()

				earlyListener, err := (&quic.Transport{
					Conn: serverPtConn,
					// createdConn: true,
					// isSingleUse: true,
				}).ListenEarly(serverTlsConfig, quicConfig)
				// listenQuic(ctx, earlyListener)
				AssertEqual(t, err, nil)
				defer earlyListener.Close()

				go func() {
					defer serverCancel()
					// defer ptConn.Close()
					// defer earlyListener.Close()

					earlyConn, err := earlyListener.Accept(handleCtx)
					if err != nil {
						reportErr(fmt.Errorf("server accept: %w", err))
						return
					}
					stream, err := earlyConn.AcceptStream(handleCtx)
					if err != nil {
						reportErr(fmt.Errorf("server accept stream: %w", err))
						return
					}

					writeCtx, writeCancel := context.WithCancel(handleCtx)
					go func() {
						defer writeCancel()
						stream.SetWriteDeadline(ioDeadline())
						m, err := stream.Write(data)
						if err != nil {
							reportErr(fmt.Errorf("server write: %w", err))
							return
						}
						if m != len(data) {
							reportErr(fmt.Errorf("server short write: %d != %d", m, len(data)))
						}
					}()

					readData := make([]byte, 0, len(data))
					buf := make([]byte, 2048)

					for len(readData) < len(data) {
						select {
						case <-handleCtx.Done():
							reportErr(handleCtx.Err())
							return
						default:
						}
						stream.SetReadDeadline(ioDeadline())
						m, err := stream.Read(buf[:min(len(buf), len(data)-len(readData))])
						if err != nil {
							reportErr(fmt.Errorf("server read: %w", err))
							return
						}
						readData = append(readData, buf[:m]...)

						// fmt.Printf("read[%d]\n", m)
						fmt.Printf("+")
					}

					if !bytes.Equal(data, readData) {
						reportErr(fmt.Errorf("server read data mismatch"))
						return
					}

					select {
					case err := <-errCh:
						reportErr(err)
						return
					case <-writeCtx.Done():
					}

				}()

				// }()

				clientTlsConfig := &tls.Config{
					ServerName:         string(tld),
					InsecureSkipVerify: true,
				}

				clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
				AssertEqual(t, err, nil)
				defer clientConn.Close()

				lossConn := newPacketLossPacketConn(packetLossN, clientConn)

				ptSettings := DefaultPacketTranslationSettings()
				ptSettings.DnsTlds = [][]byte{tld}
				// ptSettings.DnsAddr = serverAddr
				ptConn, err := NewPacketTranslationWithPrefix(handleCtx, clientPtMode, lossConn, ptSettings, headerPrefix)
				AssertEqual(t, err, nil)
				defer ptConn.Close()

				quicTransport := &quic.Transport{
					Conn: ptConn,
					// createdConn: true,
					// isSingleUse: true,
				}

				// enable 0rtt if possible
				conn, err := quicTransport.DialEarly(handleCtx, serverAddr, clientTlsConfig, quicConfig)
				AssertEqual(t, err, nil)

				stream, err := conn.OpenStream()
				AssertEqual(t, err, nil)

				writeCtx, writeCancel := context.WithCancel(handleCtx)
				go func() {
					defer writeCancel()
					stream.SetWriteDeadline(ioDeadline())
					m, err := stream.Write(data)
					if err != nil {
						reportErr(fmt.Errorf("client write: %w", err))
						return
					}
					if m != len(data) {
						reportErr(fmt.Errorf("client short write: %d != %d", m, len(data)))
					}
				}()

				readData := make([]byte, 0, len(data))
				buf := make([]byte, 2048)

				for len(readData) < len(data) {
					select {
					case err := <-errCh:
						fmt.Printf("connection issue: %s\n", err)
						return false
					case <-handleCtx.Done():
						reportErr(handleCtx.Err())
						return false
					default:
					}
					stream.SetReadDeadline(ioDeadline())
					m, err := stream.Read(buf[:min(len(buf), len(data)-len(readData))])
					if err != nil {
						reportErr(fmt.Errorf("client read: %w", err))
						return false
					}
					// AssertEqual(t, err, nil)
					readData = append(readData, buf[:m]...)

					// fmt.Printf("read[%d]\n", m)
					fmt.Printf(".")
				}

				if !bytes.Equal(data, readData) {
					reportErr(fmt.Errorf("client read data mismatch"))
					return false
				}

				select {
				case err := <-errCh:
					fmt.Printf("connection issue: %s\n", err)
					return false
				case <-writeCtx.Done():
					// case <- time.After(60 * time.Second):
					// 	t.FailNow()
				}

				select {
				case err := <-errCh:
					fmt.Printf("connection issue: %s\n", err)
					return false
				case <-serverCtx.Done():
					// case <- time.After(60 * time.Second):
					// 	t.FailNow()
				}
				select {
				case err := <-errCh:
					fmt.Printf("connection issue: %s\n", err)
					return false
				default:
				}

				return true
			}()
			fmt.Printf("\n")
			if success {
				// timeout. reform the sockets and retry
				break
			}
			fmt.Printf("connection issue. retry.\n")
			select {
			case <-ctx.Done():
			case <-time.After(1 * time.Second):
			}
		}
		if !success {
			t.FailNow()
		}
	}

}

// drops one of n outgoing packets randomly
type packetLossPacketConn struct {
	n          int
	packetConn net.PacketConn

	readRand  *mathrand.Rand
	writeRand *mathrand.Rand
}

func newPacketLossPacketConn(n int, packetConn net.PacketConn) *packetLossPacketConn {
	return &packetLossPacketConn{
		n:          n,
		packetConn: packetConn,
		readRand:   mathrand.New(mathrand.NewSource(time.Now().UnixMilli())),
		writeRand:  mathrand.New(mathrand.NewSource(time.Now().UnixMilli())),
	}
}

func (self *packetLossPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	for {
		n, addr, err = self.packetConn.ReadFrom(p)
		if err != nil {
			return
		}
		if 0 < self.n && self.readRand.Intn(self.n+1) == 0 {
			if self.readRand.Intn(2) == 0 {
				// scramble the packet
				self.readRand.Read(p[:n])
				fmt.Printf("s")
			} else {
				// drop the packet
				fmt.Printf("d")
				continue
			}
		}
		return
	}
}

func (self *packetLossPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if 0 < self.n && self.writeRand.Intn(self.n+1) == 0 {
		if self.writeRand.Intn(2) == 0 {
			// scramble the packet
			self.writeRand.Read(p)
			fmt.Printf("s")
		} else {
			// drop the packet
			n = len(p)
			fmt.Printf("d")
			return
		}
	}

	n, err = self.packetConn.WriteTo(p, addr)
	return
}

func (self *packetLossPacketConn) LocalAddr() net.Addr {
	return self.packetConn.LocalAddr()
}

func (self *packetLossPacketConn) SetDeadline(t time.Time) error {
	return self.packetConn.SetDeadline(t)
}

func (self *packetLossPacketConn) SetReadDeadline(t time.Time) error {
	return self.packetConn.SetReadDeadline(t)
}

func (self *packetLossPacketConn) SetWriteDeadline(t time.Time) error {
	return self.packetConn.SetWriteDeadline(t)
}

func (self *packetLossPacketConn) Close() error {
	return self.packetConn.Close()
}

func (self *packetLossPacketConn) SetReadBuffer(bytes int) error {
	conn, ok := self.packetConn.(interface{ SetReadBuffer(int) error })
	if !ok {
		return fmt.Errorf("Set read buffer not supporter on underlying packet conn: %T", self.packetConn)
	}
	return conn.SetReadBuffer(bytes)
}

func (self *packetLossPacketConn) SetWriteBuffer(bytes int) error {
	conn, ok := self.packetConn.(interface{ SetWriteBuffer(int) error })
	if !ok {
		return fmt.Errorf("Set write buffer not supporter on underlying packet conn: %T", self.packetConn)
	}
	return conn.SetWriteBuffer(bytes)
}

func selfSign(hosts []string, organization string, validFrom time.Duration, validFor time.Duration) (certPemBytes []byte, keyPemBytes []byte, returnErr error) {

	var priv any
	var err error

	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	// priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		returnErr = err
		return
	}

	publicKey := func(priv any) any {
		switch k := priv.(type) {
		case *rsa.PrivateKey:
			return &k.PublicKey
		case *ecdsa.PrivateKey:
			return &k.PublicKey
		case ed25519.PrivateKey:
			return k.Public().(ed25519.PublicKey)
		default:
			return nil
		}
	}

	// ECDSA, ED25519 and RSA subject keys should have the DigitalSignature
	// KeyUsage bits set in the x509.Certificate template
	keyUsage := x509.KeyUsageDigitalSignature
	// Only RSA subject keys should have the KeyEncipherment KeyUsage bits set. In
	// the context of TLS this KeyUsage is particular to RSA key exchange and
	// authentication.
	if _, isRSA := priv.(*rsa.PrivateKey); isRSA {
		keyUsage |= x509.KeyUsageKeyEncipherment
	}

	notBefore := time.Now().Add(-validFrom)
	notAfter := notBefore.Add(validFor)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		panic(fmt.Errorf("Failed to generate serial number: %v", err))
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{organization},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              keyUsage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	// we hope the client is using tls1.3 which hides the self signed cert
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey(priv), priv)
	if err != nil {
		returnErr = err
		return
	}
	certPemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		returnErr = err
		return
	}
	keyPemBytes = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return
}
