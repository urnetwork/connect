package connect

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/cryptobyte"

	"src.agwa.name/tlshacks"

	_ "embed"
)

// the let's encrypt root CAs as defined at https://letsencrypt.org/certificates/
// this includes:
// - ISRG Root X1
// - ISRG Root X2
//
//go:embed net_tls_ca.pem
var tlsCaPem string

func PinnedCertPool() (*x509.CertPool, error) {

	certPool := x509.NewCertPool()

	if !certPool.AppendCertsFromPEM([]byte(tlsCaPem)) {
		return nil, fmt.Errorf("Could not append ca certs")
	}

	return certPool, nil
}

func DefaultTlsConfig() (*tls.Config, error) {
	certPool, err := PinnedCertPool()
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}
	return tlsConfig, nil
}

// RFC 5246
type TlsContentType = byte

const (
	TlsContentTypeChangeCipherSpec TlsContentType = 0x14
	TlsContentTypeAlert            TlsContentType = 0x15
	TlsContentTypeHandshake        TlsContentType = 0x16
	TlsContentTypeApplicationData  TlsContentType = 0x17
	TlsContentTypeHeartbeat        TlsContentType = 0x18
)

type TlsVersion = uint16

const (
	// RFC 8446
	TlsVersion1_3 TlsVersion = 0x0304
	// RFC 5246
	TlsVersion1_2 TlsVersion = 0x0303
	// RFC 8996
	TlsVersion1_1 TlsVersion = 0x0302
	TlsVersion1_0 TlsVersion = 0x0301
)

type tlsHeader struct {
	contentType   TlsContentType
	tlsVersion    TlsVersion
	contentLength uint16
}

func parseTlsHeader(b []byte) *tlsHeader {
	return &tlsHeader{
		contentType:   b[0],
		tlsVersion:    binary.BigEndian.Uint16(b[1:3]),
		contentLength: binary.BigEndian.Uint16(b[3:5]),
	}
}

func (self *tlsHeader) reconstruct(content []byte) []byte {
	b := make([]byte, 5+len(content))
	b[0] = self.contentType
	binary.BigEndian.PutUint16(b[1:3], self.tlsVersion)
	binary.BigEndian.PutUint16(b[3:5], uint16(len(content)))
	copy(b[5:5+len(content)], content)
	return b
}

func (self *tlsHeader) valid() bool {
	switch self.contentType {
	case TlsContentTypeChangeCipherSpec, TlsContentTypeAlert, TlsContentTypeHandshake, TlsContentTypeApplicationData, TlsContentTypeHeartbeat:
	default:
		return false
	}
	switch self.tlsVersion {
	case TlsVersion1_3, TlsVersion1_2, TlsVersion1_1, TlsVersion1_0:
	default:
		return false
	}
	return true
}

// https://github.com/AGWA/tlshacks/blob/main/client_hello.go

type clientHelloMeta struct {
	ServerNameValueStart int
	ServerNameValueEnd   int
}

func UnmarshalClientHello(handshakeBytes []byte) (*tlshacks.ClientHelloInfo, *clientHelloMeta) {
	info := &tlshacks.ClientHelloInfo{
		Raw: handshakeBytes,
	}
	meta := &clientHelloMeta{}
	handshakeMessage := cryptobyte.String(handshakeBytes)

	handshakeMessageLength := len(handshakeMessage)

	var messageType uint8
	if !handshakeMessage.ReadUint8(&messageType) || messageType != 1 {
		// fmt.Printf("hello 1\n")
		return nil, nil
	}

	handshakeStart := handshakeMessageLength - len(handshakeMessage)

	var clientHello cryptobyte.String
	if !handshakeMessage.ReadUint24LengthPrefixed(&clientHello) || !handshakeMessage.Empty() {
		// fmt.Printf("hello 2\n")
		return nil, nil
	}

	clientHelloLength := len(clientHello)

	if !clientHello.ReadUint16((*uint16)(&info.Version)) {
		// fmt.Printf("hello 3\n")
		return nil, nil
	}

	if !clientHello.ReadBytes(&info.Random, 32) {
		// fmt.Printf("hello 4\n")
		return nil, nil
	}

	if !clientHello.ReadUint8LengthPrefixed((*cryptobyte.String)(&info.SessionID)) {
		// fmt.Printf("hello 5\n")
		return nil, nil
	}

	var cipherSuites cryptobyte.String
	if !clientHello.ReadUint16LengthPrefixed(&cipherSuites) {
		// fmt.Printf("hello 6\n")
		return nil, nil
	}
	info.CipherSuites = []tlshacks.CipherSuite{}
	for !cipherSuites.Empty() {
		// fmt.Printf("[tls]P1\n")
		var suite uint16
		if !cipherSuites.ReadUint16(&suite) {
			// fmt.Printf("hello 7\n")
			return nil, nil
		}
		info.CipherSuites = append(info.CipherSuites, tlshacks.MakeCipherSuite(suite))
	}

	var compressionMethods cryptobyte.String
	if !clientHello.ReadUint8LengthPrefixed(&compressionMethods) {
		// fmt.Printf("hello 8\n")
		return nil, nil
	}
	info.CompressionMethods = []tlshacks.CompressionMethod{}
	for !compressionMethods.Empty() {
		// fmt.Printf("[tls]P2\n")
		var method uint8
		if !compressionMethods.ReadUint8(&method) {
			// fmt.Printf("hello 9\n")
			return nil, nil
		}
		info.CompressionMethods = append(info.CompressionMethods, tlshacks.CompressionMethod(method))
	}

	info.Extensions = []tlshacks.Extension{}

	if clientHello.Empty() {
		// fmt.Printf("hello 10\n")
		return info, meta
	}

	clientHelloStart := clientHelloLength - len(clientHello)

	var extensions cryptobyte.String
	if !clientHello.ReadUint16LengthPrefixed(&extensions) {
		// fmt.Printf("hello 11\n")
		return nil, nil
	}
	extensionsLength := len(extensions)

	extensionParsers := map[uint16]func([]byte) tlshacks.ExtensionData{
		0:  tlshacks.ParseServerNameData,
		10: tlshacks.ParseSupportedGroupsData,
		11: tlshacks.ParseECPointFormatsData,
		16: tlshacks.ParseALPNData,
		18: tlshacks.ParseEmptyExtensionData,
		22: tlshacks.ParseEmptyExtensionData,
		23: tlshacks.ParseEmptyExtensionData,
		49: tlshacks.ParseEmptyExtensionData,
	}

	for !extensions.Empty() {
		// fmt.Printf("[tls]P3\n")
		var extType uint16
		var extData cryptobyte.String

		start := extensionsLength - len(extensions)
		if !extensions.ReadUint16(&extType) || !extensions.ReadUint16LengthPrefixed(&extData) {
			// fmt.Printf("hello 12\n")
			return nil, nil
		}
		end := extensionsLength - len(extensions)

		parseData := extensionParsers[extType]
		if parseData == nil {
			parseData = tlshacks.ParseUnknownExtensionData
		}
		data := parseData(extData)

		info.Extensions = append(info.Extensions, tlshacks.Extension{
			Type:    extType,
			Name:    tlshacks.Extensions[extType].Name,
			Grease:  tlshacks.Extensions[extType].Grease,
			Private: tlshacks.Extensions[extType].Private,
			Data:    data,
		})

		switch extType {
		case 0:
			info.Info.ServerName = &data.(*tlshacks.ServerNameData).HostName
			meta.ServerNameValueStart = handshakeStart + clientHelloStart + start
			meta.ServerNameValueEnd = handshakeStart + clientHelloStart + end
		case 16:
			info.Info.Protocols = data.(*tlshacks.ALPNData).Protocols
		case 18:
			info.Info.SCTs = true
		}

	}

	if !clientHello.Empty() {
		return nil, nil
	}

	// fmt.Printf("[tls]P4\n")

	info.Info.JA3String = tlshacks.JA3String(info)
	info.Info.JA3Fingerprint = tlshacks.JA3Fingerprint(info.Info.JA3String)

	// fmt.Printf("hello 14\n")
	return info, meta
}

