package connect

// vless_reality.go — the client side of the reality security.
//
// Reality borrows the tls handshake of a real site (the server name) so the
// connection looks like a visit to it. The client imitates a browser hello
// and hides its proof of the server's key in the 32-byte legacy session id:
//
//	auth key   = hkdf-sha256(x25519(client key share, server public key),
//	             salt = hello random[:20], info = "REALITY")
//	session id = aes-gcm(auth key, nonce = hello random[20:],
//	             plaintext = version (3) | 0 | unix time (4) | short id (8),
//	             aad = the client hello with a zero session id)
//
// A server that recognizes the session id answers with a throwaway ed25519
// certificate whose signature field is hmac-sha512(auth key, its public key);
// any other server is the real site, whose certificate verifies the ordinary
// way. Only the first proves the key, so the dial succeeds only then: a VLESS
// request is never sent to the real site.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The client version reality servers may bound with min and max client
// versions; a recent Xray release, which those bounds are written against.
var vlessRealityClientVersion = [3]byte{25, 10, 15}

// The offset of the legacy session id in a marshaled client hello: the
// handshake header (4), the legacy version (2), the random (32) and the
// session id length (1).
const vlessRealitySessionIdOffset = 4 + 2 + 32 + 1

// The browser hello of a fingerprint name. "random" draws one of the browser
// hellos for each dial.
func vlessClientHelloId(fingerprint string) (utls.ClientHelloID, error) {
	switch fingerprint {
	case "chrome":
		return utls.HelloChrome_Auto, nil
	case "firefox":
		return utls.HelloFirefox_Auto, nil
	case "safari":
		return utls.HelloSafari_Auto, nil
	case "ios":
		return utls.HelloIOS_Auto, nil
	case "android":
		return utls.HelloAndroid_11_OkHttp, nil
	case "edge":
		return utls.HelloEdge_Auto, nil
	case "360":
		return utls.Hello360_Auto, nil
	case "qq":
		return utls.HelloQQ_Auto, nil
	case "randomized":
		return utls.HelloRandomized, nil
	case "random":
		helloIds := []utls.ClientHelloID{
			utls.HelloChrome_Auto,
			utls.HelloFirefox_Auto,
			utls.HelloSafari_Auto,
			utls.HelloIOS_Auto,
			utls.HelloEdge_Auto,
		}
		return helloIds[rand.IntN(len(helloIds))], nil
	}
	return utls.ClientHelloID{}, &VlessConfigError{Code: VlessErrorFingerprintUnsupported, Detail: fingerprint}
}

// The session id plaintext: the client version, a reserved zero, the unix
// time and the short id.
func vlessRealitySessionIdPlaintext(shortId []byte, now time.Time) [16]byte {
	var plaintext [16]byte
	copy(plaintext[0:3], vlessRealityClientVersion[:])
	plaintext[3] = 0
	binary.BigEndian.PutUint32(plaintext[4:8], uint32(now.Unix()))
	copy(plaintext[8:16], shortId)
	return plaintext
}

// The auth key both ends derive from the x25519 exchange.
func vlessRealityAuthKey(sharedSecret []byte, helloRandom []byte) ([]byte, error) {
	if len(helloRandom) != 32 {
		return nil, errors.New("reality: client hello random must be 32 bytes")
	}
	return hkdf.Key(sha256.New, sharedSecret, helloRandom[:20], "REALITY", 32)
}

