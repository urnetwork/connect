package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// The receiver-role TLS config has to offer the X25519MLKEM768 hybrid for a
// session to be post-quantum. A caller-supplied config that leaves it out
// serves classical sessions only, and does not advertise.
func TestServesPostQuantumEncryptionNeedsTheHybridGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	classical := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		ClientAuth:       tls.RequireAnyClientCert,
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
	}
	client := testingProvideClient(t, ctx, EncryptionModeOpportunistic, classical)
	AssertEqual(t, client.EncryptionSessionManager().ServesPostQuantumEncryption(), false)
	frame, err := client.ContractManager().provideFrame()
	AssertEqual(t, err, nil)
	advertised, _ := testingProvidePostQuantumEncryption(t, frame)
	AssertEqual(t, advertised, false)

	// the crypto/tls default list leads with the hybrid
	hybridDefault := classical.Clone()
	hybridDefault.CurvePreferences = nil
	client = testingProvideClient(t, ctx, EncryptionModeOpportunistic, hybridDefault)
	AssertEqual(t, client.EncryptionSessionManager().ServesPostQuantumEncryption(), true)
}

// Runs one TLS 1.3 handshake between the two configs over an in-memory pipe
// and returns the group it negotiated, or the first error either side saw.
func sequenceTlsHandshakeForTest(t *testing.T, clientConfig *tls.Config, serverConfig *tls.Config) (tls.CurveID, error) {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	defer clientPipe.Close()
	defer serverPipe.Close()
	clientConn := tls.Client(clientPipe, clientConfig)
	serverConn := tls.Server(serverPipe, serverConfig)
	// a liveness bound only: whether the configs share a group does not depend
	// on time
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		err := serverConn.HandshakeContext(ctx)
		if err != nil {
			// unblock a client still waiting for the server flight
			serverPipe.Close()
		}
		serverErr <- err
	}()
	clientErr := clientConn.HandshakeContext(ctx)
	if clientErr != nil {
		clientPipe.Close()
	}
	if err := errors.Join(clientErr, <-serverErr); err != nil {
		return 0, err
	}
	return clientConn.ConnectionState().CurveID, nil
}

// The TLS config a new session of the client's manager takes in the role.
func sessionTlsConfigForTest(client *Client, role sequenceTlsRole) *tls.Config {
	manager := client.EncryptionSessionManager()
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	return manager.sessionTlsConfigWithLock(role)
}

// Required is the post-quantum setting's mode, and its sessions must never seal
// with a classical group. Before 2026-11-15 they offered X25519 after the
// hybrid and accepted it from a peer without the hybrid, which sealed the
// session classically and carried post-quantum traffic under it. Both roles
// matter: a provider that returns traffic in a session of its own makes the
// window client the TLS server, choosing from the provider's offer.
func TestRequiredSessionsNegotiateOnlyPostQuantumGroups(t *testing.T) {
	required := newPeerCapabilityClientForTest(t, EncryptionModeRequired, nil)
	opportunistic := newPeerCapabilityClientForTest(t, EncryptionModeOpportunistic, nil)

	peerCertificate, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	// a peer without the hybrid group, in either role
	classicalServerConfig, err := DefaultSequenceServerTlsConfig()
	AssertEqual(t, err, nil)
	classicalServerConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	classicalClientConfig := DefaultSequenceClientTlsConfig()
	classicalClientConfig.Certificates = []tls.Certificate{peerCertificate}
	classicalClientConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	// a peer with the default groups, which lead with the hybrid
	defaultServerConfig, err := DefaultSequenceServerTlsConfig()
	AssertEqual(t, err, nil)
	defaultClientConfig := DefaultSequenceClientTlsConfig()
	defaultClientConfig.Certificates = []tls.Certificate{peerCertificate}

	if curve, err := sequenceTlsHandshakeForTest(t, sessionTlsConfigForTest(required, sequenceTlsRoleClient), classicalServerConfig); err == nil {
		t.Fatalf("a required initiator sealed with a classical responder: %s", tlsKeyExchangeName(curve))
	}
	if curve, err := sequenceTlsHandshakeForTest(t, classicalClientConfig, sessionTlsConfigForTest(required, sequenceTlsRoleServer)); err == nil {
		t.Fatalf("a required responder accepted a classical initiator: %s", tlsKeyExchangeName(curve))
	}

	curve, err := sequenceTlsHandshakeForTest(t, sessionTlsConfigForTest(required, sequenceTlsRoleClient), defaultServerConfig)
	AssertEqual(t, err, nil)
	AssertEqual(t, tls.X25519MLKEM768, curve)
	curve, err = sequenceTlsHandshakeForTest(t, defaultClientConfig, sessionTlsConfigForTest(required, sequenceTlsRoleServer))
	AssertEqual(t, err, nil)
	AssertEqual(t, tls.X25519MLKEM768, curve)

	// what crypto/tls is given: post-quantum groups only under Required
	for _, role := range []sequenceTlsRole{sequenceTlsRoleClient, sequenceTlsRoleServer} {
		AssertEqual(t, []tls.CurveID{tls.X25519MLKEM768}, sessionTlsConfigForTest(required, role).CurvePreferences)
		AssertEqual(t, DefaultSequencePostQuantumCurves, sessionTlsConfigForTest(opportunistic, role).CurvePreferences)
	}

	// opportunistic sessions, the providers', still serve classical peers
	curve, err = sequenceTlsHandshakeForTest(t, sessionTlsConfigForTest(opportunistic, sequenceTlsRoleClient), classicalServerConfig)
	AssertEqual(t, err, nil)
	AssertEqual(t, tls.X25519, curve)
}

// A required session whose caller-supplied config holds no post-quantum group
// fails its epoch closed: an emptied preference list would read as the
// crypto/tls defaults, which include classical groups.
func TestRequiredSessionWithoutPostQuantumGroupFailsClosed(t *testing.T) {
	classicalClientConfig := DefaultSequenceClientTlsConfig()
	classicalClientConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	client := newPeerCapabilityClientForTest(t, EncryptionModeRequired, nil, func(settings *ClientSettings) {
		settings.EncryptionSettings.ClientTlsConfig = classicalClientConfig
	})
	AssertEqual(t, true, sessionTlsConfigForTest(client, sequenceTlsRoleClient) == nil)

	peerId := NewId()
	session := client.EncryptionSessionManager().Acquire(peerId, sequenceTlsRoleClient, false)
	if session == nil {
		t.Fatal("no session under Required")
	}
	defer session.Release()
	session.startEpoch()
	epoch := session.currentEpoch()
	if epoch == nil {
		t.Fatal("no epoch")
	}
	select {
	case <-epoch.handshakeDone:
	default:
		t.Fatal("the epoch did not fail closed at build")
	}
	if epoch.handshakeErr == nil || session.Cipher() != nil {
		t.Fatalf("a session with no post-quantum group established: %v", epoch.handshakeErr)
	}
}