func UnmarshalClientHelloServerName(handshakeBytes []byte) string {
	handshakeMessage := cryptobyte.String(handshakeBytes)

	var messageType uint8
	if !handshakeMessage.ReadUint8(&messageType) || messageType != 1 {
		// fmt.Printf("hello 1\n")
		return ""
	}

	var clientHello cryptobyte.String
	if !handshakeMessage.ReadUint24LengthPrefixed(&clientHello) || !handshakeMessage.Empty() {
		// fmt.Printf("hello 2\n")
		return ""
	}

	var version uint16
	if !clientHello.ReadUint16((*uint16)(&version)) {
		// fmt.Printf("hello 3\n")
		return ""
	}

	var random []byte
	if !clientHello.ReadBytes(&random, 32) {
		// fmt.Printf("hello 4\n")
		return ""
	}

	var sessionId cryptobyte.String
	if !clientHello.ReadUint8LengthPrefixed(&sessionId) {
		// fmt.Printf("hello 5\n")
		return ""
	}

	var cipherSuites cryptobyte.String
	if !clientHello.ReadUint16LengthPrefixed(&cipherSuites) {
		// fmt.Printf("hello 6\n")
		return ""
	}
	// info.CipherSuites = []tlshacks.CipherSuite{}
	// for !cipherSuites.Empty() {
	// 	fmt.Printf("[tls]P1\n")
	// 	var suite uint16
	// 	if !cipherSuites.ReadUint16(&suite) {
	// 		// fmt.Printf("hello 7\n")
	// 		return nil, nil
	// 	}
	// 	info.CipherSuites = append(info.CipherSuites, tlshacks.MakeCipherSuite(suite))
	// }

	var compressionMethods cryptobyte.String
	if !clientHello.ReadUint8LengthPrefixed(&compressionMethods) {
		// fmt.Printf("hello 8\n")
		return ""
	}
	// info.CompressionMethods = []tlshacks.CompressionMethod{}
	// for !compressionMethods.Empty() {
	// 	fmt.Printf("[tls]P2\n")
	// 	var method uint8
	// 	if !compressionMethods.ReadUint8(&method) {
	// 		// fmt.Printf("hello 9\n")
	// 		return nil, nil
	// 	}
	// 	info.CompressionMethods = append(info.CompressionMethods, tlshacks.CompressionMethod(method))
	// }

	// info.Extensions = []tlshacks.Extension{}

	if clientHello.Empty() {
		// fmt.Printf("hello 10\n")
		return ""
	}

	// clientHelloStart := clientHelloLength - len(clientHello)

	var extensions cryptobyte.String
	if !clientHello.ReadUint16LengthPrefixed(&extensions) {
		// fmt.Printf("hello 11\n")
		return ""
	}

	for !extensions.Empty() {
		var extType uint16
		var extData cryptobyte.String

		if !extensions.ReadUint16(&extType) || !extensions.ReadUint16LengthPrefixed(&extData) {
			// fmt.Printf("hello 12\n")
			return ""
		}

		switch extType {
		case 0:
			data := tlshacks.ParseServerNameData(extData)
			return data.(*tlshacks.ServerNameData).HostName
		}
	}

	return ""
}