// Seals the session id into the marshaled hello, in place. `hello` must hold
// a zero session id at its offset when called, since it is the aad.
func vlessRealitySealSessionId(authKey []byte, helloRaw []byte, helloRandom []byte, plaintext [16]byte) ([]byte, error) {
	if len(helloRaw) < vlessRealitySessionIdOffset+32 {
		return nil, errors.New("reality: client hello too short")
	}
	block, err := aes.NewCipher(authKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sessionId := aead.Seal(make([]byte, 0, 32), helloRandom[20:], plaintext[:], helloRaw)
	copy(helloRaw[vlessRealitySessionIdOffset:], sessionId)
	return sessionId, nil
}

// Whether the leaf proves the auth key: an ed25519 certificate whose signature
// field is hmac-sha512(auth key, its public key).
func vlessRealityLeafProvesKey(leaf *x509.Certificate, authKey []byte) bool {
	publicKey, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || len(authKey) == 0 {
		return false
	}
	mac := hmac.New(sha512.New, authKey)
	mac.Write(publicKey)
	return hmac.Equal(mac.Sum(nil), leaf.Signature)
}

// Runs the reality handshake over conn and returns the outer connection. The
// connection is closed on error.
func vlessRealityClient(
	ctx context.Context,
	conn net.Conn,
	config *VlessConfig,
	handshakeTimeout time.Duration,
) (net.Conn, error) {
	fingerprint := config.Fingerprint
	if fingerprint == "" {
		fingerprint = "chrome"
	}
	helloId, err := vlessClientHelloId(fingerprint)
	if err != nil {
		conn.Close()
		return nil, err
	}
	serverPublicKey, err := ecdh.X25519().NewPublicKey(config.PublicKey)
	if err != nil {
		conn.Close()
		return nil, &VlessConfigError{Code: VlessErrorPublicKeyInvalid}
	}

	// set before the handshake, read by the verifier inside it on this
	// goroutine
	var authKey []byte
	verified := false
	uconfig := &utls.Config{
		ServerName: config.ServerName,
		// the leaf is checked below: either it proves the key, or it is the
		// real site's and is verified as a browser would
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("reality: no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			if vlessRealityLeafProvesKey(leaf, authKey) {
				verified = true
				return nil
			}
			options := x509.VerifyOptions{
				DNSName:       config.ServerName,
				Intermediates: x509.NewCertPool(),
			}
			for _, rawCert := range rawCerts[1:] {
				if cert, err := x509.ParseCertificate(rawCert); err == nil {
					options.Intermediates.AddCert(cert)
				}
			}
			_, err = leaf.Verify(options)
			return err
		},
	}
	uconn := utls.UClient(conn, uconfig, helloId)
	success := false
	defer func() {
		if !success {
			uconn.Close()
		}
	}()

	if err := uconn.BuildHandshakeState(); err != nil {
		return nil, err
	}
	hello := uconn.HandshakeState.Hello
	if len(hello.Raw) < vlessRealitySessionIdOffset+32 || len(hello.SessionId) != 32 || len(hello.Random) != 32 {
		return nil, fmt.Errorf("reality: fingerprint %s has no 32-byte session id", fingerprint)
	}
	keyShareKeys := uconn.HandshakeState.State13.KeyShareKeys
	var clientKey *ecdh.PrivateKey
	if keyShareKeys != nil {
		clientKey = keyShareKeys.Ecdhe
		if clientKey == nil {
			clientKey = keyShareKeys.MlkemEcdhe
		}
	}
	if clientKey == nil || clientKey.Curve() != ecdh.X25519() {
		return nil, fmt.Errorf("reality: fingerprint %s offers no x25519 tls 1.3 key share", fingerprint)
	}
	sharedSecret, err := clientKey.ECDH(serverPublicKey)
	if err != nil {
		return nil, err
	}
	authKey, err = vlessRealityAuthKey(sharedSecret, hello.Random)
	if err != nil {
		return nil, err
	}
	// the aad is the hello with a zero session id
	copy(hello.Raw[vlessRealitySessionIdOffset:vlessRealitySessionIdOffset+32], make([]byte, 32))
	sessionId, err := vlessRealitySealSessionId(
		authKey,
		hello.Raw,
		hello.Random,
		vlessRealitySessionIdPlaintext(config.ShortId, time.Now()),
	)
	if err != nil {
		return nil, err
	}
	hello.SessionId = sessionId

	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer handshakeCancel()
	if err := uconn.HandshakeContext(handshakeCtx); err != nil {
		return nil, err
	}
	if !verified {
		// the real site answered: this is not, or no longer, a reality server
		// for this key
		return nil, errors.New("reality: the server did not prove the configured public key")
	}
	success = true
	return uconn, nil
}
